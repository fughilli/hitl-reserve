// Command hitl is the client for the reservation fleet. It selects a host from the
// pool ($HITL_HOSTS), queues for a matching unit, waits for it to activate, opens
// an SSH session into the unit's environment while heartbeating to hold the lease,
// and releases on exit.
//
// Subcommands:
//
//	hitl reserve [--unit N | --type T | --caps a,b] [--host URL] [-- CMD...]
//	hitl status  [--host URL]        # or all hosts in the pool
//	hitl release <id> [--host URL]
//	hitl shared  <name> [--host URL] [--unit U] [BODY|-]   # broker access (e.g. analyzer capture)
//	hitl maintenance [--release | --status] [--host URL]   # cordon + drain the host for maintenance
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/pool"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "reserve":
		err = cmdReserve(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "release":
		err = cmdRelease(os.Args[2:])
	case "shared":
		err = cmdShared(os.Args[2:])
	case "maintenance":
		err = cmdMaintenance(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `hitl — reservation client

  hitl reserve [--unit N | --type T | --caps a,b] [--host URL] [--owner S] [-- CMD...]
  hitl status  [--host URL]
  hitl release <id> [--host URL]
  hitl shared  <name> [--host URL] [--unit U] [BODY|-]
  hitl maintenance [--release | --status] [--host URL]

Host selection: --host pins one host; otherwise $HITL_HOSTS (comma/space list) is
the pool and the best matching host is chosen automatically.
`)
}

// --- reserve --------------------------------------------------------------

func cmdReserve(args []string) error {
	fs, opts := commonFlags("reserve")
	unit := fs.String("unit", "", "pin to a specific unit by name")
	typ := fs.String("type", "", "require any free unit of this type")
	caps := fs.String("caps", "", "require any free unit with these capabilities (comma-separated)")
	owner := fs.String("owner", defaultOwner(), "owner id for logs/status")
	ownerEmail := fs.String("owner-email", os.Getenv("HITL_OWNER_EMAIL"),
		"attributable email of the person this reservation is for (default $HITL_OWNER_EMAIL). "+
			"On a tailnet-verified daemon this is auto-filled from your tailscale identity and "+
			"may be omitted; supply it when reserving on someone else's behalf from tagged infra.")
	actor := fs.String("actor", envOr("HITL_ACTOR", "human"),
		"who is reserving: \"human\" (a person at a keyboard) or \"agent\" (automation acting "+
			"on the owner's behalf); default $HITL_ACTOR or human")
	fs.Parse(args)
	cmd := fs.Args()

	reqCaps := splitCSV(*caps)

	// Generate a throwaway keypair for this reservation.
	keyDir, priv, pub, err := genKey()
	if err != nil {
		return err
	}
	defer os.RemoveAll(keyDir)

	base, err := chooseHost(*opts, pool.Require{Type: *typ, Caps: reqCaps})
	if err != nil {
		return err
	}

	body, _ := json.Marshal(api.ReserveRequest{
		Owner: *owner, OwnerEmail: *ownerEmail, Actor: *actor,
		SSHPublicKey: pub, Unit: *unit, UnitType: *typ, RequireCaps: reqCaps,
	})
	var res api.Reservation
	if err := doJSON(http.MethodPost, base+"/reserve", body, &res); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "reserved id=%s on %s (state=%s)\n", res.ID, base, res.State)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Heartbeat in the background (queued and active) so the lease never lapses.
	hbDone := make(chan struct{})
	go heartbeat(ctx, base, res.ID, hbDone)
	// Always release on exit.
	defer func() {
		_ = doJSON(http.MethodPost, base+"/reservation/"+res.ID+"/release", nil, nil)
		close(hbDone)
	}()

	active, err := waitActive(ctx, base, res.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "active: unit=%s endpoint=%s:%d\n", active.Unit, active.Endpoint.Host, active.Endpoint.Port)

	return sshInto(ctx, active.Endpoint, priv, cmd)
}

