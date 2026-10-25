package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// fakeClock is a settable time source for the aging / progress / ETA tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)} }

func prio(owner, class string) api.ReserveRequest {
	r := req(owner)
	r.Priority = class
	return r
}

func prioTyped(owner, class, typ string) api.ReserveRequest {
	r := prio(owner, class)
	r.UnitType = typ
	return r
}

// newPrioManager builds a manager over units with a fake clock and a long lease
// (the tests move the clock by hours; leases are not under test here).
func newPrioManager(t *testing.T, units []runner.Unit, opts ...Option) (*Manager, *fakeRunner, *fakeClock) {
	t.Helper()
	fr := newFakeRunner()
	clk := newClock()
	all := append([]Option{WithUnits(units), WithClock(clk.now)}, opts...)
	return New("h", 24*time.Hour, fr, all...), fr, clk
}

func get(t *testing.T, m *Manager, id string) *api.Reservation {
	t.Helper()
	v, err := m.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return v
}

func mustRelease(t *testing.T, m *Manager, id string) {
	t.Helper()
	if err := m.Release(context.Background(), id, "done"); err != nil {
		t.Fatalf("release %s: %v", id, err)
	}
}

// Higher classes go first: with the unit busy, a dev, a pr-ci and a merge-queue
// waiter (arriving in that order, within the aging windows) activate merge-queue,
// then pr-ci, then dev.
func TestPriorityOrdersQueueByClass(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0"}})
	ctx := context.Background()

	holder := m.Reserve(ctx, prio("holder", api.PriorityDev))
	clk.advance(time.Second)
	dev := m.Reserve(ctx, prio("dev", api.PriorityDev))
	clk.advance(time.Minute)
	ci := m.Reserve(ctx, prio("ci", api.PriorityPRCI))
	clk.advance(time.Minute)
	mq := m.Reserve(ctx, prio("mq", api.PriorityMergeQueue))

	if p := get(t, m, mq.ID).Position; p != 1 {
		t.Fatalf("merge-queue waiter should be next in line, position=%d", p)
	}
	if p := get(t, m, ci.ID).Position; p != 2 {
		t.Fatalf("pr-ci waiter position=%d, want 2", p)
	}
	if p := get(t, m, dev.ID).Position; p != 3 {
		t.Fatalf("dev waiter position=%d, want 3", p)
	}

	want := []string{mq.ID, ci.ID, dev.ID}
	prev := holder.ID
	for _, id := range want {
		mustRelease(t, m, prev)
		if s := state(t, m, id); s != api.StateActive {
			t.Fatalf("after releasing %s expected %s active, got %s", prev, id, s)
		}
		prev = id
	}
}

// Within one class the queue is strict FIFO (all-dev == the old FIFO behaviour).
func TestPrioritySameClassIsFIFO(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0"}})
	ctx := context.Background()
	holder := m.Reserve(ctx, prio("holder", api.PriorityPRCI))
	var ids []string
	for _, o := range []string{"a", "b", "c"} {
		clk.advance(time.Second)
		ids = append(ids, m.Reserve(ctx, prio(o, api.PriorityPRCI)).ID)
	}
	prev := holder.ID
	for _, id := range ids {
		mustRelease(t, m, prev)
		if state(t, m, id) != api.StateActive {
			t.Fatalf("FIFO broken: %s should be active after %s", id, prev)
		}
		prev = id
	}
}

