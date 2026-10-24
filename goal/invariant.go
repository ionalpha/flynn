package goal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ionalpha/flynn/resource"
)

// invariantMarkLen is how many hex characters of an invariant's content hash form its
// fingerprint: 64 bits, past collision range for the terms one goal carries.
const invariantMarkLen = 16

// Invariant errors are distinct because they say different things: an authoring
// mistake (a term that could never be checked) and the failure invariants exist for (a
// run getting out from under a term).
var (
	// ErrInvariantIncomplete reports an invariant missing its id or statement. An auditor
	// handed a term with nothing written down would have to invent what it means.
	ErrInvariantIncomplete = errors.New("goal: invariant needs an id and a statement")
	// ErrInvariantDuplicate reports two invariants with one id, which would make a breach
	// ambiguous about which term was broken.
	ErrInvariantDuplicate = errors.New("goal: duplicate invariant id")
	// ErrInvariantRelaxed reports an invariant dropped or reworded after the run adopted
	// it: the run renegotiating its own terms.
	ErrInvariantRelaxed = errors.New("goal: invariant was relaxed after the run adopted it")
)

// Invariant is one term of the run: something that must stay true throughout, stated in
// the author's words, with an optional declared way to observe it. The stop condition is
// what must become true; an invariant is the counterweight to the completion pressure it
// creates. Four rules:
//
//   - An [InvariantAuditor] judges it from the record, and the reconciler asks before it
//     asks whether the goal is done. A breach settles the goal whatever the stop
//     evaluator would say.
//   - The run cannot relax it: dropping or rewording an adopted invariant is a terminal
//     spec fault ([ErrInvariantRelaxed]); adding one is allowed.
//   - A breach is recorded on the status, so editing the spec does not launder it out.
//   - A term nobody can check is not a term. Terms with no auditor wired stall the goal
//     before its first step, and a term asserting an absence is refused unless it
//     declares the search that would find a counterexample ([AssertsAbsence]).
type Invariant struct {
	// ID names the term so a breach can point at it. It is the author's, like a unit id,
	// because a content address is unwritable by hand.
	ID string `json:"id"`
	// Statement is what must stay true. It is what the auditor rules against and what a
	// breach quotes back to whoever is handed the stopped goal.
	Statement string `json:"statement"`
	// Check is the declared way to observe the statement: the command to run, the search
	// to make. A zero exit says the term holds.
	//
	// It is optional for a term about what the run did, which an auditor can rule on from
	// the record. It is required for a term claiming something is not there, which the
	// record cannot settle: admission refuses an absence claim with no search.
	Check string `json:"check,omitempty"`
}

// InvariantMark returns the fingerprint of an invariant's content: the first
// invariantMarkLen hex characters of a SHA-256 over its id, statement and check,
// NUL-separated so no two different invariants re-split into the same bytes. The status
// remembers it from adoption, so a later spec that softens the wording is caught and not
// accepted as the same term.
func InvariantMark(inv Invariant) string {
	var b strings.Builder
	b.WriteString(inv.ID)
	b.WriteByte(0)
	b.WriteString(inv.Statement)
	b.WriteByte(0)
	b.WriteString(inv.Check)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:invariantMarkLen]
}

