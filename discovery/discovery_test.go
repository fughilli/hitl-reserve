package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fughilli/hitl-reserve/runner"
)

func typeCaps(t string) []string {
	if t == "pi-player" {
		return []string{"wss-app", "led-strip"}
	}
	return nil
}

func newSeededMonitor(t *testing.T, file string) *Monitor {
	cfg, err := parseTestConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SeededFile = file
	return NewMonitor(*cfg, typeCaps, nil)
}

func parseTestConfig() (*Config, error) {
	// Seeded-only: no USB glob.
	raw := json.RawMessage(`{"enabled":true}`)
	return ParseConfig(&raw)
}

func TestSeededSingleComponentNetworkUnit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "seeded.json")
	writeFile(t, f, `[{"name":"pi-1","type":"pi-player","address":"pi1.local"}]`)

	m := newSeededMonitor(t, f)
	units, err := m.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 1 {
		t.Fatalf("want 1 seeded unit, got %d", len(units))
	}
	u := units[0]
	if u.Name != "pi-1" || u.Kind != "network" || !u.PinOnly {
		t.Fatalf("seeded unit defaults wrong: %+v", u)
	}
	if len(u.Components) != 1 || u.Components[0].Address != "pi1.local" {
		t.Fatalf("synthesized component wrong: %+v", u.Components)
	}
	if !hasCap(u.Capabilities, "wss-app") {
		t.Fatalf("type caps not unioned: %v", u.Capabilities)
	}
	if u.SSHPort < 2308 { // SeededPortBase default = SSHPortBase(2300)+Max(8)
		t.Fatalf("seeded port not from dedicated range: %d", u.SSHPort)
	}
	// Sticky port across scans.
	p1 := u.SSHPort
	units2, _ := m.Scan()
	if units2[0].SSHPort != p1 {
		t.Fatalf("seeded port not sticky: %d -> %d", p1, units2[0].SSHPort)
	}
}

func TestSeededMalformedKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "seeded.json")
	writeFile(t, f, `[{"name":"pi-1","type":"pi-player","address":"pi1.local"}]`)
	m := newSeededMonitor(t, f)
	if _, err := m.Scan(); err != nil {
		t.Fatal(err)
	}
	// Corrupt the file; Scan must not error and must keep the last good unit.
	writeFile(t, f, `{ not json`)
	units, err := m.Scan()
	if err != nil {
		t.Fatalf("malformed seeded file should not fail Scan: %v", err)
	}
	if len(units) != 1 || units[0].Name != "pi-1" {
		t.Fatalf("last-good seeded set not retained: %+v", units)
	}
}

func TestSeededPinOnlyOverride(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "seeded.json")
	writeFile(t, f, `[{"name":"pi-1","type":"pi-player","address":"x","pin_only":false}]`)
	m := newSeededMonitor(t, f)
	units, _ := m.Scan()
	if units[0].PinOnly {
		t.Fatalf("pin_only=false override ignored")
	}
}

// byIDName is the udev by-id basename an ESP32 built-in USB-JTAG enumerates as; its
// serial segment is the board's MAC.
func byIDName(mac string) string {
	return "usb-Espressif_USB_JTAG_serial_debug_unit_" + mac + "-if00"
}

// writeByIDBoards creates fake /dev/serial/by-id entries for the given MACs in a temp
// dir and returns the dir + a glob that matches them.
func writeByIDBoards(t *testing.T, macs ...string) (dir, glob string) {
	t.Helper()
	dir = t.TempDir()
	for _, mac := range macs {
		writeFile(t, filepath.Join(dir, byIDName(mac)), "")
	}
	return dir, filepath.Join(dir, "usb-Espressif_USB_JTAG_serial_debug_unit_*-if00")
}

func chipTypeCaps(typ string) []string {
	switch typ {
	case "esp32c6":
		return []string{"flash", "jtag", "chip:esp32c6"}
	case "esp32c3":
		return []string{"flash", "jtag", "chip:esp32c3"}
	}
	return nil
}

func unitByName(units []runner.Unit, name string) (runner.Unit, bool) {
	for _, u := range units {
		if u.Name == name {
			return u, true
		}
	}
	return runner.Unit{}, false
}

