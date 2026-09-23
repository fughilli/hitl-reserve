// Package engine implements the reservation state machine for one host: a FIFO
// admission queue over one or more reservable units, each with a single active
// slot, heartbeat leases, capability/type best-fit placement, and lifecycle
// callbacks into a runner.Runner to bring each unit's environment up/down. With a
// single unit it degenerates to a plain single-slot FIFO.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// maintenanceMarker is the file, under the manager's state dir, whose presence
// cordons the host. It persists cordoned state across a daemon restart: on New the
// manager reads it and comes up cordoned, so a maintenance window survives the
// restart it exists to cover, until explicitly released.
const maintenanceMarker = "maintenance"

// ErrNotFound is returned for an unknown reservation id.
var ErrNotFound = errors.New("reservation not found")

// Hook receives host-level lifecycle callbacks: OnFirstActive fires when the host
// transitions from fully idle to having any active reservation; OnAllIdle fires
// when the last active reservation ends. Both must be idempotent and best-effort
// (an error is logged, never fails a reservation). Use it to toggle a shared
// facility around activity — e.g. bring a provisioning access point up while the
// host is in use and down when it goes idle. Nil disables it.
type Hook interface {
	OnFirstActive(context.Context) error
	OnAllIdle(context.Context) error
}

// Manager owns all reservation state. Methods are safe for concurrent use.
//
// The mutex is held across runner Start/Stop (a few seconds of container
// lifecycle). Reservations are infrequent, so this serialization is fine.
type Manager struct {
	host      string
	workspace string
	lease     time.Duration
	run       runner.Runner
	units     []runner.Unit
	shared    []api.SharedResourceInfo // advertised in Status (refreshable)
	provision *api.ProvisioningNetwork // advertised in Status (or nil)
	hook      Hook

	// maxConcurrent caps how many reservations may be Active at once on this host,
	// even when more units are free. 0 = unlimited (bounded only by unit count).
	maxConcurrent int

	// stateDir is where the maintenance marker persists (empty = no persistence, the
	// marker is kept in memory only — used by tests and marker-less deployments).
	stateDir string

	mu    sync.Mutex
	items []*api.Reservation  // admission order; several may be Active (one per unit)
	keys  map[string]string   // id -> SSH pubkey (not serialized out)
	want  map[string]string   // id -> pinned unit name ("" = any)
	typ   map[string]string   // id -> pinned unit type ("" = any)
	caps  map[string][]string // id -> required capabilities (nil = none)

	// cordoned, when set, stops the admission path from activating any queued
	// reservation: new reserves still queue and hold a position, active reservations
	// keep running and drain naturally, but nothing new promotes to active until the
	// host is uncordoned. It is the "maintenance mode" flag. Guarded by mu.
	cordoned bool
	// drained is closed (and re-made on the next cordon) whenever the active count
	// reaches zero while cordoned, so a waiter blocking on full drain wakes.
	drained chan struct{}

	// Monotonic lifecycle counters, exported via Metrics().
	cReservations  uint64
	cActivations   uint64
	cReleases      uint64
	cLeaseExpiries uint64
	cStartFailures uint64
}

// Option configures a Manager.
type Option func(*Manager)

// WithUnits sets the units this host hands out. Order is the tie-break when
// several are free. A host may legitimately start with zero units and gain them
// at runtime (via discovery / seeding through SyncUnits).
func WithUnits(units []runner.Unit) Option {
	return func(m *Manager) { m.units = append([]runner.Unit(nil), units...) }
}

// WithProvisioningNetwork advertises an onboarding network in Status (see
// api.ProvisioningNetwork), so a holder can provision a DUT onto it with no
// out-of-band credentials.
func WithProvisioningNetwork(p *api.ProvisioningNetwork) Option {
	return func(m *Manager) { m.provision = p }
}

// WithWorkspace tags the host with a logical grouping (repo/fleet name), surfaced
// in Status and metrics so several projects' hosts can share one dashboard.
func WithWorkspace(ws string) Option { return func(m *Manager) { m.workspace = ws } }

// WithSharedResources advertises the host's shared resources in Status.
func WithSharedResources(s []api.SharedResourceInfo) Option {
	return func(m *Manager) { m.shared = append([]api.SharedResourceInfo(nil), s...) }
}

// WithHook wires host-level activation callbacks (see Hook).
func WithHook(h Hook) Option { return func(m *Manager) { m.hook = h } }