// Aging bounds how long CI can keep dev waiting: a merge-queue reservation that
// arrives within dev's 60 min offset goes ahead of it, one that arrives after the
// offset has elapsed queues BEHIND it — dev can't starve.
func TestAgingBoundsDevWait(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0"}})
	ctx := context.Background()

	holder := m.Reserve(ctx, prio("holder", api.PriorityMergeQueue))
	dev := m.Reserve(ctx, prio("dev", api.PriorityDev))

	clk.advance(59 * time.Minute)
	early := m.Reserve(ctx, prio("mq-early", api.PriorityMergeQueue))
	clk.advance(2 * time.Minute) // dev has now waited 61 min > its 60 min offset
	late := m.Reserve(ctx, prio("mq-late", api.PriorityMergeQueue))
	lateCI := m.Reserve(ctx, prio("ci-late", api.PriorityPRCI))

	if p := get(t, m, early.ID).Position; p != 1 {
		t.Fatalf("merge-queue inside dev's aging window should be ahead of dev, position=%d", p)
	}
	if p := get(t, m, dev.ID).Position; p != 2 {
		t.Fatalf("dev should be second, position=%d", p)
	}
	if p := get(t, m, late.ID).Position; p != 3 {
		t.Fatalf("merge-queue arriving after dev aged should queue behind it, position=%d", p)
	}
	if p := get(t, m, lateCI.ID).Position; p != 4 {
		t.Fatalf("pr-ci arriving after dev aged should queue behind it too, position=%d", p)
	}

	mustRelease(t, m, holder.ID)
	if state(t, m, early.ID) != api.StateActive {
		t.Fatal("early merge-queue should activate first")
	}
	mustRelease(t, m, early.ID)
	if state(t, m, dev.ID) != api.StateActive {
		t.Fatal("aged dev reservation should activate before later merge-queue arrivals")
	}
}

// The aging offsets are configurable; merge-queue's is pinned at 0.
func TestPriorityAgingOption(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0"}},
		WithPriorityAging(map[string]time.Duration{"dev": 5 * time.Minute, "merge-queue": time.Hour}))
	ctx := context.Background()
	m.Reserve(ctx, prio("holder", api.PriorityDev))
	dev := m.Reserve(ctx, prio("dev", api.PriorityDev))
	clk.advance(6 * time.Minute)
	mq := m.Reserve(ctx, prio("mq", api.PriorityMergeQueue))
	if get(t, m, dev.ID).Position != 1 || get(t, m, mq.ID).Position != 2 {
		t.Fatalf("with dev=5m aging a merge-queue arriving 6m later must queue behind dev")
	}
	st := m.Status()
	if st.PriorityAgingSeconds[api.PriorityMergeQueue] != 0 || st.PriorityAgingSeconds[api.PriorityDev] != 300 ||
		st.PriorityAgingSeconds[api.PriorityPRCI] != int(DefaultAging[api.PriorityPRCI].Seconds()) {
		t.Fatalf("aging in status = %v", st.PriorityAgingSeconds)
	}
}

// No preemption: a merge-queue arrival never displaces an active dev holder — it
// waits for the release like anyone else.
func TestPriorityNeverPreemptsActive(t *testing.T) {
	m, fr, clk := newPrioManager(t, []runner.Unit{{Name: "u0"}, {Name: "u1"}})
	ctx := context.Background()
	a := m.Reserve(ctx, prio("dev-a", api.PriorityDev))
	b := m.Reserve(ctx, prio("dev-b", api.PriorityDev))
	clk.advance(3 * time.Hour)
	mq := m.Reserve(ctx, prio("mq", api.PriorityMergeQueue))
	m.ReapExpired(ctx) // nothing has lapsed (24h lease); must not disturb anyone

	for _, id := range []string{a.ID, b.ID} {
		if state(t, m, id) != api.StateActive {
			t.Fatalf("active dev reservation %s was disturbed by a merge-queue arrival", id)
		}
		if fr.stopped[id] {
			t.Fatalf("runner.Stop called on active %s: preemption", id)
		}
	}
	if v := get(t, m, mq.ID); v.State != api.StateQueued || v.Position != 1 {
		t.Fatalf("merge-queue should wait at position 1, got %s/%d", v.State, v.Position)
	}
	mustRelease(t, m, b.ID)
	if state(t, m, mq.ID) != api.StateActive {
		t.Fatal("merge-queue should take the next freed unit")
	}
}

