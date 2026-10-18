package analyzer

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Synthetic WS2812 waveforms at 12 MHz (83.3ns/sample):
//
//	0 bit: HIGH 4 (333ns) + LOW 11 (917ns)
//	1 bit: HIGH 9 (750ns) + LOW 6  (500ns)      period 15 samples = 1.25us
const (
	tRate = 12_000_000
	t0h   = 4
	t0l   = 11
	t1h   = 9
	t1l   = 6
)

func samplesUs(us float64) int64 { return int64(math.Round(us * tRate / 1e6)) }

// wave builds an alternating run list, starting LOW unless the first call is high.
type wave struct {
	startHigh bool
	runs      []int64
	level     bool
}

func (w *wave) push(level bool, n int64) *wave {
	if len(w.runs) == 0 {
		w.startHigh, w.level = level, level
		w.runs = []int64{n}
		return w
	}
	if level == w.level {
		w.runs[len(w.runs)-1] += n
		return w
	}
	w.level = level
	w.runs = append(w.runs, n)
	return w
}
func (w *wave) low(n int64) *wave  { return w.push(false, n) }
func (w *wave) high(n int64) *wave { return w.push(true, n) }
func (w *wave) lowUs(us float64) *wave {
	return w.low(samplesUs(us))
}

func (w *wave) bit(one bool) *wave {
	if one {
		return w.high(t1h).low(t1l)
	}
	return w.high(t0h).low(t0l)
}

// pixel sends one LED in GRB wire order, MSB first.
func (w *wave) pixel(p Pixel) *wave {
	v := uint32(p.G)<<16 | uint32(p.R)<<8 | uint32(p.B)
	for i := 23; i >= 0; i-- {
		w.bit(v>>uint(i)&1 == 1)
	}
	return w
}

// frame sends n LEDs of a recognizable pattern (LED i = {i, 255-i, 0x5a}).
func (w *wave) frame(n int) *wave {
	for i := 0; i < n; i++ {
		w.pixel(patternPixel(i))
	}
	return w
}

func patternPixel(i int) Pixel { return Pixel{R: uint8(i), G: uint8(255 - i), B: 0x5a} }

func (w *wave) analyze(p TimingParams) *TimingAnalysis {
	if p.SampleRateHz == 0 {
		p.SampleRateHz = tRate
	}
	return AnalyzeTiming(w.startHigh, w.runs, p)
}

func flagsOf(fs []*TimingFrame) [][]string {
	var out [][]string
	for _, f := range fs {
		out = append(out, f.Flags)
	}
	return out
}

