package analyzer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The "timing" op: stream a long untriggered capture of one WS2812 line through
// sigrok-cli's raw binary output, run-length it in-process (EdgeExtractor) and
// segment it into frames (TimingAnalyzer). Unlike "capture" (one triggered frame,
// buffered to a .sr and decoded by sigrok's rgb_led_ws281x), nothing is buffered
// per sample — memory scales with frames/edges kept, so multi-second windows work.
//
//	{"op":"timing","unit":"c6-a","duration_ms":15000,"samplerate":"12m",
//	 "reset_us":50,"gap_threshold_us":20,"include_edges":false,
//	 "include_frames":true,"anomalies_only":false,"include_pixels":false}

// TimingOptions are the timing-op request fields (alongside CaptureRequest's unit/samples).
type TimingOptions struct {
	DurationMs     int     `json:"duration_ms,omitempty"`      // capture window (default 1000; capped at Config.TimingMaxMs). Ignored if samples is set.
	SampleRate     string  `json:"samplerate,omitempty"`       // sigrok samplerate, e.g. "12m" (default Config.TimingSampleRate)
	ResetUs        float64 `json:"reset_us,omitempty"`         // LOW >= this ends a frame (default 50)
	GapThresholdUs float64 `json:"gap_threshold_us,omitempty"` // report intra-frame LOWs >= this (default 20)
	SplitGapUs     float64 `json:"split_gap_us,omitempty"`     // flag frames preceded by a LOW shorter than this (default 1000)
	BitThresholdNs float64 `json:"bit_threshold_ns,omitempty"` // 0/1 HIGH split; 0 = adaptive
	IncludeFrames  *bool   `json:"include_frames,omitempty"`   // list frames (default true)
	AnomaliesOnly  bool    `json:"anomalies_only,omitempty"`   // list only flagged frames
	MaxFrames      int     `json:"max_frames,omitempty"`       // cap on listed frames (default 20000)
	IncludePixels  bool    `json:"include_pixels,omitempty"`   // decode pixels for the first max_pixel_frames frames
	MaxPixelFrames int     `json:"max_pixel_frames,omitempty"` // default 64
	IncludeEdges   bool    `json:"include_edges,omitempty"`    // raw run-length stream
	EdgesFormat    string  `json:"edges_format,omitempty"`     // "uvarint" (default, base64) | "json"
	MaxEdges       int     `json:"max_edges,omitempty"`        // default 2,000,000 runs
}

// TimingCapture is the capture metadata, including the drop/overflow verdict.
type TimingCapture struct {
	Channel          string   `json:"channel"`
	SampleRate       int      `json:"sample_rate"`
	RequestedSamples int64    `json:"requested_samples"`
	Samples          int64    `json:"samples"`              // samples actually received
	DurationMs       float64  `json:"duration_ms"`          // samples / sample_rate
	WallMs           float64  `json:"wall_ms"`              // sigrok-cli start → exit
	StreamMs         float64  `json:"stream_ms"`            // first byte → last byte
	ShortCapture     bool     `json:"short_capture"`        // fewer samples than requested (device stopped/aborted)
	DropSuspected    bool     `json:"drop_suspected"`       // see Notes
	Notes            []string `json:"notes,omitempty"`      // why drop_suspected, and other caveats
	SigrokLog        string   `json:"sigrok_log,omitempty"` // tail of sigrok-cli stderr, when non-empty
}

// TimingResult is the timing op's response.
type TimingResult struct {
	Unit            string         `json:"unit"`
	Protocol        string         `json:"protocol"` // "ws2812"
	Capture         TimingCapture  `json:"capture"`
	Summary         TimingSummary  `json:"summary"`
	Frames          []*TimingFrame `json:"frames,omitempty"`
	FramesListed    int            `json:"frames_listed"`
	FramesTruncated bool           `json:"frames_truncated,omitempty"` // hit max_frames
	Edges           *TimingEdges   `json:"edges,omitempty"`
}

