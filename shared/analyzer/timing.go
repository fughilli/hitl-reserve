package analyzer

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"sort"
)

// This file is the PURE half of the "timing" op: WS2812 frame segmentation and
// edge/bit statistics over a run-length (level, length-in-samples) stream. It has no
// I/O — the sigrok plumbing in timing_capture.go turns a raw sample stream into runs
// (EdgeExtractor) and feeds them here — so every rule below is table-tested with
// synthetic waveforms.
//
// Model of a WS2812 line (idle low):
//
//	bit      = one HIGH pulse + the LOW that follows it (T0H/T1H, then T0L/T1L)
//	frame    = a burst of bits; it ENDS at a LOW >= ResetUs (the strip latches)
//	gap      = an intra-frame LOW >= GapThresholdUs but < ResetUs (data stalled
//	           mid-frame but not long enough to latch at the configured reset)
//
// A stall long enough to latch SPLITS one transmitted frame into two received
// frames, both with fewer LEDs than the modal count ("runt"), the second one
// preceded by an unusually short inter-frame low ("short_gap_before"). Set ResetUs
// to the strip's real latch time (WS2812B-V5: ~280us; older parts ~50us) so stalls
// below it show up as gaps and stalls above it as splits.

// TimingParams tunes segmentation. Zero values take the documented defaults.
type TimingParams struct {
	SampleRateHz int // required (> 0)

	ResetUs        float64 // LOW >= this ends a frame (default 50)
	GapThresholdUs float64 // intra-frame LOW >= this is reported as a gap (default 20)
	SplitGapUs     float64 // a frame preceded by a LOW shorter than this is flagged short_gap_before (default 1000)
	BitThresholdNs float64 // HIGH >= this decodes as a 1 bit; 0 = adaptive (Otsu over the observed HIGH widths)

	MaxGapsPerFrame int // cap on the gaps listed per frame (default 64; the count is always exact)

	IncludePixels  bool // decode pixels for the first MaxPixelFrames frames
	MaxPixelFrames int  // default 64

	IncludeEdges bool // keep the raw run-length stream (see TimingEdges)
	MaxEdges     int  // run cap (default 2,000,000); beyond it Truncated is set
	EdgesFormat  string
}

func (p *TimingParams) fill() {
	if p.ResetUs <= 0 {
		p.ResetUs = 50
	}
	if p.GapThresholdUs <= 0 {
		p.GapThresholdUs = 20
	}
	if p.GapThresholdUs > p.ResetUs {
		p.GapThresholdUs = p.ResetUs
	}
	if p.SplitGapUs <= 0 {
		p.SplitGapUs = 1000
	}
	if p.MaxGapsPerFrame <= 0 {
		p.MaxGapsPerFrame = 64
	}
	if p.MaxPixelFrames <= 0 {
		p.MaxPixelFrames = 64
	}
	if p.MaxEdges <= 0 {
		p.MaxEdges = 2_000_000 // ~2.8s of a 250-LED/60fps line (~720k runs/s)
	}
	if p.MaxEdges > maxEdgesCap {
		p.MaxEdges = maxEdgesCap
	}
	if p.EdgesFormat == "" {
		p.EdgesFormat = "uvarint"
	}
}

// maxEdgesCap bounds include_edges memory/response size (1 byte/run + base64).
const maxEdgesCap = 50_000_000

// Plausibility bounds for a WS2812 data HIGH. Anything outside is counted as a
// bad bit (a glitch, a dropped-sample splice, or not a WS2812 line at all).
const (
	shortHighNs = 150
	longHighNs  = 2000
)

// Frame flags.
const (
	FlagRunt           = "runt"             // fewer LEDs than the modal frame
	FlagOversize       = "oversize"         // more LEDs than the modal frame
	FlagPartialBits    = "partial_bits"     // bits not a multiple of 24
	FlagIntraGap       = "intra_gap"        // >=1 intra-frame LOW >= GapThresholdUs
	FlagShortGapBefore = "short_gap_before" // preceded by a LOW in [ResetUs, SplitGapUs): a likely split
	FlagBadBit         = "bad_bit"          // a HIGH outside the plausible WS2812 width
	FlagTruncated      = "truncated"        // cut by the capture window (start or end)
)

