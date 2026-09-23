package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// fakeRunner records Start/Stop and hands out a deterministic endpoint per unit.
type fakeRunner struct {
	mu        sync.Mutex
	started   map[string]string // reservation id -> unit name
	stopped   map[string]bool
	failUnit  string // Start fails for this unit name
	startErrs int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{started: map[string]string{}, stopped: map[string]bool{}}
}

func (f *fakeRunner) Start(_ context.Context, id, _, _ string, unit runner.Unit) (*api.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if unit.Name == f.failUnit {
		f.startErrs++
		return nil, fmt.Errorf("boom")
	}
	f.started[id] = unit.Name
	return &api.Endpoint{Host: "h", Port: 1000 + len(f.started), User: "agent"}, nil
}

func (f *fakeRunner) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped[id] = true
	delete(f.started, id)
	return nil
}

func (f *fakeRunner) Cleanup(context.Context) error { return nil }

func req(owner string) api.ReserveRequest {
	return api.ReserveRequest{Owner: owner, SSHPublicKey: "ssh-ed25519 AAAA test"}
}

func TestSingleUnitFIFO(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	if a.State != api.StateActive {
		t.Fatalf("first reservation should be active, got %s", a.State)
	}
	b := m.Reserve(ctx, req("b"))
	if b.State != api.StateQueued || b.Position != 1 {
		t.Fatalf("second should be queued at position 1, got state=%s pos=%d", b.State, b.Position)
	}

	if err := m.Release(ctx, a.ID, "done"); err != nil {
		t.Fatal(err)
	}
	bv, _ := m.Get(b.ID)
	if bv.State != api.StateActive {
		t.Fatalf("after release, b should be active, got %s", bv.State)
	}
}

func TestMultiUnitConcurrent(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	c := m.Reserve(ctx, req("c"))
	if a.State != api.StateActive || b.State != api.StateActive {
		t.Fatalf("both units should be filled: a=%s b=%s", a.State, b.State)
	}
	if a.Unit == b.Unit {
		t.Fatalf("a and b landed on the same unit %q", a.Unit)
	}
	if c.State != api.StateQueued {
		t.Fatalf("c should be queued, got %s", c.State)
	}
	m.Release(ctx, a.ID, "done")
	cv, _ := m.Get(c.ID)
	if cv.State != api.StateActive || cv.Unit != a.Unit {
		t.Fatalf("c should promote onto freed unit %q, got state=%s unit=%s", a.Unit, cv.State, cv.Unit)
	}
}

func TestMaxConcurrentCapsActive(t *testing.T) {
	fr := newFakeRunner()
	// Four free units but a host cap of 2: only two may be active at once.
	m := New("h", time.Minute, fr,
		WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}, {Name: "u2"}, {Name: "u3"}}),
		WithMaxConcurrent(2))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	c := m.Reserve(ctx, req("c"))
	if a.State != api.StateActive || b.State != api.StateActive {
		t.Fatalf("first two should be active: a=%s b=%s", a.State, b.State)
	}
	if c.State != api.StateQueued {
		t.Fatalf("c should be queued behind the cap even though units are free, got %s", c.State)
	}
	// Releasing an active one frees a concurrency slot; the queued waiter starts.
	m.Release(ctx, a.ID, "done")
	cv, _ := m.Get(c.ID)
	if cv.State != api.StateActive {
		t.Fatalf("after release, c should promote into the freed slot, got %s", cv.State)
	}
	// Still capped at 2: a fourth request waits.
	d := m.Reserve(ctx, req("d"))
	if d.State != api.StateQueued {
		t.Fatalf("d should stay queued (cap=2, two active), got %s", d.State)
	}
}

func TestMaxConcurrentZeroUnlimited(t *testing.T) {
	fr := newFakeRunner()
	// Cap 0 is the default: every free unit is usable (no cap regression).
	m := New("h", time.Minute, fr,
		WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}, {Name: "u2"}}),
		WithMaxConcurrent(0))
	ctx := context.Background()
	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	c := m.Reserve(ctx, req("c"))
	if a.State != api.StateActive || b.State != api.StateActive || c.State != api.StateActive {
		t.Fatalf("cap 0 should leave all three active: a=%s b=%s c=%s", a.State, b.State, c.State)
	}
}

