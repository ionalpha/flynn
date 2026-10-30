package gpu

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

// A version printed in either banner form reads back as the numbers that were printed, and
// whatever surrounds the banner does not change what is read.
func TestParseCUDAVersionRoundTrips(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		major := rapid.IntRange(1, 99).Draw(rt, "major")
		minor := rapid.IntRange(0, 99).Draw(rt, "minor")
		label := rapid.SampledFrom([]string{"CUDA Version", "CUDA UMD Version"}).Draw(rt, "label")
		pad := rapid.StringMatching(`[ |\-+a-zA-Z0-9.]{0,40}`).Draw(rt, "pad")

		got := parseCUDAVersion(fmt.Sprintf("%s\n| %s: %d.%d |\n%s", pad, label, major, minor, pad))
		if len(got) != 2 || got[0] != major || got[1] != minor {
			rt.Fatalf("read %v from a banner printing %d.%d (%s, pad %q)", got, major, minor, label, pad)
		}
	})
}

// An adapter line the tool printed is reported with the name and driver it carried, and the
// compute capability when it is a number.
func TestParseQueryKeepsTheFieldsItWasGiven(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		name := rapid.StringMatching(`[A-Za-z][A-Za-z0-9 ]{0,30}[A-Za-z0-9]`).Draw(rt, "name")
		driver := rapid.StringMatching(`[0-9]{2,3}\.[0-9]{2}`).Draw(rt, "driver")
		cc := rapid.SampledFrom([]float64{5.0, 7.5, 8.9, 12.0}).Draw(rt, "cc")

		info, err := parseQuery(fmt.Sprintf("%s, %s, %.1f\n", name, driver, cc))
		if err != nil {
			rt.Fatal(err)
		}
		if info.Name != name || info.Driver != driver || info.ComputeCap != cc {
			rt.Fatalf("parsed %+v from %q, %q, %.1f", info, name, driver, cc)
		}
	})
}

// Text that is not an adapter line is never reported as one.
func TestParseQueryRefusesWhatIsNotAnAdapterLine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		line := rapid.StringMatching(`[^,\n]{0,40}`).Draw(rt, "line")
		if info, err := parseQuery(line); err == nil {
			rt.Fatalf("a line with no comma gave an adapter: %+v from %q", info, line)
		}
	})
}