func ledsOf(fs []*TimingFrame) []int {
	var out []int
	for _, f := range fs {
		out = append(out, f.LEDs)
	}
	return out
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestTimingSegmentation(t *testing.T) {
	const gapUs = 1000.0
	// ~1.25us/bit, 24 bits/LED.
	tests := []struct {
		name   string
		build  func() *wave
		params TimingParams
		leds   []int
		flags  [][]string
		check  func(t *testing.T, a *TimingAnalysis)
	}{
		{
			name: "normal frames",
			build: func() *wave {
				w := (&wave{}).lowUs(100)
				for i := 0; i < 3; i++ {
					w.frame(10).lowUs(gapUs)
				}
				return w
			},
			leds:  []int{10, 10, 10},
			flags: [][]string{nil, nil, nil},
			check: func(t *testing.T, a *TimingAnalysis) {
				s := a.Summary
				if s.Frames != 3 || s.CompleteFrames != 3 || s.ModalLEDs != 10 || s.Runts != 0 {
					t.Fatalf("summary: %+v", s)
				}
				if !reflect.DeepEqual(s.LEDHistogram, map[int]int{10: 3}) {
					t.Fatalf("histogram %v", s.LEDHistogram)
				}
				f := a.Frames[1]
				if f.Bits != 240 || f.TrailingBits != 0 || f.GapBeforeUs == nil || !near(*f.GapBeforeUs, gapUs+float64(t0l)/12, 0.01) {
					t.Fatalf("frame 1: %+v gap=%v", f, f.GapBeforeUs)
				}
				if a.Frames[0].GapBeforeUs != nil {
					t.Fatalf("leading LOW length is unknown; gap_before should be null, got %v", *a.Frames[0].GapBeforeUs)
				}
				// Duration: first rise → last fall. The pattern's bits vary so check
				// it against period − gap_before of the next frame.
				if !near(*a.Frames[1].PeriodUs, a.Frames[0].DurationUs+*a.Frames[1].GapBeforeUs, 0.01) {
					t.Fatalf("period %v != duration %v + gap %v", *a.Frames[1].PeriodUs, a.Frames[0].DurationUs, *a.Frames[1].GapBeforeUs)
				}
				b := s.Bit
				if !near(b.T0HNs.Median, 333.333, 0.01) || !near(b.T1HNs.Median, 750, 0.01) || !near(b.BitPeriodNs.Median, 1250, 0.01) {
					t.Fatalf("bit timing: %+v", b)
				}
				if b.ThresholdNs <= 333.4 || b.ThresholdNs > 750 || !b.Adaptive {
					t.Fatalf("threshold %v", b.ThresholdNs)
				}
				if s.MaxIntraGapUs > 1 || s.FramesWithGaps != 0 {
					t.Fatalf("no intra gaps expected: %+v", s)
				}
			},
		},
		{
			name: "intra-frame gap below reset stays one frame",
			build: func() *wave {
				w := (&wave{}).lowUs(100).frame(10).lowUs(gapUs)
				for i := 0; i < 101; i++ { // bit 100 then stall 30us
					w.bit(i%2 == 0)
				}
				w.lowUs(30)
				for i := 101; i < 240; i++ {
					w.bit(i%2 == 0)
				}
				return w.lowUs(gapUs).frame(10).lowUs(gapUs)
			},
			leds:  []int{10, 10, 10},
			flags: [][]string{nil, {FlagIntraGap}, nil},
			check: func(t *testing.T, a *TimingAnalysis) {
				f := a.Frames[1]
				if f.GapCount != 1 || len(f.Gaps) != 1 {
					t.Fatalf("gaps: %+v", f.Gaps)
				}
				g := f.Gaps[0]
				// The stall LOW = the bit's own LOW (500ns for a 1 at bit 100) + 30us.
				if g.Bit != 101 || !near(g.LenUs, 30.5, 0.01) || !near(f.MaxIntraGapUs, 30.5, 0.01) {
					t.Fatalf("gap %+v max=%v", g, f.MaxIntraGapUs)
				}
				if !near(g.AtUs, 100*1.25+0.75, 0.01) { // 100 full bits + bit 100's HIGH
					t.Fatalf("gap at %v", g.AtUs)
				}
				if a.Summary.FramesWithGaps != 1 || !near(a.Summary.MaxIntraGapUs, 30.5, 0.01) {
					t.Fatalf("summary %+v", a.Summary)
				}
			},
		},
		{
			name: "gap above reset splits a frame into two runts",
			build: func() *wave {
				w := (&wave{}).lowUs(100).frame(10).lowUs(gapUs).frame(10).lowUs(gapUs)
				w.frame(5).lowUs(100).frame(5).lowUs(gapUs)
				return w.frame(10).lowUs(gapUs).frame(10).lowUs(gapUs)
			},
			leds:  []int{10, 10, 5, 5, 10, 10},
			flags: [][]string{nil, nil, {FlagRunt}, {FlagRunt, FlagShortGapBefore}, nil, nil},
			check: func(t *testing.T, a *TimingAnalysis) {
				s := a.Summary
				if s.Runts != 2 || s.ShortGapBefore != 1 || s.ModalLEDs != 10 {
					t.Fatalf("summary %+v", s)
				}
				if g := *a.Frames[3].GapBeforeUs; !near(g, 100+float64(t0l)/12, 0.01) {
					t.Fatalf("split gap %v", g)
				}
			},
		},
		{
			name: "reset_us=280 keeps the same 100us stall as an intra gap",
			build: func() *wave {
				w := (&wave{}).lowUs(400).frame(10).lowUs(gapUs)
				return w.frame(5).lowUs(100).frame(5).lowUs(gapUs).frame(10).lowUs(gapUs)
			},
			params: TimingParams{ResetUs: 280},
			leds:   []int{10, 10, 10},
			flags:  [][]string{nil, {FlagIntraGap}, nil},
		},
		{
			name: "runt frame",
			build: func() *wave {
				w := (&wave{}).lowUs(100).frame(10).lowUs(gapUs).frame(10).lowUs(gapUs)
				return w.frame(7).lowUs(gapUs).frame(10).lowUs(gapUs)
			},
			leds:  []int{10, 10, 7, 10},
			flags: [][]string{nil, nil, {FlagRunt}, nil},
			check: func(t *testing.T, a *TimingAnalysis) {
				if !reflect.DeepEqual(a.Summary.LEDHistogram, map[int]int{10: 3, 7: 1}) {
					t.Fatalf("histogram %v", a.Summary.LEDHistogram)
				}
			},
		},
		{
			name: "partial bits and an oversize frame",
			build: func() *wave {
				w := (&wave{}).lowUs(100).frame(10).lowUs(gapUs).frame(10).lowUs(gapUs)
				w.frame(10).bit(true).bit(false).bit(true).lowUs(gapUs)
				return w.frame(12).lowUs(gapUs)
			},
			leds:  []int{10, 10, 10, 12},
			flags: [][]string{nil, nil, {FlagPartialBits}, {FlagOversize}},
			check: func(t *testing.T, a *TimingAnalysis) {
				if f := a.Frames[2]; f.Bits != 243 || f.TrailingBits != 3 {
					t.Fatalf("frame 2 %+v", f)
				}
			},
		},
		{
			name: "capture window cuts the first and last frames",
			build: func() *wave {
				w := (&wave{}).high(t1h).low(t1l) // joined mid-frame
				for i := 0; i < 50; i++ {
					w.bit(false)
				}
				w.lowUs(gapUs).frame(10).lowUs(gapUs).frame(10).lowUs(gapUs)
				return w.frame(3) // ends inside a frame (its last LOW is short)
			},
			leds:  []int{2, 10, 10, 3},
			flags: [][]string{{FlagTruncated}, nil, nil, {FlagTruncated}},
			check: func(t *testing.T, a *TimingAnalysis) {
				if a.Summary.CompleteFrames != 2 || !reflect.DeepEqual(a.Summary.LEDHistogram, map[int]int{10: 2}) {
					t.Fatalf("truncated frames must not count: %+v", a.Summary)
				}
			},
		},
		{
			name: "capture opens inside a short LOW",
			build: func() *wave {
				return (&wave{}).lowUs(10).frame(10).lowUs(gapUs).frame(10).lowUs(gapUs).frame(10).high(t1h)
			},
			leds:  []int{10, 10, 10},
			flags: [][]string{{FlagTruncated}, nil, {FlagTruncated}},
		},
		{
			name: "glitch HIGH is a bad bit",
			build: func() *wave {
				w := (&wave{}).lowUs(100).frame(10).lowUs(gapUs)
				w.frame(5).high(samplesUs(3)).low(t0l) // 1 glitch bit
				for i := 0; i < 23; i++ {
					w.bit(false)
				}
				return w.frame(4).lowUs(gapUs).frame(10).lowUs(gapUs)
			},
			leds:  []int{10, 10, 10},
			flags: [][]string{nil, {FlagBadBit}, nil},
			check: func(t *testing.T, a *TimingAnalysis) {
				if a.Summary.Bit.LongHighs != 1 || a.Frames[1].BadBits != 1 {
					t.Fatalf("bit %+v", a.Summary.Bit)
				}
			},
		},
		{
			name:  "idle line",
			build: func() *wave { return (&wave{}).lowUs(5000) },
			leds:  nil,
			flags: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.build().analyze(tc.params)
			if got := ledsOf(a.Frames); !reflect.DeepEqual(got, tc.leds) {
				t.Fatalf("leds = %v, want %v", got, tc.leds)
			}
			if got := flagsOf(a.Frames); !reflect.DeepEqual(got, tc.flags) {
				t.Fatalf("flags = %v, want %v", got, tc.flags)
			}
			if tc.check != nil {
				tc.check(t, a)
			}
		})
	}
}

