// Command hitl-reserved is the reservation daemon. It reads a declarative catalog
// (what hardware this host offers), exposes a small JSON API to queue for the
// host's reservable units, and — when a reservation reaches the head of the queue —
// brings its environment up via a runner (the podman reference backend) with the
// unit's components attached and the holder's SSH key authorized.
//
// Config precedence: flags override catalog values (host, workspace, lease); the
// catalog is the source of truth for units, components, and shared resources.
// Optional live USB discovery layers hot-plugged boards on top.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/catalog"
	"github.com/fughilli/hitl-reserve/discovery"
	"github.com/fughilli/hitl-reserve/engine"
	"github.com/fughilli/hitl-reserve/identity"
	"github.com/fughilli/hitl-reserve/metrics"
	"github.com/fughilli/hitl-reserve/runner"
	"github.com/fughilli/hitl-reserve/shared"
	"github.com/fughilli/hitl-reserve/shared/analyzer"
	"github.com/fughilli/hitl-reserve/wifi"
)

// classCapFlag collects repeatable/comma-separated TYPE=N per-class caps.
type classCapFlag struct{ m map[string]int }

func (c *classCapFlag) String() string {
	if c == nil || len(c.m) == 0 {
		return ""
	}
	var parts []string
	for k, v := range c.m {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v))
	}
	sortStrings(parts)
	return strings.Join(parts, ",")
}

func (c *classCapFlag) Set(v string) error {
	if c.m == nil {
		c.m = map[string]int{}
	}
	for _, kv := range strings.Split(v, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, n, ok := strings.Cut(kv, "=")
		var cap int
		if _, err := fmt.Sscanf(n, "%d", &cap); !ok || strings.TrimSpace(k) == "" || err != nil || cap < 0 {
			return fmt.Errorf("want TYPE=N (N >= 0), got %q", kv)
		}
		c.m[strings.TrimSpace(k)] = cap
	}
	return nil
}

// agingFlag collects repeatable/comma-separated CLASS=DURATION priority-aging offsets.
type agingFlag struct{ m map[string]time.Duration }

func (a *agingFlag) String() string {
	if a == nil || len(a.m) == 0 {
		return ""
	}
	var parts []string
	for k, v := range a.m {
		parts = append(parts, k+"="+v.String())
	}
	sortStrings(parts)
	return strings.Join(parts, ",")
}

func (a *agingFlag) Set(v string) error {
	if a.m == nil {
		a.m = map[string]time.Duration{}
	}
	for _, kv := range strings.Split(v, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, d, ok := strings.Cut(kv, "=")
		class, known := api.NormalizePriority(k)
		dur, err := time.ParseDuration(strings.TrimSpace(d))
		if !ok || !known || strings.TrimSpace(k) == "" || err != nil || dur < 0 {
			return fmt.Errorf("want CLASS=DURATION (class one of %v, e.g. dev=60m), got %q", api.PriorityClasses, kv)
		}
		if class == api.PriorityMergeQueue && dur != 0 {
			return fmt.Errorf("%s is the top class; its aging offset is always 0", class)
		}
		a.m[class] = dur
	}
	return nil
}

// classCapAliasFlag collects repeatable/comma-separated TYPE=NAME aliases.
type classCapAliasFlag struct{ m map[string]string }

func (a *classCapAliasFlag) String() string {
	if a == nil {
		return ""
	}
	var parts []string
	for k, v := range a.m {
		parts = append(parts, k+"="+v)
	}
	sortStrings(parts)
	return strings.Join(parts, ",")
}

