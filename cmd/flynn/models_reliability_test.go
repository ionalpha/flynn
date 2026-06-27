package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/harness"
)

// TestReliabilityColMeasured proves a probed model shows its measured tool-call reliability.
func TestReliabilityColMeasured(t *testing.T) {
	m := catalog.ModelSpec{ID: "ollama:probed", Kind: catalog.KindLocal, Quants: []catalog.Quant{{Name: "Q4_K_M"}}}
	src := harness.StaticProfiles{"ollama:probed": {ToolCallReliability: 0.82}}
	if got := reliabilityCol(m, src); got != "82% measured" {
		t.Fatalf("measured reliability cell = %q, want %q", got, "82% measured")
	}
}

// TestReliabilityColAprioriQuantFloor proves an unprobed model falls back to the quant-floor read:
// a sub-floor quant is flagged, a small model at the floor is borderline, and an ordinary one is
// simply unprobed.
func TestReliabilityColAprioriQuantFloor(t *testing.T) {
	none := harness.StaticProfiles{}
	cases := []struct {
		name  string
		quant string
		param float64
		want  string
	}{
		{"sub-floor", "Q2_K", 30, "below floor"},
		{"small at floor", "Q4_K_M", 3, "borderline"},
		{"ordinary", "Q4_K_M", 30, "unprobed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := catalog.ModelSpec{ID: "m", Kind: catalog.KindLocal, ParamsB: c.param, Quants: []catalog.Quant{{Name: c.quant}}}
			if got := reliabilityCol(m, none); got != c.want {
				t.Fatalf("reliability cell = %q, want %q", got, c.want)
			}
		})
	}
}

// TestReliabilityColAPIModel proves a hosted API model, which is not probed locally, shows a dash
// rather than a reliability verdict.
func TestReliabilityColAPIModel(t *testing.T) {
	m := catalog.ModelSpec{ID: "anthropic:claude", Kind: catalog.KindAPI}
	if got := reliabilityCol(m, harness.StaticProfiles{}); got != "-" {
		t.Fatalf("API reliability cell = %q, want %q", got, "-")
	}
}

// TestFitViewShowsReliabilityAxis proves the fit listing is two-axis: the RELIABILITY column and
// its explanation appear alongside the fit verdict.
func TestFitViewShowsReliabilityAxis(t *testing.T) {
	var b strings.Builder
	if err := runModels([]string{"--local", "--vram", "24"}, "", &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"RELIABILITY", "flynn models probe"} {
		if !strings.Contains(out, want) {
			t.Fatalf("two-axis fit output missing %q:\n%s", want, out)
		}
	}
}