// timing runs one timing capture for a unit.
func (b *Broker) timing(ctx context.Context, req CaptureRequest, o TimingOptions) (*TimingResult, error) {
	m := b.mapping(req.Unit)
	if p := Protocol(req.Protocol); p != "" {
		m.Protocol = p
	}
	if m.Protocol != "" && m.Protocol != ProtocolWS2812 {
		return nil, fmt.Errorf("timing op supports ws2812 lines only (unit is %s)", m.Protocol)
	}
	if len(m.Channels) == 0 {
		return nil, errors.New("no analyzer channels mapped for this unit")
	}
	ch := m.Channels[0]
	chIdx, err := channelIndex(ch)
	if err != nil {
		return nil, err
	}
	rateStr := o.SampleRate
	if rateStr == "" {
		rateStr = b.cfg.TimingSampleRate
	}
	rate := parseSampleRate(rateStr)
	if rate <= 0 {
		return nil, fmt.Errorf("bad samplerate %q", rateStr)
	}
	maxSamples := int64(b.cfg.TimingMaxMs) * int64(rate) / 1000
	samples := int64(req.Samples)
	if samples <= 0 {
		ms := o.DurationMs
		if ms <= 0 {
			ms = 1000
		}
		samples = int64(ms) * int64(rate) / 1000
	}
	if samples > maxSamples {
		return nil, fmt.Errorf("capture of %d samples (%.0f ms @ %s) exceeds this rig's timing cap of %d ms",
			samples, float64(samples)*1000/float64(rate), rateStr, b.cfg.TimingMaxMs)
	}

	p := TimingParams{
		SampleRateHz:   rate,
		ResetUs:        o.ResetUs,
		GapThresholdUs: o.GapThresholdUs,
		SplitGapUs:     o.SplitGapUs,
		BitThresholdNs: o.BitThresholdNs,
		IncludePixels:  o.IncludePixels,
		MaxPixelFrames: o.MaxPixelFrames,
		IncludeEdges:   o.IncludeEdges,
		EdgesFormat:    o.EdgesFormat,
		MaxEdges:       o.MaxEdges,
	}
	if p.EdgesFormat != "" && p.EdgesFormat != "uvarint" && p.EdgesFormat != "json" {
		return nil, fmt.Errorf("edges_format must be uvarint or json, got %q", p.EdgesFormat)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Single channel: fx2lafw runs 8-bit (unitsize 1) for D0-D7 and 16-bit for D8+.
	// The binary output is the raw logic payload, bit i = channel Di.
	unitsize := 1
	if chIdx >= 8 {
		unitsize = 2
	}
	args := []string{
		"--driver", b.cfg.Driver,
		"--config", "samplerate=" + rateStr,
		"--channels", ch,
		"--samples", strconv.FormatInt(samples, 10),
		"-O", "binary",
	}
	an := NewTimingAnalyzer(p)
	ex := NewEdgeExtractor(unitsize, chIdx, an.Run)
	st, err := streamCmd(ctx, b.cfg.SigrokCLI, args, ex)
	if err != nil {
		return nil, err
	}
	got := ex.Close()
	res := an.Finish()

	c := TimingCapture{
		Channel:          ch,
		SampleRate:       rate,
		RequestedSamples: samples,
		Samples:          got,
		DurationMs:       round3(float64(got) * 1000 / float64(rate)),
		WallMs:           round3(st.wall.Seconds() * 1000),
		StreamMs:         round3(st.stream.Seconds() * 1000),
		SigrokLog:        st.stderrTail,
	}
	judgeDrops(&c, st.stderrTail, res.Summary.Bit)
	if got == 0 {
		return nil, fmt.Errorf("no samples received from sigrok-cli on %s (%s)", ch, strings.TrimSpace(st.stderrTail))
	}

	out := &TimingResult{Unit: req.Unit, Protocol: string(ProtocolWS2812), Capture: c, Summary: res.Summary, Edges: res.Edges}
	if o.IncludeFrames == nil || *o.IncludeFrames {
		maxF := o.MaxFrames
		if maxF <= 0 {
			maxF = 20000
		}
		out.Frames = []*TimingFrame{}
		for _, f := range res.Frames {
			if o.AnomaliesOnly && len(f.Flags) == 0 {
				continue
			}
			if len(out.Frames) >= maxF {
				out.FramesTruncated = true
				break
			}
			out.Frames = append(out.Frames, f)
		}
		out.FramesListed = len(out.Frames)
	}
	return out, nil
}

// judgeDrops fills ShortCapture / DropSuspected / Notes.
//
// What fx2lafw does on overrun: the FX2's GPIF fills a few-KB FIFO; if the host
// doesn't drain it in time the firmware stalls and libsigrok aborts the acquisition
// (a USB transfer error, or too many empty transfers) — sigrok-cli then exits
// having delivered FEWER samples than asked. It does not splice silently, so
// "short capture" is the primary, reliable drop signal. Two secondary heuristics
// catch anything subtler: the stream taking noticeably longer than the data it
// carried (the device fell behind real time), and impossible WS2812 HIGH widths
// (a splice inside a bit).
func judgeDrops(c *TimingCapture, stderr string, bit BitTiming) {
	if c.Samples < c.RequestedSamples {
		c.ShortCapture, c.DropSuspected = true, true
		c.Notes = append(c.Notes, fmt.Sprintf("received %d of %d samples: acquisition stopped early (USB overrun/abort?) — lower samplerate or duration",
			c.Samples, c.RequestedSamples))
	}
	if c.DurationMs >= 500 && c.StreamMs > c.DurationMs*1.15+250 {
		c.DropSuspected = true
		c.Notes = append(c.Notes, fmt.Sprintf("stream took %.0f ms for %.0f ms of data: the device fell behind real time", c.StreamMs, c.DurationMs))
	}
	low := strings.ToLower(stderr)
	for _, k := range []string{"overflow", "overrun", "missing", "transfer", "timed out", "timeout"} {
		if strings.Contains(low, k) {
			c.DropSuspected = true
			c.Notes = append(c.Notes, "sigrok-cli reported "+strconv.Quote(k)+" (see sigrok_log)")
			break
		}
	}
	if bit.ShortHighs+bit.LongHighs > 0 {
		c.Notes = append(c.Notes, fmt.Sprintf("%d HIGH pulses outside 150ns-2us (glitches, a splice, or not a WS2812 line): see frames flagged bad_bit",
			bit.ShortHighs+bit.LongHighs))
	}
}

// channelIndex parses a sigrok logic channel name "D<n>" to n.
func channelIndex(ch string) (int, error) {
	s := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(ch)), "D")
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 15 {
		return 0, fmt.Errorf("timing op needs a D0-D15 channel, got %q", ch)
	}
	return n, nil
}

