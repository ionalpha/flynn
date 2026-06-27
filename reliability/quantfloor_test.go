package reliability

import (
	"strings"
	"testing"
)

func TestQuantFloor(t *testing.T) {
	cases := []struct {
		name    string
		quant   string
		paramsB float64
		below   bool
		reason  bool // whether a non-empty reason is expected
	}{
		{"q4 large clears the floor", "Q4_K_M", 32, false, false},
		{"q5 clears", "Q5_K_M", 13, false, false},
		{"q8 clears", "Q8_0", 7, false, false},
		{"fp16 clears", "fp16", 7, false, false},
		{"q3 is below the floor", "Q3_K_S", 13, true, true},
		{"q2 is below the floor", "Q2_K", 70, true, true},
		{"iq3 is below the floor", "IQ3_XS", 8, true, true},
		{"q4 small model is a caution, not below", "Q4_K_M", 3, false, true},
		{"unknown label is reported, not passed silently", "weird", 7, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			below, reason := QuantFloor(c.quant, c.paramsB)
			if below != c.below {
				t.Fatalf("below = %v, want %v (reason: %q)", below, c.below, reason)
			}
			if (reason != "") != c.reason {
				t.Fatalf("reason presence = %v (%q), want %v", reason != "", reason, c.reason)
			}
		})
	}
}

// TestQuantFloorReasonNamesQuant proves a flag is actionable: the reason mentions the quant it is
// judging, so a catalog message points at the right thing.
func TestQuantFloorReasonNamesQuant(t *testing.T) {
	if _, reason := QuantFloor("Q2_K", 13); !strings.Contains(reason, "Q2_K") {
		t.Fatalf("reason should name the quant, got %q", reason)
	}
}

// FuzzQuantFloor proves the parser never panics on an arbitrary label, and that flagging a quant
// as below the floor always comes with a reason (a silent flag would be useless).
func FuzzQuantFloor(f *testing.F) {
	for _, s := range []string{"Q4_K_M", "IQ3_XS", "fp16", "", "Q", "q999999999999999999999", "Q-4"} {
		f.Add(s, 7.0)
	}
	f.Fuzz(func(t *testing.T, quant string, paramsB float64) {
		below, reason := QuantFloor(quant, paramsB)
		if below && reason == "" {
			t.Fatalf("a below-floor flag for %q must carry a reason", quant)
		}
	})
}
