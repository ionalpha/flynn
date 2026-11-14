package stdioguard

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

// TestStatsAccountForEveryByte checks the drain's accounting for any sequence of writes:
// the count is the total, and the sample is the first SampleLimit bytes of the stream, in
// order, however the writes were split.
func TestStatsAccountForEveryByte(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		chunks := rapid.SliceOfN(rapid.SliceOfN(rapid.Byte(), 0, 700), 0, 20).Draw(rt, "chunks")
		g, strays, finish := startGuard(t)
		var all []byte
		for _, c := range chunks {
			if _, err := strays.Write(c); err != nil {
				rt.Fatalf("write: %v", err)
			}
			all = append(all, c...)
		}
		finish()

		s := g.Stats()
		if s.StrayBytes != int64(len(all)) {
			rt.Fatalf("counted %d bytes, wrote %d", s.StrayBytes, len(all))
		}
		want := all[:min(len(all), SampleLimit)]
		if !bytes.Equal(s.Sample, want) {
			rt.Fatalf("sample is not the first %d bytes of the stream", len(want))
		}
	})
}