// WithMaxConcurrent caps how many reservations may be Active at once on this host,
// even when more units are free — protecting a weak host or a shared resource (USB
// bus, radio, CPU) from N-wide load. 0 (default) means unlimited, bounded only by
// unit count. Surplus reservations wait in the admission queue and activate as
// active ones release, so throughput is preserved; only peak concurrency is bounded.
func WithMaxConcurrent(n int) Option {
	return func(m *Manager) { m.maxConcurrent = n }
}

// WithStateDir sets the directory the maintenance marker persists under (see
// Cordon). When set, New reads the marker on startup so the host comes back
// cordoned across a daemon restart. Empty (the default) keeps cordoned state in
// memory only — it does not survive a restart.
func WithStateDir(dir string) Option {
	return func(m *Manager) { m.stateDir = dir }
}

// New creates a Manager. lease is the heartbeat window: a reservation whose holder
// stops heartbeating for longer than lease is reaped (active holders and queued
// waiters alike).
func New(host string, lease time.Duration, run runner.Runner, opts ...Option) *Manager {
	m := &Manager{
		host: host, lease: lease, run: run,
		keys: map[string]string{}, want: map[string]string{},
		typ: map[string]string{}, caps: map[string][]string{},
		drained: make(chan struct{}),
	}
	for _, o := range opts {
		o(m)
	}
	// Persisted cordon: if the marker survives from a previous run, start cordoned so
	// a maintenance window holds across the restart it was taken for.
	if m.markerPath() != "" {
		if _, err := os.Stat(m.markerPath()); err == nil {
			m.cordoned = true
			log.Printf("maintenance: found marker %s; starting cordoned", m.markerPath())
		}
	}
	return m
}

// markerPath is the maintenance marker's absolute path, or "" when no state dir is
// configured (in-memory cordon only).
func (m *Manager) markerPath() string {
	if m.stateDir == "" {
		return ""
	}
	return filepath.Join(m.stateDir, maintenanceMarker)
}

// SetSharedResources refreshes the advertised shared resources (e.g. after a
// runtime channel-map change).
func (m *Manager) SetSharedResources(s []api.SharedResourceInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shared = append([]api.SharedResourceInfo(nil), s...)
}

// Units returns the configured unit names (for request validation / display).
func (m *Manager) Units() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.units))
	for i, u := range m.units {
		out[i] = u.Name
	}
	return out
}

