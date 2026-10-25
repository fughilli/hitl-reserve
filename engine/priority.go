package engine

// Priority scheduling, queue progress and ETA.
//
// ORDERING. Queued reservations are admitted in order of a VIRTUAL ARRIVAL time:
// CreatedAt + aging[class] (ties: real arrival, then admission order). The highest
// class (merge-queue) has offset 0; lower classes are ordered as though they had
// arrived their offset later. So a higher class overtakes a lower one only if it
// arrives within the lower one's offset; once that window has passed the older
// reservation is ahead of every newcomer of every class. That is aging with a
// hard bound and no extra state: a dev reservation waits at most its offset plus
// the work already ahead of it, however much CI keeps arriving — it can't starve.
// Within a class the order is strict FIFO. Nothing here ever touches an ACTIVE
// reservation: classes reorder the queue only (no preemption).
//
// We chose aging over a reserved per-class share because the bench is idle most
// of the day (28–57% daily C6 utilization) and fails only in bursts: a reserved
// share would idle boards outside bursts or need its own "unless nobody is
// waiting" escape hatch, whereas virtual-arrival aging is work-conserving by
// construction and gives each class one explicit, tunable bound.
//
// PROGRESS. A waiter's progress clock (ProgressAt) advances when its Position
// drops (something ahead of it activated or left) or when a unit it can use is
// freed or hot-plugged — i.e. whenever the queue is draining for it, even if a
// newly arrived higher class takes the freed unit. It never advances while the
// host is cordoned. Clients give up on a STALLED queue (no progress for a long
// while), not on elapsed time.
//
// ETA. A list-scheduling estimate: the competing units' slots free up at
// (recent mean hold − current holder's age), each waiter ahead takes the earliest
// slot for one mean hold, and our turn is the earliest slot after them.

import (
	"sort"
	"time"

	"github.com/fughilli/hitl-reserve/api"
	"github.com/fughilli/hitl-reserve/runner"
)

// DefaultAging is the per-class aging offset used when none is configured.
// merge-queue is 0 by definition. pr-ci yields to merge-critical runs that arrive
// within 20 min of it (about one full suite's wall time on a 4-board fleet); dev
// yields to CI for up to an hour, after which it is ahead of any newly arriving
// CI.
var DefaultAging = map[string]time.Duration{
	api.PriorityMergeQueue: 0,
	api.PriorityPRCI:       20 * time.Minute,
	api.PriorityDev:        60 * time.Minute,
}

// DefaultHold seeds a unit type's hold-time estimate before any reservation of
// that type has completed.
const DefaultHold = 10 * time.Minute

// holdEWMAWeight is the weight of the newest completed hold in the per-type mean.
const holdEWMAWeight = 0.3

// minRemaining floors an active holder's estimated remaining time: a holder
// already past the mean hold is assumed to finish soon, not to have finished.
const minRemaining = time.Minute

// WaitBuckets are the queue-wait histogram bounds (seconds): 10 s .. 4 h.
var WaitBuckets = []float64{10, 30, 60, 120, 300, 600, 900, 1200, 1800, 2400, 3600, 5400, 7200, 10800, 14400}

// WithPriorityAging overrides per-class aging offsets (classes absent keep their
// DefaultAging value; merge-queue is always 0).
func WithPriorityAging(offsets map[string]time.Duration) Option {
	return func(m *Manager) {
		for k, v := range offsets {
			c, ok := api.NormalizePriority(k)
			if !ok || c == api.PriorityMergeQueue || v < 0 {
				continue
			}
			m.aging[c] = v
		}
	}
}

// WithDefaultHold sets the hold-time estimate used for a unit type with no
// completed reservations yet.
func WithDefaultHold(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.defaultHold = d
		}
	}
}

// WithClock injects the time source (tests).
func WithClock(now func() time.Time) Option {
	return func(m *Manager) { m.now = now }
}

// --- ordering ---------------------------------------------------------------