// An absent or unknown class is dev; aliases normalize; the effective class is
// echoed back.
func TestPriorityNormalization(t *testing.T) {
	m, _, _ := newPrioManager(t, []runner.Unit{{Name: "u0"}})
	ctx := context.Background()
	cases := map[string]string{
		"":            api.PriorityDev,
		"bogus":       api.PriorityDev,
		"merge_group": api.PriorityMergeQueue,
		"MQ":          api.PriorityMergeQueue,
		"ci":          api.PriorityPRCI,
		"pr-ci":       api.PriorityPRCI,
	}
	for in, want := range cases {
		r := m.Reserve(ctx, prio("x", in))
		if r.Priority != want {
			t.Errorf("priority %q -> %q, want %q", in, r.Priority, want)
		}
	}
	st := m.Status()
	if st.QueueByClass[api.PriorityDev]+st.QueueByClass[api.PriorityMergeQueue]+st.QueueByClass[api.PriorityPRCI] != st.QueueLength {
		t.Fatalf("queue_by_class %v doesn't sum to queue_length %d", st.QueueByClass, st.QueueLength)
	}
}

// Priority order composes with per-unit-class caps: a higher-class waiter blocked
// by its unit class's cap is skipped, and the next runnable waiter (by priority
// order) takes the free unit of another class.
func TestPriorityWithClassCaps(t *testing.T) {
	m, _, clk := newPrioManager(t, rig1Units(), WithMaxConcurrentPerClass(map[string]int{"esp32c6": 1}))
	ctx := context.Background()
	m.Reserve(ctx, prioTyped("c6-holder", api.PriorityDev, "esp32c6"))
	clk.advance(time.Second)
	c6mq := m.Reserve(ctx, prioTyped("c6-mq", api.PriorityMergeQueue, "esp32c6"))
	piDev := m.Reserve(ctx, prioTyped("pi-dev", api.PriorityDev, "led-mapper-pi"))
	if state(t, m, c6mq.ID) != api.StateQueued {
		t.Fatal("c6 merge-queue waiter should be held by the esp32c6 cap")
	}
	if state(t, m, piDev.ID) != api.StateActive {
		t.Fatal("dev Pi waiter should run on a free Pi despite the higher-class c6 waiter")
	}
}

// Position counts only waiters competing for the same units.
func TestPositionCountsCompetingWaitersOnly(t *testing.T) {
	m, _, clk := newPrioManager(t, rig1Units())
	ctx := context.Background()
	for _, o := range []string{"c6-1", "c6-2"} {
		m.Reserve(ctx, prioTyped(o, api.PriorityPRCI, "esp32c6"))
	}
	for _, o := range []string{"pi-1", "pi-2", "pi-3", "pi-4"} {
		clk.advance(time.Second)
		m.Reserve(ctx, prioTyped(o, api.PriorityPRCI, "led-mapper-pi"))
	}
	clk.advance(time.Second)
	c6 := m.Reserve(ctx, prioTyped("c6-3", api.PriorityPRCI, "esp32c6"))
	if p := get(t, m, c6.ID).Position; p != 1 {
		t.Fatalf("queued Pi waiters must not count against a c6 waiter, position=%d", p)
	}
}