// HasUnit reports whether name is one of the host's units.
func (m *Manager) HasUnit(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.units {
		if u.Name == name {
			return true
		}
	}
	return false
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Recover clears any stale environments from a previous run and starts fresh.
func (m *Manager) Recover(ctx context.Context) error {
	return m.run.Cleanup(ctx)
}

// Reserve enqueues a new reservation and reconciles (may activate it immediately).
func (m *Manager) Reserve(ctx context.Context, req api.ReserveRequest) *api.Reservation {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	exp := now.Add(m.lease)
	r := &api.Reservation{
		ID:        newID(),
		Owner:     req.Owner,
		State:     api.StateQueued,
		CreatedAt: now,
		ExpiresAt: &exp, // queued waiters carry a lease too, so a dead client is reaped
	}
	m.keys[r.ID] = req.SSHPublicKey
	if req.Unit != "" {
		m.want[r.ID] = req.Unit
	}
	if req.UnitType != "" {
		m.typ[r.ID] = req.UnitType
	}
	if len(req.RequireCaps) > 0 {
		m.caps[r.ID] = append([]string(nil), req.RequireCaps...)
	}
	m.items = append(m.items, r)
	m.cReservations++
	log.Printf("reserve: id=%s owner=%q unit=%q type=%q caps=%v queued (position %d)",
		r.ID, r.Owner, req.Unit, req.UnitType, req.RequireCaps, len(m.items)-1)
	m.reconcileLocked(ctx)
	if v := m.viewLocked(r.ID); v != nil {
		return v
	}
	// reconcile dropped it (an immediate start failed). Surface that as a released
	// snapshot rather than nil, so the caller gets a diagnosable response.
	return &api.Reservation{ID: r.ID, Owner: r.Owner, State: api.StateReleased, CreatedAt: now,
		Message: "failed to start environment"}
}

// Get returns a snapshot of one reservation.
func (m *Manager) Get(id string) (*api.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.viewLocked(id); v != nil {
		return v, nil
	}
	return nil, ErrNotFound
}

// Heartbeat extends a reservation's lease (queued or active).
func (m *Manager) Heartbeat(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.findLocked(id)
	if r == nil {
		return ErrNotFound
	}
	if r.State == api.StateActive || r.State == api.StateQueued {
		exp := time.Now().Add(m.lease)
		r.ExpiresAt = &exp
	}
	return nil
}

// Release ends a reservation. If active, its environment is torn down and the next
// compatible waiter is promoted.
func (m *Manager) Release(ctx context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := m.indexLocked(id)
	if idx < 0 {
		return ErrNotFound
	}
	wasActive := m.items[idx].State == api.StateActive
	m.cReleases++
	log.Printf("release: id=%s reason=%q wasActive=%v", id, reason, wasActive)
	if wasActive {
		if err := m.run.Stop(ctx, id); err != nil {
			log.Printf("release: stop environment for %s: %v", id, err)
		}
	}
	m.removeLocked(id)
	m.reconcileLocked(ctx)
	if wasActive && !m.anyActiveLocked() {
		m.hookAllIdle(ctx)
		m.signalDrainedLocked()
	}
	return nil
}

// signalDrainedLocked wakes anyone blocked on full drain (Cordon(wait)) by closing
// the drained channel, when the host is cordoned and no reservation is active.
// Called on the active->idle edge. Idempotent: a re-cordon installs a fresh channel.
func (m *Manager) signalDrainedLocked() {
	if !m.cordoned {
		return
	}
	select {
	case <-m.drained: // already closed
	default:
		close(m.drained)
	}
}

// ReapExpired releases every reservation whose lease has lapsed — active holders
// (tearing their environment down) and queued waiters (dequeuing them) alike. Call
// periodically. Sweeping the whole queue keeps a dead waiter from stranding a slot.
func (m *Manager) ReapExpired(ctx context.Context) {
	m.mu.Lock()
	now := time.Now()
	var expired []string
	for _, r := range m.items {
		if r.ExpiresAt != nil && now.After(*r.ExpiresAt) {
			expired = append(expired, r.ID)
		}
	}
	m.cLeaseExpiries += uint64(len(expired))
	m.mu.Unlock()
	for _, id := range expired {
		_ = m.Release(ctx, id, "lease expired (no heartbeat)")
	}
}

// SyncUnits reconciles the live unit set to units — the hook discovery uses for
// hot-plug. It adds newly-attached units, drops detached ones (tearing down any
// reservation on a removed unit that is idle), preserves in-place units (and their
// live environments), adopts a changed spec on an idle unit, and reconciles so a
// waiter lands on a freshly-attached unit. A busy unit is retained even if it
// momentarily drops out of discovery (a resetting board), pruned once idle.
// Returns the added/removed names for logging.
func (m *Manager) SyncUnits(ctx context.Context, units []runner.Unit) (added, removed []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	byName := map[string]runner.Unit{}
	for _, u := range units {
		byName[u.Name] = u
	}
	cur := map[string]bool{}
	for _, u := range m.units {
		cur[u.Name] = true
	}

	changed := false
	var kept []runner.Unit
	for _, u := range m.units {
		if nu, ok := byName[u.Name]; ok {
			if !sameSpec(u, nu) && !m.unitBusyLocked(u.Name) {
				kept = append(kept, nu)
				changed = true
			} else {
				kept = append(kept, u)
			}
			continue
		}
		if m.unitBusyLocked(u.Name) {
			kept = append(kept, u) // never tear down a live session over a discovery blip
			continue
		}
		removed = append(removed, u.Name)
		m.evictUnitLocked(u.Name)
	}
	for _, u := range units {
		if !cur[u.Name] {
			kept = append(kept, u)
			added = append(added, u.Name)
		}
	}
	m.units = kept
	if len(added) > 0 || len(removed) > 0 || changed {
		m.reconcileLocked(ctx)
	}
	return added, removed
}

// Status returns the host overview: every unit with its holder, plus a summary.
func (m *Manager) Status() api.Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := api.Status{
		Host:         m.host,
		Workspace:    m.workspace,
		LeaseSeconds: int(m.lease.Seconds()),
		Shared:       m.shared,
		Provisioning: m.provision,
		Cordoned:     m.cordoned,
		Draining:     m.cordoned && m.anyActiveLocked(),
	}
	holders := map[string]*api.Reservation{}
	for _, r := range m.items {
		if r.State == api.StateActive {
			holders[r.Unit] = m.viewLocked(r.ID)
		}
	}
	for _, u := range m.units {
		us := api.UnitStatus{
			Name:         u.Name,
			Type:         u.Type,
			Kind:         u.Kind,
			PinOnly:      u.PinOnly,
			Capabilities: u.Capabilities,
			Active:       holders[u.Name],
		}
		for _, c := range u.Components {
			us.Components = append(us.Components, api.ComponentStatus{
				Name: c.Name, Type: c.Type, Kind: c.Kind, Capabilities: c.Capabilities,
			})
		}
		if us.Active == nil {
			s.FreeUnits++
		}
		s.Units = append(s.Units, us)
	}
	for _, r := range m.items {
		if r.State == api.StateQueued {
			s.QueueLength++
		}
	}
	return s
}