func waitActive(ctx context.Context, base, id string) (*api.Reservation, error) {
	lastPos := -1
	for {
		var res api.Reservation
		if err := doJSON(http.MethodGet, base+"/reservation/"+id, nil, &res); err != nil {
			return nil, err
		}
		switch res.State {
		case api.StateActive:
			if res.Endpoint == nil {
				return nil, fmt.Errorf("reservation active but no endpoint")
			}
			return &res, nil
		case api.StateReleased:
			return nil, fmt.Errorf("reservation released before activating: %s", res.Message)
		default:
			if res.Position != lastPos {
				fmt.Fprintf(os.Stderr, "queued: position %d\n", res.Position)
				lastPos = res.Position
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func heartbeat(ctx context.Context, base, id string, done <-chan struct{}) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			_ = doJSON(http.MethodPost, base+"/reservation/"+id+"/heartbeat", nil, nil)
		}
	}
}

func sshInto(ctx context.Context, ep *api.Endpoint, keyPath string, cmd []string) error {
	args := []string{
		"-i", keyPath,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-p", fmt.Sprint(ep.Port),
		fmt.Sprintf("%s@%s", ep.User, ep.Host),
	}
	args = append(args, cmd...)
	c := exec.CommandContext(ctx, "ssh", args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// --- status ---------------------------------------------------------------

func cmdStatus(args []string) error {
	fs, opts := commonFlags("status")
	fs.Parse(args)

	hosts := poolHosts(*opts)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts (set --host or $HITL_HOSTS)")
	}
	for _, base := range hosts {
		var st api.Status
		if err := doJSON(http.MethodGet, base+"/status", nil, &st); err != nil {
			fmt.Printf("%s\tUNREACHABLE (%v)\n", base, err)
			continue
		}
		maint := ""
		if st.Cordoned {
			maint = "  CORDONED"
			if st.Draining {
				maint = "  CORDONED (draining)"
			}
		}
		fmt.Printf("%s  host=%s workspace=%s  free=%d queued=%d%s\n", base, st.Host, st.Workspace, st.FreeUnits, st.QueueLength, maint)
		for _, u := range st.Units {
			state := "free"
			if u.Active != nil {
				state = "BUSY (" + u.Active.Owner + ")"
			}
			pin := ""
			if u.PinOnly {
				pin = " pin-only"
			}
			fmt.Printf("  %-16s type=%-14s kind=%-9s%s  %s\n", u.Name, u.Type, u.Kind, pin, state)
			if len(u.Capabilities) > 0 {
				fmt.Printf("      caps: %s\n", strings.Join(u.Capabilities, ", "))
			}
		}
		for _, s := range st.Shared {
			fmt.Printf("  [shared] %s kind=%s present=%v\n", s.Name, s.Kind, s.Present)
		}
		if st.Provisioning != nil {
			fmt.Printf("  [provisioning] ssid=%s psk=%s\n", st.Provisioning.SSID, st.Provisioning.PSK)
		}
	}
	return nil
}

// --- release --------------------------------------------------------------

func cmdRelease(args []string) error {
	fs, opts := commonFlags("release")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: hitl release <id> [--host URL]")
	}
	id := fs.Arg(0)
	base, err := singleHost(*opts)
	if err != nil {
		return err
	}
	if err := doJSON(http.MethodPost, base+"/reservation/"+id+"/release", nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "released %s on %s\n", id, base)
	return nil
}

// --- shared (broker access) ----------------------------------------------

func cmdShared(args []string) error {
	fs, opts := commonFlags("shared")
	unit := fs.String("unit", "", "unit to scope the request to")
	fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: hitl shared <name> [--unit U] [BODY|-]")
	}
	name := fs.Arg(0)
	var body []byte
	if fs.NArg() >= 2 {
		arg := fs.Arg(1)
		if arg == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			body = b
		} else {
			body = []byte(arg)
		}
	}
	base, err := singleHost(*opts)
	if err != nil {
		return err
	}
	url := base + "/shared/" + name
	if *unit != "" {
		url += "?unit=" + *unit
	}
	raw, err := doRaw(http.MethodPost, url, body)
	if err != nil {
		return err
	}
	os.Stdout.Write(raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

// --- maintenance (cordon + drain) -----------------------------------------

// cmdMaintenance drives the host's maintenance mode. With no flag it cordons the
// host and polls until fully drained (no active reservations), printing progress —
// the state to be in before a daemon redeploy. --status prints the current cordon
// state; --release lifts the cordon so queued reservations resume activating.
func cmdMaintenance(args []string) error {
	fs, opts := commonFlags("maintenance")
	release := fs.Bool("release", false, "lift the cordon and resume activating queued reservations")
	statusOnly := fs.Bool("status", false, "print the current cordon/drain state and exit")
	fs.Parse(args)

	base, err := singleHost(*opts)
	if err != nil {
		return err
	}

	if *statusOnly {
		var mst api.Maintenance
		if err := doJSON(http.MethodGet, base+"/maintenance", nil, &mst); err != nil {
			return err
		}
		printMaintenance(base, mst)
		return nil
	}

	if *release {
		var mst api.Maintenance
		if err := doJSON(http.MethodPost, base+"/maintenance/release", nil, &mst); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "released cordon on %s (queued reservations resume activating)\n", base)
		printMaintenance(base, mst)
		return nil
	}

	// Enter maintenance, then poll until drained, printing progress.
	var mst api.Maintenance
	if err := doJSON(http.MethodPost, base+"/maintenance", nil, &mst); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cordoned %s: %d active to drain, %d queued (held)\n", base, mst.Active, mst.Queued)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lastActive := -1
	for {
		if err := doJSON(http.MethodGet, base+"/maintenance", nil, &mst); err != nil {
			return err
		}
		if mst.Drained {
			fmt.Fprintf(os.Stderr, "drained; in maintenance (release with: hitl maintenance --release --host %s)\n", base)
			return nil
		}
		if mst.Active != lastActive {
			fmt.Fprintf(os.Stderr, "draining: %d active remaining\n", mst.Active)
			lastActive = mst.Active
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func printMaintenance(base string, m api.Maintenance) {
	state := "in service"
	if m.Cordoned {
		state = "cordoned"
		if !m.Drained {
			state = "cordoned (draining)"
		}
	}
	fmt.Printf("%s  %s  active=%d queued=%d drained=%v\n", base, state, m.Active, m.Queued, m.Drained)
}

// --- host selection -------------------------------------------------------

type commonOpts struct {
	host string
}

func commonFlags(name string) (*flag.FlagSet, *commonOpts) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	opts := &commonOpts{}
	fs.StringVar(&opts.host, "host", "", "pin to this host URL (else use $HITL_HOSTS)")
	return fs, opts
}

func poolHosts(o commonOpts) []string {
	if o.host != "" {
		return pool.Normalize(o.host)
	}
	return pool.Normalize(os.Getenv("HITL_HOSTS"))
}

func singleHost(o commonOpts) (string, error) {
	hosts := poolHosts(o)
	if len(hosts) == 0 {
		return "", fmt.Errorf("no host (set --host or $HITL_HOSTS)")
	}
	return hosts[0], nil
}

// chooseHost picks the best host for a request from the pool via /status probes.
func chooseHost(o commonOpts, req pool.Require) (string, error) {
	hosts := poolHosts(o)
	if len(hosts) == 0 {
		return "", fmt.Errorf("no hosts (set --host or $HITL_HOSTS)")
	}
	if len(hosts) == 1 && o.host != "" {
		return hosts[0], nil
	}
	probes := pool.Probes(hosts, func(base string) (*api.Status, error) {
		var st api.Status
		if err := doJSON(http.MethodGet, base+"/status", nil, &st); err != nil {
			return nil, err
		}
		return &st, nil
	})
	return pool.Pick(probes, req)
}

// --- HTTP + key helpers ---------------------------------------------------

var httpClient = &http.Client{Timeout: 15 * time.Second}

func doRaw(method, url string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e api.Error
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func doJSON(method, url string, body []byte, out any) error {
	data, err := doRaw(method, url, body)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// genKey makes a throwaway ed25519 keypair via ssh-keygen and returns the temp
// dir, private-key path, and public-key string.
func genKey() (dir, privPath, pub string, err error) {
	dir, err = os.MkdirTemp("", "hitl-key-")
	if err != nil {
		return "", "", "", err
	}
	privPath = dir + "/id"
	out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", privPath, "-C", "hitl").CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		return "", "", "", fmt.Errorf("ssh-keygen: %w: %s", err, out)
	}
	pubBytes, err := os.ReadFile(privPath + ".pub")
	if err != nil {
		os.RemoveAll(dir)
		return "", "", "", err
	}
	return dir, privPath, strings.TrimSpace(string(pubBytes)), nil
}

func defaultOwner() string {
	if v := os.Getenv("HITL_OWNER"); v != "" {
		return v
	}
	u, _ := os.Hostname()
	return "hitl-cli@" + u
}

// envOr returns $key if set and non-empty, else def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, f)
	}
	return out
}