// TestChipOverrideRelabelsBoard covers the core c3-mislabel fix: a board whose MAC
// matches a chip override is labeled with the override's type + name prefix, while a
// non-override board keeps the discovery defaults (esp32c6 / "c6-").
func TestChipOverrideRelabelsBoard(t *testing.T) {
	const (
		c3MAC = "AC:27:6E:7F:18:60" // the rig-3 esp32c3 DUT
		c6MAC = "8C:FD:49:12:56:E8" // an ordinary esp32c6
	)
	_, glob := writeByIDBoards(t, c3MAC, c6MAC)

	cfg := Config{
		Enabled:     true,
		Glob:        glob,
		Type:        "esp32c6",
		NamePrefix:  "c6-",
		SSHPortBase: 2300,
		Max:         8,
		ChipOverrides: map[string]ChipOverride{
			// Key uses colons; the board serial normalization must still match.
			c3MAC: {Type: "esp32c3", NamePrefix: "c3-"},
		},
	}
	m := NewMonitor(cfg, chipTypeCaps, nil)
	units, err := m.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Fatalf("want 2 discovered units, got %d: %+v", len(units), units)
	}

	// (a) Override MAC -> c3-... + esp32c3 type + esp32c3 caps.
	c3, ok := unitByName(units, "c3-7f1860")
	if !ok {
		t.Fatalf("override board not labeled c3-7f1860: %+v", units)
	}
	if c3.Type != "esp32c3" {
		t.Errorf("override unit type = %q, want esp32c3", c3.Type)
	}
	if !hasCap(c3.Capabilities, "chip:esp32c3") || hasCap(c3.Capabilities, "chip:esp32c6") {
		t.Errorf("override unit caps wrong: %v", c3.Capabilities)
	}
	if len(c3.Components) != 1 || c3.Components[0].Type != "esp32c3" {
		t.Errorf("override component type wrong: %+v", c3.Components)
	}

	// (b) Non-override MAC -> c6-... unchanged.
	c6, ok := unitByName(units, "c6-1256e8")
	if !ok {
		t.Fatalf("non-override board not labeled c6-1256e8: %+v", units)
	}
	if c6.Type != "esp32c6" {
		t.Errorf("non-override unit type = %q, want esp32c6", c6.Type)
	}
	if !hasCap(c6.Capabilities, "chip:esp32c6") || hasCap(c6.Capabilities, "chip:esp32c3") {
		t.Errorf("non-override unit caps wrong: %v", c6.Capabilities)
	}
}

// TestChipOverrideSeparatorInsensitive confirms an override key without colons still
// matches a board whose serial has them (and vice versa).
func TestChipOverrideSeparatorInsensitive(t *testing.T) {
	_, glob := writeByIDBoards(t, "AC:27:6E:7F:18:60")
	cfg := Config{
		Enabled: true, Glob: glob, Type: "esp32c6", NamePrefix: "c6-",
		SSHPortBase: 2300, Max: 8,
		ChipOverrides: map[string]ChipOverride{
			"ac276e7f1860": {Type: "esp32c3", NamePrefix: "c3-"}, // no separators, lowercase
		},
	}
	m := NewMonitor(cfg, chipTypeCaps, nil)
	units, err := m.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := unitByName(units, "c3-7f1860"); !ok {
		t.Fatalf("separator-insensitive override did not match: %+v", units)
	}
}

// TestChipOverrideParsesFromConfig confirms the new keys round-trip through the JSON
// discovery-config parser (DisallowUnknownFields is used by the catalog, so the
// schema must line up exactly).
func TestChipOverrideParsesFromConfig(t *testing.T) {
	raw := json.RawMessage(`{
		"enabled": true,
		"glob": "/dev/serial/by-id/usb-Espressif_USB_JTAG_serial_debug_unit_*-if00",
		"type": "esp32c6",
		"name_prefix": "c6-",
		"chip_overrides": {
			"AC:27:6E:7F:18:60": {"type": "esp32c3", "name_prefix": "c3-"}
		}
	}`)
	cfg, err := ParseConfig(&raw)
	if err != nil {
		t.Fatal(err)
	}
	ov, ok := cfg.ChipOverrides["AC:27:6E:7F:18:60"]
	if !ok || ov.Type != "esp32c3" || ov.NamePrefix != "c3-" {
		t.Fatalf("chip_overrides not parsed: %+v", cfg.ChipOverrides)
	}
	typ, prefix, matched := cfg.chipFor("ac:27:6e:7f:18:60")
	if !matched || typ != "esp32c3" || prefix != "c3-" {
		t.Fatalf("chipFor did not resolve override: %q %q %v", typ, prefix, matched)
	}
	typ, prefix, matched = cfg.chipFor("11:22:33:44:55:66")
	if matched || typ != "esp32c6" || prefix != "c6-" {
		t.Fatalf("chipFor should fall back to defaults: %q %q %v", typ, prefix, matched)
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