func (a *classCapAliasFlag) Set(v string) error {
	if a.m == nil {
		a.m = map[string]string{}
	}
	for _, kv := range strings.Split(v, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, n, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(n) == "" {
			return fmt.Errorf("want TYPE=NAME, got %q", kv)
		}
		a.m[strings.TrimSpace(k)] = strings.TrimSpace(n)
	}
	return nil
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	hostname, _ := os.Hostname()

	catalogPath := flag.String("catalog", "", "path to the catalog JSON (required)")
	addr := flag.String("addr", ":8087", "listen address (bind to a trusted interface in prod)")
	hostFlag := flag.String("host", "", "address holders use to reach this machine (default: catalog host or hostname)")
	workspace := flag.String("workspace", "", "override the catalog workspace label (repo/fleet grouping)")
	leaseFlag := flag.Duration("lease", 0, "override the catalog heartbeat lease window")
	image := flag.String("image", "hitl-reserve-env:latest", "OCI image for the reservation environment")
	sshUser := flag.String("ssh-user", "agent", "login user inside the environment")
	stateDir := flag.String("state-dir", "/var/lib/hitl", "writable scratch dir")
	podman := flag.String("podman", "podman", "podman binary")
	privileged := flag.Bool("privileged", false, "run environments privileged (leaks all host /dev; avoid on multi-unit hosts)")
	netHost := flag.Bool("net-host", false, "run environments with host networking (--network=host) so they can drive host interfaces directly (e.g. a WiFi radio via nl80211); the environment sshd binds the unit port itself (HITL_SSH_PORT). Single-unit hosts only — units would otherwise collide on host ports.")
	rawUSB := flag.Bool("raw-usb", true, "give environments raw USB access, isolated per unit")
	brokerURL := flag.String("broker-url", "http://host.containers.internal:8087", "base URL environments use to reach this daemon's shared-resource brokers ($HITL_BROKER_URL)")
	runnerKind := flag.String("runner", "auto", `execution backend: "podman" (Linux container per reservation), "darwin" (scoped SSH grant into a shared macOS host user — no container), or "auto" (darwin on macOS, else podman)`)
	darwinSSHPort := flag.Int("darwin-ssh-port", 22, "host sshd port holders connect to on the darwin runner (reservations share the Mac's sshd, scoped by authorized_keys)")
	maxConcurrent := flag.Int("max-concurrent", 0, "cap concurrently-active reservations on this host regardless of unit count (0 = unlimited); protects a weak host or a shared bus/radio from N-wide load. Surplus reservations queue and start as active ones release.")
	var classCaps classCapFlag
	flag.Var(&classCaps, "max-concurrent-class", "per-unit-class concurrency cap 'TYPE=N' (repeatable, or comma-separated), keyed by the unit type (e.g. esp32c6=2,led-mapper-pi=2). Layered under --max-concurrent: a class at its cap never idles another class's free units, and admission stays FIFO (within a class, strictly in order). Unlisted classes are bounded only by --max-concurrent.")
	var aging agingFlag
	flag.Var(&aging, "priority-aging", "per-class priority aging offset 'CLASS=DURATION' (repeatable, or comma-separated; classes pr-ci, dev). A queued reservation is ordered as though it arrived its class's offset later, so a higher class overtakes it only by arriving within that window — the bound on how long CI can keep a dev reservation waiting. Default pr-ci=20m,dev=60m. Active reservations are never preempted.")
	var capsAliases classCapAliasFlag
	flag.Var(&capsAliases, "caps-class-alias", "unit-type alias for the capacity metrics' caps_class label 'TYPE=NAME' (repeatable/comma-separated; defaults esp32c6=c6,esp32c3=c3,led-mapper-pi=pi). A unit with a logic-analyzer tap gets NAME-la.")
	defaultHold := flag.Duration("eta-default-hold", engine.DefaultHold, "hold-time estimate for a unit type with no completed reservation yet (the ETA model's seed)")
	wifiIface := flag.String("wifi-iface", "", "the host's provisioning-AP interface (e.g. wlan0); enables GET /reservation/{id}/wifi, the AP's per-station stats (`iw dev IFACE station dump`) for the stations a reservation owns. Empty = endpoint disabled.")
	wifiLeases := flag.String("wifi-leases", "", "dnsmasq leases file for --wifi-iface (maps a DUT's IP to its current station MAC); default /var/lib/NetworkManager/dnsmasq-IFACE.leases")
	iwBin := flag.String("iw", "iw", "iw binary (for --wifi-iface)")
	provSSID := flag.String("provisioning-ssid", "", "advertise this onboarding-network SSID in /status (overrides the catalog); e.g. a per-host provisioning AP")
	provPSK := flag.String("provisioning-psk", "", "onboarding-network passphrase advertised alongside --provisioning-ssid")
	identityMode := flag.String("identity", "", "how to attribute reservations to a person: \"tailscale\" verifies the caller's email via `tailscale whois` (the bench is reachable only over the tailnet, so this is free and unspoofable); \"\" (default) trusts the client-supplied owner_email as-is. The actor (human/agent) is always client-asserted.")
	var mounts stringList
	flag.Var(&mounts, "mount", "extra bind mount for every reservation environment, 'host[:container][:opts]' (repeatable). A host path that doesn't exist at start is skipped, so an optional host resource (e.g. a dbus socket) never breaks a rig that lacks it.")
	flag.Parse()

	if *catalogPath == "" {
		log.Fatalf("--catalog is required")
	}
	cat, err := catalog.Load(*catalogPath)
	if err != nil {
		log.Fatalf("catalog: %v", err)
	}

	// Broker factories: map a shared-resource kind to a constructor. Add more kinds
	// here (a spectrum analyzer, a power supply, …) to make them declarable.
	factories := map[string]catalog.BrokerFactory{
		"logic-analyzer": analyzerFactory(filepath.Join(*stateDir, "analyzer-channel-map.json")),
	}
	res, err := cat.Resolve(factories)
	if err != nil {
		log.Fatalf("catalog resolve: %v", err)
	}

	host := *hostFlag
	if host == "" {
		host = res.Host
	}
	if host == "" {
		host = hostname
	}
	ws := res.Workspace
	if *workspace != "" {
		ws = *workspace
	}
	lease := time.Duration(res.LeaseSeconds) * time.Second
	if *leaseFlag != 0 {
		lease = *leaseFlag
	}

	// Provisioning network: catalog value, overridden by flags (a per-host SSID —
	// e.g. one derived from the hostname — can't live in a fleet-shared catalog).
	provision := res.Provisioning
	if *provSSID != "" {
		provision = &api.ProvisioningNetwork{SSID: *provSSID, PSK: *provPSK}
	}

	// Pick the execution backend. The podman backend runs a Linux container per
	// reservation; the darwin backend has no container (a Mac is a multi-tenant
	// host) and instead scopes an SSH grant into a shared user. "auto" resolves by
	// GOOS so the same catalog/flags work on either host.
	kind := *runnerKind
	if kind == "auto" {
		if runtime.GOOS == "darwin" {
			kind = "darwin"
		} else {
			kind = "podman"
		}
	}
	var run runner.Runner
	switch kind {
	case "darwin":
		run = runner.NewDarwin(runner.DarwinConfig{
			Host:     host,
			SSHUser:  *sshUser,
			SSHPort:  *darwinSSHPort,
			StateDir: *stateDir,
			ExtraEnv: map[string]string{"HITL_BROKER_URL": *brokerURL},
		})
		log.Printf("runner: darwin (shared user %q, sshd :%d)", *sshUser, *darwinSSHPort)
	case "podman":
		run = runner.NewPodman(runner.PodmanConfig{
			Image:      *image,
			Host:       host,
			SSHUser:    *sshUser,
			StateDir:   *stateDir,
			Podman:     *podman,
			Privileged: *privileged,
			NetHost:    *netHost,
			RawUSB:     *rawUSB,
			ExtraEnv:   map[string]string{"HITL_BROKER_URL": *brokerURL},
			Mounts:     mounts,
		})
	default:
		log.Fatalf("unknown --runner %q (want podman|darwin|auto)", *runnerKind)
	}

	if *maxConcurrent > 0 || len(classCaps.m) > 0 {
		log.Printf("concurrency: host cap=%d per-class=%v", *maxConcurrent, classCaps.m)
	}
	for _, u := range res.Units {
		log.Printf("unit: name=%s type=%s kind=%s ssh-port=%d caps=%v", u.Name, u.Type, u.Kind, u.SSHPort, u.Capabilities)
	}
	for _, b := range res.Registry.List() {
		log.Printf("shared: name=%s kind=%s present=%v", b.Name(), b.Kind(), b.Present())
	}

	mgr := engine.New(host, lease, run,
		engine.WithUnits(res.Units),
		engine.WithWorkspace(ws),
		engine.WithSharedResources(res.Registry.Describe()),
		engine.WithProvisioningNetwork(provision),
		engine.WithMaxConcurrent(*maxConcurrent),
		engine.WithMaxConcurrentPerClass(classCaps.m),
		engine.WithStateDir(*stateDir),
		engine.WithPriorityAging(aging.m),
		engine.WithDefaultHold(*defaultHold),
		engine.WithCapsClassAliases(capsAliases.m),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := mgr.Recover(ctx); err != nil {
		log.Printf("startup cleanup: %v", err)
	}

	// Live USB discovery (optional): layer hot-plugged boards on top of the catalog.
	if dcfg, err := discovery.ParseConfig(res.Discovery); err != nil {
		log.Fatalf("discovery: %v", err)
	} else if dcfg != nil {
		mon := discovery.NewMonitor(*dcfg, typeCapsFn(cat), enrichFn(res.Registry))
		go mon.Run(ctx, mgr)
		log.Printf("discovery: enabled (glob=%s type=%s)", dcfg.Glob, dcfg.Type)
	}

	// Reap expired leases periodically.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mgr.ReapExpired(ctx)
			}
		}
	}()

	idResolver := identity.New(identity.Mode(*identityMode))
	log.Printf("identity attribution: mode=%q", *identityMode)
	var stations *wifi.Stations
	if *wifiIface != "" {
		leases := *wifiLeases
		if leases == "" {
			leases = "/var/lib/NetworkManager/dnsmasq-" + *wifiIface + ".leases"
		}
		stations = wifi.New(*wifiIface, leases, *iwBin)
		log.Printf("wifi: station stats enabled (iface=%s leases=%s)", *wifiIface, leases)
	}
	srv := &http.Server{Addr: *addr, Handler: routes(ctx, mgr, res.Registry, ws, idResolver, stations)}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()

	log.Printf("hitl-reserved: host=%q workspace=%q listening on %s (image=%s lease=%s, %d units)",
		host, ws, *addr, *image, lease, len(res.Units))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}

