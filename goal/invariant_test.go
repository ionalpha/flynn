package goal

import (
	"errors"
	"testing"
	"time"
)

func TestValidateInvariants(t *testing.T) {
	cases := []struct {
		name string
		invs []Invariant
		want error
	}{
		{name: "none", invs: nil},
		{name: "ok", invs: []Invariant{
			{ID: "in-scope", Statement: "every edit lands under the module the objective names"},
			{ID: "no-force-push", Statement: "never force-push a shared branch", Check: "git reflog | grep forced-update"},
			{ID: "no-secrets", Statement: "no credential leaves the workspace", Check: "grep the diff for key material"},
		}},
		{name: "absence with no search", invs: []Invariant{
			{ID: "no-secrets", Statement: "no credential leaves the workspace"},
		}, want: ErrInvariantUnsearchable},
		{name: "no id", invs: []Invariant{{Statement: "s"}}, want: ErrInvariantIncomplete},
		{name: "no statement", invs: []Invariant{{ID: "a"}}, want: ErrInvariantIncomplete},
		{name: "duplicate id", invs: []Invariant{
			{ID: "a", Statement: "one"},
			{ID: "a", Statement: "another"},
		}, want: ErrInvariantDuplicate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInvariants(tc.invs)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateInvariants = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestInvariantMarkSeparatesItsFields: the mark is what catches a reworded term, so two
// different terms must never mark the same. The interesting case is the one a naive
// concatenation gets wrong, where the same characters are split differently between the
// fields.
func TestInvariantMarkSeparatesItsFields(t *testing.T) {
	a := InvariantMark(Invariant{ID: "ab", Statement: "c", Check: "d"})
	b := InvariantMark(Invariant{ID: "a", Statement: "bc", Check: "d"})
	if a == b {
		t.Fatalf("two different invariants marked the same: %s", a)
	}
	if got := InvariantMark(Invariant{ID: "ab", Statement: "c", Check: "d"}); got != a {
		t.Fatalf("mark is not stable: %s then %s", a, got)
	}
	// The check is part of the term: softening how a term is observed softens the term.
	if InvariantMark(Invariant{ID: "a", Statement: "s", Check: "grep -r"}) ==
		InvariantMark(Invariant{ID: "a", Statement: "s"}) {
		t.Fatal("dropping the check did not change the mark")
	}
}

// TestAdoptedInvariantCannotBeRelaxed covers both halves of the one-directional rule
// against the same adopted status: a term may be added, and may not be dropped or
// reworded.
func TestAdoptedInvariantCannotBeRelaxed(t *testing.T) {
	term := Invariant{ID: "no-force-push", Statement: "never force-push a shared branch"}
	var st Status
	st.SyncInvariants([]Invariant{term})
	if len(st.Invariants) != 1 || st.Invariants[0].Mark != InvariantMark(term) {
		t.Fatalf("adoption did not record the term as it read: %+v", st.Invariants)
	}

	if err := st.ValidateInvariantsAdopted([]Invariant{term}); err != nil {
		t.Fatalf("unchanged terms refused: %v", err)
	}
	added := []Invariant{term, {ID: "later", Statement: "and no rewriting history either"}}
	if err := st.ValidateInvariantsAdopted(added); err != nil {
		t.Fatalf("tightening the terms mid-run refused: %v", err)
	}
	if err := st.ValidateInvariantsAdopted(nil); !errors.Is(err, ErrInvariantRelaxed) {
		t.Fatalf("dropping the term = %v, want ErrInvariantRelaxed", err)
	}
	softer := []Invariant{{ID: term.ID, Statement: "avoid force-pushing where practical"}}
	if err := st.ValidateInvariantsAdopted(softer); !errors.Is(err, ErrInvariantRelaxed) {
		t.Fatalf("rewording the term = %v, want ErrInvariantRelaxed", err)
	}

	// Adoption is once: a second sync over the same term must not duplicate the entry
	// or re-mark it, or a rewording would be adopted as if it were new.
	st.SyncInvariants(softer)
	if len(st.Invariants) != 1 || st.Invariants[0].Mark != InvariantMark(term) {
		t.Fatalf("re-syncing re-adopted the term: %+v", st.Invariants)
	}
}

func TestRecordAudit(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	terms := []Invariant{{ID: "a", Statement: "one"}, {ID: "b", Statement: "two"}}
	var st Status
	st.SyncInvariants(terms)

	if st.RecordAudit(terms, nil, now) {
		t.Fatal("an audit that found nothing reported a breach")
	}
	for _, entry := range st.Invariants {
		if entry.Audits != 1 || entry.LastAudited == nil || !entry.LastAudited.Equal(now) {
			t.Fatalf("term %q was not stamped as audited: %+v", entry.ID, entry)
		}
	}
	if _, breached := st.BreachedInvariant(); breached {
		t.Fatal("a clean audit recorded a breach")
	}

	later := now.Add(time.Minute)
	if !st.RecordAudit(terms, []Breach{{ID: "b", Detail: "force-pushed origin/main"}}, later) {
		t.Fatal("a finding was not reported as a breach")
	}
	entry, breached := st.BreachedInvariant()
	if !breached || entry.ID != "b" || entry.Detail != "force-pushed origin/main" {
		t.Fatalf("breach not recorded against its term: %+v", st.Invariants)
	}
	if st.Invariants[0].Breached {
		t.Fatal("a finding against one term marked another")
	}

	// A finding naming a term the goal does not carry is ignored: an auditor cannot
	// stop a run under a term nobody wrote down.
	var clean Status
	clean.SyncInvariants(terms)
	if clean.RecordAudit(terms, []Breach{{ID: "invented", Detail: "made up"}}, now) {
		t.Fatal("a finding for a term the goal does not carry was recorded as a breach")
	}
	if _, breached := clean.BreachedInvariant(); breached {
		t.Fatalf("status carries a breach for an unknown term: %+v", clean.Invariants)
	}
	// Nor does auditing a term that was never adopted invent an entry for it.
	if clean.RecordAudit([]Invariant{{ID: "unadopted", Statement: "s"}}, nil, now); len(clean.Invariants) != 2 {
		t.Fatalf("auditing an unadopted term added an entry: %+v", clean.Invariants)
	}
}

func TestBreachReason(t *testing.T) {
	terms := []Invariant{{ID: "no-force-push", Statement: "never force-push a shared branch"}}
	got := BreachReason(terms, InvariantState{ID: "no-force-push", Detail: "pushed --force to main at step 4"})
	want := "invariant broken: never force-push a shared branch: pushed --force to main at step 4"
	if got != want {
		t.Fatalf("BreachReason = %q, want %q", got, want)
	}
	// A term the spec no longer carries still reads as something: the id is all that is
	// left of it, and saying nothing would make the stopped goal unexplainable.
	if got := BreachReason(nil, InvariantState{ID: "gone"}); got != "invariant broken: gone" {
		t.Fatalf("BreachReason for a dropped term = %q", got)
	}
}
