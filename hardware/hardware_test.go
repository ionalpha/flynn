package hardware

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestParseNvidiaSMI(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		wantOK    bool
		wantBytes int64
		wantName  string
	}{
		{"one gpu", "24564, NVIDIA GeForce RTX 4090", true, 24564 * 1024 * 1024, "NVIDIA GeForce RTX 4090"},
		{"extra whitespace", "  8192 ,  Tesla T4  \n", true, 8192 * 1024 * 1024, "Tesla T4"},
		{"first of several", "16384, A\n40960, B", true, 16384 * 1024 * 1024, "A"},
		{"empty", "", false, 0, ""},
		{"garbage", "no gpu here", false, 0, ""},
		{"zero memory", "0, Nothing", false, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, ok := parseNvidiaSMI(tc.out)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
			if ok && (b.VRAMBytes != tc.wantBytes || b.GPUName != tc.wantName) {
				t.Fatalf("got %+v, want %d/%q", b, tc.wantBytes, tc.wantName)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int64
	}{
		{"typical", "MemTotal:       16384256 kB\nMemFree:  100 kB", 16384256 * 1024},
		{"extra spaces", "MemTotal:\t  8192000   kB\n", 8192000 * 1024},
		{"not first line", "MemFree: 1 kB\nMemTotal: 4096 kB\n", 4096 * 1024},
		{"no unit suffix", "MemTotal: 4096\n", 4096 * 1024},
		{"missing", "MemFree: 100 kB\n", 0},
		{"empty", "", 0},
		{"garbage value", "MemTotal: lots kB\n", 0},
		{"zero", "MemTotal: 0 kB\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMeminfo(tc.in); got != tc.want {
				t.Fatalf("parseMeminfo(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseByteCount(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"17179869184", 17179869184},
		{"  8589934592\n", 8589934592},
		{"", 0},
		{"nan", 0},
		{"0", 0},
		{"-4", 0},
	}
	for _, tc := range cases {
		if got := parseByteCount(tc.in); got != tc.want {
			t.Fatalf("parseByteCount(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestParseMeminfoProperty checks the parser scales any positive kibibyte total to
// bytes regardless of the whitespace and unit-suffix variations the kernel may print.
func TestParseMeminfoProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		kib := rapid.Int64Range(1, 1<<40).Draw(rt, "kib")
		suffix := rapid.SampledFrom([]string{" kB", "kB", ""}).Draw(rt, "suffix")
		in := fmt.Sprintf("MemFree: 1 kB\nMemTotal:   %d%s\n", kib, suffix)
		if got := parseMeminfo(in); got != kib*1024 {
			rt.Fatalf("parseMeminfo = %d, want %d", got, kib*1024)
		}
	})
}

// TestParseNvidiaSMIProperty checks the parser over any well-formed first row: a
// positive mebibyte figure scales to bytes and the name is read back trimmed,
// whatever surrounding whitespace the tool emits.
func TestParseNvidiaSMIProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		mib := rapid.Int64Range(1, 1_000_000).Draw(rt, "mib")
		name := rapid.StringMatching(`[A-Za-z0-9 ]{1,24}`).Draw(rt, "name")
		line := fmt.Sprintf("  %d ,  %s  ", mib, name)
		b, ok := parseNvidiaSMI(line)
		if !ok {
			rt.Fatalf("should parse %q", line)
		}
		if b.VRAMBytes != mib*1024*1024 {
			rt.Fatalf("vram %d, want %d", b.VRAMBytes, mib*1024*1024)
		}
		if b.GPUName != strings.TrimSpace(name) {
			rt.Fatalf("name %q, want %q", b.GPUName, strings.TrimSpace(name))
		}
	})
}