// Progress: the clock runs while nothing happens, resets when a usable unit frees
// or the position drops, does NOT reset when a higher class jumps ahead, freezes
// while cordoned and restarts on uncordon.
func TestQueueProgressClock(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0"}, {Name: "u1"}})
	ctx := context.Background()
	h0 := m.Reserve(ctx, prio("h0", api.PriorityPRCI))
	h1 := m.Reserve(ctx, prio("h1", api.PriorityPRCI))
	w1 := m.Reserve(ctx, prio("w1", api.PriorityDev))
	w2 := m.Reserve(ctx, prio("w2", api.PriorityDev))

	clk.advance(10 * time.Minute)
	v := get(t, m, w2.ID)
	if v.SinceProgressSeconds != 600 || v.ProgressAt == nil || v.Priority != api.PriorityDev {
		t.Fatalf("stalled 10m: since_progress=%d progress_at=%v", v.SinceProgressSeconds, v.ProgressAt)
	}

	// A higher class jumping ahead is NOT progress for w2 (its position grows).
	clk.advance(time.Minute)
	mq := m.Reserve(ctx, prio("mq", api.PriorityMergeQueue))
	if v := get(t, m, w2.ID); v.Position != 3 || v.SinceProgressSeconds != 660 {
		t.Fatalf("after a merge-queue arrival: position=%d since=%d (want 3, 660)", v.Position, v.SinceProgressSeconds)
	}

	// A unit frees (mq takes it): the queue drained for w2 -> progress.
	clk.advance(time.Minute)
	mustRelease(t, m, h0.ID)
	if state(t, m, mq.ID) != api.StateActive {
		t.Fatal("mq should have taken the freed unit")
	}
	if v := get(t, m, w2.ID); v.SinceProgressSeconds != 0 || v.Position != 2 {
		t.Fatalf("unit freed: since=%d position=%d (want 0, 2)", v.SinceProgressSeconds, v.Position)
	}

	// Cordoned: releases drain but are not progress for waiters.
	clk.advance(5 * time.Minute)
	m.Cordon()
	mustRelease(t, m, h1.ID)
	v = get(t, m, w2.ID)
	if v.SinceProgressSeconds != 300 || v.Blocked != api.BlockedCordoned || v.EtaSeconds != nil {
		t.Fatalf("cordoned: since=%d blocked=%q eta=%v", v.SinceProgressSeconds, v.Blocked, v.EtaSeconds)
	}

	// Uncordon restarts the clock and w1 activates (w2's position drops).
	clk.advance(time.Hour)
	m.Uncordon(ctx)
	if state(t, m, w1.ID) != api.StateActive {
		t.Fatal("w1 should activate on uncordon")
	}
	if v := get(t, m, w2.ID); v.SinceProgressSeconds != 0 || v.Blocked != "" || v.Position != 1 {
		t.Fatalf("after uncordon: since=%d blocked=%q position=%d", v.SinceProgressSeconds, v.Blocked, v.Position)
	}

	// Position drop alone (a waiter ahead gives up) is progress.
	w3 := m.Reserve(ctx, prio("w3", api.PriorityDev))
	clk.advance(2 * time.Minute)
	mustRelease(t, m, w2.ID) // queued w2 abandons; w3 moves up
	if v := get(t, m, w3.ID); v.SinceProgressSeconds != 0 || v.Position != 1 {
		t.Fatalf("waiter ahead left: since=%d position=%d", v.SinceProgressSeconds, v.Position)
	}
}

// Hot-plugging a unit a waiter can use is progress; a waiter whose unit is gone is
// reported blocked (no-unit).
func TestProgressOnHotplugAndBlockedNoUnit(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0", Type: "esp32c6"}})
	ctx := context.Background()
	m.Reserve(ctx, prioTyped("h", api.PriorityDev, "esp32c6"))
	sdr := m.Reserve(ctx, prioTyped("sdr", api.PriorityDev, "hackrf"))
	if v := get(t, m, sdr.ID); v.Blocked != api.BlockedNoUnit || v.EtaSeconds != nil {
		t.Fatalf("no hackrf unit: blocked=%q eta=%v", v.Blocked, v.EtaSeconds)
	}
	w := m.Reserve(ctx, prioTyped("w", api.PriorityDev, "esp32c6"))
	clk.advance(7 * time.Minute)
	m.SyncUnits(ctx, []runner.Unit{{Name: "u0", Type: "esp32c6"}, {Name: "u1", Type: "esp32c6"}})
	if v := get(t, m, w.ID); v.State != api.StateActive {
		t.Fatalf("w should land on the hot-plugged unit, state=%s", v.State)
	}
	if v := get(t, m, sdr.ID); v.SinceProgressSeconds != 420 {
		t.Fatalf("an unrelated unit appearing is not progress for sdr: since=%d", v.SinceProgressSeconds)
	}
}