func TestCapabilityBestFit(t *testing.T) {
	fr := newFakeRunner()
	// u-plain has no caps; u-la additionally has the analyzer cap.
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{
		{Name: "u-plain", Capabilities: []string{"flash"}},
		{Name: "u-la", Capabilities: []string{"flash", "logic-analyzer"}},
	}))
	ctx := context.Background()

	// A plain request should take the tightest-fitting unit (u-plain), leaving the
	// over-provisioned analyzer unit free for work that needs it.
	a := m.Reserve(ctx, api.ReserveRequest{Owner: "a", SSHPublicKey: "k", RequireCaps: []string{"flash"}})
	if a.Unit != "u-plain" {
		t.Fatalf("best-fit should pick u-plain, got %q", a.Unit)
	}
	b := m.Reserve(ctx, api.ReserveRequest{Owner: "b", SSHPublicKey: "k", RequireCaps: []string{"logic-analyzer"}})
	if b.Unit != "u-la" {
		t.Fatalf("analyzer request should land on u-la, got %q", b.Unit)
	}
}

func TestPinOnlyUnit(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{
		{Name: "usb0", Type: "esp32c6", Capabilities: []string{"wss-app"}},
		{Name: "pi0", Type: "led-mapper-pi", PinOnly: true, Capabilities: []string{"wss-app"}},
	}))
	ctx := context.Background()

	// A bare / caps-only request must never land on the pin-only unit.
	a := m.Reserve(ctx, api.ReserveRequest{Owner: "a", SSHPublicKey: "k", RequireCaps: []string{"wss-app"}})
	if a.Unit != "usb0" {
		t.Fatalf("caps-only request must avoid pin-only unit; got %q", a.Unit)
	}
	// Even with usb0 busy, a caps-only request queues rather than taking pi0.
	b := m.Reserve(ctx, api.ReserveRequest{Owner: "b", SSHPublicKey: "k", RequireCaps: []string{"wss-app"}})
	if b.State != api.StateQueued {
		t.Fatalf("second caps-only request should queue (pin-only excluded), got %s on %s", b.State, b.Unit)
	}
	// An explicit type target reaches the pin-only unit.
	c := m.Reserve(ctx, api.ReserveRequest{Owner: "c", SSHPublicKey: "k", UnitType: "led-mapper-pi"})
	if c.State != api.StateActive || c.Unit != "pi0" {
		t.Fatalf("type-targeted request should activate pi0, got state=%s unit=%s", c.State, c.Unit)
	}
}

func TestNamePinToBusyUnitQueues(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, api.ReserveRequest{Owner: "a", SSHPublicKey: "k", Unit: "u0"})
	if a.Unit != "u0" {
		t.Fatalf("expected u0, got %q", a.Unit)
	}
	// Pin b to u0 too: it must wait for u0 even though u1 is free.
	b := m.Reserve(ctx, api.ReserveRequest{Owner: "b", SSHPublicKey: "k", Unit: "u0"})
	if b.State != api.StateQueued {
		t.Fatalf("b pinned to busy u0 should queue, got %s on %s", b.State, b.Unit)
	}
}

func TestLeaseReap(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", 10*time.Millisecond, fr, WithUnits([]runner.Unit{{Name: "u0"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	_ = b
	time.Sleep(30 * time.Millisecond)
	m.ReapExpired(ctx)
	// a's lease lapsed → reaped; b promoted; then b also lapses on the next reap.
	if _, err := m.Get(a.ID); err != ErrNotFound {
		t.Fatalf("expired active holder should be reaped, got %v", err)
	}
}

func TestSyncUnitsHotPlug(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}}))
	ctx := context.Background()

	// Queue two; only u0 exists so the second waits.
	m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	if b.State != api.StateQueued {
		t.Fatalf("b should queue with one unit")
	}
	// Hot-plug u1: the waiter should activate onto it.
	added, _ := m.SyncUnits(ctx, []runner.Unit{{Name: "u0"}, {Name: "u1"}})
	if len(added) != 1 || added[0] != "u1" {
		t.Fatalf("expected u1 added, got %v", added)
	}
	bv, _ := m.Get(b.ID)
	if bv.State != api.StateActive || bv.Unit != "u1" {
		t.Fatalf("b should activate on hot-plugged u1, got state=%s unit=%s", bv.State, bv.Unit)
	}
}

