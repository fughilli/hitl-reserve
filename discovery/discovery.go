// Package discovery layers runtime-detected components on top of the declarative
// catalog: it enumerates USB boards attached to the host by their stable
// /dev/serial/by-id/* symlinks and synthesizes one reservable unit per board, with
// a stable serial-derived name, its tty pinned to /dev/ttyACM0, and sticky sshd
// ports. Discovery is live — a Monitor polls and reconciles the unit set through
// engine.Manager.SyncUnits, so boards hot-plugged/removed after boot come and go
// without a daemon restart.
//
// It is optional and additive: a fully static bench needs none of it, and a mixed
// bench declares its fixed/composite/network units in the catalog while letting
// discovery fill in interchangeable single-board units.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fughilli/hitl-reserve/runner"
)

// Config configures runtime discovery (parsed from the catalog's `discovery`):
// USB auto-detection (Glob) and/or a seeded-units file (SeededFile). Either or both
// may be set; the monitor merges what they find and reconciles it live.
type Config struct {
	Enabled          bool   `json:"enabled"`
	Glob             string `json:"glob,omitempty"`              // USB by-id glob; empty = no USB scan
	Type             string `json:"type,omitempty"`              // resource type assigned to each discovered board
	NamePrefix       string `json:"name_prefix,omitempty"`       // unit/component name prefix (default "usb-")
	SSHPortBase      int    `json:"ssh_port_base,omitempty"`     // default 2300
	Max              int    `json:"max,omitempty"`               // max concurrent discovered USB units (default 8)
	IntervalSeconds  int    `json:"interval_seconds,omitempty"`  // rescan cadence (default 3)
	RetentionSeconds int    `json:"retention_seconds,omitempty"` // absence before "unplugged" (default 30)

	// SeededFile is an optional path to a JSON file of units seeded at runtime (see
	// SeededUnit) — the general way to attach a unit the host can't auto-discover,
	// e.g. a network-reachable device. The monitor reads it every poll and merges
	// those units with the USB-discovered set, so an operator adds/removes one by
	// editing (or dropping) the file, no redeploy. Empty disables it.
	SeededFile string `json:"seeded_file,omitempty"`
	// SeededPortBase is where seeded units' sticky sshd ports start (default
	// SSHPortBase+Max — a dedicated range above the USB pool so a hot-plugged board
	// can never collide with a seeded unit). SeededMax bounds them (default 8).
	SeededPortBase int `json:"seeded_ssh_port_base,omitempty"`
	SeededMax      int `json:"seeded_max,omitempty"`
}

// ParseConfig parses a raw discovery config, filling defaults. A nil raw or one
// with enabled=false yields (nil, nil) — discovery off.
func ParseConfig(raw *json.RawMessage) (*Config, error) {
	if raw == nil {
		return nil, nil
	}
	var c Config
	if err := json.Unmarshal(*raw, &c); err != nil {
		return nil, fmt.Errorf("parse discovery config: %w", err)
	}
	if !c.Enabled {
		return nil, nil
	}
	// Glob is NOT defaulted: empty means "no USB scan" (a seeded-only host). Set it
	// explicitly to auto-discover USB boards.
	if c.NamePrefix == "" {
		c.NamePrefix = "usb-"
	}
	if c.SSHPortBase == 0 {
		c.SSHPortBase = 2300
	}
	if c.Max == 0 {
		c.Max = 8
	}
	if c.IntervalSeconds == 0 {
		c.IntervalSeconds = 3
	}
	if c.RetentionSeconds == 0 {
		c.RetentionSeconds = 30
	}
	if c.SeededPortBase == 0 {
		c.SeededPortBase = c.SSHPortBase + c.Max // dedicated range above the USB pool
	}
	if c.SeededMax == 0 {
		c.SeededMax = 8
	}
	return &c, nil
}

// SeededComponent is one component of a seeded unit.
type SeededComponent struct {
	Name         string            `json:"name"`
	Type         string            `json:"type,omitempty"`
	Kind         string            `json:"kind,omitempty"` // default "network"
	Devices      []string          `json:"devices,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Address      string            `json:"address,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
}