func (m *Manager) virtualArrivalLocked(r *api.Reservation) time.Time {
	return r.CreatedAt.Add(m.aging[r.Priority])
}

// queuedOrderLocked returns the queued reservations in scheduling order.
func (m *Manager) queuedOrderLocked() []*api.Reservation {
	var q []*api.Reservation
	for _, r := range m.items {
		if r.State == api.StateQueued {
			q = append(q, r)
		}
	}
	sort.SliceStable(q, func(i, j int) bool {
		vi, vj := m.virtualArrivalLocked(q[i]), m.virtualArrivalLocked(q[j])
		if !vi.Equal(vj) {
			return vi.Before(vj)
		}
		return q[i].CreatedAt.Before(q[j].CreatedAt) // stable sort keeps admission order on a full tie
	})
	return q
}

// candidatesLocked is the set of unit indices that could EVER serve r (busy or
// not, ignoring caps): its placement constraints only.
func (m *Manager) candidatesLocked(r *api.Reservation) map[int]bool {
	c := map[int]bool{}
	for i := range m.units {
		if m.unitServesLocked(&m.units[i], r) {
			c[i] = true
		}
	}
	return c
}

func overlaps(a, b map[int]bool) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// queueModel is one consistent snapshot of the queue for position/ETA/progress.
type queueModel struct {
	order []*api.Reservation
	cands map[string]map[int]bool
	pos   map[string]int // 1 + competing waiters ahead
}

func (m *Manager) queueModelLocked() queueModel {
	qm := queueModel{order: m.queuedOrderLocked(), cands: map[string]map[int]bool{}, pos: map[string]int{}}
	for _, r := range qm.order {
		qm.cands[r.ID] = m.candidatesLocked(r)
	}
	for i, r := range qm.order {
		n := 1
		for _, a := range qm.order[:i] {
			if overlaps(qm.cands[a.ID], qm.cands[r.ID]) {
				n++
			}
		}
		qm.pos[r.ID] = n
	}
	return qm
}

// aheadLocked lists the competing waiters ahead of position idx in qm.order.
func (qm queueModel) ahead(idx int, cands map[int]bool) []*api.Reservation {
	var out []*api.Reservation
	for _, a := range qm.order[:idx] {
		if overlaps(qm.cands[a.ID], cands) {
			out = append(out, a)
		}
	}
	return out
}

// --- progress ---------------------------------------------------------------

// refreshProgressLocked recomputes every waiter's Position and stamps progress on
// those whose Position dropped since the last refresh. Called after every queue
// mutation (the end of reconcile).
func (m *Manager) refreshProgressLocked() {
	qm := m.queueModelLocked()
	now := m.now()
	live := map[string]bool{}
	for _, r := range qm.order {
		live[r.ID] = true
		p := qm.pos[r.ID]
		if last, ok := m.lastPos[r.ID]; ok && p < last && !m.cordoned {
			m.progress[r.ID] = now
		}
		m.lastPos[r.ID] = p
	}
	for id := range m.lastPos {
		if !live[id] {
			delete(m.lastPos, id)
		}
	}
}

// markUnitProgressLocked stamps progress on every waiter the named unit could
// serve — the unit just became free (released) or appeared (hot-plug).
func (m *Manager) markUnitProgressLocked(name string) {
	if m.cordoned {
		return
	}
	var u *runner.Unit
	for i := range m.units {
		if m.units[i].Name == name {
			u = &m.units[i]
			break
		}
	}
	if u == nil {
		return
	}
	now := m.now()
	for _, r := range m.items {
		if r.State == api.StateQueued && m.unitServesLocked(u, r) {
			m.progress[r.ID] = now
		}
	}
}

// --- hold estimates + ETA ----------------------------------------------------

func (m *Manager) holdEstimateLocked(unitType string) time.Duration {
	if d, ok := m.holdEst[unitType]; ok {
		return d
	}
	return m.defaultHold
}