func TestStartFailureDropsAndContinues(t *testing.T) {
	fr := newFakeRunner()
	fr.failUnit = "u0"
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}}))
	ctx := context.Background()

	// u0 start fails; the reservation should be dropped and the next placement (u1)
	// still succeed for a following waiter.
	a := m.Reserve(ctx, api.ReserveRequest{Owner: "a", SSHPublicKey: "k", Unit: "u0"})
	if a.State != api.StateReleased {
		t.Fatalf("failed-start reservation should come back released, got %s", a.State)
	}
	if _, err := m.Get(a.ID); err != ErrNotFound {
		t.Fatalf("failed-start reservation should be dropped, got %v", err)
	}
	b := m.Reserve(ctx, api.ReserveRequest{Owner: "b", SSHPublicKey: "k", Unit: "u1"})
	if b.State != api.StateActive {
		t.Fatalf("b on healthy u1 should activate, got %s", b.State)
	}
}

func TestAppendNote(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	if _, err := m.AppendNote(a.ID, "agent", "first"); err != nil {
		t.Fatal(err)
	}
	v, err := m.AppendNote(a.ID, "user", "second")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Scratchpad) != 2 {
		t.Fatalf("expected 2 notes, got %d", len(v.Scratchpad))
	}
	// Ordered oldest-first, with author and timestamp captured.
	if v.Scratchpad[0].Text != "first" || v.Scratchpad[1].Text != "second" {
		t.Fatalf("notes out of order: %+v", v.Scratchpad)
	}
	if v.Scratchpad[0].Author != "agent" || v.Scratchpad[0].Time.IsZero() {
		t.Fatalf("note metadata not recorded: %+v", v.Scratchpad[0])
	}
	// The returned snapshot must not alias live state.
	v.Scratchpad[0].Text = "mutated"
	again, _ := m.Get(a.ID)
	if again.Scratchpad[0].Text != "first" {
		t.Fatalf("snapshot aliased live scratchpad: %q", again.Scratchpad[0].Text)
	}
	if _, err := m.AppendNote("nope", "x", "y"); err != ErrNotFound {
		t.Fatalf("unknown id should be ErrNotFound, got %v", err)
	}
}

func TestSetAnnotation(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	if _, err := m.SetAnnotation(a.ID, "view", "http://example/x"); err != nil {
		t.Fatal(err)
	}
	// Overwriting the same key replaces the value.
	v, err := m.SetAnnotation(a.ID, "view", "http://example/y")
	if err != nil {
		t.Fatal(err)
	}
	if v.Annotations["view"] != "http://example/y" {
		t.Fatalf("annotation not overwritten: %v", v.Annotations)
	}
	// Snapshot must not alias live map.
	v.Annotations["view"] = "mutated"
	again, _ := m.Get(a.ID)
	if again.Annotations["view"] != "http://example/y" {
		t.Fatalf("snapshot aliased live annotations: %q", again.Annotations["view"])
	}
	if _, err := m.SetAnnotation("nope", "k", "v"); err != ErrNotFound {
		t.Fatalf("unknown id should be ErrNotFound, got %v", err)
	}
}

// hookRec records lifecycle callbacks.
type hookRec struct{ first, idle int }

func (h *hookRec) OnFirstActive(context.Context) error { h.first++; return nil }
func (h *hookRec) OnAllIdle(context.Context) error     { h.idle++; return nil }

func TestHookFirstActiveAllIdle(t *testing.T) {
	fr := newFakeRunner()
	h := &hookRec{}
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}}), WithHook(h))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	if h.first != 1 {
		t.Fatalf("OnFirstActive should fire once for the idle->busy edge, got %d", h.first)
	}
	m.Release(ctx, a.ID, "done")
	if h.idle != 0 {
		t.Fatalf("OnAllIdle must not fire while b still active, got %d", h.idle)
	}
	m.Release(ctx, b.ID, "done")
	if h.idle != 1 {
		t.Fatalf("OnAllIdle should fire once when last active releases, got %d", h.idle)
	}
}