// TimingGap is one intra-frame LOW >= GapThresholdUs, relative to the frame start.
type TimingGap struct {
	AtUs  float64 `json:"at_us"`  // offset of the gap's falling edge from the frame start
	LenUs float64 `json:"len_us"` // length of the LOW
	Bit   int     `json:"bit"`    // the gap follows this many bits of the frame (bit/24 = LED index)
}

// TimingFrame is one segmented frame. Times are microseconds from the first sample.
type TimingFrame struct {
	Index         int         `json:"index"`
	Channel       string      `json:"channel,omitempty"`   // analyzer channel, e.g. "D6" (set by the timing op)
	StartSample   int64       `json:"start_sample"`        // first rising edge, in samples since the capture's first sample (one clock for every channel of a capture)
	StartUs       float64     `json:"start_us"`            // first rising edge
	DurationUs    float64     `json:"duration_us"`         // first rising edge → last falling edge
	GapBeforeUs   *float64    `json:"gap_before_us"`       // LOW preceding the frame (null when the capture started there)
	PeriodUs      *float64    `json:"period_us,omitempty"` // start − previous frame's start
	Bits          int         `json:"bits"`                // HIGH pulses
	LEDs          int         `json:"leds"`                // bits / 24
	TrailingBits  int         `json:"trailing_bits"`       // bits % 24
	MaxIntraGapUs float64     `json:"max_intra_gap_us"`    // longest LOW inside the frame (any length)
	GapCount      int         `json:"gap_count,omitempty"` // intra LOWs >= GapThresholdUs
	Gaps          []TimingGap `json:"gaps,omitempty"`      // first MaxGapsPerFrame of them
	BadBits       int         `json:"bad_bits,omitempty"`  // HIGHs outside [150ns, 2us]
	Truncated     bool        `json:"truncated,omitempty"` // cut by the capture window
	Flags         []string    `json:"flags,omitempty"`     // see Flag* constants
	Pixels        []Pixel     `json:"pixels,omitempty"`    // decoded (GRB wire → RGB), include_pixels only
	highs         []uint16    // per-bit HIGH width in samples (kept only for pixel decode)
	startSample   int64
	endSample     int64
}

// Stat is a distribution summary.
type Stat struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	P99    float64 `json:"p99"`
	Max    float64 `json:"max"`
}

// BitTiming summarizes bit-level timing in nanoseconds, for sanity-checking the
// driver against the WS2812 spec (T0H ~400, T1H ~800, period ~1250).
type BitTiming struct {
	ThresholdNs float64 `json:"threshold_ns"` // HIGH width splitting 0 from 1 bits
	Adaptive    bool    `json:"adaptive"`     // threshold derived from the data
	T0HNs       Stat    `json:"t0h_ns"`
	T1HNs       Stat    `json:"t1h_ns"`
	LowNs       Stat    `json:"low_ns"`        // intra-frame LOWs below GapThresholdUs
	BitPeriodNs Stat    `json:"bit_period_ns"` // rise→rise, within a frame, excluding gaps
	ShortHighs  int     `json:"short_highs"`   // HIGH < 150ns
	LongHighs   int     `json:"long_highs"`    // HIGH > 2us
}

