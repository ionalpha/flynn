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

// A goal's invariants are the terms of the run: things that must stay true while the
// work happens, as opposed to the stop condition, which is the one thing that must
// become true for the work to be over.
//
// The distinction is the whole point. A stop condition creates completion pressure by
// design: a run is given a statement of done, an independent grader that refuses, a
// step budget counting down, and hours with nobody watching. Under that pressure
// anything else stated in prose reads as an obstacle on the path to the only outcome
// being measured, and the observed failure is an agent that works around a guard while
// looking locally compliant at every step. An invariant is the counterweight, and it is
// only a counterweight if the run cannot spend it.
//
// Four rules are what make it unspendable.
//
// It is not the goal's to declare satisfied. The stop condition is judged by asking
// whether the objective has been achieved, which is a question the run's own account of
// its work answers. An invariant is judged by an auditor that reads the record of what
// the run did, and the reconciler asks it before it asks whether the goal is done. A
// breach settles the goal whatever the stop evaluator was about to say, so there is no
// ordering on which finishing the task outranks the terms it was given.
//
// It cannot be relaxed by the run. Once a reconcile has adopted an invariant onto the
// status, dropping it or rewording it is refused as a terminal spec fault. Adding one is
// always allowed: tightening the terms mid-run is legitimate, and loosening them is the
// exact move this exists to foreclose. That asymmetry is deliberate, and it is why the
// rule is not the unit graph's (a unit nothing was spent on is not yet a commitment; an
// invariant is a commitment from the moment it is stated).
//
// A breach outlives the reconcile that found it. It is recorded on the status, and a
// goal carrying a recorded breach can never converge, so editing the spec to re-run a
// breached goal does not launder the breach out of the record.
//
// A term nobody can check is not a term. A goal that states terms with no auditor wired
// stalls before its first step, because the alternative is a run that reads as governed,
// is never checked, and finishes indistinguishable from one whose terms held. A term that
// asserts an absence is refused outright unless it declares the search that would find a
// counterexample, since nothing in a run's record is the absence and "I could not find
// any" is worth what the search behind it was worth (absence.go).

// invariantMarkLen is how many hex characters of an invariant's content hash form its
// fingerprint. Sixteen (64 bits) is far past collision range for the handful of terms
// one goal carries, and short enough to stay readable in a status dump.
const invariantMarkLen = 16

// Invariant errors. They separate a term that could never be checked from a term the
// run tried to get out from under, because the two say different things: the first is
// an authoring mistake, the second is the failure mode invariants exist for.
var (
	// ErrInvariantIncomplete reports an invariant missing its id or its statement. A
	// term with nothing written down cannot be audited, and an auditor handed one
	// would have to invent what it means.
	ErrInvariantIncomplete = errors.New("goal: invariant needs an id and a statement")
	// ErrInvariantDuplicate reports two invariants claiming the same id, which would
	// make a breach ambiguous about which term was broken.
	ErrInvariantDuplicate = errors.New("goal: duplicate invariant id")
	// ErrInvariantRelaxed reports an invariant dropped or reworded after the run
	// adopted it: the terms of the run being renegotiated by the run.
	ErrInvariantRelaxed = errors.New("goal: invariant was relaxed after the run adopted it")
)

// Invariant is one term of the run: something that must hold throughout, stated in the
// author's words, with an optional declared way to observe it.
type Invariant struct {
	// ID names the term so a breach can point at it and a status entry can track it
	// across reconciles. It is the author's, like a unit id, because the statement is
	// meant to be legible and a content address is unwritable by hand.
	ID string `json:"id"`
	// Statement is what must stay true, in the author's words. It is what the auditor
	// rules against and what a breach quotes back, so it is written to be read by
	// whoever is handed the stopped goal.
	Statement string `json:"statement"`
	// Check is the declared way to observe the statement: the command to run, the
	// search to make, the place to look. A zero exit says the term holds.
	//
	// It is optional for a term that claims something about what the run did, since an
	// auditor handed one with no check can rule on the run's record. It is required for
	// a term that claims something is not there, because the record cannot settle that:
	// admission refuses an absence claim with no search (see AssertsAbsence). Where a
	// check can be written, writing it is what turns the term from a judgement call
	// into an observation.
	Check string `json:"check,omitempty"`
}

