package engine

// Capacity accounting: the counters behind a "demand vs capacity" (buy more
// hardware?) dashboard. Everything here is a monotonic counter or a histogram, so
// it aggregates across hosts and over any PromQL window:
//
//   - unit busy seconds (supply used), per unit + caps class + priority + outcome,
//     credited when the hold ENDS (the outcome is only known then);
//   - unit available seconds (supply offered): time a unit was present in the
//     catalog/discovery set while the host was NOT cordoned, accrued continuously;
//   - per request: arrivals, abandonments, request→activation wait (activated or
//     abandoned) and activation→release hold, keyed by the REQUEST's caps class.
//
// CAPS CLASS. A unit's caps class is its type alias ("esp32c6" → "c6") plus "-la"
// when it carries a logic-analyzer tap capability. A request's caps class is the
// same alias of the type it targets (its pinned unit's type, its unit_type, or the
// one type all its candidate units share) plus "-la" iff it REQUIRES a logic
// analyzer. So demand for "c6" can be served by "c6" and "c6-la" units; demand for
// "c6-la" only by "c6-la" units.

import (
	"sort"
	"strings"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/metrics"
	"github.com/fughilli/hitl-reserve/runner"
)

// Busy-time outcomes (hitl_unit_busy_seconds_total{outcome}).
const (
	OutcomeReleased    = "released"     // the holder (or an operator) released it
	OutcomeExpired     = "expired"      // reaped: the holder stopped heartbeating
	OutcomeCancelled   = "cancelled"    // released with ?outcome=cancelled (e.g. a CI run cancelled mid-test)
	OutcomeStartFailed = "start_failed" // the environment never came up
)

// ReasonCancelled is the Release reason that records OutcomeCancelled.
const ReasonCancelled = "cancelled by holder"

// Wait results (hitl_reservation_wait_seconds{result}).
const (
	ResultActivated = "activated"
	ResultAbandoned = "abandoned" // left the queue without activating (client gave up, expired, evicted)
)

// DefaultCapsClassAliases shortens unit types for the caps_class label.
var DefaultCapsClassAliases = map[string]string{
	"esp32c6":       "c6",
	"esp32c3":       "c3",
	"led-mapper-pi": "pi",
}

// HoldBuckets are the hold-time histogram bounds (seconds): 30 s .. 4 h.
var HoldBuckets = []float64{30, 60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200, 10800, 14400}

// WithCapsClassAliases overrides/extends the unit-type → caps_class alias map.
func WithCapsClassAliases(aliases map[string]string) Option {
	return func(m *Manager) {
		for k, v := range aliases {
			if k != "" && v != "" {
				m.capsAlias[k] = v
			}
		}
	}
}

type capKey struct{ prio, caps string }
type waitKey struct{ prio, caps, result string }
type holdKey struct{ prio, caps, owner string }
type busyKey struct{ unit, typ, caps, prio, outcome string }

type availState struct {
	typ, caps string
	accum     time.Duration
	since     *time.Time // open interval start while available
}

// capacity is the accounting state (guarded by Manager.mu).
type capacity struct {
	reqClass  map[string]string // reservation id -> request caps class
	requests  map[capKey]uint64
	abandoned map[capKey]uint64
	wait      map[waitKey]*metrics.Histogram
	hold      map[holdKey]*metrics.Histogram
	busy      map[busyKey]float64
	avail     map[string]*availState // unit name -> availability
}

func newCapacity() capacity {
	return capacity{
		reqClass: map[string]string{}, requests: map[capKey]uint64{}, abandoned: map[capKey]uint64{},
		wait: map[waitKey]*metrics.Histogram{}, hold: map[holdKey]*metrics.Histogram{},
		busy: map[busyKey]float64{}, avail: map[string]*availState{},
	}
}

func (m *Manager) typeAlias(t string) string {
	if a, ok := m.capsAlias[t]; ok {
		return a
	}
	if t == "" {
		return "unit"
	}
	return t
}

