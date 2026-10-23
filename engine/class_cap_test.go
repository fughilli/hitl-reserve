package engine

import (
	"context"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// rig1Units mirrors hitl-rig-1: two USB ESP32-C6 boards plus two pin-only network
// Pi DUTs (reached only by an explicit type/name target).
func rig1Units() []runner.Unit {
	return []runner.Unit{
		{Name: "c6-a", Type: "esp32c6"},
		{Name: "c6-b", Type: "esp32c6"},
		{Name: "pi-1", Type: "led-mapper-pi", PinOnly: true},
		{Name: "pi-2", Type: "led-mapper-pi", PinOnly: true},
	}
}

func typed(owner, typ string) api.ReserveRequest {
	r := req(owner)
	r.UnitType = typ
	return r
}

func state(t *testing.T, m *Manager, id string) api.State {
	t.Helper()
	v, err := m.Get(id)
	if err != nil {
		return api.StateReleased
	}
	return v.State
}

// The regression: under a host-wide cap alone, two Pi jobs held both slots and the
// two free C6 units sat idle behind them. With per-class caps the C6 waiters run.
func TestClassCapsDoNotIdleOtherClass(t *testing.T) {
	ctx := context.Background()

	// Old behaviour (host cap only): the C6 waiters queue behind the Pi jobs.
	old := New("h", time.Minute, newFakeRunner(), WithUnits(rig1Units()), WithMaxConcurrent(2))
	old.Reserve(ctx, typed("p1", "led-mapper-pi"))
	old.Reserve(ctx, typed("p2", "led-mapper-pi"))
	if c := old.Reserve(ctx, typed("c1", "esp32c6")); c.State != api.StateQueued {
		t.Fatalf("precondition: host cap alone should queue the C6 job, got %s", c.State)
	}

	m := New("h", time.Minute, newFakeRunner(), WithUnits(rig1Units()),
		WithMaxConcurrent(4),
		WithMaxConcurrentPerClass(map[string]int{"esp32c6": 2, "led-mapper-pi": 2}))
	p1 := m.Reserve(ctx, typed("p1", "led-mapper-pi"))
	p2 := m.Reserve(ctx, typed("p2", "led-mapper-pi"))
	c1 := m.Reserve(ctx, typed("c1", "esp32c6"))
	c2 := m.Reserve(ctx, req("c2")) // unpinned: never lands on a pin-only Pi
	for _, r := range []*api.Reservation{p1, p2, c1, c2} {
		if r.State != api.StateActive {
			t.Fatalf("%s should be active with per-class caps, got %s", r.Owner, r.State)
		}
	}
	if st := m.Status(); st.FreeUnits != 0 || st.MaxConcurrent != 4 ||
		st.MaxConcurrentPerClass["esp32c6"] != 2 {
		t.Fatalf("status: free=%d max=%d perClass=%v", st.FreeUnits, st.MaxConcurrent, st.MaxConcurrentPerClass)
	}
}

// A class cap bounds that class below its unit count; other classes are unaffected,
// and a capped waiter doesn't block a later waiter of another class (no HOL block).
func TestClassCapBoundsOnlyItsClass(t *testing.T) {
	ctx := context.Background()
	units := []runner.Unit{
		{Name: "c6-a", Type: "esp32c6"}, {Name: "c6-b", Type: "esp32c6"}, {Name: "c6-c", Type: "esp32c6"},
		{Name: "pi-1", Type: "led-mapper-pi", PinOnly: true},
	}
	m := New("h", time.Minute, newFakeRunner(), WithUnits(units),
		WithMaxConcurrentPerClass(map[string]int{"esp32c6": 2}))
	a := m.Reserve(ctx, typed("a", "esp32c6"))
	b := m.Reserve(ctx, typed("b", "esp32c6"))
	c := m.Reserve(ctx, typed("c", "esp32c6")) // a third C6 is free, but the class is capped
	p := m.Reserve(ctx, typed("p", "led-mapper-pi"))
	if a.State != api.StateActive || b.State != api.StateActive {
		t.Fatalf("a,b should be active: %s %s", a.State, b.State)
	}
	if c.State != api.StateQueued {
		t.Fatalf("c should queue behind the esp32c6 class cap, got %s", c.State)
	}
	if p.State != api.StateActive {
		t.Fatalf("the Pi job behind a capped C6 waiter should still run (no HOL block), got %s", p.State)
	}
	m.Release(ctx, a.ID, "done")
	if s := state(t, m, c.ID); s != api.StateActive {
		t.Fatalf("c should promote once its class has room, got %s", s)
	}
}

// FIFO within a class: with the class cap at 1, same-class waiters activate in
// strict admission order even when interleaved with another class's waiters.
func TestClassCapFIFOWithinClass(t *testing.T) {
	ctx := context.Background()
	m := New("h", time.Minute, newFakeRunner(), WithUnits(rig1Units()),
		WithMaxConcurrentPerClass(map[string]int{"esp32c6": 1, "led-mapper-pi": 1}))
	var c6, pi []*api.Reservation
	for i := 0; i < 4; i++ {
		c6 = append(c6, m.Reserve(ctx, typed("c6", "esp32c6")))
		pi = append(pi, m.Reserve(ctx, typed("pi", "led-mapper-pi")))
	}
	for i := 0; i < 4; i++ {
		for j := range c6 {
			want := api.StateQueued
			if j == i {
				want = api.StateActive
			}
			if j < i {
				want = api.StateReleased
			}
			if s := state(t, m, c6[j].ID); s != want {
				t.Fatalf("round %d: c6[%d] = %s, want %s", i, j, s, want)
			}
			if s := state(t, m, pi[j].ID); s != want {
				t.Fatalf("round %d: pi[%d] = %s, want %s", i, j, s, want)
			}
		}
		m.Release(ctx, c6[i].ID, "done")
		m.Release(ctx, pi[i].ID, "done")
	}
}

// No class starvation under a binding host cap: a Pi waiter queued behind a long
// run of C6 work gets the next freed slot once it is the oldest runnable waiter —
// later C6 arrivals never overtake it.
func TestClassNotStarvedUnderHostCap(t *testing.T) {
	ctx := context.Background()
	m := New("h", time.Minute, newFakeRunner(), WithUnits(rig1Units()),
		WithMaxConcurrent(2),
		WithMaxConcurrentPerClass(map[string]int{"esp32c6": 2, "led-mapper-pi": 2}))
	a := m.Reserve(ctx, typed("a", "esp32c6"))
	b := m.Reserve(ctx, typed("b", "esp32c6"))
	p := m.Reserve(ctx, typed("p", "led-mapper-pi"))
	if p.State != api.StateQueued {
		t.Fatalf("host cap is full, p should queue, got %s", p.State)
	}
	// A stream of later C6 arrivals.
	var later []*api.Reservation
	for i := 0; i < 5; i++ {
		later = append(later, m.Reserve(ctx, typed("late", "esp32c6")))
	}
	m.Release(ctx, a.ID, "done")
	if s := state(t, m, p.ID); s != api.StateActive {
		t.Fatalf("the oldest waiter (the Pi job) must take the freed slot, got %s", s)
	}
	for _, r := range later {
		if s := state(t, m, r.ID); s != api.StateQueued {
			t.Fatalf("a later C6 arrival overtook the older Pi waiter: %s", s)
		}
	}
	// And the C6 class resumes in order afterwards.
	m.Release(ctx, b.ID, "done")
	if s := state(t, m, later[0].ID); s != api.StateActive {
		t.Fatalf("later[0] should be next, got %s", s)
	}
	if s := state(t, m, later[1].ID); s != api.StateQueued {
		t.Fatalf("later[1] should still wait, got %s", s)
	}
}

// A caps-only request that several classes can serve skips a capped-out class and
// lands on a free unit of a class with room.
func TestClassCapCapsOnlyRequestFallsThrough(t *testing.T) {
	ctx := context.Background()
	units := []runner.Unit{
		{Name: "c6-a", Type: "esp32c6", Capabilities: []string{"improv"}},
		{Name: "c6-b", Type: "esp32c6", Capabilities: []string{"improv"}},
		{Name: "c3-a", Type: "esp32c3", Capabilities: []string{"improv", "chip:esp32c3"}},
	}
	m := New("h", time.Minute, newFakeRunner(), WithUnits(units),
		WithMaxConcurrentPerClass(map[string]int{"esp32c6": 1}))
	r := req("x")
	r.RequireCaps = []string{"improv"}
	a := m.Reserve(ctx, r)
	b := m.Reserve(ctx, r)
	if a.State != api.StateActive || a.UnitType != "esp32c6" {
		t.Fatalf("a should land best-fit on a c6: %s %s", a.State, a.UnitType)
	}
	if b.State != api.StateActive || b.Unit != "c3-a" {
		t.Fatalf("b should fall through the capped esp32c6 class onto c3-a, got %s %s", b.State, b.Unit)
	}
}

// Non-positive caps are ignored (uncapped), and no option means the old behaviour.
func TestClassCapNonPositiveIgnored(t *testing.T) {
	ctx := context.Background()
	m := New("h", time.Minute, newFakeRunner(), WithUnits(rig1Units()),
		WithMaxConcurrentPerClass(map[string]int{"esp32c6": 0, "led-mapper-pi": -1}))
	a := m.Reserve(ctx, typed("a", "esp32c6"))
	b := m.Reserve(ctx, typed("b", "esp32c6"))
	if a.State != api.StateActive || b.State != api.StateActive {
		t.Fatalf("0/-1 caps must not block: %s %s", a.State, b.State)
	}
	if st := m.Status(); st.MaxConcurrentPerClass != nil {
		t.Fatalf("no effective caps should be advertised, got %v", st.MaxConcurrentPerClass)
	}
}