// ETA: a list-scheduling estimate from the holders' remaining time and the mean
// hold of the unit type, learned from completed reservations.
func TestQueueETA(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0", Type: "esp32c6"}}, WithDefaultHold(10*time.Minute))
	ctx := context.Background()
	h := m.Reserve(ctx, prio("h", api.PriorityPRCI))
	clk.advance(4 * time.Minute)
	w1 := m.Reserve(ctx, prio("w1", api.PriorityPRCI))
	w2 := m.Reserve(ctx, prio("w2", api.PriorityPRCI))

	eta := func(id string) int {
		v := get(t, m, id)
		if v.EtaSeconds == nil {
			t.Fatalf("%s: no ETA", id)
		}
		return *v.EtaSeconds
	}
	if got := eta(w1.ID); got != 6*60 {
		t.Fatalf("w1 eta=%ds, want 360 (10m hold - 4m elapsed)", got)
	}
	if got := eta(w2.ID); got != 16*60 {
		t.Fatalf("w2 eta=%ds, want 960 (one more 10m hold)", got)
	}
	if v := get(t, m, h.ID); v.EtaSeconds != nil || v.Position != 0 {
		t.Fatalf("active reservation should carry no ETA and position 0: %+v", v)
	}

	// The holder finishes after 20m: the mean becomes 20m (first sample).
	clk.advance(16 * time.Minute)
	mustRelease(t, m, h.ID)
	if got := eta(w2.ID); got != 20*60 {
		t.Fatalf("after a 20m hold, w2 eta=%ds, want 1200", got)
	}
	// A holder past the mean is assumed to finish within a minute, not already done.
	clk.advance(30 * time.Minute)
	if got := eta(w2.ID); got != 60 {
		t.Fatalf("overdue holder: w2 eta=%ds, want the 60s floor", got)
	}
	// A lease expiry is not a real hold and does not move the mean.
	m.mu.Lock()
	before := m.holdEstimateLocked("esp32c6")
	m.mu.Unlock()
	if err := m.Release(ctx, w1.ID, reasonLeaseExpired); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	after := m.holdEstimateLocked("esp32c6")
	m.mu.Unlock()
	if before != after {
		t.Fatalf("lease expiry moved the hold estimate %s -> %s", before, after)
	}
}

// ETA uses every competing unit in parallel, bounded by the class cap.
func TestQueueETAParallelUnitsAndCaps(t *testing.T) {
	units := []runner.Unit{{Name: "a", Type: "esp32c6"}, {Name: "b", Type: "esp32c6"}}
	m, _, _ := newPrioManager(t, units, WithDefaultHold(10*time.Minute))
	ctx := context.Background()
	m.Reserve(ctx, prio("h1", api.PriorityPRCI))
	m.Reserve(ctx, prio("h2", api.PriorityPRCI))
	w1 := m.Reserve(ctx, prio("w1", api.PriorityPRCI))
	w2 := m.Reserve(ctx, prio("w2", api.PriorityPRCI))
	w3 := m.Reserve(ctx, prio("w3", api.PriorityPRCI))
	for id, want := range map[string]int{w1.ID: 600, w2.ID: 600, w3.ID: 1200} {
		if v := get(t, m, id); v.EtaSeconds == nil || *v.EtaSeconds != want {
			t.Fatalf("%s eta=%v want %d", v.Owner, v.EtaSeconds, want)
		}
	}

	capped, _, _ := newPrioManager(t, units, WithDefaultHold(10*time.Minute),
		WithMaxConcurrentPerClass(map[string]int{"esp32c6": 1}))
	capped.Reserve(ctx, prio("h1", api.PriorityPRCI))
	x1 := capped.Reserve(ctx, prio("x1", api.PriorityPRCI))
	x2 := capped.Reserve(ctx, prio("x2", api.PriorityPRCI))
	if v := get(t, capped, x2.ID); v.EtaSeconds == nil || *v.EtaSeconds != 1200 {
		t.Fatalf("cap 1 serializes the 2 units: x2 eta=%v want 1200", v.EtaSeconds)
	}
	_ = x1
}

