// DarwinRunner is the macOS execution backend. There is no container: a Mac is a
// multi-tenant host where a reservation is a SCOPED SSH grant into a shared
// daemon user, with the unit's env forced per-key. The iPhone (devicectl /
// usbmux), the Simulator (simctl), and an attached ESP32-C6 (esptool) are all
// reached host-native from that session — exactly the tools a Mac already has,
// which a Linux container can't run.
//
// Mechanism: the daemon owns a single "managed" authorized_keys file that the
// host sshd reads for the daemon user (AuthorizedKeysFile, set by the nix-darwin
// module). Each active reservation contributes ONE line to it: the holder's
// pubkey prefixed with per-key `environment="K=V"` options carrying the unit's
// env. The file is rebuilt atomically from the set of active reservations on
// every Start/Stop, so a key is authorized exactly while its reservation is
// active and vanishes the instant it ends. Exclusivity is the engine's job (one
// active reservation per unit), so a shared sshd is safe — each key only ever
// carries its own unit's env, and two concurrently-active units (e.g. ios-phone
// + ios-sim) log in as the same user but land in different env.
//
// `environment=` options require `PermitUserEnvironment yes` in sshd_config (the
// nix-darwin module sets it for the daemon user's Match block). This file builds
// on every platform (pure os/filepath), so the linux binary keeps compiling; it
// is simply never selected off darwin (see main.go's runtime.GOOS switch).
package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fughilli/hitl-reserve/api"
)

// DarwinConfig configures the DarwinRunner.
type DarwinConfig struct {
	// Host is the address holders use to reach this Mac (e.g. its tailnet name).
	Host string
	// SSHUser is the shared daemon login user the holder sshes into.
	SSHUser string
	// SSHPort is the host sshd port holders connect to (the Mac's real sshd, 22 by
	// default). Unlike the podman backend there is no per-unit published port —
	// reservations share the host sshd, scoped by their authorized_keys entry.
	SSHPort int
	// StateDir is a writable dir for per-reservation scratch. The managed
	// authorized_keys the host sshd reads for SSHUser is written to
	// StateDir/authorized_keys; per-reservation lines live under StateDir/res/<id>.
	StateDir string
	// AddressEnv is the env var a unit with exactly one network component's Address
	// is injected under (default "HITL_ADDRESS"). Every network component's address
	// is also injected as HITL_ADDRESS_<NAME> regardless.
	AddressEnv string
	// ExtraEnv is injected into every reservation session (e.g. a broker URL).
	ExtraEnv map[string]string
}

// DarwinRunner implements Runner by maintaining a managed authorized_keys file.
type DarwinRunner struct {
	cfg DarwinConfig
}

// NewDarwin builds a DarwinRunner, filling defaults.
func NewDarwin(cfg DarwinConfig) *DarwinRunner {
	if cfg.SSHUser == "" {
		cfg.SSHUser = "hitl"
	}
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 22
	}
	if cfg.AddressEnv == "" {
		cfg.AddressEnv = "HITL_ADDRESS"
	}
	return &DarwinRunner{cfg: cfg}
}

func (d *DarwinRunner) resDir() string { return filepath.Join(d.cfg.StateDir, "res") }

