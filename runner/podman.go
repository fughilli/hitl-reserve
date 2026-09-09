package runner

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fughilli/hitl-reserve/api"
)

// PodmanConfig configures how reservation environments are launched. Per-unit
// settings (ssh port, components, their device nodes and env) live on Unit and
// arrive at Start.
type PodmanConfig struct {
	// Image is the OCI image reference for the reservation environment. It must run
	// an sshd on :22 that installs the key mounted at /run/hitl/authorized_keys and
	// logs in $HITL_SSH_USER (see the reference entrypoint in docs/DESIGN.md).
	Image string
	// Host is the address holders use to reach this machine (e.g. its tailnet name).
	Host string
	// SSHUser is the login user inside the environment.
	SSHUser string
	// StateDir is a writable dir for per-reservation scratch (authorized_keys, the
	// private raw-USB tree).
	StateDir string
	// Podman is the podman binary (defaults to "podman" on PATH).
	Podman string
	// Privileged runs containers privileged. AVOID on a multi-unit host: a
	// privileged container bind-mounts the whole host /dev, so every unit's nodes
	// leak into every environment. With it off, a unit is confined to its explicit
	// --device nodes plus (with RawUSB) its own boards' nodes in a private
	// /dev/bus/usb. Kept as an escape hatch.
	Privileged bool
	// RawUSB gives each environment raw USB access to its unit's boards, isolated to
	// just those boards' nodes in a private /dev/bus/usb tree (see isolateUSB) that
	// tracks re-enumerations. Needed for libusb control paths (flashing, USB-JTAG,
	// SDR control). When a board's port can't be resolved it falls back to a
	// whole-bus mount so non-hardware reservations still start.
	RawUSB bool
	// AddressEnv is the env var a unit with exactly one network component's Address
	// is injected under (default "HITL_ADDRESS"). Every network component's address
	// is also injected as HITL_ADDRESS_<NAME> regardless.
	AddressEnv string
	// ExtraEnv is injected into every environment (e.g. a shared-resource broker URL
	// the toolbox uses to reach the daemon's logic analyzer).
	ExtraEnv map[string]string
	// Mounts are extra "-v" bind mounts added to every environment.
	Mounts []string
}

// PodmanRunner implements Runner by shelling out to podman.
type PodmanRunner struct {
	cfg PodmanConfig
	mu  sync.Mutex
	// usbSync holds each reservation's raw-USB refresher cancel func, keeping its
	// private tree current across the boards' re-enumerations. Cancelled on Stop.
	usbSync map[string]func()
}

// NewPodman builds a PodmanRunner, filling defaults.
func NewPodman(cfg PodmanConfig) *PodmanRunner {
	if cfg.Podman == "" {
		cfg.Podman = "podman"
	}
	if cfg.SSHUser == "" {
		cfg.SSHUser = "agent"
	}
	if cfg.AddressEnv == "" {
		cfg.AddressEnv = "HITL_ADDRESS"
	}
	return &PodmanRunner{cfg: cfg, usbSync: map[string]func(){}}
}

// containerName is deterministic per reservation so Stop/Cleanup can find it.
func containerName(id string) string { return "hitl-" + id }

