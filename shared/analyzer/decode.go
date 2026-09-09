package analyzer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Protocol is a wire protocol the analyzer can decode. WS2812 and SPI both funnel
// through sigrok's RGB-LED decoders, which emit "#rrggbb" in logical RGB order
// regardless of wire order — so one parser handles both.
type Protocol string

const (
	// ProtocolWS2812 is a single-wire, self-clocked WS281x line (GRB on the wire;
	// the decoder normalizes it to RGB).
	ProtocolWS2812 Protocol = "ws2812"
	// ProtocolSPI is APA102/SK9822-style RGB clocked over SPI (clk + data).
	ProtocolSPI Protocol = "spi"
	// ProtocolSPIRaw returns the raw MOSI byte stream (no RGB interpretation), for
	// validating SPI wire framing. Channels are [clk, mosi, cs]; cs frames.
	ProtocolSPIRaw Protocol = "spi-raw"
)

var hexByte = regexp.MustCompile(`\b([0-9a-fA-F]{2})\b`)

// parseSPIBytes turns sigrok-cli `-A spi=mosi-data` output into the MOSI byte
// stream, one byte per annotated line (the last hex-byte token on the line).
func parseSPIBytes(stdout string) ([]byte, error) {
	var out []byte
	for _, line := range strings.Split(stdout, "\n") {
		m := hexByte.FindAllStringSubmatch(line, -1)
		if len(m) == 0 {
			continue
		}
		v, err := strconv.ParseUint(m[len(m)-1][1], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("parse spi byte %q: %w", m[len(m)-1][1], err)
		}
		out = append(out, byte(v))
	}
	return out, nil
}

var rgbHex = regexp.MustCompile(`#([0-9a-fA-F]{6})`)

// parseRGBHex turns sigrok-cli "rgb" annotation output into pixels, one per line
// carrying a "#rrggbb" token, in order. Non-matching lines are ignored so it's
// robust to extra sigrok chatter.
func parseRGBHex(stdout string) ([]Pixel, error) {
	var out []Pixel
	for _, line := range strings.Split(stdout, "\n") {
		m := rgbHex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[1], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("parse pixel %q: %w", m[1], err)
		}
		out = append(out, Pixel{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)})
	}
	return out, nil
}

// decoderArgs returns the sigrok-cli protocol-decoder + annotation flags for a
// protocol given its channel assignment. WS2812 taps one channel (din); SPI taps
// two (clk, data), decoded by rgb_led_spi stacked on the spi decoder.
func decoderArgs(proto Protocol, channels []string) ([]string, error) {
	switch proto {
	case ProtocolWS2812:
		if len(channels) < 1 {
			return nil, fmt.Errorf("ws2812 needs 1 channel (din), got %d", len(channels))
		}
		return []string{
			"-P", "rgb_led_ws281x:din=" + channels[0],
			"-A", "rgb_led_ws281x=rgb",
		}, nil
	case ProtocolSPI:
		if len(channels) < 2 {
			return nil, fmt.Errorf("spi needs 2 channels (clk, data), got %d", len(channels))
		}
		return []string{
			"-P", "spi:clk=" + channels[0] + ":mosi=" + channels[1] + ":cpol=0:cpha=0",
			"-P", "rgb_led_spi",
			"-A", "rgb_led_spi=rgb",
		}, nil
	case ProtocolSPIRaw:
		if len(channels) < 2 {
			return nil, fmt.Errorf("spi-raw needs >=2 channels (clk, mosi[, cs]), got %d", len(channels))
		}
		spi := "spi:clk=" + channels[0] + ":mosi=" + channels[1] + ":cpol=0:cpha=0"
		if len(channels) >= 3 {
			spi += ":cs=" + channels[2]
		}
		return []string{"-P", spi, "-A", "spi=mosi-data"}, nil
	default:
		return nil, fmt.Errorf("unknown protocol %q", proto)
	}
}
