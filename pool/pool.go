// Package pool turns a list of reservation-host endpoints (from $HITL_HOSTS) into
// a single chosen host: it queries each host's /status and picks one that can
// serve the request, so a client doesn't queue behind a busy host when another
// sits idle.
//
// Selection: among hosts that have a matching free unit right now, pick the one
// whose tightest free unit has the fewest capabilities beyond what's required
// (best-fit — keeps scarce over-provisioned hosts free), breaking ties by idle,
// then more free units, then shortest queue, then input order. If nothing matching
// is free anywhere but some host has a matching unit busy, route there to WAIT in
// its FIFO. Unreachable hosts sort last; an all-down pool is an error.
package pool

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"

	"github.com/fughilli/hitl-reserve/api"
)

// DefaultPort is the daemon API port assumed for bare hosts in $HITL_HOSTS.
const DefaultPort = "8087"

// Normalize splits a $HITL_HOSTS value (comma/whitespace-separated) into canonical
// base URLs. Entries may be a bare host, host:port, or a full URL; bare hosts get
// http:// and :8087. Order and duplicates are preserved except exact-duplicate URLs.
func Normalize(list string) []string {
	fields := strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		u := normalizeOne(f)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

func normalizeOne(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	if u.Port() == "" {
		u.Host = u.Hostname() + ":" + DefaultPort
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}

// Probe is one host's queried state (or the error querying it produced).
type Probe struct {
	URL    string
	Status *api.Status
	Err    error
}

func (p Probe) reachable() bool { return p.Err == nil && p.Status != nil }

func (p Probe) idle() bool { return p.reachable() && p.Status.FreeUnits > 0 }

func (p Probe) freeSlots() int {
	if !p.reachable() {
		return 0
	}
	return p.Status.FreeUnits
}

func (p Probe) queue() int {
	if !p.reachable() {
		return int(^uint(0) >> 1)
	}
	return p.Status.QueueLength
}

// StatusFn fetches one host's /status. Injected so Pick is unit-testable.
type StatusFn func(base string) (*api.Status, error)

// Probes queries every host in the pool (preserving order).
func Probes(hosts []string, get StatusFn) []Probe {
	out := make([]Probe, 0, len(hosts))
	for _, s := range hosts {
		st, err := get(s)
		out = append(out, Probe{URL: s, Status: st, Err: err})
	}
	return out
}

// Require narrows the pool to hosts that can serve a reservation of this unit type
// and/or with these capabilities.
type Require struct {
	Type string   // require a unit of this type ("" = any)
	Caps []string // require a unit whose capabilities ⊇ Caps
}

// unitServes reports whether a free UnitStatus can host a request for type/caps
// under the pin rules the pool can see (an explicit type target may land on a
// pin-only unit; a caps-only/unconstrained request never does).
func unitServes(u api.UnitStatus, typ string, caps []string) bool {
	return u.Active == nil && unitMatches(u, typ, caps)
}

// unitMatches is unitServes WITHOUT the free requirement: could the unit serve the
// request once free. Used to route a request to a host for QUEUEING when nothing
// matching is free right now.
func unitMatches(u api.UnitStatus, typ string, caps []string) bool {
	if typ != "" && u.Type != typ {
		return false
	}
	if !capsSubset(caps, u.Capabilities) {
		return false
	}
	if typ == "" && u.PinOnly {
		return false
	}
	return true
}

func hasFreeServingUnit(p Probe, typ string, caps []string) bool {
	if !p.reachable() {
		return false
	}
	for _, u := range p.Status.Units {
		if unitServes(u, typ, caps) {
			return true
		}
	}
	return false
}

func hasQueueableUnit(p Probe, typ string, caps []string) bool {
	if !p.reachable() {
		return false
	}
	for _, u := range p.Status.Units {
		if unitMatches(u, typ, caps) {
			return true
		}
	}
	return false
}

// fitScore is the fewest capabilities beyond `caps` among a host's free serving
// units — the tightest unit it can offer now. A host with no free serving unit
// scores MaxInt. Lower is better.
func fitScore(p Probe, typ string, caps []string) int {
	best := math.MaxInt
	if !p.reachable() {
		return best
	}
	for _, u := range p.Status.Units {
		if !unitServes(u, typ, caps) {
			continue
		}
		if e := extraCaps(u.Capabilities, caps); e < best {
			best = e
		}
	}
	return best
}

// Pick chooses the best host from already-collected probes, or an error if none
// can serve. An optional Require narrows to hosts with a matching unit.
func Pick(probes []Probe, req ...Require) (string, error) {
	if len(probes) == 0 {
		return "", fmt.Errorf("no hosts in pool")
	}
	var r Require
	if len(req) > 0 {
		r = req[0]
	}
	if r.Type != "" || len(r.Caps) > 0 {
		var free, queueable []Probe
		for _, p := range probes {
			if hasFreeServingUnit(p, r.Type, r.Caps) {
				free = append(free, p)
			} else if hasQueueableUnit(p, r.Type, r.Caps) {
				queueable = append(queueable, p)
			}
		}
		switch {
		case len(free) > 0:
			probes = free
		case len(queueable) > 0:
			probes = queueable
		default:
			return "", fmt.Errorf("no host with a unit matching %s in pool of %d: %s", describeReq(r), len(probes), summarizeErrs(probes))
		}
	}
	fit := make(map[string]int, len(probes))
	for _, p := range probes {
		fit[p.URL] = fitScore(p, r.Type, r.Caps)
	}
	ordered := make([]Probe, len(probes))
	copy(ordered, probes)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.reachable() != b.reachable() {
			return a.reachable()
		}
		if fa, fb := fit[a.URL], fit[b.URL]; fa != fb {
			return fa < fb
		}
		if a.idle() != b.idle() {
			return a.idle()
		}
		if fa, fb := a.freeSlots(), b.freeSlots(); fa != fb {
			return fa > fb
		}
		return a.queue() < b.queue()
	})
	best := ordered[0]
	if !best.reachable() {
		return "", fmt.Errorf("no reachable host in pool of %d: %v", len(probes), summarizeErrs(probes))
	}
	return best.URL, nil
}

func describeReq(r Require) string {
	switch {
	case r.Type != "" && len(r.Caps) > 0:
		return fmt.Sprintf("type %s + caps %v", r.Type, r.Caps)
	case r.Type != "":
		return fmt.Sprintf("type %s", r.Type)
	default:
		return fmt.Sprintf("caps %v", r.Caps)
	}
}

func summarizeErrs(probes []Probe) string {
	var parts []string
	for _, p := range probes {
		if p.Err != nil {
			parts = append(parts, fmt.Sprintf("%s: %v", p.URL, p.Err))
		}
	}
	return strings.Join(parts, "; ")
}

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

func capsSubset(need, have []string) bool {
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