// TimingSummary aggregates every frame (not just the listed ones).
type TimingSummary struct {
	Frames           int         `json:"frames"`
	CompleteFrames   int         `json:"complete_frames"` // not truncated by the capture window
	ModalLEDs        int         `json:"modal_leds"`      // most common LED count among complete frames
	LEDHistogram     map[int]int `json:"led_histogram"`   // leds → frame count (complete frames)
	Runts            int         `json:"runts"`
	Oversize         int         `json:"oversize"`
	PartialBitFrames int         `json:"partial_bit_frames"`
	FramesWithGaps   int         `json:"frames_with_gaps"` // >=1 intra LOW >= GapThresholdUs
	ShortGapBefore   int         `json:"short_gap_before"` // likely splits
	BadBitFrames     int         `json:"bad_bit_frames"`
	MaxIntraGapUs    float64     `json:"max_intra_gap_us"`
	PeriodUs         Stat        `json:"period_us"`        // frame start → next frame start
	DurationUs       Stat        `json:"duration_us"`      // complete frames
	GapBeforeUs      Stat        `json:"gap_before_us"`    // inter-frame LOW
	IntraGapMaxUs    Stat        `json:"intra_gap_max_us"` // per-frame max intra LOW (jitter distribution)
	Bit              BitTiming   `json:"bit"`
}

// TimingEdges is the raw run-length stream: runs of alternating level, starting at
// StartLevel, each a length in samples (ns = samples * 1e9 / sample_rate). Encoding
// "uvarint" packs the runs as unsigned LEB128 varints, base64 (std) encoded, in
// Data (a WS2812 run is 1 byte, so ~1.3 base64 chars/edge); "json" lists them in
// Runs. Truncated means the stream hit MaxEdges and later runs are missing.
type TimingEdges struct {
	Encoding   string  `json:"encoding"`
	Unit       string  `json:"unit"` // "samples"
	StartLevel int     `json:"start_level"`
	Count      int     `json:"count"`
	Truncated  bool    `json:"truncated"`
	Data       string  `json:"data,omitempty"`
	Runs       []int64 `json:"runs,omitempty"`
}

// TimingAnalysis is the pure analysis result.
type TimingAnalysis struct {
	Samples int64          `json:"-"`
	Summary TimingSummary  `json:"summary"`
	Frames  []*TimingFrame `json:"frames"`
	Edges   *TimingEdges   `json:"edges,omitempty"`
}

// TimingAnalyzer is the streaming segmenter. Feed every run with Run (levels must
// alternate; equal-level neighbours are merged) and call Finish once. Memory is
// bounded by the number of frames, not samples: bit stats go into histograms.
type TimingAnalyzer struct {
	p       TimingParams
	nsPer   float64
	reset   int64 // samples
	gapThr  int64
	pending bool
	pLevel  bool
	pLen    int64
	pos     int64 // sample index where the pending run starts
	nRuns   int

	cur        *TimingFrame
	startOK    bool // the next frame's start follows a LOW >= reset
	lastHigh   int64
	gapBefore  int64 // -1 = unknown (capture started in it)
	frames     []*TimingFrame
	highs      hist
	lows       hist
	periods    hist
	shortHighs int
	longHighs  int

	edges    []byte
	edgeRuns []int64
	nEdges   int
	edgeTrnc bool
	startLvl int
}

// NewTimingAnalyzer builds a streaming analyzer.
func NewTimingAnalyzer(p TimingParams) *TimingAnalyzer {
	p.fill()
	a := &TimingAnalyzer{p: p, gapBefore: -1, startLvl: -1}
	a.nsPer = 1e9 / float64(p.SampleRateHz)
	a.reset = int64(math.Ceil(p.ResetUs * 1000 / a.nsPer))
	a.gapThr = int64(math.Ceil(p.GapThresholdUs * 1000 / a.nsPer))
	return a
}

// Run appends one run of `n` samples at `level`.
func (a *TimingAnalyzer) Run(level bool, n int64) {
	if n <= 0 {
		return
	}
	if a.pending && level == a.pLevel {
		a.pLen += n
		return
	}
	if a.pending {
		a.process(a.pLevel, a.pLen, false)
	}
	a.pending, a.pLevel, a.pLen = true, level, n
}

// Finish flushes the final run (treated as cut by the capture end) and returns the
// analysis.
func (a *TimingAnalyzer) Finish() *TimingAnalysis {
	if a.pending {
		a.process(a.pLevel, a.pLen, true)
		a.pending = false
	}
	if a.cur != nil { // the capture ended inside a frame
		a.cur.Truncated = true
		a.closeFrame(a.pos)
	}
	return a.result()
}

