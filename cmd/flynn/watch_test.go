package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/watch"
)

func TestWatchObjectiveCarriesProvenance(t *testing.T) {
	m := watch.Marker{Kind: watch.Act, File: "src/app.go", Line: 12, Text: "rename to count", Code: "var x = 1"}
	obj := watchObjective(m)

	// The provenance (file:line and kind) must be in the objective itself, so it is
	// recorded on the run's opening event and sealed into the record.
	if !strings.Contains(obj, "src/app.go:12 (ai!)") {
		t.Errorf("objective missing provenance:\n%s", obj)
	}
	if !strings.Contains(obj, "rename to count") {
		t.Errorf("objective missing the request text:\n%s", obj)
	}
	if !strings.Contains(obj, "var x = 1") {
		t.Errorf("objective missing the line context:\n%s", obj)
	}
	if !strings.Contains(obj, "editing the code in place") {
		t.Errorf("an ai! objective should ask for an edit:\n%s", obj)
	}
}

func TestWatchObjectiveAskDoesNotEdit(t *testing.T) {
	m := watch.Marker{Kind: watch.Ask, File: "a.go", Line: 3, Text: "why is this global"}
	obj := watchObjective(m)

	if !strings.Contains(obj, "a.go:3 (ai?)") {
		t.Errorf("objective missing provenance:\n%s", obj)
	}
	if !strings.Contains(obj, "do not edit files") {
		t.Errorf("an ai? objective should not ask for an edit:\n%s", obj)
	}
	if strings.Contains(obj, "On that line:") {
		t.Errorf("objective should omit the line block when there is no code:\n%s", obj)
	}
}