// GET /queue's model: per-class depth/oldest wait, per-type ETA for a newcomer of
// each class, and the dev back-off hint.
func TestQueueSummary(t *testing.T) {
	m, _, clk := newPrioManager(t, rig1Units(), WithDefaultHold(30*time.Minute))
	ctx := context.Background()
	m.Reserve(ctx, prioTyped("h1", api.PriorityPRCI, "esp32c6"))
	m.Reserve(ctx, prioTyped("h2", api.PriorityPRCI, "esp32c6"))
	clk.advance(time.Minute)
	m.Reserve(ctx, prioTyped("q-ci", api.PriorityPRCI, "esp32c6"))
	m.Reserve(ctx, prioTyped("q-ci2", api.PriorityPRCI, "esp32c6"))
	clk.advance(time.Minute)
	m.Reserve(ctx, prioTyped("q-dev", api.PriorityDev, "esp32c6"))

	q := m.QueueSummary()
	if q.QueueLength != 3 || len(q.Classes) != 3 {
		t.Fatalf("summary: %+v", q)
	}
	byClass := map[string]api.ClassQueue{}
	for _, c := range q.Classes {
		byClass[c.Class] = c
	}
	if byClass[api.PriorityPRCI].Queued != 2 || byClass[api.PriorityPRCI].OldestWaitSeconds != 60 ||
		byClass[api.PriorityDev].Queued != 1 || byClass[api.PriorityMergeQueue].Queued != 0 {
		t.Fatalf("classes: %+v", q.Classes)
	}
	var c6, pi api.TypeQueue
	for _, tq := range q.Types {
		switch tq.Type {
		case "esp32c6":
			c6 = tq
		case "led-mapper-pi":
			pi = tq
		}
	}
	if c6.Units != 2 || c6.Free != 0 || c6.Queued != 3 || c6.HoldEstimateSeconds != 1800 {
		t.Fatalf("esp32c6: %+v", c6)
	}
	// Two units busy for 28m more. A merge-queue newcomer goes ahead of everyone
	// queued (all arrived within the aging windows): 28m. A pr-ci newcomer is
	// behind both pr-ci waiters, which take the two slots: 58m. A dev newcomer is
	// behind all three; the third takes a slot at 58m, but the other slot frees
	// at 58m too: 58m.
	if c6.EtaNewSeconds[api.PriorityMergeQueue] != 28*60 ||
		c6.EtaNewSeconds[api.PriorityPRCI] != 58*60 ||
		c6.EtaNewSeconds[api.PriorityDev] != 58*60 {
		t.Fatalf("eta-new: %v", c6.EtaNewSeconds)
	}
	if c6.RetryAfterSeconds != 600 {
		t.Fatalf("dev should be told to back off 10m, got %d", c6.RetryAfterSeconds)
	}
	if pi.Free != 2 || pi.EtaNewSeconds[api.PriorityDev] != 0 || pi.RetryAfterSeconds != 0 {
		t.Fatalf("idle Pis should say go ahead: %+v", pi)
	}

	m.Cordon()
	q = m.QueueSummary()
	if !q.Cordoned || q.Types[0].EtaNewSeconds != nil || q.Types[0].RetryAfterSeconds == 0 {
		t.Fatalf("cordoned summary should drop ETAs and say back off: %+v", q)
	}
}

func TestRetryAfterConvention(t *testing.T) {
	for eta, want := range map[time.Duration]time.Duration{
		0:                0,
		10 * time.Minute: 0,
		11 * time.Minute: 330 * time.Second,
		time.Hour:        10 * time.Minute,
	} {
		if got := RetryAfter(eta); got != want {
			t.Errorf("RetryAfter(%s)=%s want %s", eta, got, want)
		}
	}
}

// Metrics: per-class queue depth and the oldest waiter's age.
func TestPriorityClassGauges(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "u0"}})
	ctx := context.Background()
	m.Reserve(ctx, prio("h", api.PriorityDev))
	m.Reserve(ctx, prio("mq", api.PriorityMergeQueue))
	clk.advance(30 * time.Second)
	m.Reserve(ctx, prio("dev", api.PriorityDev))
	clk.advance(60 * time.Second)

	snap := m.Metrics()
	if len(snap.Classes) != 3 {
		t.Fatalf("want 3 classes, got %d", len(snap.Classes))
	}
	by := map[string]ClassMetric{}
	for _, c := range snap.Classes {
		by[c.Class] = c
	}
	if by[api.PriorityMergeQueue].Queued != 1 || by[api.PriorityMergeQueue].OldestWaitSeconds != 90 ||
		by[api.PriorityDev].Queued != 1 || by[api.PriorityDev].OldestWaitSeconds != 60 ||
		by[api.PriorityPRCI].Queued != 0 || by[api.PriorityPRCI].OldestWaitSeconds != 0 {
		t.Fatalf("class gauges: %+v", snap.Classes)
	}
	if !strings.HasPrefix(api.PriorityMergeQueue, "merge") {
		t.Fatal("sanity")
	}
}
