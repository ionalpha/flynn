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

// invariantMarkLen is 64 bits, past collision range for one goal's terms.
const invariantMarkLen = 16

var (
	// ErrInvariantIncomplete reports an invariant missing its id or statement, which an
	// auditor would have to invent.
	ErrInvariantIncomplete = errors.New("goal: invariant needs an id and a statement")
	// ErrInvariantDuplicate reports a shared id, which makes a breach ambiguous.
	ErrInvariantDuplicate = errors.New("goal: duplicate invariant id")
	// ErrInvariantRelaxed reports an adopted invariant dropped or reworded.
	ErrInvariantRelaxed = errors.New("goal: invariant was relaxed after the run adopted it")
)

// Invariant is a term that must stay true throughout, against the pressure of the stop
// condition. An [InvariantAuditor] rules on it before the stop evaluator, so a breach
// settles the goal whatever the evaluator says. Relaxing one is [ErrInvariantRelaxed].
// A breach is kept on the status, so a spec edit cannot erase it. Terms with no auditor
// stall before the first step, and an absence claim needs a Check ([AssertsAbsence]).
type Invariant struct {
	// ID is author-written, because a content address cannot be typed by hand.
	ID string `json:"id"`
	// Statement is what the auditor rules against and a breach quotes.
	Statement string `json:"statement"`
	// Check is a command whose zero exit says the term holds. Admission requires it for an
	// absence claim, because the record cannot show something is not there.
	Check string `json:"check,omitempty"`
}

// InvariantMark hashes id, statement and check NUL-separated, so no two invariants share
// bytes. The status keeps it from adoption, so softened wording is caught.
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

// InvariantState is one term's adoption mark, audit history and breach.
type InvariantState struct {
	ID string `json:"id"`
	// Mark is [InvariantMark] at adoption; admission refuses a later mismatch.
	Mark string `json:"mark"`
	// LastAudited nil means carried but not yet checked.
	LastAudited *time.Time `json:"lastAudited,omitempty"`
	// Audits shows a stopped goal's term was checked, and not merely carried.
	Audits int `json:"audits,omitempty"`
	// Breached is never cleared, so the run cannot work its way out of a breach.
	Breached bool   `json:"breached,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Breach is quoted to whoever gets the stopped goal, so Detail says what happened.
type Breach struct {
	ID     string `json:"id"`
	Detail string `json:"detail"`
}

// InvariantAuditor returns one finding per broken term. Every term is offered every time,
// because a term that held at step 3 says nothing about step 4. status is this pass's
// unwritten status, because r.Status predates the step. An auditor that cannot check
// returns an error, because only it knows if the failure is transient.
type InvariantAuditor interface {
	Audit(ctx context.Context, r resource.Resource, spec Spec, status Status, terms []Invariant) ([]Breach, error)
}

// ValidateInvariants runs at admission. It refuses an absence claim with no Check,
// because "no credentials remain" audited from the record passes most easily for the run
// that did not look.
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

// ValidateInvariantsAdopted lets new terms pass, because another obligation is never a
// way out of one.
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

// SyncInvariants adopts new terms, after which [Status.ValidateInvariantsAdopted] holds
// them. A dropped term's entry is kept, because a later admission catches the drop by it.
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

// RecordAudit reports whether any term is now breached. A finding for a term the goal
// does not carry is ignored, so an auditor cannot stop a run on an unwritten term.
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
			continue
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

// BreachedInvariant is asked before the stop evaluator, so a breached goal is never
// judged done, whatever the spec says since.
func (s Status) BreachedInvariant() (InvariantState, bool) {
	for _, st := range s.Invariants {
		if st.Breached {
			return st, true
		}
	}
	return InvariantState{}, false
}

// BreachReason falls back to the id for a term the spec has dropped.
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
