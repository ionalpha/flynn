package rubric_test

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/rubric"
)

func TestDefaultRubricShape(t *testing.T) {
	r := rubric.Default()
	names := map[string]bool{}
	for _, ax := range r.Axes {
		if strings.TrimSpace(ax.Guide) == "" {
			t.Fatalf("axis %q ships without a guide", ax.Name)
		}
		names[ax.Name] = true
	}
	for _, want := range []string{"design", "originality", "craft", "functionality"} {
		if !names[want] {
			t.Fatalf("default rubric missing the %q axis", want)
		}
	}
	// Every axis must ship at least one calibration band, or its scale drifts.
	banded := map[string]bool{}
	for _, b := range r.Bands {
		banded[b.Axis] = true
	}
	for name := range names {
		if !banded[name] {
			t.Fatalf("axis %q ships without a calibration band", name)
		}
	}
}

func TestDefaultRubricWeightsAestheticAxesHigher(t *testing.T) {
	// The tuning knob: design and originality are weighted above craft and functionality
	// on purpose, to push a generator graded against this rubric toward aesthetic risk.
	w := map[string]float64{}
	for _, ax := range rubric.Default().Axes {
		w[ax.Name] = ax.Weight
	}
	if !(w["design"] > w["craft"] && w["originality"] > w["functionality"]) {
		t.Fatalf("aesthetic axes are not weighted higher: %v", w)
	}
}

func TestDefaultRubricAllTopScoresPass(t *testing.T) {
	r := rubric.Default()
	top := map[string]rubric.RawScore{}
	for _, ax := range r.Axes {
		top[ax.Name] = rubric.RawScore{Score: r.Max}
	}
	if a := r.Assemble(top, nil); !a.Passed || !approx(a.Overall, 1.0) {
		t.Fatalf("top marks should score 1.0 and pass, got overall=%v passed=%v", a.Overall, a.Passed)
	}
}
