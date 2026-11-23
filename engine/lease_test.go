package engine

import (
	"context"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// leaseMgr builds a manager on the shared fakeClock (priority_test.go).
func leaseMgr(c *fakeClock, lease time.Duration, units ...string) (*Manager, *fakeRunner) {
	fr := newFakeRunner()
	var us []runner.Unit
	for _, u := range units {
		us = append(us, runner.Unit{Name: u})
	}
	return New("h", lease, fr, WithUnits(us), WithClock(c.now)), fr
}

// A waiter whose client died while queued must not get a fresh lease when it is
// promoted: it keeps the expiry its last heartbeat earned, so the reaper frees the
// unit at that point instead of one full lease after activation.
func TestActivationDoesNotRenewADeadWaitersLease(t *testing.T) {
	c := newClock()
	m, _ := leaseMgr(c, 5*time.Minute, "u0")
	ctx := context.Background()

	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b")) // queued behind a
	if b.State != api.StateQueued {
		t.Fatalf("b should queue, got %s", b.State)
	}
	c.advance(3 * time.Minute) // b's client dies now (no more heartbeats); a heartbeats
	if err := m.Heartbeat(a.ID); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	m.Release(ctx, a.ID, "done") // b promoted at t=4m with ~1m of lease left
	bv, _ := m.Get(b.ID)
	if bv.State != api.StateActive {
		t.Fatalf("b should be active, got %s", bv.State)
	}
	if want := c.t.Add(time.Minute); !bv.ExpiresAt.Equal(want) {
		t.Fatalf("activation must keep the heartbeat expiry %v, got %v", want, bv.ExpiresAt)
	}
	c.advance(61 * time.Second)
	m.ReapExpired(ctx)
	if _, err := m.Get(b.ID); err != ErrNotFound {
		t.Fatalf("dead holder should be reaped one lease after its LAST heartbeat, got %v", err)
	}
}

// A live waiter (heartbeating while queued) keeps a full lease across activation.
func TestActivationKeepsALiveWaitersLease(t *testing.T) {
	c := newClock()
	m, _ := leaseMgr(c, 5*time.Minute, "u0")
	ctx := context.Background()
	a := m.Reserve(ctx, req("a"))
	b := m.Reserve(ctx, req("b"))
	c.advance(4 * time.Minute)
	_ = m.Heartbeat(a.ID)
	_ = m.Heartbeat(b.ID)
	m.Release(ctx, a.ID, "done")
	c.advance(4 * time.Minute) // < lease since b's last heartbeat
	m.ReapExpired(ctx)
	if bv, err := m.Get(b.ID); err != nil || bv.State != api.StateActive {
		t.Fatalf("live b must survive, got %v %v", bv, err)
	}
}

// A waiter whose lease already lapsed is never started: when the unit frees, the
// next LIVE waiter gets it and the dead one stays for the reaper (no container
// started for nobody).
func TestLapsedWaiterIsSkippedNotStarted(t *testing.T) {
	c := newClock()
	m, fr := leaseMgr(c, 5*time.Minute, "u0")
	ctx := context.Background()
	a := m.Reserve(ctx, req("a"))
	dead := m.Reserve(ctx, req("dead"))
	c.advance(2 * time.Minute)
	live := m.Reserve(ctx, req("live"))
	c.advance(4 * time.Minute) // dead's lease lapsed (6m since its last sign of life)
	_ = m.Heartbeat(a.ID)
	_ = m.Heartbeat(live.ID)
	m.Release(ctx, a.ID, "done")
	if lv, _ := m.Get(live.ID); lv.State != api.StateActive {
		t.Fatalf("the live waiter should take the freed unit, got %s", lv.State)
	}
	if _, ok := fr.started[dead.ID]; ok {
		t.Fatalf("a lapsed waiter must never get an environment")
	}
	m.ReapExpired(ctx)
	if _, err := m.Get(dead.ID); err != ErrNotFound {
		t.Fatalf("the lapsed waiter should be reaped, got %v", err)
	}
}
