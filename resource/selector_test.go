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
	for _, bad := range []string{"tier in (a", "tier in a)", "tier zzz (a,b)"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Fatalf("ParseSelector(%q) = nil error, want error", bad)
		}
	}
}