// InvariantState is the observed state of one term: the content it was adopted under,
// when it was last audited, and the breach if one was found.
type InvariantState struct {
	// ID is the invariant this entry observes.
	ID string `json:"id"`
	// Mark is [InvariantMark] of the invariant as adopted. A later spec whose invariant of
	// the same id marks differently has reworded a term the run is held to, and admission
	// refuses it.
	Mark string `json:"mark"`
	// LastAudited is when an auditor last ruled on this term; nil means carried but not
	// yet checked.
	LastAudited *time.Time `json:"lastAudited,omitempty"`
	// Audits counts how many times the term has been ruled on, so a stopped goal can show
	// a term was checked and not merely carried.
	Audits int `json:"audits,omitempty"`
	// Breached records that the term was found broken, and Detail is what the auditor
	// found. It is never cleared: a breach is a fact about what the run did, and the run
	// cannot work its way back out of it.
	Breached bool   `json:"breached,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Breach is an auditor's finding that one term has been broken. Detail is quoted to
// whoever is handed the stopped goal, so it says what happened.
type Breach struct {
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// InvariantAuditor rules on whether a run has broken any of its terms, returning a
// finding per broken term. Every term is offered every time: a term is a standing
// obligation, so one that held at step 3 says nothing about step 4.
//
// The status is passed separately from r because it is the one this pass has built and
// not yet written; an auditor reading r.Status would rule on the run as it stood before
// the step it is asked about.
//
// An auditor that cannot answer returns an error and not an empty finding: "I could not
// check" and "I checked and it holds" are the difference between a guard and a
// formality, and only the auditor can say whether its failure is transient. The
// production auditor runs each term's declared check against the run's workspace.
type InvariantAuditor interface {
	Audit(ctx context.Context, r resource.Resource, spec Spec, status Status, terms []Invariant) ([]Breach, error)
}

// ValidateInvariants refuses a set of terms that could never be audited: one missing its
// id or statement, two claiming the same id, or one asserting an absence with no search
// declared to find a counterexample. It runs at admission, before anything is dispatched.
//
// The absence rule is the one that matters, because it refuses the term that would
// otherwise pass: a claim that no credentials remain, audited from the run's account of
// itself, is settled most easily for the run that did not look. See absence.go.
func ValidateInvariants(invs []Invariant) error {
	seen := make(map[string]bool, len(invs))
	for i, inv := range invs {
		if inv.ID == "" || inv.Statement == "" {
			return fmt.Errorf("%w: invariant %d", ErrInvariantIncomplete, i)
		}
		if seen[inv.ID] {
			return fmt.Errorf("%w: %q", ErrInvariantDuplicate, inv.ID)
		}
		seen[inv.ID] = true
		if strings.TrimSpace(inv.Check) == "" && AssertsAbsence(inv.Statement) {
			return fmt.Errorf("%w: %q says %q", ErrInvariantUnsearchable, inv.ID, inv.Statement)
		}
	}
	return nil
}

// ValidateInvariantsAdopted checks the terms on the spec against the terms the status
// has adopted: each adopted term must still be present and still fingerprint to its
// mark. New terms pass, because tightening a run in flight is legitimate and acquiring
// another obligation is never a way out of one.
func (s Status) ValidateInvariantsAdopted(invs []Invariant) error {
	if len(s.Invariants) == 0 {
		return nil
	}
	by := make(map[string]Invariant, len(invs))
	for _, inv := range invs {
		by[inv.ID] = inv
	}
	for _, st := range s.Invariants {
		cur, ok := by[st.ID]
		if !ok {
			return fmt.Errorf("%w: %q was dropped", ErrInvariantRelaxed, st.ID)
		}
		if mark := InvariantMark(cur); mark != st.Mark {
			return fmt.Errorf("%w: %q was reworded (adopted %s, now %s)", ErrInvariantRelaxed, st.ID, st.Mark, mark)
		}
	}
	return nil
}

// SyncInvariants adopts any term the status has not seen and preserves what is recorded
// about the rest. Adoption commits a term: [Status.ValidateInvariantsAdopted] then refuses
// to let it be dropped or reworded. An adopted term is kept even when the spec no longer
// carries it, since this runs after that check and the surviving entry is what a later
// admission catches the drop against.
func (s *Status) SyncInvariants(invs []Invariant) {
	if len(invs) == 0 {
		return
	}
	have := make(map[string]bool, len(s.Invariants))
	for _, st := range s.Invariants {
		have[st.ID] = true
	}
	for _, inv := range invs {
		if have[inv.ID] {
			continue
		}
		s.Invariants = append(s.Invariants, InvariantState{ID: inv.ID, Mark: InvariantMark(inv)})
	}
}

// RecordAudit folds an audit into the status: each checked term has its audit counted
// and stamped, and each finding is recorded against its term. It reports whether any
// term is now breached.
//
// A finding naming a term the goal does not carry is ignored: recording it would let an
// auditor stop a run for a reason its author never wrote down.
func (s *Status) RecordAudit(checked []Invariant, breaches []Breach, now time.Time) bool {
	found := make(map[string]string, len(breaches))
	for _, b := range breaches {
		found[b.ID] = b.Detail
	}
	by := make(map[string]int, len(s.Invariants))
	for i, st := range s.Invariants {
		by[st.ID] = i
	}
	broke := false
	for _, inv := range checked {
		i, ok := by[inv.ID]
		if !ok {
			continue // not adopted: nothing to record against
		}
		st := &s.Invariants[i]
		st.Audits++
		stamp := now
		st.LastAudited = &stamp
		if detail, broken := found[inv.ID]; broken {
			st.Breached = true
			st.Detail = detail
			broke = true
		}
	}
	return broke
}

// BreachedInvariant returns the first term recorded as broken. The reconciler asks it
// before the stop evaluator, so a goal carrying a breach cannot be judged done on a
// later pass, whatever its spec has been edited to say since.
func (s Status) BreachedInvariant() (InvariantState, bool) {
	for _, st := range s.Invariants {
		if st.Breached {
			return st, true
		}
	}
	return InvariantState{}, false
}

// BreachReason is the message a breached goal settles under: the broken term, quoted,
// and what the audit found. It falls back to the id for a term the spec no longer
// carries.
func BreachReason(invs []Invariant, st InvariantState) string {
	statement := st.ID
	for _, inv := range invs {
		if inv.ID == st.ID {
			statement = inv.Statement
			break
		}
	}
	msg := "invariant broken: " + statement
	if st.Detail != "" {
		msg += ": " + st.Detail
	}
	return msg
}