func TestTimingPixels(t *testing.T) {
	w := (&wave{}).lowUs(100).frame(250).lowUs(1000).frame(250).lowUs(1000)
	a := w.analyze(TimingParams{IncludePixels: true, MaxPixelFrames: 1})
	if len(a.Frames) != 2 || len(a.Frames[0].Pixels) != 250 || a.Frames[1].Pixels != nil {
		t.Fatalf("pixels only for the first frame: %d %d", len(a.Frames[0].Pixels), len(a.Frames[1].Pixels))
	}
	for i, p := range a.Frames[0].Pixels {
		if p != patternPixel(i) {
			t.Fatalf("pixel %d = %+v want %+v", i, p, patternPixel(i))
		}
	}
	// A fixed threshold decodes identically.
	b := w.analyze(TimingParams{IncludePixels: true, BitThresholdNs: 550})
	if !reflect.DeepEqual(a.Frames[0].Pixels, b.Frames[0].Pixels) || b.Summary.Bit.Adaptive {
		t.Fatal("fixed 550ns threshold should decode the same pixels")
	}
	// An all-zero (black) frame has one HIGH cluster: no bit may decode as 1.
	z := &wave{}
	z.lowUs(100)
	for i := 0; i < 24*4; i++ {
		z.bit(false)
	}
	z.lowUs(1000)
	za := z.analyze(TimingParams{IncludePixels: true})
	for _, p := range za.Frames[0].Pixels {
		if p != (Pixel{}) {
			t.Fatalf("black frame decoded %+v (threshold %v)", p, za.Summary.Bit.ThresholdNs)
		}
	}
}

