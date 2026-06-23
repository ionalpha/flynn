package bus_test

import (
	"testing"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/bus/bustest"
)

// TestMemoryBusConformance holds the in-process default to the full Bus contract.
func TestMemoryBusConformance(t *testing.T) {
	bustest.RunSuite(t, func() bus.Bus { return bus.NewMemory() })
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, subject string
		want             bool
	}{
		{"a", "a", true},
		{"a", "b", false},
		{"a.b", "a.b", true},
		{"a.b", "a.c", false},
		{"a.*", "a.b", true},
		{"a.*", "a.b.c", false}, // * is exactly one token
		{"*.b", "a.b", true},
		{"a.>", "a.b", true},
		{"a.>", "a.b.c", true},
		{"a.>", "a", false}, // > needs at least one trailing token
		{">", "a.b.c", true},
		{"a.b", "a.b.c", false},
		{"a.b.c", "a.b", false},
		{"", "a", false},
		{"a", "", false},
		{"a.>.c", "a.b.c", false}, // > only valid as final token
	}
	for _, tc := range cases {
		if got := bus.Match(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

func TestValidSubject(t *testing.T) {
	valid := []string{"a", "a.b", "a.b.c", "orders.created.line"}
	invalid := []string{"", "a.", ".a", "a..b", "a.*", "*.a", "a.>", ">", "a b", "a\tb"}
	for _, s := range valid {
		if !bus.ValidSubject(s) {
			t.Errorf("ValidSubject(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if bus.ValidSubject(s) {
			t.Errorf("ValidSubject(%q) = true, want false", s)
		}
	}
}

func TestValidPattern(t *testing.T) {
	valid := []string{"a", "a.b", "a.*", "*.b", "a.>", ">", "*.*", "a.*.>"}
	invalid := []string{"", "a.", ".a", "a..b", "a.>.b", ">.a", "a b"}
	for _, s := range valid {
		if !bus.ValidPattern(s) {
			t.Errorf("ValidPattern(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if bus.ValidPattern(s) {
			t.Errorf("ValidPattern(%q) = true, want false", s)
		}
	}
}
