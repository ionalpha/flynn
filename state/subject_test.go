package state_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/state"
)

// The subject is a key, so what matters is that two writers naming one topic in
// their own house style land on the same string. The conformance suite checks a
// store stores what this returns; this checks the rule itself, including the
// inputs a store never sees because they were rejected on the way in.
func TestNormalizeSubject(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"already canonical", "db-choice", "db-choice"},
		{"spaced and capitalized", "DB Choice", "db-choice"},
		{"underscored", "db_choice", "db-choice"},
		{"punctuated", "db.choice!", "db-choice"},
		{"runs collapse", "db   ///   choice", "db-choice"},
		{"ends trimmed", "  -db-choice-  ", "db-choice"},
		{"digits kept", "postgres 16", "postgres-16"},
		{"empty stays empty", "", ""},
		{"whitespace only is empty, not invalid", "   ", ""},
		// A subject written in a script with no ASCII in it keys on itself rather
		// than collapsing to nothing, which is what an ASCII-range check would do.
		{"non-Latin script", "データベース", "データベース"},
		{"mixed scripts", "Ελλάδα deploy", "ελλάδα-deploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := state.NormalizeSubject(tc.in)
			if err != nil {
				t.Fatalf("NormalizeSubject(%q) = error %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeSubject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A subject the writer named but that leaves nothing to key on is refused. The
// alternative - storing it as the empty subject - hands back an item that looks
// unsubjected to every later read, at exactly the point the writer was relying on
// the key.
func TestNormalizeSubjectRejectsUnkeyable(t *testing.T) {
	for _, in := range []string{"-", "---", ".", " ?! ", "!@#$%"} {
		got, err := state.NormalizeSubject(in)
		if !errors.Is(err, state.ErrInvalid) {
			t.Fatalf("NormalizeSubject(%q) = (%q, %v), want ErrInvalid", in, got, err)
		}
	}
}

// Normalizing is idempotent, which is what lets a caller filter a recall with a
// subject it read off a stored item without checking whether it had been through
// the write path already.
func TestProp_NormalizeSubjectIdempotent(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := rapid.String().Draw(rt, "subject")
		once, err := state.NormalizeSubject(in)
		if err != nil {
			// Nothing to key on: the rejection is the assertion, and there is no
			// canonical form to compare against.
			return
		}
		twice, err := state.NormalizeSubject(once)
		if err != nil {
			rt.Fatalf("NormalizeSubject(%q) rejected its own output %q: %v", in, once, err)
		}
		if twice != once {
			rt.Fatalf("NormalizeSubject(%q) = %q, then %q: not idempotent", in, once, twice)
		}
		for _, r := range once {
			if r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) {
				continue
			}
			rt.Fatalf("NormalizeSubject(%q) = %q, which is not a slug", in, once)
		}
		if strings.HasPrefix(once, "-") || strings.HasSuffix(once, "-") || strings.Contains(once, "--") {
			rt.Fatalf("NormalizeSubject(%q) = %q, which has a stray or doubled hyphen", in, once)
		}
	})
}

// Canonical form for supersession is the same bargain anchors make: order carries
// nothing, so fixing it buys an encoding that is identical however the caller
// happened to list the ids.
func TestNormalizeSupersedes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		self string
		want []string
	}{
		{"nil stays nil", nil, "", nil},
		{"empty stays nil", []string{}, "", nil},
		{"sorted", []string{"b", "a"}, "", []string{"a", "b"}},
		{"deduplicated", []string{"b", "a", "b"}, "", []string{"a", "b"}},
		{"self elsewhere in the list is fine", []string{"a"}, "z", []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := state.NormalizeSupersedes(tc.in, tc.self)
			if err != nil {
				t.Fatalf("NormalizeSupersedes(%v) = error %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("NormalizeSupersedes(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeSupersedesRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		self string
	}{
		{"blank id", []string{""}, ""},
		{"whitespace id", []string{"  "}, ""},
		{"self", []string{"a", "z"}, "z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := state.NormalizeSupersedes(tc.in, tc.self)
			if !errors.Is(err, state.ErrInvalid) {
				t.Fatalf("NormalizeSupersedes(%v, %q) = (%v, %v), want ErrInvalid", tc.in, tc.self, got, err)
			}
		})
	}
}

// The subject filter is a key match, and the empty subject is one of the keys it
// can be asked for. Selects is where every backend that filters in Go shares the
// rule, so this pins it directly rather than through a store.
func TestRecallQuerySelectsBySubject(t *testing.T) {
	item := state.MemoryItem{Kind: "decision", Subject: "db-choice"}
	unsubjected := state.MemoryItem{Kind: "decision"}
	for _, tc := range []struct {
		name string
		q    state.RecallQuery
		it   state.MemoryItem
		want bool
	}{
		{"no filter matches everything", state.RecallQuery{}, item, true},
		{"exact match", state.RecallQuery{Subjects: []string{"db-choice"}}, item, true},
		{"any of several", state.RecallQuery{Subjects: []string{"queue-choice", "db-choice"}}, item, true},
		{"non-matching subject", state.RecallQuery{Subjects: []string{"queue-choice"}}, item, false},
		{"un-normalized filter matches nothing", state.RecallQuery{Subjects: []string{"DB Choice"}}, item, false},
		{"the empty subject is askable", state.RecallQuery{Subjects: []string{""}}, unsubjected, true},
		{"a subject filter excludes the unsubjected", state.RecallQuery{Subjects: []string{"db-choice"}}, unsubjected, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.Selects(tc.it, time.Time{}); got != tc.want {
				t.Fatalf("Selects = %v, want %v", got, tc.want)
			}
		})
	}
}