func TestTimingEdges(t *testing.T) {
	w := (&wave{}).lowUs(100).frame(2).lowUs(1000)
	a := w.analyze(TimingParams{IncludeEdges: true})
	e := a.Edges
	if e == nil || e.Encoding != "uvarint" || e.StartLevel != 0 || e.Truncated || e.Count != len(w.runs) {
		t.Fatalf("edges %+v (want %d runs)", e, len(w.runs))
	}
	runs, err := DecodeUvarintRuns(e.Data)
	if err != nil || !reflect.DeepEqual(runs, w.runs) {
		t.Fatalf("round trip: %v %v", err, runs)
	}
	j := w.analyze(TimingParams{IncludeEdges: true, EdgesFormat: "json", MaxEdges: 5})
	if !j.Edges.Truncated || !reflect.DeepEqual(j.Edges.Runs, w.runs[:5]) || j.Edges.Count != 5 {
		t.Fatalf("json/truncated edges %+v", j.Edges)
	}
	// Truncating the edge list must not affect the analysis.
	if j.Summary.Frames != 1 || j.Frames[0].LEDs != 2 {
		t.Fatalf("analysis changed by edge cap: %+v", j.Summary)
	}
}

func TestEdgeExtractor(t *testing.T) {
	type run struct {
		l bool
		n int64
	}
	collect := func(unitsize, ch int, chunks ...[]byte) ([]run, int64) {
		var got []run
		e := NewEdgeExtractor(unitsize, ch, func(l bool, n int64) { got = append(got, run{l, n}) })
		for _, c := range chunks {
			e.Write(c)
		}
		return got, e.Close()
	}
	// Channel D2 (mask 0x04); other bits are noise that must be ignored.
	data := []byte{0x00, 0x01, 0x04, 0x05, 0xff, 0x02, 0x00, 0x04}
	got, n := collect(1, 2, data[:3], data[3:])
	want := []run{{false, 2}, {true, 3}, {false, 2}, {true, 1}}
	if !reflect.DeepEqual(got, want) || n != 8 {
		t.Fatalf("unitsize 1: %v n=%d", got, n)
	}
	// 16-bit samples, channel D9 = byte 1 bit 1; chunks split mid-sample.
	d16 := []byte{0xff, 0x00, 0x00, 0x02, 0x00, 0x02, 0xff, 0xfd, 0x00}
	got, n = collect(2, 9, d16[:1], d16[1:4], d16[4:])
	want = []run{{false, 1}, {true, 2}, {false, 1}}
	if !reflect.DeepEqual(got, want) || n != 4 {
		t.Fatalf("unitsize 2: %v n=%d", got, n)
	}
}

func TestHistStat(t *testing.T) {
	var h hist
	for _, v := range []int64{5, 1, 3, 3, 9, 100000} {
		h.add(v)
	}
	s := h.stat(0, math.MaxInt64, 1)
	if s.N != 6 || s.Min != 1 || s.Median != 3 || s.Max != 100000 || s.P99 != 100000 {
		t.Fatalf("stat %+v", s)
	}
	if s := h.stat(4, 50, 2); s.N != 2 || s.Min != 10 || s.Max != 18 {
		t.Fatalf("range stat %+v", s)
	}
}