// recordHoldLocked folds a completed hold into its unit type's mean.
func (m *Manager) recordHoldLocked(unitType string, d time.Duration) {
	if d <= 0 {
		return
	}
	if old, ok := m.holdEst[unitType]; ok {
		m.holdEst[unitType] = time.Duration(float64(old)*(1-holdEWMAWeight) + float64(d)*holdEWMAWeight)
	} else {
		m.holdEst[unitType] = d
	}
}

type slot struct {
	free time.Duration // from now
	typ  string
}

// etaLocked estimates the wait for a reservation whose candidate units are cands
// with the given competing waiters ahead. ok is false when no estimate exists
// (no candidate unit).
func (m *Manager) etaLocked(cands map[int]bool, ahead []*api.Reservation) (time.Duration, bool) {
	if len(cands) == 0 {
		return 0, false
	}
	now := m.now()
	holder := map[string]*api.Reservation{}
	for _, r := range m.items {
		if r.State == api.StateActive {
			holder[r.Unit] = r
		}
	}
	var slots []slot
	perType := map[string]int{}
	idx := make([]int, 0, len(cands))
	for i := range cands {
		idx = append(idx, i)
	}
	sort.Ints(idx) // deterministic
	for _, i := range idx {
		u := m.units[i]
		perType[u.Type]++
		s := slot{typ: u.Type}
		if h := holder[u.Name]; h != nil {
			rem := m.holdEstimateLocked(u.Type)
			if h.StartedAt != nil {
				rem -= now.Sub(*h.StartedAt)
			}
			if rem < minRemaining {
				rem = minRemaining
			}
			s.free = rem
		}
		slots = append(slots, s)
	}
	// Concurrency caps bound how many of these slots run in parallel.
	parallel := 0
	for t, n := range perType {
		if c, ok := m.classCap[t]; ok && c < n {
			n = c
		}
		parallel += n
	}
	if m.maxConcurrent > 0 && parallel > m.maxConcurrent {
		parallel = m.maxConcurrent
	}
	if parallel < 1 {
		parallel = 1
	}
	// Active holders occupy the cap first (a free unit in a capped-out class can't
	// start anyone until a holder leaves), then free units fill the rest.
	sort.SliceStable(slots, func(i, j int) bool {
		bi, bj := slots[i].free > 0, slots[j].free > 0
		if bi != bj {
			return bi
		}
		return slots[i].free < slots[j].free
	})
	if len(slots) > parallel {
		slots = slots[:parallel]
	}
	popMin := func() int {
		best := 0
		for i := range slots {
			if slots[i].free < slots[best].free {
				best = i
			}
		}
		return best
	}
	for range ahead {
		i := popMin()
		slots[i].free += m.holdEstimateLocked(slots[i].typ)
	}
	return slots[popMin()].free, true
}