func hasAnalyzerCap(caps []string) bool {
	for _, c := range caps {
		if strings.HasPrefix(c, "logic-analyzer") {
			return true
		}
	}
	return false
}

// unitCapsClass is a unit's caps_class label.
func (m *Manager) unitCapsClass(u runner.Unit) string {
	c := m.typeAlias(u.Type)
	if hasAnalyzerCap(u.Capabilities) {
		c += "-la"
	}
	return c
}

// requestCapsClassLocked is a queued reservation's caps_class label (see the file
// comment). "none" when nothing on the host could serve it, "mixed" when its
// candidates span several types and it named none.
func (m *Manager) requestCapsClassLocked(r *api.Reservation) string {
	typ := m.typ[r.ID]
	if want := m.want[r.ID]; want != "" {
		for _, u := range m.units {
			if u.Name == want {
				typ = u.Type
			}
		}
	}
	if typ == "" {
		seen := map[string]bool{}
		for i := range m.candidatesLocked(r) {
			seen[m.units[i].Type] = true
		}
		switch len(seen) {
		case 0:
			return "none"
		case 1:
			for t := range seen {
				typ = t
			}
		default:
			return "mixed"
		}
	}
	c := m.typeAlias(typ)
	if hasAnalyzerCap(m.caps[r.ID]) {
		c += "-la"
	}
	return c
}

// ownerKind is the hold histogram's owner_kind: "ci" for CI priority classes, else
// the client-asserted actor ("agent" or "human").
func ownerKind(r *api.Reservation) string {
	if r.Priority == api.PriorityMergeQueue || r.Priority == api.PriorityPRCI {
		return "ci"
	}
	if r.Actor == "agent" {
		return "agent"
	}
	return "human"
}

// --- hooks (all called with m.mu held) -------------------------------------------

func (m *Manager) capOnReserveLocked(r *api.Reservation) {
	c := m.requestCapsClassLocked(r)
	m.capa.reqClass[r.ID] = c
	m.capa.requests[capKey{r.Priority, c}]++
}

func (m *Manager) observeWaitLocked(r *api.Reservation, result string, wait time.Duration) {
	k := waitKey{r.Priority, m.capa.reqClass[r.ID], result}
	h := m.capa.wait[k]
	if h == nil {
		h = metrics.NewHistogram(WaitBuckets)
		m.capa.wait[k] = h
	}
	h.Observe(wait.Seconds())
}

// capOnAbandonLocked: a queued reservation leaves without activating.
func (m *Manager) capOnAbandonLocked(r *api.Reservation) {
	m.capa.abandoned[capKey{r.Priority, m.capa.reqClass[r.ID]}]++
	m.observeWaitLocked(r, ResultAbandoned, m.now().Sub(r.CreatedAt))
}

// capOnHoldEndLocked: an active reservation ends with outcome.
func (m *Manager) capOnHoldEndLocked(r *api.Reservation, outcome string) {
	if r.StartedAt == nil {
		return
	}
	d := m.now().Sub(*r.StartedAt)
	if d < 0 {
		d = 0
	}
	m.addBusyLocked(r.Unit, r.Priority, outcome, d)
	k := holdKey{r.Priority, m.capa.reqClass[r.ID], ownerKind(r)}
	h := m.capa.hold[k]
	if h == nil {
		h = metrics.NewHistogram(HoldBuckets)
		m.capa.hold[k] = h
	}
	h.Observe(d.Seconds())
}

func (m *Manager) addBusyLocked(unit, prio, outcome string, d time.Duration) {
	typ, caps := "", m.typeAlias("")
	for _, u := range m.units {
		if u.Name == unit {
			typ, caps = u.Type, m.unitCapsClass(u)
		}
	}
	if a := m.capa.avail[unit]; a != nil && typ == "" {
		typ, caps = a.typ, a.caps
	}
	m.capa.busy[busyKey{unit, typ, caps, prio, outcome}] += d.Seconds()
}