// DeviceMetric is one unit's slice of a MetricsSnapshot.
type DeviceMetric struct {
	Name string
	Type string
	Busy bool
}

// MetricsSnapshot is a point-in-time view of the manager for the /metrics endpoint.
type MetricsSnapshot struct {
	Host         string
	Workspace    string
	LeaseSeconds float64

	UnitsTotal  int
	UnitsBusy   int
	QueueDepth  int
	ActiveTotal int

	Units []DeviceMetric

	Reservations  uint64
	Activations   uint64
	Releases      uint64
	LeaseExpiries uint64
	StartFailures uint64
}

// Metrics returns a MetricsSnapshot for /metrics.
func (m *Manager) Metrics() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := map[string]bool{}
	active, queued := 0, 0
	for _, r := range m.items {
		switch r.State {
		case api.StateActive:
			busy[r.Unit] = true
			active++
		case api.StateQueued:
			queued++
		}
	}
	snap := MetricsSnapshot{
		Host:          m.host,
		Workspace:     m.workspace,
		LeaseSeconds:  m.lease.Seconds(),
		UnitsTotal:    len(m.units),
		QueueDepth:    queued,
		ActiveTotal:   active,
		Reservations:  m.cReservations,
		Activations:   m.cActivations,
		Releases:      m.cReleases,
		LeaseExpiries: m.cLeaseExpiries,
		StartFailures: m.cStartFailures,
	}
	for _, u := range m.units {
		b := busy[u.Name]
		if b {
			snap.UnitsBusy++
		}
		snap.Units = append(snap.Units, DeviceMetric{Name: u.Name, Type: u.Type, Busy: b})
	}
	return snap
}

// --- maintenance / drain --------------------------------------------------

// Cordon puts the host into maintenance mode: queued reservations stop activating
// (new reserves still queue and hold a position), while active reservations keep
// running and drain naturally via release or lease expiry. It writes the marker so
// the cordon survives a daemon restart (see New). Returns a snapshot of the drain
// state (see api.Maintenance). It is idempotent — cordoning an already-cordoned
// host just returns the current state.
func (m *Manager) Cordon() api.Maintenance {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cordoned {
		m.cordoned = true
		m.drained = make(chan struct{}) // fresh drain latch for this window
		if p := m.markerPath(); p != "" {
			if err := os.MkdirAll(m.stateDir, 0o755); err != nil {
				log.Printf("maintenance: mkdir state dir: %v", err)
			}
			if err := os.WriteFile(p, []byte("cordoned\n"), 0o644); err != nil {
				log.Printf("maintenance: write marker %s: %v", p, err)
			}
		}
		log.Printf("maintenance: cordoned (marker=%q)", m.markerPath())
	}
	// If already drained at cordon time, latch it so a waiter returns immediately.
	if !m.anyActiveLocked() {
		m.signalDrainedLocked()
	}
	return m.maintenanceLocked()
}

// Uncordon leaves maintenance mode: it removes the marker and lets queued
// reservations activate again, reconciling immediately so waiters that piled up
// during the window promote onto free units. Idempotent.
func (m *Manager) Uncordon(ctx context.Context) api.Maintenance {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cordoned {
		m.cordoned = false
		if p := m.markerPath(); p != "" {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				log.Printf("maintenance: remove marker %s: %v", p, err)
			}
		}
		log.Printf("maintenance: uncordoned; resuming activation")
		m.reconcileLocked(ctx)
	}
	return m.maintenanceLocked()
}

