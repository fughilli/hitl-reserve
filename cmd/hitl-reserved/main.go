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
	"strings"
	"syscall"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/catalog"
	"github.com/fughilli/hitl-reserve/discovery"
	"github.com/fughilli/hitl-reserve/engine"
	"github.com/fughilli/hitl-reserve/metrics"
	"github.com/fughilli/hitl-reserve/runner"
	"github.com/fughilli/hitl-reserve/shared"
	"github.com/fughilli/hitl-reserve/shared/analyzer"
)

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
	maxConcurrent := flag.Int("max-concurrent", 0, "cap concurrently-active reservations on this host regardless of unit count (0 = unlimited); protects a weak host or a shared bus/radio from N-wide load. Surplus reservations queue and start as active ones release.")
	provSSID := flag.String("provisioning-ssid", "", "advertise this onboarding-network SSID in /status (overrides the catalog); e.g. a per-host provisioning AP")
	provPSK := flag.String("provisioning-psk", "", "onboarding-network passphrase advertised alongside --provisioning-ssid")
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

	run := runner.NewPodman(runner.PodmanConfig{
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

	srv := &http.Server{Addr: *addr, Handler: routes(ctx, mgr, res.Registry, ws)}
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
			SigrokCLI  string                      `json:"sigrok_cli"`
			Map        map[string]analyzer.UnitMap `json:"map"`
		}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &cfg); err != nil {
				return nil, err
			}
		}
		return analyzer.New(analyzer.Config{
			Name:          name,
			Driver:        cfg.Driver,
			SampleRate:    cfg.SampleRate,
			Samples:       cfg.Samples,
			SigrokCLI:     cfg.SigrokCLI,
			Map:           cfg.Map,
			MapPath:       mapPath,
			HardwareProbe: analyzer.FX2Present,
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

func routes(ctx context.Context, mgr *engine.Manager, reg *shared.Registry, workspace string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.Status())
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
		if err := mgr.Release(ctx, r.PathValue("id"), "released by holder"); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
