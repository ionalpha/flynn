package state_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/state"
)

// Canonical form is what makes the same anchor set encode, and so content-hash,
// identically on every backend. The conformance suite checks a store returns it;
// this checks the rule itself, including the shapes a store never sees because
// they were rejected or collapsed on the way in.
func TestNormalizeAnchors(t *testing.T) {
	widget := state.Anchor{Kind: "widget", ID: "w-1"}
	widget2 := state.Anchor{Kind: "widget", ID: "w-2"}
	gadget := state.Anchor{Kind: "gadget", ID: "g-9"}

	for _, tc := range []struct {
		name string
		in   []state.Anchor
		want []state.Anchor
	}{
		{"nil is nil", nil, nil},
		// Empty and nil normalize to the same thing, so an item with no anchors has
		// one representation rather than two that hash differently.
		{"empty is nil", []state.Anchor{}, nil},
		{"sorted by kind first", []state.Anchor{widget, gadget}, []state.Anchor{gadget, widget}},
		{"then by id", []state.Anchor{widget2, widget}, []state.Anchor{widget, widget2}},
		{"exact duplicates collapse", []state.Anchor{widget, widget, widget}, []state.Anchor{widget}},
		{
			"duplicates collapse across a reordering",
			[]state.Anchor{widget, gadget, widget2, gadget},
			[]state.Anchor{gadget, widget, widget2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := state.NormalizeAnchors(tc.in)
			if err != nil {
				t.Fatalf("NormalizeAnchors(%v): %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("NormalizeAnchors(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Half an anchor can never match a recall, so it is rejected at the write rather
// than stored as a ref that is dead on arrival. This is the whole of the
// validation: shape, never resolution.
func TestNormalizeAnchorsRejectsHalfAnAnchor(t *testing.T) {
	for _, bad := range []state.Anchor{
		{Kind: "widget"},
		{ID: "w-1"},
		{},
	} {
		got, err := state.NormalizeAnchors([]state.Anchor{{Kind: "widget", ID: "w-1"}, bad})
		if !errors.Is(err, state.ErrInvalid) {
			t.Errorf("NormalizeAnchors with %+v = %v, want ErrInvalid", bad, err)
		}
		if got != nil {
			t.Errorf("NormalizeAnchors with %+v returned %v alongside its error, want none", bad, got)
		}
		if bad.Valid() {
			t.Errorf("%+v reports itself valid", bad)
		}
	}
}

// An anchor whose referent does not exist, or never did, is a well-formed anchor.
// Nothing in this package can resolve a ref - whatever owns the referent is
// outside Flynn - so a store that rejected or dropped unresolvable anchors would
// be inventing an answer it has no way to compute, and would discard facts whose
// subject it merely could not see.
func TestAnchorValidityIsShapeNotExistence(t *testing.T) {
	for _, a := range []state.Anchor{
		{Kind: "widget", ID: "deleted-yesterday"},
		{Kind: "a-system-flynn-has-never-heard-of", ID: "0"},
		{Kind: "file", ID: "/path/that/is/not/here"},
	} {
		if !a.Valid() {
			t.Errorf("%+v is well formed but reports itself invalid", a)
		}
	}
}

// The skill anchor is the one kind this package names, because a skill is the one
// referent Flynn issues an id for and can read back with no host present. That is
// what makes a ride-along on skill_read work standalone, so the constant and the
// helpers are part of the contract rather than a caller's private convention.
func TestSkillAnchorIsFlynnsOwnReferent(t *testing.T) {
	a := state.SkillAnchor("sk-1")
	if a.Kind != state.AnchorKindSkill || a.ID != "sk-1" {
		t.Errorf("SkillAnchor(sk-1) = %+v, want kind %q and that id", a, state.AnchorKindSkill)
	}
	if !a.Valid() {
		t.Errorf("SkillAnchor(sk-1) = %+v reports itself invalid", a)
	}
}

// An empty id yields the zero anchor rather than one referring to nothing under a
// real kind, and the plural form drops it. A caller passing the ids of the skills a
// run read should not have to filter that list to build anchors from it.
func TestSkillAnchorsSkipEmptyIDs(t *testing.T) {
	if a := state.SkillAnchor(""); a != (state.Anchor{}) {
		t.Errorf("SkillAnchor(\"\") = %+v, want the zero anchor", a)
	}
	for _, ids := range [][]string{nil, {}, {""}, {"", ""}} {
		if got := state.SkillAnchors(ids); got != nil {
			t.Errorf("SkillAnchors(%q) = %v, want none", ids, got)
		}
	}
	got := state.SkillAnchors([]string{"sk-1", "", "sk-2"})
	want := []state.Anchor{{Kind: state.AnchorKindSkill, ID: "sk-1"}, {Kind: state.AnchorKindSkill, ID: "sk-2"}}
	if !slices.Equal(got, want) {
		t.Errorf("SkillAnchors = %v, want %v", got, want)
	}
}
