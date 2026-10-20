package analyzer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// levels expands a wave into one level per sample.
func (w *wave) levels() []bool {
	var out []bool
	lvl := w.startHigh
	for _, n := range w.runs {
		for i := int64(0); i < n; i++ {
			out = append(out, lvl)
		}
		lvl = !lvl
	}
	return out
}

// interleave packs per-channel waves into one 8-bit fx2lafw sample stream (bit i =
// D<chans[i]>), padding the shorter lines with idle-low, plus noise on `noiseMask`.
func interleave(chans []int, waves []*wave, noiseMask byte) []byte {
	lv := make([][]bool, len(waves))
	n := 0
	for i, w := range waves {
		lv[i] = w.levels()
		if len(lv[i]) > n {
			n = len(lv[i])
		}
	}
	raw := make([]byte, n)
	for s := 0; s < n; s++ {
		b := noiseMask
		for i, l := range lv {
			if s < len(l) && l[s] {
				b |= 1 << uint(chans[i])
			}
		}
		raw[s] = b
	}
	return raw
}

// fakeSigrok writes a sigrok-cli stand-in that records its args and streams `raw`.
func fakeSigrok(t *testing.T, raw []byte) (script, argsPath string) {
	t.Helper()
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "stream.bin")
	argsPath = filepath.Join(dir, "args")
	if err := os.WriteFile(dataPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(dir, "sigrok-cli")
	body := "#!/bin/sh\necho \"$@\" > " + argsPath + "\nexec cat " + dataPath + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, argsPath
}

// Two WS2812 lines (D6, D7) in ONE stream: each channel is segmented on its own,
// every frame is tagged with its channel, and the frames' start_sample values are
// on the shared clock (their known offset survives exactly).
func TestTimingTwoChannels(t *testing.T) {
	// D6: frames at 100us and 1100us+; D7: same content, shifted 37us later, and
	// one extra frame so the channels are distinguishable by count.
	const shiftUs = 37
	d6 := (&wave{}).lowUs(100).frame(12).lowUs(1000).frame(12).lowUs(1000)
	d7 := (&wave{}).lowUs(100 + shiftUs).frame(12).lowUs(1000).frame(12).lowUs(1000).frame(3).lowUs(500)
	raw := interleave([]int{6, 7}, []*wave{d6, d7}, 0x01) // noise on D0
	script, argsPath := fakeSigrok(t, raw)
	b := New(Config{Driver: "fx2lafw", SigrokCLI: script, Map: map[string]UnitMap{
		"c6-a": {Channels: []string{"D6", "D7"}},
	}})

	body := `{"op":"timing","samples":` + itoa(len(raw)) + `,"include_pixels":true}`
	resp, status, err := b.Access(context.Background(), "c6-a", []byte(body))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v resp=%s", status, err, resp)
	}
	args, _ := os.ReadFile(argsPath)
	if !strings.Contains(string(args), "--channels D6,D7") {
		t.Fatalf("one acquisition of both channels expected, args %q", args)
	}
	var res TimingResult
	if err := json.Unmarshal(resp, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Channels) != 2 || res.Channels[0].Channel != "D6" || res.Channels[1].Channel != "D7" {
		t.Fatalf("channels %s", resp)
	}
	a, c := res.Channels[0], res.Channels[1]
	if a.Summary.Frames != 2 || c.Summary.Frames != 3 {
		t.Fatalf("per-channel frame counts D6=%d D7=%d", a.Summary.Frames, c.Summary.Frames)
	}
	for _, blk := range res.Channels {
		if blk.Capture.Channel != blk.Channel || blk.Capture.Samples != int64(len(raw)) {
			t.Fatalf("capture meta %+v", blk.Capture)
		}
		for _, f := range blk.Frames {
			if f.Channel != blk.Channel {
				t.Fatalf("frame %d of %s tagged %q", f.Index, blk.Channel, f.Channel)
			}
		}
	}
	// Shared sample clock: D7's frames start exactly shiftUs later.
	want := samplesUs(shiftUs)
	for i := 0; i < 2; i++ {
		if d := c.Frames[i].StartSample - a.Frames[i].StartSample; d != want {
			t.Fatalf("frame %d D7-D6 = %d samples, want %d", i, d, want)
		}
		if !near(c.Frames[i].StartUs-a.Frames[i].StartUs, shiftUs, 0.01) {
			t.Fatalf("frame %d start_us %v vs %v", i, a.Frames[i].StartUs, c.Frames[i].StartUs)
		}
	}
	// Each channel decodes its own pixels.
	if len(a.Frames[0].Pixels) != 12 || a.Frames[0].Pixels[5] != patternPixel(5) || len(c.Frames[2].Pixels) != 3 {
		t.Fatalf("pixels D6=%v D7=%v", a.Frames[0].Pixels, c.Frames[2].Pixels)
	}
	// Back-compat: the top level is exactly the primary channel's result.
	if res.Capture.Channel != "D6" || res.Summary.Frames != 2 || res.FramesListed != 2 ||
		!reflect.DeepEqual(res.Frames, a.Frames) {
		t.Fatalf("top level should mirror D6: %+v", res.Capture)
	}
	var generic map[string]any
	_ = json.Unmarshal(resp, &generic)
	fr := generic["channels"].([]any)[1].(map[string]any)["frames"].([]any)[0].(map[string]any)
	if fr["channel"] != "D7" || fr["start_sample"] == nil {
		t.Fatalf("frame JSON lacks channel/start_sample: %v", fr)
	}
}

