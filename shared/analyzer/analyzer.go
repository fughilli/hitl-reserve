// Package analyzer is a reference SHARED RESOURCE: a logic analyzer whose channels
// are wired a few at a time to each unit's signal, so one cheap instrument serves
// every unit on the host. It implements shared.Broker (+ shared.Binder): it maps a
// unit name to its channel subset + wire protocol, runs a triggered sigrok capture
// scoped to those channels, decodes it, and serializes access so concurrent
// per-unit reservations can't collide on the single device.
//
// The instrument stays on the host with the daemon (which owns USB); it is never
// passed into a reservation environment. Holders request captures over the daemon's
// broker endpoint (POST /shared/{name}), naming their unit — so nothing about
// raw-USB isolation (package runner) changes.
//
// This package is a self-contained example of the broker pattern; it shells out to
// sigrok-cli and supports the WS2812 and SPI RGB decoders plus a raw-SPI byte mode.
package analyzer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/fughilli/hitl-reserve/api"
)

// UnitMap is one unit's slice of the shared analyzer: which channel(s) tap its
// signal line(s) and how to decode them. Sharing one instrument across units means
// assigning each a disjoint channel subset.
type UnitMap struct {
	Channels []string `json:"channels"` // sigrok channel names, e.g. ["D0"] or ["D0","D1"] (spi clk,data)
	Protocol Protocol `json:"protocol"` // ws2812 (default), spi, or spi-raw
}

// Config configures the broker.
type Config struct {
	// Name is the resource id on the host (default "logic-analyzer").
	Name string
	// SigrokCLI is the sigrok-cli binary (default "sigrok-cli").
	SigrokCLI string
	// Driver is the sigrok capture driver, e.g. "fx2lafw". "" leaves the broker
	// dormant (Present()==false) so the same daemon runs on a host with no analyzer.
	Driver string
	// SampleRate is the sigrok samplerate config, e.g. "24m".
	SampleRate string
	// Samples is the default capture length in samples.
	Samples int
	// Map is the initial unit→channel map, keyed by unit name; key "" is the
	// default/fallback. Bindings from the catalog overlay this.
	Map map[string]UnitMap
	// MapPath, if set, is a JSON file the map is persisted to (loaded at New,
	// overlaying Map so runtime edits survive restarts; rewritten by map.set).
	MapPath string
	// HardwareProbe reports whether the instrument is physically attached. nil means
	// "assume present" (unit tests); production passes a real probe (e.g. a sysfs USB
	// scan) so a host with the driver configured but no instrument stays dormant.
	HardwareProbe func() bool
}

// Broker owns the single shared analyzer and serializes captures on it.
type Broker struct {
	cfg     Config
	name    string
	mu      sync.Mutex   // exactly one capture at a time on the one instrument
	mapMu   sync.RWMutex // guards cfg.Map
	present bool
}

// New builds a broker, filling defaults. A dormant broker (empty Driver, or a
// HardwareProbe that reports absent) is valid and reports Present()==false.
func New(cfg Config) *Broker {
	if cfg.Name == "" {
		cfg.Name = "logic-analyzer"
	}
	if cfg.SigrokCLI == "" {
		cfg.SigrokCLI = "sigrok-cli"
	}
	if cfg.SampleRate == "" {
		cfg.SampleRate = "24m"
	}
	if cfg.Samples == 0 {
		cfg.Samples = 5000000 // ≈208ms @24MHz; span the DUT's frame cadence (see splanc notes)
	}
	if cfg.Map == nil {
		cfg.Map = map[string]UnitMap{}
	}
	if cfg.MapPath != "" {
		if persisted, err := loadMapFile(cfg.MapPath); err == nil {
			for k, v := range persisted {
				cfg.Map[k] = v
			}
		}
	}
	present := cfg.Driver != ""
	if present && cfg.HardwareProbe != nil {
		present = cfg.HardwareProbe()
	}
	return &Broker{cfg: cfg, name: cfg.Name, present: present}
}

// Name implements shared.Broker.
func (b *Broker) Name() string { return b.name }

// Kind implements shared.Broker.
func (b *Broker) Kind() string { return "logic-analyzer" }

