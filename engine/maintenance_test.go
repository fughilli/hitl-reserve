package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// TestCordonBlocksActivation checks that a queued reservation does not activate
// while the host is cordoned (it keeps its position), and activates once uncordoned.
func TestCordonBlocksActivation(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}}))
	ctx := context.Background()

	ms := m.Cordon()
	if !ms.Cordoned || !ms.Drained {
		t.Fatalf("fresh cordon on an idle host should be cordoned+drained, got %+v", ms)
	}

	// A reserve while cordoned must queue, not activate, even though units are free.
	a := m.Reserve(ctx, req("a"))
	if a.State != api.StateQueued {
		t.Fatalf("reservation should queue while cordoned, got %s on %s", a.State, a.Unit)
	}
	if ms := m.Maintenance(); ms.Queued != 1 || ms.Active != 0 {
		t.Fatalf("expected queued=1 active=0 while cordoned, got %+v", ms)
	}

	// Uncordon: the waiter should promote onto a free unit.
	m.Uncordon(ctx)
	av, _ := m.Get(a.ID)
	if av.State != api.StateActive {
		t.Fatalf("after uncordon, queued reservation should activate, got %s", av.State)
	}
}

// TestCordonDrainsActiveReservations checks that cordoning does not tear down active
// reservations — they keep running and drain on release, and the drain snapshot
// tracks the active count down to zero.
func TestCordonDrainsActiveReservations(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}, {Name: "u1"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	if a.State != api.StateActive || b.State != api.StateActive {
		t.Fatalf("both should be active before cordon: a=%s b=%s", a.State, b.State)
	}

	ms := m.Cordon()
	if !ms.Cordoned || ms.Drained || ms.Active != 2 {
		t.Fatalf("cordon with 2 active should be cordoned, not drained, active=2, got %+v", ms)
	}
	// Active reservations still exist (not torn down).
	if av, _ := m.Get(a.ID); av.State != api.StateActive {
		t.Fatalf("cordon must not tear down active reservation a, got %s", av.State)
	}

	m.Release(ctx, a.ID, "done")
	if ms := m.Maintenance(); ms.Active != 1 || ms.Drained {
		t.Fatalf("after releasing one, active=1 not drained, got %+v", ms)
	}
	m.Release(ctx, b.ID, "done")
	if ms := m.Maintenance(); ms.Active != 0 || !ms.Drained {
		t.Fatalf("after releasing both, active=0 drained, got %+v", ms)
	}
}

// TestWaitDrained checks WaitDrained returns once the last active reservation
// releases while cordoned.
func TestWaitDrained(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}}))
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	m.Cordon()

	done := make(chan api.Maintenance, 1)
	go func() { done <- m.WaitDrained(ctx) }()

	// Should still be blocked (a is active).
	select {
	case <-done:
		t.Fatal("WaitDrained returned before drain")
	case <-time.After(20 * time.Millisecond):
	}

	m.Release(ctx, a.ID, "done")
	select {
	case ms := <-done:
		if !ms.Drained || ms.Active != 0 {
			t.Fatalf("WaitDrained should report drained, got %+v", ms)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitDrained did not return after drain")
	}
}

// TestMarkerPersistsAcrossRestart checks the marker is written on cordon, read on a
// simulated restart (a fresh Manager over the same state dir starts cordoned), and
// cleared on release so a later restart is not cordoned.
func TestMarkerPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, maintenanceMarker)
	ctx := context.Background()

	// First daemon: cordon writes the marker.
	m1 := New("h", time.Minute, newFakeRunner(), WithUnits([]runner.Unit{{Name: "u0"}}), WithStateDir(dir))
	m1.Cordon()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cordon should write marker %s: %v", marker, err)
	}

	// Simulated restart: a fresh Manager over the same state dir starts cordoned, so
	// a queued reservation does not activate.
	m2 := New("h", time.Minute, newFakeRunner(), WithUnits([]runner.Unit{{Name: "u0"}}), WithStateDir(dir))
	if !m2.Maintenance().Cordoned {
		t.Fatal("restarted manager should start cordoned from the marker")
	}
	a := m2.Reserve(ctx, req("a"))
	if a.State != api.StateQueued {
		t.Fatalf("restarted-cordoned host must not activate a reservation, got %s", a.State)
	}

	// Release clears the marker; a further restart is not cordoned and activates.
	m2.Uncordon(ctx)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("release should remove marker, stat err=%v", err)
	}
	// The waiter queued during the cordon activates after uncordon.
	if av, _ := m2.Get(a.ID); av.State != api.StateActive {
		t.Fatalf("after uncordon the waiter should activate, got %s", av.State)
	}

	m3 := New("h", time.Minute, newFakeRunner(), WithUnits([]runner.Unit{{Name: "u0"}}), WithStateDir(dir))
	if m3.Maintenance().Cordoned {
		t.Fatal("after release, a restarted manager should not be cordoned")
	}
	b := m3.Reserve(ctx, req("b"))
	if b.State != api.StateActive {
		t.Fatalf("uncordoned restart should activate normally, got %s", b.State)
	}
}

// TestStatusReportsCordon checks Status surfaces cordoned/draining.
func TestStatusReportsCordon(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}}))
	ctx := context.Background()

	if st := m.Status(); st.Cordoned || st.Draining {
		t.Fatalf("fresh host should not be cordoned/draining, got %+v", st)
	}
	m.Reserve(ctx, req("a"))
	m.Cordon()
	st := m.Status()
	if !st.Cordoned || !st.Draining {
		t.Fatalf("cordoned host with an active reservation should be cordoned+draining, got %+v", st)
	}
}