func (a *TimingAnalyzer) process(level bool, n int64, last bool) {
	first := a.nRuns == 0
	a.nRuns++
	a.recordEdge(level, n)
	start := a.pos
	a.pos += n
	if level {
		if a.cur == nil {
			f := &TimingFrame{Index: len(a.frames), startSample: start}
			if a.gapBefore >= 0 {
				g := a.us(a.gapBefore)
				f.GapBeforeUs = &g
			}
			// Genuine start only if a LOW >= reset precedes it; otherwise the capture
			// opened mid-bit or inside a short LOW and may have joined mid-frame.
			f.Truncated = !a.startOK
			a.cur = f
		}
		f := a.cur
		f.Bits++
		a.highs.add(n)
		ns := float64(n) * a.nsPer
		if last {
			f.Truncated = true // the HIGH itself was cut; don't count it as bad
		} else if ns < shortHighNs {
			a.shortHighs++
			f.BadBits++
		} else if ns > longHighNs {
			a.longHighs++
			f.BadBits++
		}
		if a.p.IncludePixels && f.Index < a.p.MaxPixelFrames {
			f.highs = append(f.highs, sat16(n))
		}
		a.lastHigh = n
		f.endSample = a.pos
		return
	}
	// LOW
	if a.cur == nil {
		// Only the capture's first run can be a LOW outside a frame (later LOWs that
		// end a frame are consumed below). Its true length is unknown (gap_before =
		// null), but if it already spans reset the next frame's start is genuine.
		a.startOK = first && n >= a.reset
		a.gapBefore = -1
		return
	}
	if n >= a.reset {
		a.closeFrame(start)
		a.startOK, a.gapBefore = true, n
		return
	}
	if last {
		// A trailing LOW shorter than reset: can't tell an end-of-frame from a stall.
		a.cur.Truncated = true
		a.closeFrame(start)
		return
	}
	f := a.cur
	us := a.us(n)
	if us > f.MaxIntraGapUs {
		f.MaxIntraGapUs = us
	}
	if n >= a.gapThr {
		f.GapCount++
		if len(f.Gaps) < a.p.MaxGapsPerFrame {
			f.Gaps = append(f.Gaps, TimingGap{AtUs: a.us(start - f.startSample), LenUs: us, Bit: f.Bits})
		}
	} else {
		a.lows.add(n)
		a.periods.add(a.lastHigh + n)
	}
}

func (a *TimingAnalyzer) closeFrame(end int64) {
	f := a.cur
	a.cur = nil
	f.endSample = end
	a.frames = append(a.frames, f)
}

func (a *TimingAnalyzer) us(samples int64) float64 { return round3(float64(samples) * a.nsPer / 1000) }

func (a *TimingAnalyzer) recordEdge(level bool, n int64) {
	if !a.p.IncludeEdges {
		return
	}
	if a.startLvl < 0 {
		a.startLvl = b2i(level)
	}
	if a.nEdges >= a.p.MaxEdges {
		a.edgeTrnc = true
		return
	}
	a.nEdges++
	if a.p.EdgesFormat == "json" {
		a.edgeRuns = append(a.edgeRuns, n)
		return
	}
	a.edges = binary.AppendUvarint(a.edges, uint64(n))
}

