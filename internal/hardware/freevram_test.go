package hardware

import (
	"context"
	"testing"
)

func TestParseFreeVRAM(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int64
		ok   bool
	}{
		{"one gpu", "15360\n", 15360 * 1024 * 1024, true},
		{"first row of two, with blank lines and CRLF", "\n 1024 \r\n2048\n", 1024 * 1024 * 1024, true},
		{"empty output", "", 0, false},
		{"hidden figure", "N/A\n", 0, false},
		{"negative", "-5\n", 0, false},
		{"unusable first row is not skipped", "N/A\n2048\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFreeVRAM(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Errorf("parseFreeVRAM(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestFreeVRAMRunsTheQueryAndReadsIt drives FreeVRAM through the probe seam: a driver that
// answers, one that prints nothing usable, a tool that is missing, and a tool that exits
// non-zero. Only the first gives a figure, and the query asked for is memory.free alone.
func TestFreeVRAMRunsTheQueryAndReadsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		host fakeHost
		want int64
		ok   bool
	}{
		{"answers", fakeHost{cmdOut: map[string]string{"nvidia-smi": "14000\n"}}, 14000 * 1024 * 1024, true},
		{"prints N/A", fakeHost{cmdOut: map[string]string{"nvidia-smi": "N/A\n"}}, 0, false},
		{"not installed", fakeHost{}, 0, false},
		{"exits non-zero", fakeHost{cmdErr: map[string]bool{"nvidia-smi": true}}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.host.install(t)
			got, ok := FreeVRAM(context.Background())
			if got != tc.want || ok != tc.ok {
				t.Errorf("FreeVRAM() = %d, %v; want %d, %v", got, ok, tc.want, tc.ok)
			}
			if len(tc.host.calls) != 1 || tc.host.calls[0] != "nvidia-smi" {
				t.Errorf("ran %v, want one nvidia-smi call", tc.host.calls)
			}
		})
	}
}