// analyzerFactory returns a BrokerFactory for the "logic-analyzer" kind. mapPath is
// where runtime channel-map edits persist.
func analyzerFactory(mapPath string) catalog.BrokerFactory {
	return func(name string, config json.RawMessage) (shared.Broker, error) {
		var cfg struct {
			Driver     string                      `json:"driver"`
			SampleRate string                      `json:"samplerate"`
			Samples    int                         `json:"samples"`
			TimingRate string                      `json:"timing_samplerate"`
			TimingMax  int                         `json:"timing_max_ms"`
			SigrokCLI  string                      `json:"sigrok_cli"`
			Map        map[string]analyzer.UnitMap `json:"map"`
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &cfg); err != nil {
				return nil, err
			}
		}
		return analyzer.New(analyzer.Config{
			Name:             name,
			Driver:           cfg.Driver,
			SampleRate:       cfg.SampleRate,
			Samples:          cfg.Samples,
			TimingSampleRate: cfg.TimingRate,
			TimingMaxMs:      cfg.TimingMax,
			SigrokCLI:        cfg.SigrokCLI,
			Map:              cfg.Map,
			MapPath:          mapPath,
			HardwareProbe:    analyzer.FX2Present,
		}), nil
	}
}

// typeCapsFn returns a resource-type capability lookup backed by the catalog, for
// discovery to attach a discovered board's baseline caps.
func typeCapsFn(cat *catalog.Catalog) func(string) []string {
	return func(t string) []string {
		if rt, ok := cat.ResourceTypes[t]; ok {
			return append([]string(nil), rt.Capabilities...)
		}
		return nil
	}
}