func (a *TimingAnalyzer) result() *TimingAnalysis {
	res := &TimingAnalysis{Samples: a.pos, Frames: a.frames}
	s := &res.Summary
	s.Frames = len(a.frames)
	s.LEDHistogram = map[int]int{}

	// Bit threshold: fixed, or Otsu over the HIGH-width histogram.
	thr := int64(0)
	if a.p.BitThresholdNs > 0 {
		thr = int64(math.Ceil(a.p.BitThresholdNs / a.nsPer))
	} else {
		thr = a.highs.otsu()
		s.Bit.Adaptive = true
	}
	s.Bit.ThresholdNs = round3(float64(thr) * a.nsPer)
	s.Bit.T0HNs = a.highs.stat(0, thr-1, a.nsPer)
	s.Bit.T1HNs = a.highs.stat(thr, math.MaxInt64, a.nsPer)
	s.Bit.LowNs = a.lows.stat(0, math.MaxInt64, a.nsPer)
	s.Bit.BitPeriodNs = a.periods.stat(0, math.MaxInt64, a.nsPer)
	s.Bit.ShortHighs, s.Bit.LongHighs = a.shortHighs, a.longHighs

	for _, f := range a.frames {
		if !f.Truncated {
			s.CompleteFrames++
			s.LEDHistogram[f.Bits/24]++
		}
	}
	modal, best := 0, -1
	for leds, n := range s.LEDHistogram {
		if n > best || (n == best && leds > modal) {
			modal, best = leds, n
		}
	}
	s.ModalLEDs = modal

	var periods, durs, gapsBefore, intraMax []float64
	for i, f := range a.frames {
		f.LEDs, f.TrailingBits = f.Bits/24, f.Bits%24
		f.StartSample = f.startSample
		f.StartUs = a.us(f.startSample)
		f.DurationUs = a.us(f.endSample - f.startSample)
		if i > 0 {
			p := a.us(f.startSample - a.frames[i-1].startSample)
			f.PeriodUs = &p
			periods = append(periods, p)
		}
		if f.GapBeforeUs != nil {
			gapsBefore = append(gapsBefore, *f.GapBeforeUs)
		}
		if f.MaxIntraGapUs > s.MaxIntraGapUs {
			s.MaxIntraGapUs = f.MaxIntraGapUs
		}
		if f.Truncated {
			f.Flags = append(f.Flags, FlagTruncated)
		} else {
			durs = append(durs, f.DurationUs)
			intraMax = append(intraMax, f.MaxIntraGapUs)
			switch {
			case best > 0 && f.LEDs < modal:
				f.Flags = append(f.Flags, FlagRunt)
				s.Runts++
			case best > 0 && f.LEDs > modal:
				f.Flags = append(f.Flags, FlagOversize)
				s.Oversize++
			}
			if f.TrailingBits != 0 {
				f.Flags = append(f.Flags, FlagPartialBits)
				s.PartialBitFrames++
			}
		}
		if f.GapCount > 0 {
			f.Flags = append(f.Flags, FlagIntraGap)
			s.FramesWithGaps++
		}
		if f.GapBeforeUs != nil && *f.GapBeforeUs < a.p.SplitGapUs {
			f.Flags = append(f.Flags, FlagShortGapBefore)
			s.ShortGapBefore++
		}
		if f.BadBits > 0 {
			f.Flags = append(f.Flags, FlagBadBit)
			s.BadBitFrames++
		}
		if f.highs != nil {
			f.Pixels = decodePixels(f.highs, thr)
			f.highs = nil
		}
	}
	s.PeriodUs = floatStat(periods)
	s.DurationUs = floatStat(durs)
	s.GapBeforeUs = floatStat(gapsBefore)
	s.IntraGapMaxUs = floatStat(intraMax)

	if a.p.IncludeEdges {
		e := &TimingEdges{Encoding: a.p.EdgesFormat, Unit: "samples", StartLevel: a.startLvl, Count: a.nEdges, Truncated: a.edgeTrnc}
		if e.StartLevel < 0 {
			e.StartLevel = 0
		}
		if a.p.EdgesFormat == "json" {
			e.Runs = a.edgeRuns
			if e.Runs == nil {
				e.Runs = []int64{}
			}
		} else {
			e.Data = base64.StdEncoding.EncodeToString(a.edges)
		}
		res.Edges = e
	}
	return res
}