type streamStats struct {
	wall, stream time.Duration
	stderrTail   string
}

// streamCmd runs bin with args, copying its stdout into w as it arrives, and
// returns timing + the tail of stderr.
func streamCmd(ctx context.Context, bin string, args []string, w io.Writer) (streamStats, error) {
	var st streamStats
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return st, err
	}
	tail := &tailBuf{max: 4096}
	cmd.Stderr = tail
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return st, fmt.Errorf("sigrok-cli: %w", err)
	}
	r := bufio.NewReaderSize(stdout, 1<<20)
	buf := make([]byte, 1<<20)
	var first, last time.Time
	var rerr error
	for {
		n, err := r.Read(buf)
		if n > 0 {
			now := time.Now()
			if first.IsZero() {
				first = now
			}
			last = now
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
	}
	werr := cmd.Wait()
	st.wall = time.Since(t0)
	if !first.IsZero() {
		st.stream = last.Sub(first)
	}
	st.stderrTail = tail.String()
	if ctx.Err() != nil {
		return st, fmt.Errorf("timing capture cancelled: %w", ctx.Err())
	}
	if werr != nil {
		return st, fmt.Errorf("sigrok capture: %w: %s", werr, strings.TrimSpace(st.stderrTail))
	}
	if rerr != nil {
		return st, fmt.Errorf("reading sigrok output: %w", rerr)
	}
	return st, nil
}

// tailBuf keeps the last max bytes written.
type tailBuf struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