// Maintenance returns the current cordon/drain snapshot.
func (m *Manager) Maintenance() api.Maintenance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maintenanceLocked()
}

// WaitDrained blocks until the host is fully drained (cordoned with no active
// reservation) or ctx is done, then returns the final snapshot. It is meant to be
// called right after Cordon by an operator draining the rig for maintenance. If the
// host is not cordoned it returns at once (there is no drain to wait for).
func (m *Manager) WaitDrained(ctx context.Context) api.Maintenance {
	for {
		m.mu.Lock()
		if !m.cordoned || !m.anyActiveLocked() {
			s := m.maintenanceLocked()
			m.mu.Unlock()
			return s
		}
		ch := m.drained
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return m.Maintenance()
		case <-ch:
		}
	}
}

// maintenanceLocked builds the current cordon/drain snapshot.
func (m *Manager) maintenanceLocked() api.Maintenance {
	active, queued := 0, 0
	for _, r := range m.items {
		switch r.State {
		case api.StateActive:
			active++
		case api.StateQueued:
			queued++
		}
	}
	return api.Maintenance{
		Cordoned: m.cordoned,
		Active:   active,
		Queued:   queued,
		Drained:  m.cordoned && active == 0,
	}
}

// --- locked helpers -------------------------------------------------------

func (m *Manager) findLocked(id string) *api.Reservation {
	i := m.indexLocked(id)
	if i < 0 {
		return nil
	}
	return m.items[i]
}

