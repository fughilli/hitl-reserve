package analyzer

import "testing"

// BenchmarkTimingPipeline measures the in-process cost per sample (EdgeExtractor +
// TimingAnalyzer) on a realistic 250-LED/60fps stream, to size the samplerate a
// host CPU can keep up with alongside sigrok-cli.
func BenchmarkTimingPipeline(b *testing.B) {
	w := (&wave{}).lowUs(100)
	for i := 0; i < 6; i++ {
		w.frame(250).lowUs(16667 - 7500)
	}
	var raw []byte
	lvl := w.startHigh
	for _, n := range w.runs {
		v := byte(0)
		if lvl {
			v = 1
		}
		for i := int64(0); i < n; i++ {
			raw = append(raw, v)
		}
		lvl = !lvl
	}
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := NewTimingAnalyzer(TimingParams{SampleRateHz: tRate})
		e := NewEdgeExtractor(1, 0, a.Run)
		for off := 0; off < len(raw); off += 1 << 16 {
			end := off + 1<<16
			if end > len(raw) {
				end = len(raw)
			}
			e.Write(raw[off:end])
		}
		e.Close()
		a.Finish()
	}
}