func (p *PodmanRunner) podman(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, p.cfg.Podman, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("podman %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (p *PodmanRunner) Start(ctx context.Context, id, owner, sshKey string, unit Unit) (*api.Endpoint, error) {
	name := containerName(id)
	_, _ = p.podman(ctx, "rm", "-f", name) // clear any stale container with this name

	// Write the holder's authorized_keys to a per-reservation file and mount it.
	keyDir := filepath.Join(p.cfg.StateDir, id)
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state: %w", err)
	}
	authKeys := filepath.Join(keyDir, "authorized_keys")
	if err := os.WriteFile(authKeys, []byte(strings.TrimSpace(sshKey)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write authorized_keys: %w", err)
	}

	args := []string{
		"run", "-d", "--name", name,
		"--label", "hitl=1",
		"--label", "hitl.owner=" + owner,
		"--label", "hitl.unit=" + unit.Name,
		"-p", fmt.Sprintf("%d:22", unit.SSHPort),
		"-v", authKeys + ":/run/hitl/authorized_keys:ro",
		"-e", "HITL_SSH_USER=" + p.cfg.SSHUser,
		"-e", "HITL_UNIT=" + unit.Name,
	}
	if unit.Type != "" {
		args = append(args, "-e", "HITL_UNIT_TYPE="+unit.Type)
	}
	for _, k := range sortedKeys(p.cfg.ExtraEnv) {
		args = append(args, "-e", k+"="+p.cfg.ExtraEnv[k])
	}
	for _, m := range p.cfg.Mounts {
		args = append(args, "-v", m)
	}

	// Merge env across components (component env, then addresses), then unit-level
	// env (unit keys win). Collect USB ttys for raw-USB isolation across the unit.
	env := map[string]string{}
	var ttys, deviceArgs []string
	var addresses []string
	names := make([]string, 0, len(unit.Components))
	for _, c := range unit.Components {
		names = append(names, c.Name)
		for k, v := range c.Env {
			env[k] = v
		}
		if c.Address != "" {
			addr := resolveAddr(c.Address)
			env["HITL_ADDRESS_"+envKey(c.Name)] = addr
			addresses = append(addresses, addr)
		}
		if c.Kind == "network" {
			continue // network components own no local board: no USB, no --device
		}
		for _, d := range c.Devices {
			if d == "" {
				continue
			}
			if tty := ttyOf(d); tty != "" {
				ttys = append(ttys, tty)
			}
			if arg, ok := deviceMapping(d); ok {
				deviceArgs = append(deviceArgs, "--device", arg)
			} else {
				log.Printf("podman: device %q not present, skipping", d)
			}
		}
	}
	if len(addresses) == 1 {
		env[p.cfg.AddressEnv] = addresses[0]
	}
	env["HITL_COMPONENTS"] = strings.Join(names, ",")
	for k, v := range unit.Env {
		env[k] = v
	}
	for _, k := range sortedKeys(env) {
		args = append(args, "-e", k+"="+env[k])
	}

	// Raw USB, isolated to this unit's boards (their nodes only), when enabled and
	// the unit has USB components; else the explicit --device mappings alone.
	if p.cfg.RawUSB && len(ttys) > 0 {
		args = append(args, p.isolateUSB(id, unit.Name, ttys)...)
	}
	args = append(args, deviceArgs...)
	if p.cfg.Privileged {
		args = append(args, "--privileged")
	}
	args = append(args, p.cfg.Image)

	if out, err := p.podman(ctx, args...); err != nil {
		return nil, fmt.Errorf("start: %w (%s)", err, out)
	}
	// Don't report ready until sshd actually accepts (host-key gen + exec takes a
	// couple seconds), so holders don't race it.
	if err := waitTCP(ctx, fmt.Sprintf("127.0.0.1:%d", unit.SSHPort), 60*time.Second); err != nil {
		log.Printf("podman: %s sshd not ready: %v (returning endpoint anyway)", name, err)
	}
	log.Printf("podman: started %s (owner=%q unit=%s) sshd on %s:%d", name, owner, unit.Name, p.cfg.Host, unit.SSHPort)
	return &api.Endpoint{Host: p.cfg.Host, Port: unit.SSHPort, User: p.cfg.SSHUser}, nil
}

// isolateUSB returns the podman args exposing raw USB to this reservation, scoped
// to the unit's boards, and starts a refresher that keeps the private tree current
// across the boards' re-enumerations. Falls back to a whole-bus mount when no port
// resolves (e.g. no board attached), so non-hardware reservations still start.
func (p *PodmanRunner) isolateUSB(id, unitName string, ttys []string) []string {
	if _, err := os.Stat("/dev/bus/usb"); err != nil {
		return nil // no raw USB on this host at all
	}
	wholeBus := []string{"-v", "/dev/bus/usb:/dev/bus/usb", "--device-cgroup-rule", "c 189:* rwm"}

	var portDirs []string
	var nodes []usbNode
	for _, tty := range ttys {
		host := tty
		if r, err := filepath.EvalSymlinks(tty); err == nil {
			host = r
		}
		portDir, portID, err := resolveUSBPort(host)
		if err != nil {
			log.Printf("podman: %s raw-USB isolation for %s unavailable (%v)", unitName, tty, err)
			continue
		}
		node, err := readUSBNode(portDir)
		if err != nil {
			log.Printf("podman: %s raw-USB node for %s unavailable (port %s: %v)", unitName, tty, portID, err)
			continue
		}
		portDirs = append(portDirs, portDir)
		nodes = append(nodes, node)
	}
	if len(portDirs) == 0 {
		log.Printf("podman: %s no resolvable USB ports; whole-bus fallback", unitName)
		return wholeBus
	}

	destDir := filepath.Join(p.cfg.StateDir, id, "usb", "bus", "usb")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		log.Printf("podman: %s raw-USB isolation dir: %v; whole-bus fallback", unitName, err)
		return wholeBus
	}
	if err := syncUSBNodes(destDir, nodes); err != nil {
		log.Printf("podman: %s raw-USB node seed: %v; whole-bus fallback", unitName, err)
		return wholeBus
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	if prev := p.usbSync[id]; prev != nil {
		prev()
	}
	p.usbSync[id] = cancel
	p.mu.Unlock()
	go refreshUSBNodes(ctx, unitName, portDirs, destDir)

	log.Printf("podman: %s raw USB isolated to %d port(s) -> %s", unitName, len(portDirs), destDir)
	return []string{
		"-v", destDir + ":/dev/bus/usb",
		"--device-cgroup-rule", "c 189:* rwm",
		// Belt-and-suspenders: the container can't mknod a node for a neighbour's
		// board (unprivileged podman drops MKNOD already; make it explicit).
		"--cap-drop", "mknod",
	}
}

// refreshUSBNodes keeps destDir's node set current for the container's lifetime.
// A board re-enumerates on every reset (its node minor changes), so it polls each
// port and re-syncs. A briefly-absent board (mid-reset) keeps its last-known node
// through a grace window; only a board gone past the window is dropped.
func refreshUSBNodes(ctx context.Context, unitName string, portDirs []string, destDir string) {
	const interval = 1 * time.Second
	const grace = 5 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	lastGood := map[string]usbNode{}
	absentSince := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			var nodes []usbNode
			for _, pd := range portDirs {
				node, err := readUSBNode(pd)
				if err != nil {
					if absentSince[pd].IsZero() {
						absentSince[pd] = now
					}
					if now.Sub(absentSince[pd]) < grace {
						if n, ok := lastGood[pd]; ok {
							nodes = append(nodes, n) // hold through a reset
						}
					}
					continue
				}
				delete(absentSince, pd)
				lastGood[pd] = node
				nodes = append(nodes, node)
			}
			if err := syncUSBNodes(destDir, nodes); err != nil {
				log.Printf("podman: %s raw-USB refresh: %v", unitName, err)
			}
		}
	}
}

