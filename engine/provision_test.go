package engine

import (
	"context"
	"testing"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

func TestProvisioningInStatus(t *testing.T) {
	fr := newFakeRunner()
	p := &api.ProvisioningNetwork{SSID: "rig-1", PSK: "secret"}
	m := New("h", time.Minute, fr, WithUnits([]runner.Unit{{Name: "u0"}}), WithProvisioningNetwork(p))
	st := m.Status()
	if st.Provisioning == nil || st.Provisioning.SSID != "rig-1" || st.Provisioning.PSK != "secret" {
		t.Fatalf("provisioning not advertised: %+v", st.Provisioning)
	}
}

// A host may start with zero units (discovery/seeding will add them). It must not
// synthesize a phantom placeholder unit.
func TestNoPhantomUnit(t *testing.T) {
	fr := newFakeRunner()
	m := New("h", time.Minute, fr) // no WithUnits
	st := m.Status()
	if len(st.Units) != 0 {
		t.Fatalf("expected zero units, got %d (%+v)", len(st.Units), st.Units)
	}
	if st.FreeUnits != 0 {
		t.Fatalf("expected 0 free units, got %d", st.FreeUnits)
	}
	// A reservation just queues until a unit appears via SyncUnits.
	r := m.Reserve(context.Background(), req("a"))
	if r.State != api.StateQueued {
		t.Fatalf("reservation on an empty host should queue, got %s on %q", r.State, r.Unit)
	}
	added, _ := m.SyncUnits(context.Background(), []runner.Unit{{Name: "u0"}})
	if len(added) != 1 {
		t.Fatalf("expected u0 added, got %v", added)
	}
	rv, _ := m.Get(r.ID)
	if rv.State != api.StateActive || rv.Unit != "u0" {
		t.Fatalf("waiter should activate on the added unit, got %s/%s", rv.State, rv.Unit)
	}
}