// Start authorizes the holder's key (env-scoped to the unit) by writing its
// authorized_keys line and rebuilding the managed file, then returns the shared
// sshd endpoint. Idempotent: a second Start for the same id overwrites its line.
func (d *DarwinRunner) Start(ctx context.Context, id, owner, sshKey string, unit Unit) (*api.Endpoint, error) {
	if strings.TrimSpace(sshKey) == "" {
		return nil, fmt.Errorf("darwin runner: empty ssh key for reservation %s", id)
	}
	dir := filepath.Join(d.resDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state: %w", err)
	}
	line := authLine(sshKey, d.unitEnv(unit))
	if err := os.WriteFile(filepath.Join(dir, "authline"), []byte(line+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write authline: %w", err)
	}
	if err := d.rebuildAuthorizedKeys(); err != nil {
		return nil, err
	}
	return &api.Endpoint{Host: d.cfg.Host, Port: d.cfg.SSHPort, User: d.cfg.SSHUser}, nil
}

// Stop revokes the reservation's key and rebuilds the managed file. Safe for an
// unknown id (nothing to remove — the rebuild just re-emits the survivors).
func (d *DarwinRunner) Stop(ctx context.Context, id string) error {
	_ = os.RemoveAll(filepath.Join(d.resDir(), id))
	return d.rebuildAuthorizedKeys()
}

// Cleanup drops every reservation grant (e.g. at daemon startup after a crash) so
// a fresh queue starts from an empty authorized_keys.
func (d *DarwinRunner) Cleanup(ctx context.Context) error {
	if err := os.RemoveAll(d.resDir()); err != nil {
		return fmt.Errorf("clear reservations: %w", err)
	}
	if err := os.MkdirAll(d.resDir(), 0o700); err != nil {
		return err
	}
	return d.rebuildAuthorizedKeys()
}

// unitEnv assembles the env forced into the reservation's SSH session, mirroring
// the podman backend: ExtraEnv, then each component's Env and its resolved
// Address (as HITL_ADDRESS_<NAME>, plus AddressEnv when there's exactly one
// network component), then unit-level Env (unit keys win on conflict), plus the
// HITL_UNIT / HITL_UNIT_TYPE / HITL_SSH_USER markers.
func (d *DarwinRunner) unitEnv(unit Unit) map[string]string {
	env := map[string]string{}
	for k, v := range d.cfg.ExtraEnv {
		env[k] = v
	}
	env["HITL_SSH_USER"] = d.cfg.SSHUser
	env["HITL_UNIT"] = unit.Name
	if unit.Type != "" {
		env["HITL_UNIT_TYPE"] = unit.Type
	}
	var addresses []string
	for _, c := range unit.Components {
		for k, v := range c.Env {
			env[k] = v
		}
		if c.Address != "" {
			addr := resolveAddr(c.Address)
			env["HITL_ADDRESS_"+envKey(c.Name)] = addr
			addresses = append(addresses, addr)
		}
	}
	if len(addresses) == 1 {
		env[d.cfg.AddressEnv] = addresses[0]
	}
	for k, v := range unit.Env {
		env[k] = v // unit-level env wins
	}
	return env
}

// rebuildAuthorizedKeys atomically rewrites StateDir/authorized_keys as the
// concatenation of every active reservation's authline. Atomic (temp + rename)
// so the host sshd never reads a half-written file mid-rebuild.
func (d *DarwinRunner) rebuildAuthorizedKeys() error {
	entries, err := os.ReadDir(d.resDir())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read reservations: %w", err)
	}
	var b strings.Builder
	b.WriteString("# Managed by hitl-reserved (DarwinRunner) — do not edit; rebuilt per reservation.\n")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d.resDir(), e.Name(), "authline"))
		if err != nil {
			continue // a half-torn-down reservation dir: skip, it re-emits on the next rebuild
		}
		b.Write(data)
		if n := len(data); n > 0 && data[n-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	dst := filepath.Join(d.cfg.StateDir, "authorized_keys")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write authorized_keys: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("install authorized_keys: %w", err)
	}
	return nil
}

// authLine builds one authorized_keys line: the pubkey prefixed with per-key
// `environment="K=V"` options (sorted for determinism). Env values are sanitized
// — sshd options are comma-separated and double-quoted, so a stray quote,
// backslash, comma or newline could break out of the option or inject another
// authorized_keys directive; we drop those characters rather than trust the
// value. Exported-free (package-internal) so darwin.go and its test share it.
func authLine(sshKey string, env map[string]string) string {
	var opts []string
	for _, k := range sortedKeys(env) {
		opts = append(opts, `environment="`+sanitizeEnv(k)+"="+sanitizeEnv(env[k])+`"`)
	}
	key := strings.TrimSpace(sshKey)
	if len(opts) == 0 {
		return key
	}
	return strings.Join(opts, ",") + " " + key
}

// sanitizeEnv strips characters that would let an env value break out of its
// double-quoted authorized_keys option (or inject a new key line).
func sanitizeEnv(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '"', '\\', ',', 0:
			return -1
		}
		return r
	}, v)
}
