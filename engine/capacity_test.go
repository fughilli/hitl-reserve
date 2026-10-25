package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// rigLAUnits: one plain C6, one C6 tapped by the logic analyzer, one pin-only Pi.
func rigLAUnits() []runner.Unit {
	return []runner.Unit{
		{Name: "c6-a", Type: "esp32c6", Capabilities: []string{"flash"}},
		{Name: "c6-b", Type: "esp32c6", Capabilities: []string{"flash", "logic-analyzer-led-strip"}},
		{Name: "pi-1", Type: "led-mapper-pi", PinOnly: true},
	}
}

func capSnap(m *Manager) CapacitySnapshot { return m.Metrics().Capacity }

func availOf(s CapacitySnapshot, unit string) UnitCapacity {
	for _, u := range s.Units {
		if u.Unit == unit {
			return u
		}
	}
	return UnitCapacity{}
}

func busyOf(s CapacitySnapshot, unit, prio, outcome string) (float64, BusySeconds) {
	for _, b := range s.Busy {
		if b.Unit == unit && b.Prio == prio && b.Outcome == outcome {
			return b.Seconds, b
		}
	}
	return -1, BusySeconds{}
}

func reqOf(s CapacitySnapshot, prio, caps string) RequestCounts {
	for _, r := range s.Requests {
		if r.Prio == prio && r.CapsClass == caps {
			return r
		}
	}
	return RequestCounts{}
}

// Unit caps classes: type alias + "-la" for an analyzer-tapped unit; request caps
// classes: the targeted type's alias + "-la" only if the request NEEDS the analyzer.
func TestCapsClassLabels(t *testing.T) {
	m, _, _ := newPrioManager(t, rigLAUnits())
	ctx := context.Background()
	s := capSnap(m)
	for unit, want := range map[string]string{"c6-a": "c6", "c6-b": "c6-la", "pi-1": "pi"} {
		if got := availOf(s, unit).CapsClass; got != want {
			t.Errorf("unit %s caps_class=%q want %q", unit, got, want)
		}
	}

	m.Reserve(ctx, prioTyped("c6", api.PriorityPRCI, "esp32c6"))
	la := prio("la", api.PriorityPRCI)
	la.RequireCaps = []string{"logic-analyzer-led-strip"}
	m.Reserve(ctx, la)
	m.Reserve(ctx, prio("any", api.PriorityDev)) // caps-only/unpinned: both candidates are c6 (Pi is pin-only)
	pin := prio("pin", api.PriorityDev)
	pin.Unit = "pi-1"
	m.Reserve(ctx, pin)
	m.Reserve(ctx, prioTyped("sdr", api.PriorityDev, "hackrf"))

	s = capSnap(m)
	for _, c := range []struct{ prio, caps string }{
		{api.PriorityPRCI, "c6"}, {api.PriorityPRCI, "c6-la"}, {api.PriorityDev, "c6"},
		{api.PriorityDev, "pi"}, {api.PriorityDev, "hackrf"},
	} {
		if r := reqOf(s, c.prio, c.caps); r.Requests != 1 {
			t.Errorf("requests{%s,%s}=%d want 1 (all: %+v)", c.prio, c.caps, r.Requests, s.Requests)
		}
	}
}

// Busy seconds are credited at release with the hold's outcome; holds land in the
// hold histogram by owner kind; waits land in the wait histogram by result.
func TestCapacityBusyHoldWait(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "c6-a", Type: "esp32c6"}})
	ctx := context.Background()

	ci := m.Reserve(ctx, prioTyped("ci", api.PriorityPRCI, "esp32c6"))
	dev := prioTyped("agent", api.PriorityDev, "esp32c6")
	dev.Actor = "agent"
	d := m.Reserve(ctx, dev)
	gone := m.Reserve(ctx, prioTyped("gone", api.PriorityMergeQueue, "esp32c6"))

	clk.advance(2 * time.Minute)
	mustRelease(t, m, gone.ID) // abandoned after 120s in the queue
	clk.advance(3 * time.Minute)
	mustRelease(t, m, ci.ID) // 300s hold, released; dev activates after a 300s wait
	clk.advance(10 * time.Minute)
	if err := m.Release(ctx, d.ID, ReasonCancelled); err != nil { // 600s hold, cancelled
		t.Fatal(err)
	}

	s := capSnap(m)
	if v, b := busyOf(s, "c6-a", api.PriorityPRCI, OutcomeReleased); v != 300 || b.CapsClass != "c6" || b.Type != "esp32c6" {
		t.Fatalf("pr-ci busy: %v %+v", v, b)
	}
	if v, _ := busyOf(s, "c6-a", api.PriorityDev, OutcomeCancelled); v != 600 {
		t.Fatalf("dev cancelled busy: %v (all %+v)", v, s.Busy)
	}
	if r := reqOf(s, api.PriorityMergeQueue, "c6"); r.Requests != 1 || r.Abandoned != 1 {
		t.Fatalf("merge-queue request counts: %+v", r)
	}
	waits := map[string]float64{}
	for _, w := range s.Waits {
		waits[w.Prio+"/"+w.Result] = w.Hist.Sum
		if w.Hist.Count != 1 || w.CapsClass != "c6" {
			t.Fatalf("wait series %+v", w)
		}
	}
	if waits["merge-queue/abandoned"] != 120 || waits["dev/activated"] != 300 || waits["pr-ci/activated"] != 0 {
		t.Fatalf("waits: %v", waits)
	}
	holds := map[string]float64{}
	for _, h := range s.Holds {
		holds[h.Prio+"/"+h.OwnerKind] = h.Hist.Sum
	}
	if holds["pr-ci/ci"] != 300 || holds["dev/agent"] != 600 {
		t.Fatalf("holds: %v", holds)
	}
}