// A request-level `channels` overrides a single-channel mapping (no map.set
// needed), and a single-channel capture keeps the old shape: no `channels` block.
func TestTimingChannelsOverrideAndSingleCompat(t *testing.T) {
	d6 := (&wave{}).lowUs(100).frame(4).lowUs(1000)
	d7 := (&wave{}).lowUs(300).frame(5).lowUs(800)
	raw := interleave([]int{6, 7}, []*wave{d6, d7}, 0)
	script, argsPath := fakeSigrok(t, raw)
	b := New(Config{Driver: "fx2lafw", SigrokCLI: script, Map: map[string]UnitMap{
		"c6-a": {Channels: []string{"D6"}},
		"c6-b": {Channels: []string{"D7"}},
	}})

	resp, status, _ := b.Access(context.Background(), "c6-a",
		[]byte(`{"op":"timing","samples":`+itoa(len(raw))+`,"channels":["d7","D6","D7"]}`))
	if status != 200 {
		t.Fatalf("status %d %s", status, resp)
	}
	args, _ := os.ReadFile(argsPath)
	if !strings.Contains(string(args), "--channels D7,D6 ") {
		t.Fatalf("override should be normalized + deduped, args %q", args)
	}
	var res TimingResult
	_ = json.Unmarshal(resp, &res)
	if len(res.Channels) != 2 || res.Capture.Channel != "D7" || res.Frames[0].LEDs != 5 || res.Channels[1].Frames[0].LEDs != 4 {
		t.Fatalf("override result %s", resp)
	}
	if got := b.Snapshot()["c6-a"].Channels; !reflect.DeepEqual(got, []string{"D6"}) {
		t.Fatalf("the rig-wide map must not change: %v", got)
	}

	resp, status, _ = b.Access(context.Background(), "c6-b", []byte(`{"op":"timing","samples":`+itoa(len(raw))+`}`))
	if status != 200 {
		t.Fatalf("status %d %s", status, resp)
	}
	var generic map[string]any
	_ = json.Unmarshal(resp, &generic)
	if _, ok := generic["channels"]; ok {
		t.Fatalf("single-channel response grew a channels block: %s", resp)
	}
	args, _ = os.ReadFile(argsPath)
	if !strings.Contains(string(args), "--channels D7 ") {
		t.Fatalf("single channel args %q", args)
	}
	res = TimingResult{}
	_ = json.Unmarshal(resp, &res)
	if res.Summary.Frames != 1 || res.Frames[0].LEDs != 5 || res.Frames[0].Channel != "D7" {
		t.Fatalf("single-channel result %s", resp)
	}

	// A bad channel in the override is refused before touching the device.
	if _, status, _ = b.Access(context.Background(), "c6-a", []byte(`{"op":"timing","channels":["D6","X9"]}`)); status != 500 {
		t.Fatalf("bad channel should fail, got %d", status)
	}
}

// The triggered capture op decodes every channel of a multi-channel ws2812 map
// from the one .sr (one rgb decoder run per din).
func TestCaptureTwoChannels(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sigrok-cli")
	// capture: write a non-empty .sr; decode: answer per din.
	body := `#!/bin/sh
case "$*" in
  *"-o "*) for a in "$@"; do out="$a"; done; echo sr > "$out" ;;
  *din=D6*) echo "rgb_led_ws281x-1: #ff0000"; echo "rgb_led_ws281x-1: #00ff00" ;;
  *din=D7*) echo "rgb_led_ws281x-1: #0000ff" ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	b := New(Config{Driver: "fx2lafw", SigrokCLI: script, Map: map[string]UnitMap{"c6-a": {Channels: []string{"D6"}}}})

	resp, status, _ := b.Access(context.Background(), "c6-a", []byte(`{"op":"capture"}`))
	if status != 200 {
		t.Fatalf("status %d %s", status, resp)
	}
	var res CaptureResult
	_ = json.Unmarshal(resp, &res)
	if len(res.Pixels) != 2 || res.Channels != nil {
		t.Fatalf("single-channel capture changed shape: %s", resp)
	}

	resp, status, _ = b.Access(context.Background(), "c6-a", []byte(`{"op":"capture","channels":["D6","D7"]}`))
	if status != 200 {
		t.Fatalf("status %d %s", status, resp)
	}
	res = CaptureResult{}
	_ = json.Unmarshal(resp, &res)
	if len(res.Pixels) != 2 || len(res.Channels) != 2 ||
		res.Channels[0].Channel != "D6" || len(res.Channels[0].Pixels) != 2 ||
		res.Channels[1].Channel != "D7" || len(res.Channels[1].Pixels) != 1 || res.Channels[1].Pixels[0].B != 0xff {
		t.Fatalf("two-channel capture %s", resp)
	}
}
