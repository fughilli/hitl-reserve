package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