// A lapsed lease is credited as outcome=expired.
func TestCapacityExpiredOutcome(t *testing.T) {
	fr := newFakeRunner()
	clk := newClock()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0", Type: "esp32c6"}}), WithClock(clk.now))
	ctx := context.Background()
	m.Reserve(ctx, prio("h", api.PriorityPRCI))
	clk.advance(2 * time.Minute)
	m.ReapExpired(ctx)
	if v, _ := busyOf(capSnap(m), "u0", api.PriorityPRCI, OutcomeExpired); v != 120 {
		t.Fatalf("expired busy=%v", v)
	}
}

type failRunner struct{ fakeRunner }

func (f *failRunner) Start(context.Context, string, string, string, runner.Unit) (*api.Endpoint, error) {
	return nil, errors.New("boom")
}

// A failed environment start is accounted as outcome=start_failed (zero-length
// here: the fake fails instantly) and is not counted as abandoned.
func TestCapacityStartFailed(t *testing.T) {
	m := New("h", time.Hour, &failRunner{}, WithUnits([]runner.Unit{{Name: "u0", Type: "esp32c6"}}), WithClock(newClock().now))
	m.Reserve(context.Background(), prio("x", api.PriorityDev))
	s := capSnap(m)
	if v, _ := busyOf(s, "u0", api.PriorityDev, OutcomeStartFailed); v != 0 {
		t.Fatalf("start_failed series missing/nonzero: %v (%+v)", v, s.Busy)
	}
	if r := reqOf(s, api.PriorityDev, "c6"); r.Requests != 1 || r.Abandoned != 0 {
		t.Fatalf("requests: %+v", r)
	}
}

// Available seconds accrue while the unit is present and the host isn't cordoned
// (busy or idle), stop while cordoned or unplugged, and never go backwards.
func TestCapacityAvailableSeconds(t *testing.T) {
	m, _, clk := newPrioManager(t, []runner.Unit{{Name: "a", Type: "esp32c6"}, {Name: "b", Type: "esp32c6"}})
	ctx := context.Background()
	m.Reserve(ctx, prio("busy", api.PriorityDev)) // busy time still counts as available
	clk.advance(10 * time.Minute)
	if v := availOf(capSnap(m), "a").AvailableSeconds; v != 600 {
		t.Fatalf("a available=%v want 600", v)
	}
	m.Cordon()
	clk.advance(time.Hour)
	if v := availOf(capSnap(m), "b").AvailableSeconds; v != 600 {
		t.Fatalf("cordoned time must not count: b=%v", v)
	}
	m.Uncordon(ctx)
	clk.advance(5 * time.Minute)
	// b unplugged (a is busy, so discovery keeps it).
	m.SyncUnits(ctx, []runner.Unit{{Name: "a", Type: "esp32c6"}})
	clk.advance(time.Hour)
	s := capSnap(m)
	if v := availOf(s, "b").AvailableSeconds; v != 900 {
		t.Fatalf("unplugged b should stop at 900, got %v", v)
	}
	if v := availOf(s, "a").AvailableSeconds; v != 900+3600 {
		t.Fatalf("a available=%v want 4500", v)
	}
	// Re-plugged b resumes from where it stopped.
	m.SyncUnits(ctx, []runner.Unit{{Name: "a", Type: "esp32c6"}, {Name: "b", Type: "esp32c6"}})
	clk.advance(time.Minute)
	if v := availOf(capSnap(m), "b").AvailableSeconds; v != 960 {
		t.Fatalf("replugged b=%v want 960", v)
	}
}
