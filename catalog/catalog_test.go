package catalog

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/shared"
)

// stubBroker is a Binder that grants a fixed capability from a binding.
type stubBroker struct {
	name string
}

func (s *stubBroker) Name() string  { return s.name }
func (s *stubBroker) Kind() string  { return "logic-analyzer" }
func (s *stubBroker) Present() bool { return true }
func (s *stubBroker) Describe() api.SharedResourceInfo {
	return api.SharedResourceInfo{Name: s.name, Kind: "logic-analyzer", Present: true}
}
func (s *stubBroker) Access(context.Context, string, []byte) ([]byte, int, error) {
	return []byte("{}"), 200, nil
}
func (s *stubBroker) Bind(unit string, config json.RawMessage) ([]string, error) {
	var m struct {
		Protocol string `json:"protocol"`
	}
	_ = json.Unmarshal(config, &m)
	return []string{"logic-analyzer", "logic-analyzer-" + m.Protocol}, nil
}

func factories() map[string]BrokerFactory {
	return map[string]BrokerFactory{
		"logic-analyzer": func(name string, _ json.RawMessage) (shared.Broker, error) {
			return &stubBroker{name: name}, nil
		},
	}
}

const example = `{
  "host": "rig-1",
  "workspace": "demo",
  "lease_seconds": 600,
  "ssh_port_base": 2222,
  "resource_types": {
    "esp32c6": {"kind": "usb", "capabilities": ["flash", "wss-app", "led-strip"]},
    "hackrf-one": {"kind": "usb", "capabilities": ["sdr", "rx", "tx"]},
    "led-mapper-pi": {"kind": "network", "capabilities": ["wss-app", "led-strip"]}
  },
  "shared_resources": [
    {"name": "la", "kind": "logic-analyzer", "config": {"driver": "fx2lafw"}}
  ],
  "components": [
    {"name": "c6-a", "type": "esp32c6", "devices": ["/dev/serial/by-id/x:/dev/ttyACM0"]},
    {"name": "sdr-a", "type": "hackrf-one"},
    {"name": "pi-1", "type": "led-mapper-pi", "address": "pi1.local"}
  ],
  "units": [
    {"name": "c6-a", "type": "esp32c6", "components": ["c6-a"],
     "shared": [{"resource": "la", "config": {"channels": ["D0"], "protocol": "ws2812"}}]},
    {"name": "c6-a+sdr", "type": "esp32c6+hackrf", "components": ["c6-a", "sdr-a"]},
    {"name": "pi-1", "type": "led-mapper-pi", "pin_only": true, "components": ["pi-1"]}
  ]
}`

func TestResolveExample(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Resolve(factories())
	if err != nil {
		t.Fatal(err)
	}
	if res.Host != "rig-1" || res.Workspace != "demo" || res.LeaseSeconds != 600 {
		t.Fatalf("header wrong: %+v", res)
	}
	if len(res.Units) != 3 {
		t.Fatalf("want 3 units, got %d", len(res.Units))
	}
	byName := map[string]int{}
	for i, u := range res.Units {
		byName[u.Name] = i
	}

	// Single USB unit: caps = type caps + analyzer binding caps.
	u := res.Units[byName["c6-a"]]
	if u.Kind != "usb" {
		t.Errorf("c6-a kind = %q, want usb", u.Kind)
	}
	if !hasCap(u.Capabilities, "flash") || !hasCap(u.Capabilities, "logic-analyzer-ws2812") {
		t.Errorf("c6-a caps missing expected: %v", u.Capabilities)
	}

	// Composite unit: two components, union of both types' caps, kind=composite.
	comp := res.Units[byName["c6-a+sdr"]]
	if comp.Kind != "composite" || len(comp.Components) != 2 {
		t.Errorf("composite wrong: kind=%s components=%d", comp.Kind, len(comp.Components))
	}
	if !hasCap(comp.Capabilities, "sdr") || !hasCap(comp.Capabilities, "flash") {
		t.Errorf("composite caps should union both components: %v", comp.Capabilities)
	}

	// Network unit: pin-only, kind=network.
	pi := res.Units[byName["pi-1"]]
	if pi.Kind != "network" || !pi.PinOnly {
		t.Errorf("pi unit wrong: kind=%s pinOnly=%v", pi.Kind, pi.PinOnly)
	}
	if pi.Components[0].Address != "pi1.local" {
		t.Errorf("pi address not carried: %q", pi.Components[0].Address)
	}

	// Ports auto-assigned and distinct.
	ports := map[int]bool{}
	for _, u := range res.Units {
		if u.SSHPort == 0 || ports[u.SSHPort] {
			t.Errorf("bad/duplicate port for %s: %d", u.Name, u.SSHPort)
		}
		ports[u.SSHPort] = true
	}

	if _, ok := res.Registry.Get("la"); !ok {
		t.Errorf("shared resource la not registered")
	}
}

func TestResolveErrors(t *testing.T) {
	cases := map[string]string{
		"unknown component":   `{"units":[{"name":"u","components":["nope"]}]}`,
		"unknown type":        `{"components":[{"name":"c","type":"ghost"}],"units":[{"name":"u","components":["c"]}]}`,
		"unknown shared":      `{"components":[{"name":"c","type":"t"}],"resource_types":{"t":{}},"units":[{"name":"u","components":["c"],"shared":[{"resource":"nope"}]}]}`,
		"unknown broker kind": `{"shared_resources":[{"name":"x","kind":"mystery"}]}`,
		"dup unit":            `{"components":[{"name":"c","type":"t"}],"resource_types":{"t":{}},"units":[{"name":"u","components":["c"]},{"name":"u","components":["c"]}]}`,
	}
	for name, js := range cases {
		c, err := Parse([]byte(js))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if _, err := c.Resolve(factories()); err == nil {
			t.Errorf("%s: expected resolve error, got nil", name)
		}
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	if _, err := Parse([]byte(`{"bogus": 1}`)); err == nil {
		t.Fatal("expected error on unknown field")
	}
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
