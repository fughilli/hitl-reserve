package pool

import (
	"testing"

	"github.com/fughilli/hitl-reserve/api"
)

func TestNormalize(t *testing.T) {
	got := Normalize("rig-1, rig-2:9000 http://rig-3:8087/  rig-1")
	want := []string{"http://rig-1:8087", "http://rig-2:9000", "http://rig-3:8087"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func status(free, queued int, units ...api.UnitStatus) *api.Status {
	return &api.Status{FreeUnits: free, QueueLength: queued, Units: units}
}

func unit(name, typ string, busy bool, pinOnly bool, caps ...string) api.UnitStatus {
	u := api.UnitStatus{Name: name, Type: typ, PinOnly: pinOnly, Capabilities: caps}
	if busy {
		u.Active = &api.Reservation{ID: "x"}
	}
	return u
}

func TestPickIdleBeatsBusy(t *testing.T) {
	probes := []Probe{
		{URL: "busy", Status: status(0, 2, unit("a", "t", true, false))},
		{URL: "idle", Status: status(1, 0, unit("b", "t", false, false))},
	}
	got, err := Pick(probes)
	if err != nil || got != "idle" {
		t.Fatalf("got %q err %v; want idle", got, err)
	}
}

func TestPickBestFit(t *testing.T) {
	// Both have a free unit serving "flash"; prefer the one whose free unit has
	// fewer extra caps, keeping the analyzer host free.
	probes := []Probe{
		{URL: "la-host", Status: status(1, 0, unit("u", "t", false, false, "flash", "logic-analyzer"))},
		{URL: "plain-host", Status: status(1, 0, unit("u", "t", false, false, "flash"))},
	}
	got, err := Pick(probes, Require{Caps: []string{"flash"}})
	if err != nil || got != "plain-host" {
		t.Fatalf("got %q err %v; want plain-host", got, err)
	}
}

func TestPickCapabilityFilter(t *testing.T) {
	probes := []Probe{
		{URL: "no-la", Status: status(1, 0, unit("u", "t", false, false, "flash"))},
		{URL: "la", Status: status(1, 0, unit("u", "t", false, false, "flash", "logic-analyzer"))},
	}
	got, err := Pick(probes, Require{Caps: []string{"logic-analyzer"}})
	if err != nil || got != "la" {
		t.Fatalf("got %q err %v; want la", got, err)
	}
}

func TestPickQueueableWhenNoneFree(t *testing.T) {
	// The only matching unit is busy → route there to queue rather than fast-fail.
	probes := []Probe{
		{URL: "other", Status: status(1, 0, unit("u", "other-type", false, false, "flash"))},
		{URL: "target", Status: status(0, 1, unit("u", "special", true, false, "flash", "special-cap"))},
	}
	got, err := Pick(probes, Require{Caps: []string{"special-cap"}})
	if err != nil || got != "target" {
		t.Fatalf("got %q err %v; want target (queueable)", got, err)
	}
}

func TestPickPinOnlyExcludedFromCapsOnly(t *testing.T) {
	// A caps-only request must not select a host whose only match is pin-only.
	probes := []Probe{
		{URL: "pin", Status: status(1, 0, unit("pi", "led-mapper-pi", false, true, "wss-app"))},
	}
	_, err := Pick(probes, Require{Caps: []string{"wss-app"}})
	if err == nil {
		t.Fatal("caps-only request should not match a pin-only unit")
	}
	// But a type target reaches it.
	got, err := Pick(probes, Require{Type: "led-mapper-pi"})
	if err != nil || got != "pin" {
		t.Fatalf("type target should reach pin-only host, got %q err %v", got, err)
	}
}

func TestPickAllUnreachable(t *testing.T) {
	probes := []Probe{{URL: "a", Err: errString("down")}, {URL: "b", Err: errString("down")}}
	if _, err := Pick(probes); err == nil {
		t.Fatal("expected error when all hosts unreachable")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
