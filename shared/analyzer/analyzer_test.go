package analyzer

import (
	"context"
	"encoding/json"
	"testing"
)

func TestParseRGBHex(t *testing.T) {
	out := "rgb_led_ws281x-1: #ff0000\nrgb_led_ws281x-1: #00ff00\nnoise\nrgb_led_ws281x-1: #0000ff\n"
	px, err := parseRGBHex(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(px) != 3 || px[0].R != 0xff || px[1].G != 0xff || px[2].B != 0xff {
		t.Fatalf("bad decode: %+v", px)
	}
}

func TestParseSPIBytes(t *testing.T) {
	out := "spi-1: 0a\nspi-1: ff\nspi-1: 00\n"
	b, err := parseSPIBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 3 || b[0] != 0x0a || b[1] != 0xff || b[2] != 0x00 {
		t.Fatalf("bad decode: %v", b)
	}
}

func TestDecoderArgs(t *testing.T) {
	if _, err := decoderArgs(ProtocolWS2812, nil); err == nil {
		t.Fatal("ws2812 with no channels should error")
	}
	a, err := decoderArgs(ProtocolSPI, []string{"D0", "D1"})
	if err != nil {
		t.Fatal(err)
	}
	if a[1] != "spi:clk=D0:mosi=D1:cpol=0:cpha=0" {
		t.Fatalf("unexpected spi args: %v", a)
	}
}

func TestBindGrantsCaps(t *testing.T) {
	b := New(Config{Driver: "fx2lafw"}) // HardwareProbe nil => present
	if !b.Present() {
		t.Fatal("broker should be present with driver and nil probe")
	}
	caps, err := b.Bind("c6-a", json.RawMessage(`{"channels":["D0"],"protocol":"ws2812"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(caps, "logic-analyzer-led-strip") {
		t.Fatalf("ws2812 bind should grant led-strip cap: %v", caps)
	}
	spiCaps, _ := b.Bind("pi-1", json.RawMessage(`{"channels":["D0","D1"],"protocol":"spi"}`))
	if !contains(spiCaps, "logic-analyzer-spi") {
		t.Fatalf("spi bind should grant spi cap: %v", spiCaps)
	}
	if len(b.TapCaps("c6-a")) == 0 {
		t.Fatal("TapCaps should report c6-a as tapped")
	}
	if b.TapCaps("unmapped") != nil {
		t.Fatal("unmapped unit with explicit taps present should not be tapped")
	}
}

func TestAccessMapOps(t *testing.T) {
	b := New(Config{Driver: "fx2lafw"})
	// map.set then map.get round-trips.
	set := `{"op":"map.set","map":{"c6-a":{"channels":["D2"],"protocol":"ws2812"}}}`
	resp, status, err := b.Access(context.Background(), "", []byte(set))
	if err != nil || status != 200 {
		t.Fatalf("map.set: status=%d err=%v", status, err)
	}
	var m map[string]UnitMap
	if err := json.Unmarshal(resp, &m); err != nil {
		t.Fatal(err)
	}
	if len(m["c6-a"].Channels) != 1 || m["c6-a"].Channels[0] != "D2" {
		t.Fatalf("map not applied: %+v", m)
	}
}

func TestAccessDormant(t *testing.T) {
	b := New(Config{}) // no driver => dormant
	if b.Present() {
		t.Fatal("no-driver broker should be dormant")
	}
	_, status, _ := b.Access(context.Background(), "", nil)
	if status != 503 {
		t.Fatalf("dormant broker should 503, got %d", status)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