// annotateQueuedLocked fills a queued snapshot's priority-aware fields.
func (m *Manager) annotateQueuedLocked(cp *api.Reservation, qm queueModel) {
	now := m.now()
	at := cp.CreatedAt
	if p, ok := m.progress[cp.ID]; ok {
		at = p
	}
	cp.ProgressAt = &at
	cp.SinceProgressSeconds = int(now.Sub(at).Seconds())
	idx := -1
	for i, r := range qm.order {
		if r.ID == cp.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	cp.Position = qm.pos[cp.ID]
	cands := qm.cands[cp.ID]
	switch {
	case m.cordoned:
		cp.Blocked = api.BlockedCordoned
	case len(cands) == 0:
		cp.Blocked = api.BlockedNoUnit
	default:
		if eta, ok := m.etaLocked(cands, qm.ahead(idx, cands)); ok {
			s := int(eta.Seconds())
			cp.EtaSeconds = &s
		}
	}
}

// --- queue summary (GET /queue) -----------------------------------------------

// RetryAfter is the dev back-off hint for an ETA: 0 (go ahead) up to devGoAhead,
// else half the ETA clamped to [1 min, 10 min].
func RetryAfter(eta time.Duration) time.Duration {
	const devGoAhead = 10 * time.Minute
	if eta <= devGoAhead {
		return 0
	}
	r := eta / 2
	if r < time.Minute {
		r = time.Minute
	}
	if r > 10*time.Minute {
		r = 10 * time.Minute
	}
	return r
}

// QueueSummary returns the cheap queue overview served at GET /queue.
func (m *Manager) QueueSummary() api.QueueSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	qm := m.queueModelLocked()
	s := api.QueueSummary{Host: m.host, Workspace: m.workspace, Time: now, Cordoned: m.cordoned, QueueLength: len(qm.order)}
	for _, c := range api.PriorityClasses {
		cq := api.ClassQueue{Class: c, AgingSeconds: int(m.aging[c].Seconds())}
		for _, r := range qm.order {
			if r.Priority == c {
				cq.Queued++
				if w := int(now.Sub(r.CreatedAt).Seconds()); w > cq.OldestWaitSeconds {
					cq.OldestWaitSeconds = w
				}
			}
		}
		s.Classes = append(s.Classes, cq)
	}
	busy := map[string]bool{}
	for _, r := range m.items {
		if r.State == api.StateActive {
			busy[r.Unit] = true
		}
	}
	var types []string
	seen := map[string]bool{}
	for _, u := range m.units {
		if !seen[u.Type] {
			seen[u.Type] = true
			types = append(types, u.Type)
		}
	}
	sort.Strings(types)
	for _, t := range types {
		tq := api.TypeQueue{Type: t, HoldEstimateSeconds: int(m.holdEstimateLocked(t).Seconds())}
		for _, u := range m.units {
			if u.Type == t {
				tq.Units++
				if !busy[u.Name] {
					tq.Free++
				}
			}
		}
		// A hypothetical type-pinned newcomer of each class, enqueued now.
		probe := &api.Reservation{ID: "\x00probe", State: api.StateQueued, CreatedAt: now}
		m.typ[probe.ID] = t
		cands := m.candidatesLocked(probe)
		delete(m.typ, probe.ID)
		for _, r := range qm.order {
			if overlaps(qm.cands[r.ID], cands) {
				tq.Queued++
			}
		}
		if !m.cordoned {
			tq.EtaNewSeconds = map[string]int{}
			for _, c := range api.PriorityClasses {
				va := now.Add(m.aging[c])
				var ahead []*api.Reservation
				for _, r := range qm.order {
					if !m.virtualArrivalLocked(r).After(va) && overlaps(qm.cands[r.ID], cands) {
						ahead = append(ahead, r)
					}
				}
				if eta, ok := m.etaLocked(cands, ahead); ok {
					tq.EtaNewSeconds[c] = int(eta.Seconds())
				}
			}
			tq.RetryAfterSeconds = int(RetryAfter(time.Duration(tq.EtaNewSeconds[api.PriorityDev]) * time.Second).Seconds())
		} else {
			tq.RetryAfterSeconds = int((10 * time.Minute).Seconds())
		}
		s.Types = append(s.Types, tq)
	}
	return s
}

// --- metrics -------------------------------------------------------------------

// ClassMetric is one scheduling class's slice of a MetricsSnapshot.
type ClassMetric struct {
	Class             string
	Queued            int
	OldestWaitSeconds float64
}

func (m *Manager) classMetricsLocked() []ClassMetric {
	now := m.now()
	var out []ClassMetric
	for _, c := range api.PriorityClasses {
		cm := ClassMetric{Class: c}
		for _, r := range m.items {
			if r.State == api.StateQueued && r.Priority == c {
				cm.Queued++
				if w := now.Sub(r.CreatedAt).Seconds(); w > cm.OldestWaitSeconds {
					cm.OldestWaitSeconds = w
				}
			}
		}
		out = append(out, cm)
	}
	return out
}