// InvariantMark returns the fingerprint of an invariant's content: the first
// invariantMarkLen hex characters of a SHA-256 over its id, statement and check,
// NUL-separated so no two different invariants can be re-split into the same byte
// string. It is what makes the no-relax rule enforceable: the status remembers what the
// term said when it was adopted, so a later spec that softens the wording is caught
// rather than accepted as the same term.
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
	// Mark is InvariantMark of the invariant as it read when the run adopted it. A
	// later spec whose invariant of the same id marks differently has reworded a term
	// the run is already being held to, and admission refuses it.
	Mark string `json:"mark"`
	// LastAudited is when an auditor last ruled on this term. Nil means the term is
	// carried but has not been checked yet, which is every term on a goal whose first
	// step has not completed.
	LastAudited *time.Time `json:"lastAudited,omitempty"`
	// Audits counts how many times this term has been ruled on, so a stopped goal can
	// show that a term was actually checked rather than merely carried.
	Audits int `json:"audits,omitempty"`
	// Breached records that the term was found broken, and Detail is what the auditor
	// found. It is never cleared: a breach is a fact about what the run did, not a
	// state the run can work its way back out of.
	Breached bool   `json:"breached,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Breach is an auditor's finding that one term of the run has been broken, naming the
// term and what was observed. Detail is what gets quoted to whoever is handed the
// stopped goal, so it says what happened, not that something happened.
type Breach struct {
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// InvariantAuditor rules on whether a run has broken any of its terms. It is handed the
// goal's record, its spec and the status this reconcile is working on, and the terms it
// carries, and returns a finding per broken term. Every term is offered every time: a
// term is a standing obligation, so one that held at step 3 says nothing about step 4.
//
// The status is passed separately from r because it is the one this pass has built and
// not yet written: an auditor reading r.Status would be ruling on the run as it stood
// before the step it is being asked about.
//
// It is a port, not a policy: the production auditor runs each term's declared check
// against the run's own workspace, and a test supplies a deterministic one. An auditor
// that cannot answer returns an error rather than an empty finding, because "I could not
// check" and "I checked and it holds" are the difference between a guard and a
// formality, and only the auditor can say whether its failure is transient.
type InvariantAuditor interface {
	Audit(ctx context.Context, r resource.Resource, spec Spec, status Status, terms []Invariant) ([]Breach, error)
}

// ValidateInvariants refuses a set of terms that could never be audited: one missing its
// id or statement, two claiming the same id, or one asserting an absence with no search
// declared to find a counterexample. It runs at admission, before anything is dispatched,
// so a goal is never part-way through work it is being held to terms it cannot be judged
// against.
//
// The absence rule is the one with teeth, because the term it refuses is the one that
// would otherwise pass. A term saying no credentials remain, audited by reading the run's
// account of itself, is settled by a sentence the run writes about a search it may never
// have made, and it settles that way most easily for exactly the run that did not look.
// See absence.go for why a search is the only thing that can settle it.
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
// has adopted: every adopted term must still be present, and must still fingerprint to
// the mark it was adopted under. A term that is gone, or that now reads differently, is
// the run renegotiating what it agreed to, and it is refused.
//
// New terms pass: the check is one-directional on purpose. Tightening the terms of a run
// that is already going is a legitimate thing for its author to do, and there is no
// version of "the run got out of an obligation" that involves acquiring another one.
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

// SyncInvariants brings the status's observation of the terms into line with the spec,
// adopting any term it has not seen before and preserving what is already recorded
// about the rest. Adoption is what commits a term: from here ValidateInvariantsAdopted
// will refuse to let it be dropped or reworded.
//
// A term already adopted is kept even if the spec no longer carries it, because losing
// it is exactly what admission refuses, and this runs after that check: the entry
// surviving here is what a later admission catches the drop against.
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

// RecordAudit folds an audit into the status: every term that was checked has its audit
// counted and stamped, and every finding is recorded against its term. It reports
// whether any term is now breached.
//
// A finding naming a term the goal does not carry is ignored rather than trusted. The
// auditor is handed the terms and rules on those; a finding about something else is a
// breach of a term nobody agreed to, and recording it would let an auditor stop a run
// for a reason its author never wrote down.
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

// BreachedInvariant returns the first term recorded as broken, and whether there is one.
// It is what makes a breach outlive the reconcile that found it: the reconciler asks
// this before it asks the stop evaluator anything, so a goal carrying a breach cannot be
// judged done on a later pass, whatever its spec has been edited to say since.
func (s Status) BreachedInvariant() (InvariantState, bool) {
	for _, st := range s.Invariants {
		if st.Breached {
			return st, true
		}
	}
	return InvariantState{}, false
}

// BreachReason is the message a breached goal settles under: the term that was broken,
// quoted, and what the audit found. The statement is looked up from the spec so the
// message reads as the term rather than as its id, and falls back to the id for a term
// the spec no longer carries.
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