func (m *Manager) indexLocked(id string) int {
	for i, r := range m.items {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func (m *Manager) viewLocked(id string) *api.Reservation {
	i := m.indexLocked(id)
	if i < 0 {
		return nil
	}
	cp := *m.items[i]
	cp.Position = i
	return &cp
}

func (m *Manager) unitBusyLocked(name string) bool {
	for _, r := range m.items {
		if r.State == api.StateActive && r.Unit == name {
			return true
		}
	}
	return false
}

func (m *Manager) anyActiveLocked() bool {
	for _, r := range m.items {
		if r.State == api.StateActive {
			return true
		}
	}
	return false
}

// evictUnitLocked drops what's tied to a removed idle unit: it dequeues any waiter
// pinned to it by name (an unpinned waiter stays — another unit can serve it).
func (m *Manager) evictUnitLocked(name string) {
	var doomed []string
	for _, r := range m.items {
		if r.State == api.StateQueued && m.want[r.ID] == name {
			doomed = append(doomed, r.ID)
		}
	}
	for _, id := range doomed {
		log.Printf("evict: reservation %s dropped (unit %s removed)", id, name)
		m.removeLocked(id)
	}
}

// reconcileLocked brings up an environment on every free unit, each fed the
// earliest compatible waiter. Loops until no free unit has a waiter, so a batch of
// reservations fills all units and a failed start doesn't strand the rest.
func (m *Manager) reconcileLocked(ctx context.Context) {
	for {
		wasIdle := !m.anyActiveLocked()
		unit, head := m.nextAssignmentLocked()
		if unit == nil {
			return
		}
		ep, err := m.run.Start(ctx, head.ID, head.Owner, m.keys[head.ID], *unit)
		if err != nil {
			log.Printf("reconcile: start environment for %s on %s failed: %v", head.ID, unit.Name, err)
			m.cStartFailures++
			head.Message = "failed to start environment: " + err.Error()
			m.removeLocked(head.ID)
			continue
		}
		now := time.Now()
		exp := now.Add(m.lease)
		head.State = api.StateActive
		head.StartedAt = &now
		head.ExpiresAt = &exp
		head.Endpoint = ep
		head.Unit = unit.Name
		head.UnitType = unit.Type
		m.cActivations++
		log.Printf("reconcile: id=%s active unit=%s endpoint=%s:%d", head.ID, unit.Name, ep.Host, ep.Port)
		if wasIdle {
			m.hookFirstActive(ctx)
		}
	}
}

// nextAssignmentLocked pairs the earliest queued waiter that can run with the
// best-fitting free unit for it, or (nil, nil). It considers every free unit so a
// waiter needing a later unit's capabilities still activates while an earlier free
// unit has no compatible work.
func (m *Manager) nextAssignmentLocked() (*runner.Unit, *api.Reservation) {
	// Cordoned for maintenance: never promote a queued reservation to active. Waiters
	// keep their positions and activate once uncordoned; active ones drain naturally.
	if m.cordoned {
		return nil, nil
	}
	busy := map[string]bool{}
	for _, r := range m.items {
		if r.State == api.StateActive {
			busy[r.Unit] = true
		}
	}
	// Per-host concurrency cap: don't activate beyond maxConcurrent environments at
	// once, even when units are free (len(busy) == the active-reservation count —
	// one active reservation per unit). Surplus reservations stay queued and start
	// as active ones release. 0 = unlimited.
	if m.maxConcurrent > 0 && len(busy) >= m.maxConcurrent {
		return nil, nil
	}
	for _, r := range m.items {
		if r.State != api.StateQueued {
			continue
		}
		best := -1
		bestExtra := 0
		for i := range m.units {
			if busy[m.units[i].Name] || !m.unitServesLocked(&m.units[i], r) {
				continue
			}
			// Best-fit: the free unit with the fewest capabilities beyond what this
			// reservation requires, so scarce over-provisioned units stay free for the
			// work that needs them.
			if extra := extraCaps(m.units[i].Capabilities, m.caps[r.ID]); best < 0 || extra < bestExtra {
				best, bestExtra = i, extra
			}
		}
		if best >= 0 {
			return &m.units[best], r
		}
	}
	return nil, nil
}

// unitServesLocked reports whether unit may host queued reservation r: it must
// match r's type pin (if any) and satisfy r's capability requirement, and name
// pinning is honored. A pin-only unit is reached only by an explicit target (a
// name or a type pin) — never by a bare "any unit" or a capability-only request.
func (m *Manager) unitServesLocked(unit *runner.Unit, r *api.Reservation) bool {
	if t := m.typ[r.ID]; t != "" && unit.Type != t {
		return false
	}
	if need := m.caps[r.ID]; len(need) > 0 && !hasCaps(unit.Capabilities, need) {
		return false
	}
	switch want := m.want[r.ID]; {
	case want == unit.Name:
		return true // pinned to this unit by name
	case want != "":
		return false // pinned to a different unit
	case m.typ[r.ID] != "":
		return true // pinned by type — an explicit target; opts in past pin-only
	default:
		return !unit.PinOnly // caps-only or unpinned: never a pin-only unit
	}
}

func (m *Manager) removeLocked(id string) {
	i := m.indexLocked(id)
	if i < 0 {
		return
	}
	m.items = append(m.items[:i], m.items[i+1:]...)
	delete(m.keys, id)
	delete(m.want, id)
	delete(m.typ, id)
	delete(m.caps, id)
}

func (m *Manager) hookFirstActive(ctx context.Context) {
	if m.hook == nil {
		return
	}
	if err := m.hook.OnFirstActive(ctx); err != nil {
		log.Printf("hook: OnFirstActive: %v", err)
	}
}

func (m *Manager) hookAllIdle(ctx context.Context) {
	if m.hook == nil {
		return
	}
	if err := m.hook.OnAllIdle(ctx); err != nil {
		log.Printf("hook: OnAllIdle: %v", err)
	}
}

// sameSpec reports whether two units carry the same runtime spec — the fields a
// re-seed can change that flow into the environment. Used to detect a re-seeded
// unit whose spec changed so an idle unit adopts it in place.
func sameSpec(a, b runner.Unit) bool {
	return a.Type == b.Type && a.Kind == b.Kind && a.PinOnly == b.PinOnly &&
		a.SSHPort == b.SSHPort &&
		reflect.DeepEqual(a.Capabilities, b.Capabilities) &&
		reflect.DeepEqual(a.Components, b.Components) &&
		reflect.DeepEqual(a.Env, b.Env)
}

// extraCaps counts the capabilities a unit has beyond those required.
func extraCaps(have, need []string) int {
	req := make(map[string]bool, len(need))
	for _, c := range need {
		req[c] = true
	}
	n := 0
	for _, c := range have {
		if !req[c] {
			n++
		}
	}
	return n
}

// hasCaps reports whether have ⊇ need.
func hasCaps(have, need []string) bool {
	set := make(map[string]bool, len(have))
	for _, c := range have {
		set[c] = true
	}
	for _, c := range need {
		if !set[c] {
			return false
		}
	}
	return true
}

// SortUnitsByName is a small helper for callers assembling unit lists.
func SortUnitsByName(u []runner.Unit) {
	sort.Slice(u, func(i, j int) bool { return u[i].Name < u[j].Name })
}