// Present implements shared.Broker.
func (b *Broker) Present() bool { return b != nil && b.present }

// Describe implements shared.Broker.
func (b *Broker) Describe() api.SharedResourceInfo {
	info := api.SharedResourceInfo{Name: b.name, Kind: "logic-analyzer", Present: b.Present()}
	if !b.Present() {
		return info
	}
	b.mapMu.RLock()
	defer b.mapMu.RUnlock()
	protoSeen, chSeen := map[string]bool{}, map[string]bool{}
	var protocols, channels []string
	add := func(m UnitMap) {
		p := string(m.Protocol)
		if p == "" {
			p = string(ProtocolWS2812)
		}
		if !protoSeen[p] {
			protoSeen[p] = true
			protocols = append(protocols, p)
		}
		for _, c := range m.Channels {
			if !chSeen[c] {
				chSeen[c] = true
				channels = append(channels, c)
			}
		}
	}
	for _, m := range b.cfg.Map {
		add(m)
	}
	if len(protocols) == 0 {
		add(UnitMap{Channels: []string{"D0"}, Protocol: ProtocolWS2812})
	}
	sort.Strings(protocols)
	sort.Strings(channels)
	info.Attributes = map[string]any{
		"driver":    b.cfg.Driver,
		"protocols": protocols,
		"channels":  channels,
	}
	return info
}

// Bind implements shared.Binder: register a unit's channel/protocol slice from the
// catalog and return the capabilities it gains. The capability names the SIGNAL so
// tests can match precisely (a ws2812 tap → "logic-analyzer-led-strip"; an spi tap
// → "logic-analyzer-spi"), plus the generic "logic-analyzer".
func (b *Broker) Bind(unit string, config json.RawMessage) ([]string, error) {
	var m UnitMap
	if len(config) > 0 {
		if err := json.Unmarshal(config, &m); err != nil {
			return nil, fmt.Errorf("analyzer binding for %q: %w", unit, err)
		}
	}
	if len(m.Channels) == 0 {
		return nil, fmt.Errorf("analyzer binding for %q: channels required", unit)
	}
	if m.Protocol == "" {
		m.Protocol = ProtocolWS2812
	}
	b.mapMu.Lock()
	b.cfg.Map[unit] = m
	b.mapMu.Unlock()
	return capsForProtocol(m.Protocol), nil
}

// capsForProtocol maps a protocol to the capabilities a bound unit advertises.
func capsForProtocol(p Protocol) []string {
	switch p {
	case ProtocolSPI, ProtocolSPIRaw:
		return []string{"logic-analyzer", "logic-analyzer-spi"}
	default:
		return []string{"logic-analyzer", "logic-analyzer-led-strip"}
	}
}

// accessRequest is the broker's request envelope (POST /shared/{name}). Op selects
// the operation; an empty Op means "capture".
type accessRequest struct {
	Op string `json:"op,omitempty"` // "capture" (default), "map.get", "map.set"
	CaptureRequest
	Map map[string]UnitMap `json:"map,omitempty"` // for op=map.set
}

// CaptureRequest asks for a decoded trace of a unit's tapped line.
type CaptureRequest struct {
	Unit     string `json:"unit,omitempty"`     // overrides the Access unit; "" uses the default mapping
	Protocol string `json:"protocol,omitempty"` // overrides the mapping's protocol
	Samples  int    `json:"samples,omitempty"`  // 0 = the broker default
	SaveSR   bool   `json:"save_sr,omitempty"`  // include the raw .sr session (base64)
}

// Pixel is one decoded LED, 8-bit per channel in logical RGB order.
type Pixel struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
}

// CaptureResult is the decoded trace.
type CaptureResult struct {
	Unit       string  `json:"unit"`
	Protocol   string  `json:"protocol"`
	Pixels     []Pixel `json:"pixels,omitempty"`
	Bytes      []byte  `json:"bytes,omitempty"` // raw MOSI bytes for spi-raw (base64 in JSON)
	SampleRate int     `json:"sample_rate,omitempty"`
	SR         string  `json:"sr,omitempty"` // base64 .sr session when SaveSR
}