// stopUSBSync cancels and forgets a reservation's raw-USB refresher, if any.
func (p *PodmanRunner) stopUSBSync(id string) {
	p.mu.Lock()
	if cancel := p.usbSync[id]; cancel != nil {
		cancel()
		delete(p.usbSync, id)
	}
	p.mu.Unlock()
}

func (p *PodmanRunner) Stop(ctx context.Context, id string) error {
	p.stopUSBSync(id)
	name := containerName(id)
	_, err := p.podman(ctx, "rm", "-f", "-t", "5", name)
	_ = os.RemoveAll(filepath.Join(p.cfg.StateDir, id))
	if err != nil && !strings.Contains(err.Error(), "no such container") {
		return err
	}
	return nil
}

// Cleanup removes every container we labeled, e.g. after a daemon crash.
func (p *PodmanRunner) Cleanup(ctx context.Context) error {
	p.mu.Lock()
	for id, cancel := range p.usbSync {
		cancel()
		delete(p.usbSync, id)
	}
	p.mu.Unlock()

	out, err := p.podman(ctx, "ps", "-aq", "--filter", "label=hitl=1")
	if err != nil {
		return err
	}
	for _, cid := range strings.Fields(out) {
		if _, err := p.podman(ctx, "rm", "-f", cid); err != nil {
			log.Printf("cleanup: %v", err)
		}
	}
	return nil
}

// ttyOf returns the host tty path of a "host[:container]" device mapping when the
// container path is the pinned serial tty (or unset), else "". It anchors raw-USB
// isolation on the board's serial node.
func ttyOf(d string) string {
	host, container := d, ""
	if i := strings.LastIndex(d, ":/"); i >= 0 {
		host, container = d[:i], d[i+1:]
	}
	if container != "" && container != "/dev/ttyACM0" && !strings.HasPrefix(container, "/dev/tty") {
		return ""
	}
	return host
}

// deviceMapping resolves one "host[:container]" --device spec into a concrete
// podman --device value, or ok=false if the host device isn't present. It splits
// on the last ":/" (a by-id name can itself contain colons, e.g. a MAC) and
// resolves any symlink to the real node (podman --device wants a real node, and it
// tracks a board that re-enumerated to a different ttyACMx since discovery).
func deviceMapping(d string) (arg string, ok bool) {
	host, container := d, ""
	if i := strings.LastIndex(d, ":/"); i >= 0 {
		host, container = d[:i], d[i+1:]
	}
	real := host
	if r, err := filepath.EvalSymlinks(host); err == nil {
		real = r
	}
	if _, err := os.Stat(real); err != nil {
		return "", false
	}
	if container != "" {
		return real + ":" + container, true
	}
	return real, true
}

// sortedKeys returns m's keys in sorted order, for deterministic arg ordering.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// envKey upper-cases a component name into an env-var-safe suffix.
func envKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// resolveAddr resolves a component management address to an IP the environment can
// dial. A network component may be given by hostname (e.g. foo.local), but the
// environment may lack an mDNS resolver, so resolve it HERE via `getent hosts`
// (the host glibc NSS) and inject the IP. An IP literal or empty passes through;
// any failure keeps the original name.
func resolveAddr(addr string) string {
	host, port := addr, ""
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host, port = h, p
	}
	if host == "" || net.ParseIP(host) != nil {
		return addr
	}
	out, err := exec.Command("getent", "hosts", host).Output()
	if err != nil {
		log.Printf("resolveAddr: getent hosts %q failed: %v (using name)", host, err)
		return addr
	}
	f := strings.Fields(string(out))
	if len(f) == 0 || net.ParseIP(f[0]) == nil {
		return addr
	}
	if port != "" {
		return net.JoinHostPort(f[0], port)
	}
	return f[0]
}

// waitTCP blocks until addr accepts a connection or timeout.
func waitTCP(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no listener on %s after %s: %w", addr, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
