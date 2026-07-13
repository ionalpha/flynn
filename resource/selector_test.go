package resource

import "testing"

func TestParseAndMatchSelector(t *testing.T) {
	labels := map[string]string{"tier": "pro", "region": "eu"}
	cases := []struct {
		sel   string
		match bool
	}{
		{"", true},
		{"tier=pro", true},
		{"tier==pro", true},
		{"tier=free", false},
		{"tier!=free", true},
		{"tier!=pro", false},
		{"region", true},
		{"missing", false},
		{"!missing", true},
		{"!tier", false},
		{"tier in (pro, free)", true},
		{"tier in (free)", false},
		{"tier notin (free)", true},
		{"tier notin (pro)", false},
		{"tier=pro, region=eu", true},
		{"tier=pro, region=us", false},
		{"tier in (pro), !archived", true},
	}
	for _, tc := range cases {
		t.Run(tc.sel, func(t *testing.T) {
			sel, err := ParseSelector(tc.sel)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sel, err)
			}
			if got := sel.Matches(labels); got != tc.match {
				t.Fatalf("Matches(%q) = %v, want %v", tc.sel, got, tc.match)
			}
		})
	}
}

func TestParseSelectorErrors(t *testing.T) {
	bad := []string{
		"tier in (a",         // unbalanced: never closed
		"tier in a)",         // unbalanced: closed without opening
		"tier zzz (a,b)",     // unknown set operator
		"tier in (a,b) junk", // a set requirement must end at its closing paren
		"in (a,b)",           // no key before the operator
		"a b c in (x)",       // a set requirement head is exactly key and operator
	}
	for _, sel := range bad {
		if _, err := ParseSelector(sel); err == nil {
			t.Fatalf("ParseSelector(%q) = nil error, want error", sel)
		}
	}
}

// TestEverythingMatchesEverything locks the empty selector: it is the "no filter"
// value every List call passes when a caller has no constraint, so it must match
// any label set, including none at all.
func TestEverythingMatchesEverything(t *testing.T) {
	sel := Everything()
	if len(sel) != 0 {
		t.Fatalf("Everything() must be the empty selector, got %v", sel)
	}
	for _, labels := range []map[string]string{nil, {}, {"tier": "pro"}} {
		if !sel.Matches(labels) {
			t.Fatalf("Everything() must match %v", labels)
		}
	}
}

// TestUnknownOperatorMatchesNothing is the fail-closed rule for a requirement built
// by hand (not parsed) with an operator this package does not define: it must match
// nothing rather than silently admit every resource into a query result.
func TestUnknownOperatorMatchesNothing(t *testing.T) {
	sel := Selector{{Key: "tier", Op: Op(99), Values: []string{"pro"}}}
	if sel.Matches(map[string]string{"tier": "pro"}) {
		t.Fatal("an unknown operator must not match")
	}
	if sel.Matches(nil) {
		t.Fatal("an unknown operator must not match an empty label set either")
	}
}