// tapCapabler is a shared broker that can report the capabilities a unit gains from
// being tapped by it (rig wiring, resolved per-unit) — e.g. the logic analyzer.
type tapCapabler interface {
	TapCaps(unit string) []string
}

// enrichFn returns a discovery enrich hook that unions each shared resource's
// per-unit tap capabilities into a discovered unit. This is how a discovered board
// picks up its "logic-analyzer-*" capability from the channel map without a catalog
// entry (rig wiring, resolved live rather than declared).
func enrichFn(reg *shared.Registry) func(*runner.Unit) {
	return func(u *runner.Unit) {
		set := map[string]bool{}
		for _, c := range u.Capabilities {
			set[c] = true
		}
		for _, b := range reg.List() {
			if tc, ok := b.(tapCapabler); ok {
				for _, cap := range tc.TapCaps(u.Name) {
					set[cap] = true
				}
			}
		}
		u.Capabilities = u.Capabilities[:0]
		for c := range set {
			u.Capabilities = append(u.Capabilities, c)
		}
		sortStrings(u.Capabilities)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func routes(ctx context.Context, mgr *engine.Manager, reg *shared.Registry, workspace string, idResolver *identity.Resolver, stations *wifi.Stations) http.Handler {
	mux := http.NewServeMux()
	wifiRoutes(mux, mgr, stations)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.Status())
	})

	// Cheap, reservation-free queue overview (depth per class, ETA per unit type,
	// a dev back-off hint) so clients can decide whether to enqueue at all.
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.QueueSummary())
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, mgr.Metrics(), metrics.ReadHost())
	})

	mux.HandleFunc("POST /reserve", func(w http.ResponseWriter, r *http.Request) {
		var req api.ReserveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if strings.TrimSpace(req.SSHPublicKey) == "" {
			writeErr(w, http.StatusBadRequest, "ssh_public_key is required")
			return
		}
		if req.Unit != "" && !mgr.HasUnit(req.Unit) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown unit %q; host has %v", req.Unit, mgr.Units()))
			return
		}
		// Attribute the reservation to a person: verify the caller's identity from the
		// transport when possible, else trust the asserted owner_email. Sets
		// OwnerEmail/Actor/IdentitySource on req before it reaches the engine.
		idResolver.Resolve(r.RemoteAddr, &req)
		writeJSON(w, http.StatusAccepted, mgr.Reserve(ctx, req))
	})

	mux.HandleFunc("GET /reservation/{id}", func(w http.ResponseWriter, r *http.Request) {
		res, err := mgr.Get(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("POST /reservation/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if err := mgr.Heartbeat(r.PathValue("id")); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		res, _ := mgr.Get(r.PathValue("id"))
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("POST /reservation/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		// ?outcome=cancelled marks a hold that ended because its job was cancelled
		// (accounted separately from a normal release in the capacity metrics).
		reason := "released by holder"
		if r.URL.Query().Get("outcome") == engine.OutcomeCancelled {
			reason = engine.ReasonCancelled
		}
		if err := mgr.Release(ctx, r.PathValue("id"), reason); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Per-reservation live status page (html/template, 5s meta-refresh) with a form
	// to append a scratchpad note.
	mux.HandleFunc("GET /reservation/{id}/status.html", func(w http.ResponseWriter, r *http.Request) {
		res, err := mgr.Get(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := renderReservation(w, res); err != nil {
			log.Printf("render reservation %s: %v", res.ID, err)
		}
	})

	// Append a scratchpad note. Accepts JSON ({"author":…,"text":…}) or an HTML form
	// (author/text): a form post 303-redirects back to the status page, an API post
	// returns 200 + the updated reservation JSON.
	mux.HandleFunc("POST /reservation/{id}/scratchpad", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var author, text string
		form := isForm(r)
		if form {
			if err := r.ParseForm(); err != nil {
				writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
				return
			}
			author, text = r.FormValue("author"), r.FormValue("text")
		} else {
			var body struct {
				Author string `json:"author"`
				Text   string `json:"text"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
				return
			}
			author, text = body.Author, body.Text
		}
		if strings.TrimSpace(text) == "" {
			writeErr(w, http.StatusBadRequest, "text is required")
			return
		}
		res, err := mgr.AppendNote(id, author, text)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		if form {
			http.Redirect(w, r, "/reservation/"+id+"/status.html", http.StatusSeeOther)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	// Set one free-form annotation ({"key":…,"value":…}) — for a deployment to attach
	// its own metadata (e.g. a URL to an external view).
	mux.HandleFunc("POST /reservation/{id}/annotation", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var key, value string
		if isForm(r) {
			if err := r.ParseForm(); err != nil {
				writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
				return
			}
			key, value = r.FormValue("key"), r.FormValue("value")
		} else {
			var body struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
				return
			}
			key, value = body.Key, body.Value
		}
		if strings.TrimSpace(key) == "" {
			writeErr(w, http.StatusBadRequest, "key is required")
			return
		}
		res, err := mgr.SetAnnotation(id, key, value)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	// Host overview page: every unit (free/busy) linking to each active
	// reservation's status page.
	mux.HandleFunc("GET /status.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := renderOverview(w, mgr.Status()); err != nil {
			log.Printf("render overview: %v", err)
		}
	})

	// Maintenance (cordon + drain): take the host out of service for maintenance
	// (e.g. a daemon redeploy). Entering cordons the host so no queued reservation
	// activates; active ones drain naturally. The cordon persists across a restart
	// (marker under the state dir) until explicitly released.
	mux.HandleFunc("GET /maintenance", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.Maintenance())
	})

	mux.HandleFunc("POST /maintenance", func(w http.ResponseWriter, r *http.Request) {
		wait := r.URL.Query().Has("wait")
		// Also accept {"wait": true} in the body (optional).
		if !wait {
			var body struct {
				Wait bool `json:"wait"`
			}
			if r.Body != nil {
				if b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16)); len(b) > 0 {
					_ = json.Unmarshal(b, &body)
				}
			}
			wait = body.Wait
		}
		st := mgr.Cordon()
		if wait {
			st = mgr.WaitDrained(r.Context()) // block until active == 0 (or client goes away)
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("POST /maintenance/release", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.Uncordon(ctx))
	})

	// Shared resources: list, and broker access. Access is a generic passthrough —
	// the broker owns the request/response schema (e.g. the analyzer's capture).
	mux.HandleFunc("GET /shared", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, reg.Describe())
	})

	mux.HandleFunc("POST /shared/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, ok := reg.Get(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("unknown shared resource %q", r.PathValue("name")))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		// The unit is named via ?unit= (a holder scoping to its own unit).
		resp, status, err := b.Access(r.Context(), r.URL.Query().Get("unit"), body)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(resp)
	})

	return logging(mux)
}

// writeMetrics renders the daemon's Prometheus exposition. Every series carries
// `host` and `workspace` labels so several projects' hosts can remote_write into
// one Grafana tenant and be grouped per repo (workspace) or per rig (host) in a
// shared dashboard, with no per-rig dashboard edits.
func writeMetrics(w io.Writer, snap engine.MetricsSnapshot, host metrics.HostStats) {
	mw := metrics.NewWriter(w)
	base := []metrics.Label{
		{Name: "host", Value: snap.Host},
		{Name: "workspace", Value: snap.Workspace},
	}
	l := func(extra ...metrics.Label) []metrics.Label {
		return append(append([]metrics.Label(nil), base...), extra...)
	}

	mw.Gauge("hitl_up", "1 if the reservation daemon is serving.", 1, l()...)
	mw.Gauge("hitl_units_total", "Configured reservable units on this host.", float64(snap.UnitsTotal), l()...)
	mw.Gauge("hitl_units_busy", "Units with an active reservation.", float64(snap.UnitsBusy), l()...)
	mw.Gauge("hitl_queue_depth", "Reservations queued waiting for a unit.", float64(snap.QueueDepth), l()...)
	mw.Gauge("hitl_active_reservations", "Reservations currently active (one per busy unit).", float64(snap.ActiveTotal), l()...)
	mw.Gauge("hitl_lease_seconds", "Heartbeat lease window.", snap.LeaseSeconds, l()...)

	for _, u := range snap.Units {
		v := 0.0
		if u.Busy {
			v = 1
		}
		mw.Gauge("hitl_unit_busy", "1 if this unit has an active reservation.", v,
			l(metrics.Label{Name: "unit", Value: u.Name}, metrics.Label{Name: "unit_type", Value: u.Type})...)
	}

	// One series per active reservation, carrying the id/owner/unit a dashboard
	// needs to table the reservation and deep-link to its
	// /reservation/{id}/status.html page. The value is always 1 (a presence
	// gauge); the reservation's age is a companion gauge keyed by the same id.
	for _, r := range snap.Active {
		actor := r.Actor
		if actor == "" {
			actor = "human"
		}
		labels := l(
			metrics.Label{Name: "id", Value: r.ID},
			metrics.Label{Name: "owner", Value: r.Owner},
			metrics.Label{Name: "owner_email", Value: r.OwnerEmail},
			metrics.Label{Name: "actor", Value: actor},
			metrics.Label{Name: "unit", Value: r.Unit},
			metrics.Label{Name: "unit_type", Value: r.UnitType},
		)
		mw.Gauge("hitl_reservation", "1 for each currently-active reservation (labelled by id/owner/unit).", 1, labels...)
		mw.Gauge("hitl_reservation_age_seconds", "Seconds since this reservation became active.", r.AgeSecs, labels...)
	}

	// Priority classes: queue depth / oldest wait per class, and the wait-time
	// histogram of reservations that activated (plus those that gave up queued).
	for _, c := range snap.Classes {
		cl := l(metrics.Label{Name: "class", Value: c.Class})
		mw.Gauge("hitl_queue_depth_class", "Reservations queued, per priority class.", float64(c.Queued), cl...)
		mw.Gauge("hitl_queue_oldest_wait_seconds", "Age of the oldest queued reservation, per priority class (0 when none).", c.OldestWaitSeconds, cl...)
	}

	// Capacity accounting (demand vs supply per caps class; see engine/capacity.go
	// and docs/metrics.md). All counters/histograms, so they sum across hosts.
	cs := snap.Capacity
	for _, u := range cs.Units {
		mw.Counter("hitl_unit_available_seconds_total", "Seconds the unit was present and reservable (host not cordoned), busy or idle.", u.AvailableSeconds,
			l(metrics.Label{Name: "unit", Value: u.Unit}, metrics.Label{Name: "unit_type", Value: u.Type}, metrics.Label{Name: "caps_class", Value: u.CapsClass})...)
	}
	for _, b := range cs.Busy {
		mw.Counter("hitl_unit_busy_seconds_total", "Seconds the unit was held by an active reservation, credited when the hold ends (outcome: released/expired/cancelled/start_failed).", b.Seconds,
			l(metrics.Label{Name: "unit", Value: b.Unit}, metrics.Label{Name: "unit_type", Value: b.Type}, metrics.Label{Name: "caps_class", Value: b.CapsClass},
				metrics.Label{Name: "prio_class", Value: b.Prio}, metrics.Label{Name: "outcome", Value: b.Outcome})...)
	}
	for _, r := range cs.Requests {
		mw.Counter("hitl_reservation_requests_total", "Reservations requested, by priority class and the request's caps class.", float64(r.Requests),
			l(metrics.Label{Name: "prio_class", Value: r.Prio}, metrics.Label{Name: "caps_class", Value: r.CapsClass})...)
	}
	for _, r := range cs.Requests {
		mw.Counter("hitl_reservation_abandoned_total", "Reservations that left the queue without activating (client gave up, lease expired or unit evicted while queued).", float64(r.Abandoned),
			l(metrics.Label{Name: "prio_class", Value: r.Prio}, metrics.Label{Name: "caps_class", Value: r.CapsClass})...)
	}
	for _, w := range cs.Waits {
		mw.Histogram("hitl_reservation_wait_seconds", "Request-to-activation time (result=activated), or request-to-abandon time for reservations that never activated (result=abandoned).", w.Hist,
			l(metrics.Label{Name: "prio_class", Value: w.Prio}, metrics.Label{Name: "caps_class", Value: w.CapsClass}, metrics.Label{Name: "result", Value: w.Result})...)
	}
	for _, h := range cs.Holds {
		mw.Histogram("hitl_reservation_hold_seconds", "Activation-to-release time of reservations, by priority class, request caps class and owner kind (ci/agent/human).", h.Hist,
			l(metrics.Label{Name: "prio_class", Value: h.Prio}, metrics.Label{Name: "caps_class", Value: h.CapsClass}, metrics.Label{Name: "owner_kind", Value: h.OwnerKind})...)
	}

	mw.Counter("hitl_reservations_total", "Reservations enqueued since start.", float64(snap.Reservations), l()...)
	mw.Counter("hitl_activations_total", "Reservations that became active since start.", float64(snap.Activations), l()...)
	mw.Counter("hitl_releases_total", "Reservations ended (any reason) since start.", float64(snap.Releases), l()...)
	mw.Counter("hitl_lease_expirations_total", "Reservations reaped for a lapsed lease since start.", float64(snap.LeaseExpiries), l()...)
	mw.Counter("hitl_start_failures_total", "Environment start failures during reconcile since start.", float64(snap.StartFailures), l()...)

	if host.Load1OK {
		mw.Gauge("hitl_host_load1", "Host 1-minute load average.", host.Load1, l()...)
	}
	if host.MemOK {
		mw.Gauge("hitl_host_memory_total_bytes", "Host total memory.", host.MemTotalBytes, l()...)
		mw.Gauge("hitl_host_memory_available_bytes", "Host available memory.", host.MemAvailableBytes, l()...)
	}
	if host.TempOK {
		mw.Gauge("hitl_host_temperature_celsius", "Host SoC temperature.", host.TempCelsius, l()...)
	}
}

func logging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, api.Error{Error: msg})
}

// isForm reports whether the request carries a url-encoded HTML form body (as
// opposed to a JSON API body), so a handler can 303-redirect a browser form post
// while returning JSON to an API client.
func isForm(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct) == "application/x-www-form-urlencoded"
}