// TestAccessTimingOp drives the whole op through a fake sigrok-cli that emits a
// synthetic binary stream, checking args, the JSON shape, and drop detection.
func TestAccessTimingOp(t *testing.T) {
	dir := t.TempDir()
	w := (&wave{}).lowUs(100).frame(10).lowUs(1000).frame(10).lowUs(1000)
	var raw []byte
	lvl := w.startHigh
	for _, n := range w.runs {
		b := byte(0x10) // noise on D4
		if lvl {
			b |= 0x08 // the line is on D3
		}
		for i := int64(0); i < n; i++ {
			raw = append(raw, b)
		}
		lvl = !lvl
	}
	dataPath := filepath.Join(dir, "stream.bin")
	argsPath := filepath.Join(dir, "args")
	if err := os.WriteFile(dataPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "sigrok-cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+argsPath+"\necho 'fx2lafw: something' >&2\nexec cat "+dataPath+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := New(Config{Driver: "fx2lafw", SigrokCLI: script, Map: map[string]UnitMap{"c6-a": {Channels: []string{"D3"}}}})

	body := `{"op":"timing","samples":` + itoa(len(raw)) + `,"include_edges":true,"anomalies_only":false}`
	resp, status, err := b.Access(context.Background(), "c6-a", []byte(body))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v resp=%s", status, err, resp)
	}
	var res TimingResult
	if err := json.Unmarshal(resp, &res); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsPath)
	for _, want := range []string{"--driver fx2lafw", "samplerate=12m", "--channels D3", "--samples " + itoa(len(raw)), "-O binary"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("sigrok args %q missing %q", args, want)
		}
	}
	c := res.Capture
	if res.Unit != "c6-a" || c.Samples != int64(len(raw)) || c.ShortCapture || c.DropSuspected || c.SampleRate != tRate {
		t.Fatalf("capture %+v", c)
	}
	if !strings.Contains(c.SigrokLog, "fx2lafw: something") {
		t.Fatalf("sigrok log %q", c.SigrokLog)
	}
	if res.Summary.Frames != 2 || res.FramesListed != 2 || res.Frames[0].LEDs != 10 || res.Edges == nil {
		t.Fatalf("result %s", resp)
	}
	// Generic JSON shape the CLI/harness consumes.
	var generic map[string]any
	_ = json.Unmarshal(resp, &generic)
	for _, k := range []string{"capture", "summary", "frames", "edges"} {
		if _, ok := generic[k]; !ok {
			t.Fatalf("missing %q in %s", k, resp)
		}
	}
	if _, ok := generic["summary"].(map[string]any)["led_histogram"].(map[string]any)["10"]; !ok {
		t.Fatalf("led_histogram keyed by LED count: %s", resp)
	}

	// Asking for more than the device delivers = a short (dropped) capture.
	resp, status, _ = b.Access(context.Background(), "c6-a", []byte(`{"op":"timing","samples":`+itoa(len(raw)*2)+`,"include_frames":false}`))
	if status != 200 {
		t.Fatalf("status %d %s", status, resp)
	}
	res = TimingResult{}
	_ = json.Unmarshal(resp, &res)
	if !res.Capture.ShortCapture || !res.Capture.DropSuspected || len(res.Capture.Notes) == 0 || res.Frames != nil {
		t.Fatalf("short capture not flagged: %s", resp)
	}

	// Over the cap is refused before touching the device.
	_, status, _ = b.Access(context.Background(), "c6-a", []byte(`{"op":"timing","duration_ms":600000}`))
	if status != 500 {
		t.Fatalf("over-cap request should fail, got %d", status)
	}
	// Non-ws2812 units are refused.
	b.Bind("pi-1", json.RawMessage(`{"channels":["D0","D1"],"protocol":"spi"}`))
	if _, status, _ = b.Access(context.Background(), "pi-1", []byte(`{"op":"timing"}`)); status != 500 {
		t.Fatalf("spi timing should fail, got %d", status)
	}
}

// The original capture request body must still decode to the same fields.
func TestCaptureRequestBackCompat(t *testing.T) {
	var req accessRequest
	if err := json.Unmarshal([]byte(`{"op":"capture","unit":"c6-a","samples":123,"save_sr":true,"protocol":"ws2812"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Op != "capture" || req.Unit != "c6-a" || req.Samples != 123 || !req.SaveSR || req.Protocol != "ws2812" {
		t.Fatalf("decoded %+v", req)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
