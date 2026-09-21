package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readAuthKeys(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "authorized_keys"))
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	return string(b)
}

func TestDarwinStartStopManagesAuthorizedKeys(t *testing.T) {
	dir := t.TempDir()
	d := NewDarwin(DarwinConfig{Host: "mac.tailnet", SSHUser: "hitl", SSHPort: 2200, StateDir: dir})
	ctx := context.Background()

	ep, err := d.Start(ctx, "res-1", "alice", "ssh-ed25519 AAAAkey1 alice", Unit{
		Name: "ios-phone", Type: "ios",
		Components: []Component{{Name: "c6-c", Env: map[string]string{"HITL_SERIAL": "AB12"}}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ep.Host != "mac.tailnet" || ep.Port != 2200 || ep.User != "hitl" {
		t.Fatalf("endpoint = %+v, want mac.tailnet:2200 user hitl", ep)
	}
	ak := readAuthKeys(t, dir)
	if !strings.Contains(ak, "AAAAkey1") {
		t.Errorf("authorized_keys missing the holder key:\n%s", ak)
	}
	for _, want := range []string{
		`environment="HITL_SERIAL=AB12"`,
		`environment="HITL_UNIT=ios-phone"`,
		`environment="HITL_UNIT_TYPE=ios"`,
	} {
		if !strings.Contains(ak, want) {
			t.Errorf("authorized_keys missing forced env %s:\n%s", want, ak)
		}
	}

	// A second concurrent reservation adds its own line without disturbing the first.
	if _, err := d.Start(ctx, "res-2", "bob", "ssh-ed25519 AAAAkey2 bob", Unit{Name: "ios-sim"}); err != nil {
		t.Fatalf("Start res-2: %v", err)
	}
	ak = readAuthKeys(t, dir)
	if !strings.Contains(ak, "AAAAkey1") || !strings.Contains(ak, "AAAAkey2") {
		t.Errorf("both reservations should be authorized concurrently:\n%s", ak)
	}

	// Stopping res-1 revokes ONLY its key.
	if err := d.Stop(ctx, "res-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ak = readAuthKeys(t, dir)
	if strings.Contains(ak, "AAAAkey1") {
		t.Errorf("res-1 key should be revoked after Stop:\n%s", ak)
	}
	if !strings.Contains(ak, "AAAAkey2") {
		t.Errorf("res-2 key should survive res-1's Stop:\n%s", ak)
	}

	// Stop of an unknown id is safe and leaves survivors intact.
	if err := d.Stop(ctx, "res-unknown"); err != nil {
		t.Errorf("Stop unknown id: %v", err)
	}

	// Cleanup drops everything.
	if err := d.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if ak := readAuthKeys(t, dir); strings.Contains(ak, "AAAAkey2") {
		t.Errorf("Cleanup should clear all grants:\n%s", ak)
	}
}

func TestDarwinRejectsEmptyKey(t *testing.T) {
	d := NewDarwin(DarwinConfig{StateDir: t.TempDir()})
	if _, err := d.Start(context.Background(), "r", "o", "   ", Unit{Name: "u"}); err == nil {
		t.Fatal("Start with an empty ssh key should error")
	}
}

func TestDarwinUnitEnvPrecedenceAndAddresses(t *testing.T) {
	d := NewDarwin(DarwinConfig{StateDir: t.TempDir(), ExtraEnv: map[string]string{"HITL_BROKER_URL": "http://x"}})
	env := d.unitEnv(Unit{
		Name: "u", Type: "t",
		Components: []Component{
			{Name: "phone", Env: map[string]string{"K": "component"}},
			{Name: "dev-net", Kind: "network", Address: "10.0.0.5"},
		},
		Env: map[string]string{"K": "unit-wins"},
	})
	if env["K"] != "unit-wins" {
		t.Errorf("unit-level env should win: K=%q", env["K"])
	}
	if env["HITL_BROKER_URL"] != "http://x" {
		t.Errorf("ExtraEnv missing: %q", env["HITL_BROKER_URL"])
	}
	if env["HITL_ADDRESS_DEV_NET"] != "10.0.0.5" {
		t.Errorf("per-component address env missing: %q", env["HITL_ADDRESS_DEV_NET"])
	}
	if env["HITL_ADDRESS"] != "10.0.0.5" {
		t.Errorf("single network component should set HITL_ADDRESS: %q", env["HITL_ADDRESS"])
	}
}

func TestDarwinAuthLineSanitizesEnv(t *testing.T) {
	// A malicious env value must not break out of its quoted option or inject a
	// second key line — strip quote/comma/backslash/newline.
	line := authLine("ssh-ed25519 KEY user", map[string]string{
		"EVIL": "a\"b,c\\d\nssh-ed25519 INJECTED attacker",
	})
	if strings.Contains(line, "INJECTED\n") || strings.Count(line, "\n") != 0 {
		t.Errorf("authLine must be a single line with no injected key: %q", line)
	}
	if strings.ContainsAny(strings.SplitN(line, " ssh-", 2)[0], "\n") {
		t.Errorf("options segment must have no newline: %q", line)
	}
	if strings.Contains(line, `abc`) == false {
		t.Errorf("sanitized value should keep safe chars: %q", line)
	}
	// The quote/comma/backslash/newline are all gone from the value.
	optSeg := strings.SplitN(line, " ssh-", 2)[0]
	if strings.ContainsAny(strings.TrimPrefix(optSeg, `environment="EVIL=`), `",\`) {
		// the only quotes/backslashes allowed are the option's own closing quote
		if !strings.HasSuffix(optSeg, `"`) {
			t.Errorf("value not sanitized: %q", optSeg)
		}
	}
}

func TestDarwinAuthLineNoEnvIsBareKey(t *testing.T) {
	if got := authLine("  ssh-ed25519 KEY user  ", nil); got != "ssh-ed25519 KEY user" {
		t.Errorf("authLine(no env) = %q, want the trimmed bare key", got)
	}
}