// SeededUnit is one entry in the seeded-units file. For the common single-component
// case, set the unit-level Type/Kind/Address/Devices/Env directly and leave
// Components empty (one component is synthesized from them); for a composite, list
// Components. Capabilities are the union of each component's resource-type caps
// (looked up via the monitor's typeCaps), explicit component/unit Capabilities, and
// the enrich hook (e.g. shared-resource taps).
type SeededUnit struct {
	Name         string            `json:"name"`
	Type         string            `json:"type,omitempty"`
	Kind         string            `json:"kind,omitempty"`     // default "network"
	PinOnly      *bool             `json:"pin_only,omitempty"` // default true (seeded units are usually scarce/network)
	SSHPort      int               `json:"ssh_port,omitempty"` // 0 = sticky-assign from SeededPortBase
	Address      string            `json:"address,omitempty"`
	Devices      []string          `json:"devices,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Components   []SeededComponent `json:"components,omitempty"`
}

// Syncer is the subset of engine.Manager a Monitor drives.
type Syncer interface {
	SyncUnits(ctx context.Context, units []runner.Unit) (added, removed []string)
}

// Monitor discovers USB units and remembers each board across scans: its sshd port
// is sticky (so a board keeps its port — and its live reservation — even when a
// different board is unplugged), and a board is only treated as gone once it has
// been absent for the whole retention window (tolerating boards that re-enumerate
// on reset). Driven from a single goroutine; not safe for concurrent use.
type Monitor struct {
	cfg      Config
	typeCaps func(string) []string  // resource-type capability lookup (from the catalog)
	enrich   func(*runner.Unit)     // optional post-processing (e.g. union shared-resource caps)
	now      func() time.Time       // injectable for tests
	last     map[string]runner.Unit // USB: stable name -> last-known unit (sticky port/spec)
	seen     map[string]time.Time   // USB: stable name -> last time present
	// Seeded units: sticky port by name, and the last cleanly-parsed set (reused if
	// a later read is malformed, so a bad edit never disturbs the USB units).
	seededLast     map[string]runner.Unit
	seededLastGood []runner.Unit
}

// NewMonitor builds a Monitor. typeCaps supplies a discovered board's capabilities
// from its resource type; enrich (may be nil) post-processes each unit (e.g. to
// union in shared-resource tap capabilities).
func NewMonitor(cfg Config, typeCaps func(string) []string, enrich func(*runner.Unit)) *Monitor {
	if typeCaps == nil {
		typeCaps = func(string) []string { return nil }
	}
	return &Monitor{
		cfg: cfg, typeCaps: typeCaps, enrich: enrich, now: time.Now,
		last: map[string]runner.Unit{}, seen: map[string]time.Time{},
		seededLast: map[string]runner.Unit{},
	}
}

// Scan returns the current unit set: USB-discovered boards (if a glob is set) plus
// any runtime-seeded units (if a seeded file is set), each with sticky ports. A USB
// glob error is returned. A malformed seeded file is NOT fatal — the last good
// seeded set is reused so a bad edit never disturbs the USB units.
func (m *Monitor) Scan() ([]runner.Unit, error) {
	usb, err := m.scanUSB()
	if err != nil {
		return nil, err
	}
	seeded, serr := m.readSeeded()
	if serr != nil {
		log.Printf("discover: seeded units: %v; keeping %d cached", serr, len(m.seededLastGood))
		seeded = m.seededLastGood
	} else {
		m.seededLastGood = seeded
	}
	out := append(append(make([]runner.Unit, 0, len(usb)+len(seeded)), usb...), seeded...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// scanUSB globs the host and returns the retained USB unit set with sticky ports.
// Empty glob (no USB scan configured) yields no units.
func (m *Monitor) scanUSB() ([]runner.Unit, error) {
	if m.cfg.Glob == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(m.cfg.Glob)
	if err != nil {
		return nil, fmt.Errorf("discover glob %q: %w", m.cfg.Glob, err)
	}
	boards := boardsFromByID(matches, m.cfg.NamePrefix)
	now := m.now()

	used := map[int]bool{}
	for _, u := range m.last {
		used[u.SSHPort] = true
	}
	present := map[string]bool{}
	for _, b := range boards {
		present[b.name] = true
		m.seen[b.name] = now
		if prev, ok := m.last[b.name]; ok {
			// Known board: keep its sticky port + device spec, but re-derive caps
			// (an enrich source like a channel-map may have changed since first sight).
			m.last[b.name] = m.build(prev.Name, prev.SSHPort, b)
			continue
		}
		port := m.allocPort(used)
		if port == 0 {
			log.Printf("discover: %s attached but all %d unit ports are in use; ignoring", b.name, m.cfg.Max)
			continue
		}
		used[port] = true
		m.last[b.name] = m.build(b.name, port, b)
	}
	// Absent boards: drop only those gone for the whole retention window.
	retention := time.Duration(m.cfg.RetentionSeconds) * time.Second
	for name := range m.last {
		if present[name] {
			continue
		}
		if now.Sub(m.seen[name]) > retention {
			delete(m.last, name)
			delete(m.seen, name)
		}
	}

	out := make([]runner.Unit, 0, len(m.last))
	for _, u := range m.last {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// build assembles a unit for one discovered board.
func (m *Monitor) build(name string, port int, b board) runner.Unit {
	comp := runner.Component{
		Name:         name,
		Type:         m.cfg.Type,
		Kind:         "usb",
		Capabilities: m.typeCaps(m.cfg.Type),
		Devices:      b.devices,
		Env:          b.env,
	}
	u := runner.Unit{
		Name:         name,
		Type:         m.cfg.Type,
		Kind:         "usb",
		Capabilities: append([]string(nil), comp.Capabilities...),
		Components:   []runner.Component{comp},
		SSHPort:      port,
	}
	if m.enrich != nil {
		m.enrich(&u)
	}
	return u
}

func (m *Monitor) allocPort(used map[int]bool) int {
	for i := 0; i < m.cfg.Max; i++ {
		if p := m.cfg.SSHPortBase + i; !used[p] {
			return p
		}
	}
	return 0
}

// readSeeded parses the seeded-units file into units with sticky ports from the
// dedicated seeded range. An unset file yields no units; a missing file is not an
// error (it just means nothing seeded, and forgets any prior seeds). A parse or
// validation error is returned WITHOUT mutating state, so the caller keeps the last
// good set. Runs on the monitor's single goroutine, so seededLast needs no lock.
func (m *Monitor) readSeeded() ([]runner.Unit, error) {
	if m.cfg.SeededFile == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(m.cfg.SeededFile)
	if err != nil {
		if os.IsNotExist(err) {
			m.seededLast = map[string]runner.Unit{}
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", m.cfg.SeededFile, err)
	}
	var specs []SeededUnit
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", m.cfg.SeededFile, err)
	}
	used := map[int]bool{}
	for _, u := range m.seededLast {
		used[u.SSHPort] = true
	}
	names := map[string]bool{}
	next := map[string]runner.Unit{}
	out := make([]runner.Unit, 0, len(specs))
	for i, s := range specs {
		if s.Name == "" {
			return nil, fmt.Errorf("seeded unit #%d: name is required", i+1)
		}
		if names[s.Name] {
			return nil, fmt.Errorf("seeded unit %q: duplicate name", s.Name)
		}
		names[s.Name] = true
		// Sticky port: reuse the unit's prior port, else allocate from the range.
		port := s.SSHPort
		if port == 0 {
			if prev, ok := m.seededLast[s.Name]; ok {
				port = prev.SSHPort
			} else if port = m.allocSeededPort(used); port == 0 {
				log.Printf("discover: seeded unit %s but all %d seeded ports are in use; ignoring", s.Name, m.cfg.SeededMax)
				continue
			}
		}
		used[port] = true
		u := m.buildSeeded(s, port)
		next[s.Name] = u
		out = append(out, u)
	}
	m.seededLast = next
	return out, nil
}

// buildSeeded assembles a runner.Unit from a SeededUnit. With no explicit
// Components it synthesizes one from the unit-level fields (the common
// single-component case).
func (m *Monitor) buildSeeded(s SeededUnit, port int) runner.Unit {
	kind := s.Kind
	if kind == "" {
		kind = "network"
	}
	pinOnly := true // seeded units are usually scarce/network — default to pin-only
	if s.PinOnly != nil {
		pinOnly = *s.PinOnly
	}
	comps := s.Components
	if len(comps) == 0 {
		comps = []SeededComponent{{
			Name: s.Name, Type: s.Type, Kind: kind,
			Devices: s.Devices, Env: s.Env, Address: s.Address,
		}}
	}
	capSet := map[string]bool{}
	network := 0
	var rc []runner.Component
	for _, c := range comps {
		ck := c.Kind
		if ck == "" {
			ck = kind
		}
		caps := append([]string(nil), m.typeCaps(c.Type)...)
		caps = append(caps, c.Capabilities...)
		for _, cap := range caps {
			capSet[cap] = true
		}
		if ck == "network" {
			network++
		}
		rc = append(rc, runner.Component{
			Name: c.Name, Type: c.Type, Kind: ck, Capabilities: caps,
			Devices: c.Devices, Env: c.Env, Address: c.Address,
		})
	}
	for _, cap := range s.Capabilities {
		capSet[cap] = true
	}
	caps := make([]string, 0, len(capSet))
	for c := range capSet {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	uKind := "usb"
	switch {
	case len(rc) > 1:
		uKind = "composite"
	case network == len(rc) && len(rc) > 0:
		uKind = "network"
	}
	u := runner.Unit{
		Name: s.Name, Type: s.Type, Kind: uKind, PinOnly: pinOnly,
		Capabilities: caps, Components: rc, SSHPort: port,
	}
	if m.enrich != nil {
		m.enrich(&u)
	}
	return u
}

func (m *Monitor) allocSeededPort(used map[int]bool) int {
	for i := 0; i < m.cfg.SeededMax; i++ {
		if p := m.cfg.SeededPortBase + i; !used[p] {
			return p
		}
	}
	return 0
}

// Run polls the glob every interval and reconciles the syncer's unit set.
func (m *Monitor) Run(ctx context.Context, s Syncer) {
	t := time.NewTicker(time.Duration(m.cfg.IntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			units, err := m.Scan()
			if err != nil {
				log.Printf("discover: %v", err)
				continue
			}
			added, removed := s.SyncUnits(ctx, units)
			for _, n := range added {
				log.Printf("discover: unit %s attached", n)
			}
			for _, n := range removed {
				log.Printf("discover: unit %s removed", n)
			}
		}
	}
}

// board is one physical board discovered on the host, identified by a stable name
// derived from its USB serial so it keeps that identity across re-enumeration.
type board struct {
	name    string
	devices []string
	env     map[string]string
}

// boardsFromByID turns /dev/serial/by-id paths into boards. It keeps only each
// board's primary CDC-ACM interface (…-if00, or names with no -if token), dedupes
// by resolved tty and derived name, and pins each tty to /dev/ttyACM0.
func boardsFromByID(paths []string, prefix string) []board {
	seenTTY, seenName := map[string]bool{}, map[string]bool{}
	var out []board
	for _, path := range paths {
		base := filepath.Base(path)
		if i := strings.Index(base, "-if"); i >= 0 && !strings.HasPrefix(base[i:], "-if00") {
			continue
		}
		target := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			target = r
		}
		if seenTTY[target] {
			continue
		}
		name := nameFromByID(base, prefix)
		if seenName[name] {
			continue
		}
		seenTTY[target], seenName[name] = true, true
		b := board{name: name, devices: []string{path + ":/dev/ttyACM0"}}
		if s := espSerialFromByID(base); s != "" {
			b.env = map[string]string{"HITL_ADAPTER_SERIAL": s}
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// nameFromByID derives a stable, shell-safe unit name from a by-id name: the
// board's USB serial (an ESP32-C6's is its MAC), trimmed to a short suffix, behind
// the configured prefix — so the name follows the physical board across hot-plug.
func nameFromByID(base, prefix string) string {
	id := espSerialFromByID(base)
	if id == "" {
		id = strings.TrimPrefix(base, "usb-")
		if i := strings.Index(id, "-if"); i >= 0 {
			id = id[:i]
		}
	}
	var b strings.Builder
	for _, r := range id {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteByte(byte(r))
		}
	}
	s := strings.ToLower(b.String())
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	return prefix + s
}

// espSerialFromByID pulls the USB serial out of an ESP32-C6 built-in USB-JTAG
// by-id name; a debugger matches that value via `adapter serial`. Empty for other
// adapters.
func espSerialFromByID(base string) string {
	const prefix = "usb-Espressif_USB_JTAG_serial_debug_unit_"
	if !strings.HasPrefix(base, prefix) {
		return ""
	}
	s := strings.TrimPrefix(base, prefix)
	if i := strings.Index(s, "-if"); i >= 0 {
		s = s[:i]
	}
	return s
}
