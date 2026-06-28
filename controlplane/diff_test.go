package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/ionalpha/flynn/resource"
)

func diffRes(spec, status string) resource.Resource {
	return resource.Resource{Spec: json.RawMessage(spec), Status: json.RawMessage(status)}
}

func TestDiff(t *testing.T) {
	a := diffRes(`{"model":"opus","driver":"sw","nested":{"k":1}}`, `{"phase":"Running"}`)
	b := diffRes(`{"model":"sonnet","driver":"sw","nested":{"k":2},"extra":"x"}`, `{"phase":"Running"}`)

	got := Diff(a, b)
	want := []FieldDelta{
		{Field: "spec.extra", A: "", B: "x"},          // present only on B
		{Field: "spec.model", A: "opus", B: "sonnet"}, // changed scalar
		{Field: "spec.nested.k", A: "1", B: "2"},      // changed nested field, at field granularity
	}
	if len(got) != len(want) {
		t.Fatalf("Diff returned %d deltas, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// An unchanged field (driver) and an unchanged status (phase) are not reported.
	for _, d := range got {
		if d.Field == "spec.driver" || d.Field == "status.phase" {
			t.Fatalf("unchanged field %q must not appear in the diff", d.Field)
		}
	}
}

func TestDiffIdentical(t *testing.T) {
	a := diffRes(`{"model":"opus"}`, `{"phase":"Idle"}`)
	if d := Diff(a, a); len(d) != 0 {
		t.Fatalf("identical resources must have no deltas, got %+v", d)
	}
}

func TestDiffEmptySides(t *testing.T) {
	// A resource with no spec/status compared against one with content reports every
	// field as added, never panics on the empty side.
	empty := resource.Resource{}
	full := diffRes(`{"model":"opus"}`, "")
	got := Diff(empty, full)
	if len(got) != 1 || got[0] != (FieldDelta{Field: "spec.model", A: "", B: "opus"}) {
		t.Fatalf("diff against empty = %+v", got)
	}
}