// AnalyzeTiming is the one-shot form of TimingAnalyzer over a run list that
// alternates level starting at startLevel.
func AnalyzeTiming(startLevel bool, runs []int64, p TimingParams) *TimingAnalysis {
	a := NewTimingAnalyzer(p)
	lvl := startLevel
	for _, n := range runs {
		a.Run(lvl, n)
		lvl = !lvl
	}
	return a.Finish()
}

// DecodeUvarintRuns decodes TimingEdges.Data (uvarint encoding) back to run lengths.
func DecodeUvarintRuns(data string) ([]int64, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, err
	}
	var out []int64
	for len(raw) > 0 {
		v, n := binary.Uvarint(raw)
		if n <= 0 {
			return nil, errBadVarint
		}
		out = append(out, int64(v))
		raw = raw[n:]
	}
	return out, nil
}

type timingErr string

func (e timingErr) Error() string { return string(e) }

const errBadVarint = timingErr("malformed uvarint run stream")

// decodePixels turns per-bit HIGH widths into pixels: MSB-first GRB on the wire,
// normalized to logical RGB like sigrok's rgb_led_ws281x. Trailing bits are dropped.
func decodePixels(highs []uint16, thr int64) []Pixel {
	px := make([]Pixel, 0, len(highs)/24)
	for i := 0; i+24 <= len(highs); i += 24 {
		var v uint32
		for j := 0; j < 24; j++ {
			v <<= 1
			if int64(highs[i+j]) >= thr {
				v |= 1
			}
		}
		px = append(px, Pixel{G: uint8(v >> 16), R: uint8(v >> 8), B: uint8(v)})
	}
	return px
}

// --- EdgeExtractor ------------------------------------------------------------

// EdgeExtractor turns a raw sigrok "binary" logic stream (unitsize bytes per
// sample, little-endian, bit i = channel Di) into runs of one channel, feeding a
// sink. Chunks may split samples arbitrarily.
type EdgeExtractor struct {
	unitsize int
	byteIdx  int  // which byte of the sample carries the channel
	mask     byte // bit within that byte
	sink     func(level bool, n int64)
	level    bool
	run      int64
	carry    int   // bytes of a partial sample already consumed
	samples  int64 // complete samples seen
	started  bool
	curByte  byte
}

// NewEdgeExtractor reads channel index ch (D<ch>) from samples of unitsize bytes.
func NewEdgeExtractor(unitsize, ch int, sink func(level bool, n int64)) *EdgeExtractor {
	if unitsize < 1 {
		unitsize = 1
	}
	return &EdgeExtractor{unitsize: unitsize, byteIdx: ch / 8, mask: 1 << uint(ch%8), sink: sink}
}

// Write implements io.Writer.
func (e *EdgeExtractor) Write(p []byte) (int, error) {
	n := len(p)
	if e.unitsize == 1 {
		for _, b := range p {
			e.sample(b&e.mask != 0)
		}
		return n, nil
	}
	for _, b := range p {
		if e.carry == e.byteIdx {
			e.curByte = b
		}
		e.carry++
		if e.carry == e.unitsize {
			e.carry = 0
			e.sample(e.curByte&e.mask != 0)
		}
	}
	return n, nil
}

func (e *EdgeExtractor) sample(l bool) {
	e.samples++
	if !e.started {
		e.started, e.level, e.run = true, l, 1
		return
	}
	if l == e.level {
		e.run++
		return
	}
	e.sink(e.level, e.run)
	e.level, e.run = l, 1
}

// Close flushes the final run. It returns the number of complete samples seen.
func (e *EdgeExtractor) Close() int64 {
	if e.started && e.run > 0 {
		e.sink(e.level, e.run)
		e.run = 0
	}
	return e.samples
}

// --- histograms / stats -------------------------------------------------------

// hist is an exact histogram over small non-negative sample counts; values past
// the bin cap fall into an overflow tally (with their max tracked) so a stray huge
// value can't blow memory. Medians/p99 that land in overflow report the max.
type hist struct {
	bins     []int64
	n        int64
	over     int64
	overMax  int64
	min, max int64
}