// Access implements shared.Broker: dispatch on the request Op.
func (b *Broker) Access(ctx context.Context, unit string, body []byte) ([]byte, int, error) {
	if !b.Present() {
		return jsonErr("this host has no logic analyzer configured", 503)
	}
	var req accessRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return jsonErr("invalid body: "+err.Error(), 400)
		}
	}
	switch req.Op {
	case "", "capture":
		if req.Unit == "" {
			req.Unit = unit
		}
		res, err := b.capture(ctx, req.CaptureRequest)
		if err != nil {
			return jsonErr(err.Error(), 500)
		}
		return okJSON(res)
	case "map.get":
		return okJSON(b.Snapshot())
	case "map.set":
		if err := b.SetMap(req.Map); err != nil {
			return jsonErr(err.Error(), 500)
		}
		return okJSON(b.Snapshot())
	default:
		return jsonErr("unknown op "+strconv.Quote(req.Op), 400)
	}
}

// capture runs one triggered capture for a unit and returns the decoded pixels. It
// serializes on the broker mutex (the single instrument does one at a time), then
// shells sigrok-cli to capture the unit's channels to a temp .sr and decode it.
func (b *Broker) capture(ctx context.Context, req CaptureRequest) (*CaptureResult, error) {
	m := b.mapping(req.Unit)
	if p := Protocol(req.Protocol); p != "" {
		m.Protocol = p
	}
	if m.Protocol == "" {
		m.Protocol = ProtocolWS2812
	}
	samples := req.Samples
	if samples <= 0 {
		samples = b.cfg.Samples
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	dir, err := os.MkdirTemp("", "hitl-capture-")
	if err != nil {
		return nil, fmt.Errorf("scratch dir: %w", err)
	}
	defer os.RemoveAll(dir)
	srPath := filepath.Join(dir, "capture.sr")

	if err := b.captureToSR(ctx, srPath, m, samples); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(srPath); err != nil || fi.Size() == 0 {
		return nil, fmt.Errorf("no data captured on %s (trigger %s=r never fired — is the unit driving this line? wrong channel?)",
			strings.Join(m.Channels, ","), m.Channels[0])
	}
	res := &CaptureResult{Unit: req.Unit, Protocol: string(m.Protocol), SampleRate: b.SampleRateHz()}
	if m.Protocol == ProtocolSPIRaw {
		data, err := b.decodeSRBytes(ctx, srPath, m)
		if err != nil {
			return nil, err
		}
		res.Bytes = data
	} else {
		pixels, err := b.decodeSR(ctx, srPath, m)
		if err != nil {
			return nil, err
		}
		res.Pixels = pixels
	}
	if req.SaveSR {
		raw, err := os.ReadFile(srPath)
		if err != nil {
			return nil, fmt.Errorf("read .sr: %w", err)
		}
		res.SR = base64.StdEncoding.EncodeToString(raw)
	}
	return res, nil
}

// SampleRateHz parses the configured samplerate to Hz (0 if unparseable).
func (b *Broker) SampleRateHz() int { return parseSampleRate(b.cfg.SampleRate) }

// mapping resolves a unit name to its channel/protocol assignment, falling back to
// the default ("") entry, then a single-channel D0/ws2812 tap.
func (b *Broker) mapping(unit string) UnitMap {
	b.mapMu.RLock()
	defer b.mapMu.RUnlock()
	if m, ok := b.cfg.Map[unit]; ok {
		return m
	}
	if m, ok := b.cfg.Map[""]; ok {
		return m
	}
	return UnitMap{Channels: []string{"D0"}, Protocol: ProtocolWS2812}
}

// Snapshot returns a copy of the current unit→channel map.
func (b *Broker) Snapshot() map[string]UnitMap {
	b.mapMu.RLock()
	defer b.mapMu.RUnlock()
	out := make(map[string]UnitMap, len(b.cfg.Map))
	for k, v := range b.cfg.Map {
		out[k] = UnitMap{Channels: append([]string(nil), v.Channels...), Protocol: v.Protocol}
	}
	return out
}

// SetMap replaces the unit→channel map wholesale and persists it (if MapPath set).
func (b *Broker) SetMap(m map[string]UnitMap) error {
	b.mapMu.Lock()
	next := make(map[string]UnitMap, len(m))
	for k, v := range m {
		next[k] = UnitMap{Channels: append([]string(nil), v.Channels...), Protocol: v.Protocol}
	}
	b.cfg.Map = next
	path := b.cfg.MapPath
	b.mapMu.Unlock()
	if path == "" {
		return nil
	}
	return saveMapFile(path, next)
}

// captureToSR arms a rising-edge trigger on the unit's primary channel and
// captures `samples` samples of its channel subset to srPath.
func (b *Broker) captureToSR(ctx context.Context, srPath string, m UnitMap, samples int) error {
	if len(m.Channels) == 0 {
		return errors.New("no analyzer channels mapped for this unit")
	}
	// captureratio keeps a slice of samples BEFORE the trigger. We trigger on the
	// first RISING edge of the line, but a WS2812 bit begins with a rising edge —
	// so without pre-trigger samples the capture starts AT bit 0's rising edge and
	// sigrok's rgb_led_ws281x decoder, which needs to see the low→high transition,
	// misses bit 0 entirely: every pixel decodes one bit short (e.g. red 00-FF-00
	// GRB reads back as (254,1,0)) and the final pixel runs off the end (N-1 of N
	// decode). A small pre-trigger window captures the idle-low ahead of bit 0 so
	// the first transition is seen and the whole frame aligns. 2% of the default
	// (~4ms) is well inside the inter-frame gap, so it adds idle-low, not a
	// neighbouring frame.
	args := []string{
		"--driver", b.cfg.Driver,
		"--config", "samplerate=" + b.cfg.SampleRate + ":captureratio=2",
		"--channels", strings.Join(m.Channels, ","),
		"--triggers", m.Channels[0] + "=r",
		"--samples", strconv.Itoa(samples),
		"-o", srPath,
	}
	if out, err := runCmd(ctx, b.cfg.SigrokCLI, args...); err != nil {
		return fmt.Errorf("sigrok capture: %w: %s", err, out)
	}
	return nil
}

// decodeSR runs the protocol decoder over a captured .sr and parses the pixels.
func (b *Broker) decodeSR(ctx context.Context, srPath string, m UnitMap) ([]Pixel, error) {
	dargs, err := decoderArgs(m.Protocol, m.Channels)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-i", srPath}, dargs...)
	out, err := runCmd(ctx, b.cfg.SigrokCLI, args...)
	if err != nil {
		return nil, fmt.Errorf("sigrok decode: %w: %s", err, out)
	}
	return parseRGBHex(out)
}

// decodeSRBytes runs the spi decoder and returns the raw MOSI byte stream.
func (b *Broker) decodeSRBytes(ctx context.Context, srPath string, m UnitMap) ([]byte, error) {
	dargs, err := decoderArgs(m.Protocol, m.Channels)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-i", srPath}, dargs...)
	out, err := runCmd(ctx, b.cfg.SigrokCLI, args...)
	if err != nil {
		return nil, fmt.Errorf("sigrok decode: %w: %s", err, out)
	}
	return parseSPIBytes(out)
}

func runCmd(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}

// --- map persistence ------------------------------------------------------

func loadMapFile(path string) (map[string]UnitMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]UnitMap{}, nil
		}
		return nil, err
	}
	var m map[string]UnitMap
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	// Normalize the "default" alias to "".
	if v, ok := m["default"]; ok {
		m[""] = v
		delete(m, "default")
	}
	return m, nil
}

func saveMapFile(path string, m map[string]UnitMap) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- helpers --------------------------------------------------------------

func jsonErr(msg string, status int) ([]byte, int, error) {
	b, _ := json.Marshal(api.Error{Error: msg})
	return b, status, nil
}

func okJSON(v any) ([]byte, int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, 500, err
	}
	return b, 200, nil
}

func parseSampleRate(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimSuffix(s, "hz")
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1_000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "g"):
		mult, s = 1_000_000_000, strings.TrimSuffix(s, "g")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int(f * float64(mult))
}