// syncAvailLocked opens/closes each unit's availability interval: a unit accrues
// available time while it is present and the host is not cordoned.
func (m *Manager) syncAvailLocked() {
	now := m.now()
	present := map[string]bool{}
	for _, u := range m.units {
		present[u.Name] = true
		a := m.capa.avail[u.Name]
		if a == nil {
			a = &availState{}
			m.capa.avail[u.Name] = a
		}
		a.typ, a.caps = u.Type, m.unitCapsClass(u)
	}
	for name, a := range m.capa.avail {
		open := present[name] && !m.cordoned
		switch {
		case open && a.since == nil:
			t := now
			a.since = &t
		case !open && a.since != nil:
			a.accum += now.Sub(*a.since)
			a.since = nil
		}
	}
}

func sortBy[T any](xs []T, key func(T) string) {
	sort.SliceStable(xs, func(i, j int) bool { return key(xs[i]) < key(xs[j]) })
}

// --- snapshot ----------------------------------------------------------------------

// UnitCapacity is one unit's supply counters.
type UnitCapacity struct {
	Unit, Type, CapsClass string
	AvailableSeconds      float64
}

// BusySeconds is one hitl_unit_busy_seconds_total series.
type BusySeconds struct {
	Unit, Type, CapsClass, Prio, Outcome string
	Seconds                              float64
}

// RequestCounts is one (prio, caps) pair's request/abandon counters.
type RequestCounts struct {
	Prio, CapsClass     string
	Requests, Abandoned uint64
}

// WaitSeries / HoldSeries are one histogram series each.
type WaitSeries struct {
	Prio, CapsClass, Result string
	Hist                    metrics.Histogram
}

type HoldSeries struct {
	Prio, CapsClass, OwnerKind string
	Hist                       metrics.Histogram
}

// CapacitySnapshot is the capacity half of a MetricsSnapshot (sorted for stable
// output).
type CapacitySnapshot struct {
	Units    []UnitCapacity
	Busy     []BusySeconds
	Requests []RequestCounts
	Waits    []WaitSeries
	Holds    []HoldSeries
}

func (m *Manager) capacitySnapshotLocked() CapacitySnapshot {
	now := m.now()
	var s CapacitySnapshot
	for name, a := range m.capa.avail {
		v := a.accum
		if a.since != nil {
			v += now.Sub(*a.since)
		}
		s.Units = append(s.Units, UnitCapacity{Unit: name, Type: a.typ, CapsClass: a.caps, AvailableSeconds: v.Seconds()})
	}
	sortBy(s.Units, func(u UnitCapacity) string { return u.Unit })
	for k, v := range m.capa.busy {
		s.Busy = append(s.Busy, BusySeconds{k.unit, k.typ, k.caps, k.prio, k.outcome, v})
	}
	sortBy(s.Busy, func(b BusySeconds) string { return b.Unit + "\x00" + b.Prio + "\x00" + b.Outcome })
	keys := map[capKey]bool{}
	for k := range m.capa.requests {
		keys[k] = true
	}
	for k := range m.capa.abandoned {
		keys[k] = true
	}
	for k := range keys {
		s.Requests = append(s.Requests, RequestCounts{k.prio, k.caps, m.capa.requests[k], m.capa.abandoned[k]})
	}
	sortBy(s.Requests, func(r RequestCounts) string { return r.Prio + "\x00" + r.CapsClass })
	for k, h := range m.capa.wait {
		s.Waits = append(s.Waits, WaitSeries{k.prio, k.caps, k.result, h.Clone()})
	}
	sortBy(s.Waits, func(w WaitSeries) string { return w.Prio + "\x00" + w.CapsClass + "\x00" + w.Result })
	for k, h := range m.capa.hold {
		s.Holds = append(s.Holds, HoldSeries{k.prio, k.caps, k.owner, h.Clone()})
	}
	sortBy(s.Holds, func(h HoldSeries) string { return h.Prio + "\x00" + h.CapsClass + "\x00" + h.OwnerKind })
	return s
}