const histCap = 1 << 16

func (h *hist) add(v int64) {
	if h.n == 0 || v < h.min {
		h.min = v
	}
	if v > h.max {
		h.max = v
	}
	h.n++
	if v >= histCap {
		h.over++
		if v > h.overMax {
			h.overMax = v
		}
		return
	}
	if int(v) >= len(h.bins) {
		nb := make([]int64, int(v)+1+len(h.bins))
		if len(nb) > histCap {
			nb = nb[:histCap]
		}
		copy(nb, h.bins)
		h.bins = nb
	}
	h.bins[v]++
}

// stat summarizes the values in [lo, hi] scaled by k (samples → ns).
func (h *hist) stat(lo, hi int64, k float64) Stat {
	var n int64
	mn, mx := int64(-1), int64(-1)
	for v := lo; v < int64(len(h.bins)) && v <= hi; v++ {
		if c := h.bins[v]; c > 0 {
			n += c
			if mn < 0 {
				mn = v
			}
			mx = v
		}
	}
	overIn := hi >= histCap && h.over > 0
	if overIn {
		n += h.over
		if mn < 0 {
			mn = h.overMax
		}
		mx = h.overMax
	}
	if n == 0 {
		return Stat{}
	}
	q := func(frac float64) float64 {
		target := int64(math.Ceil(frac * float64(n)))
		if target < 1 {
			target = 1
		}
		var acc int64
		for v := lo; v < int64(len(h.bins)) && v <= hi; v++ {
			acc += h.bins[v]
			if acc >= target {
				return float64(v)
			}
		}
		return float64(h.overMax)
	}
	return Stat{N: int(n), Min: round3(float64(mn) * k), Median: round3(q(0.5) * k), P99: round3(q(0.99) * k), Max: round3(float64(mx) * k)}
}

// otsu returns the threshold t maximizing between-class variance of {v < t} vs
// {v >= t}: the split between the T0H and T1H clusters. With one cluster (all 0s or
// all 1s) it degenerates; guard by requiring the split to sit between the min and
// max, and fall back to the midpoint.
func (h *hist) otsu() int64 {
	if h.n == 0 {
		return 1
	}
	var total, sum float64
	for v, c := range h.bins {
		total += float64(c)
		sum += float64(v) * float64(c)
	}
	var wB, sumB, best float64
	bestT, bestEnd := int64(-1), int64(-1)
	for t := 1; t < len(h.bins); t++ {
		c := float64(h.bins[t-1])
		wB += c
		sumB += float64(t-1) * c
		wF := total - wB
		if wB == 0 || wF == 0 {
			continue
		}
		mB, mF := sumB/wB, (sum-sumB)/wF
		bv := wB * wF * (mB - mF) * (mB - mF)
		switch {
		case bv > best*(1+1e-9):
			best, bestT, bestEnd = bv, int64(t), int64(t)
		case bv >= best*(1-1e-9) && bestEnd == int64(t-1):
			bestEnd = int64(t) // empty bins between the clusters: a plateau
		}
	}
	if bestT >= 0 {
		bestT = (bestT + bestEnd + 1) / 2 // centre of the plateau, not its edge
	}
	// One cluster (e.g. an all-zero frame) separates poorly: if the two "classes"
	// are within 25% of each other, the data has no T1H — pick 1.5x the cluster so
	// it all decodes as 0 rather than splitting noise.
	if bestT < 0 || float64(h.max) < 1.25*float64(h.min) {
		return h.max + h.max/2 + 1
	}
	return bestT
}

func floatStat(v []float64) Stat {
	if len(v) == 0 {
		return Stat{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	at := func(frac float64) float64 {
		i := int(math.Ceil(frac*float64(len(s)))) - 1
		if i < 0 {
			i = 0
		}
		return s[i]
	}
	return Stat{N: len(s), Min: s[0], Median: at(0.5), P99: at(0.99), Max: s[len(s)-1]}
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func sat16(n int64) uint16 {
	if n > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(n)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
