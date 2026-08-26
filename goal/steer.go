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

// A steer is what an operator says to a run that is already going: use the other table,
// stop touching the migration, this is the wrong branch. It leaves the objective and the
// stop condition exactly as they were, and the run is held to it separately.
//
// Steering that is only injected as text is steering that gets ignored. The clearest
// record of that is a timeline rather than an argument: a human sent five messages in
// thirty minutes, each folded into the prompt as advisory prose, and the loop finished the
// build, passed its own review ten seconds after being asked to read the guidance, and
// committed thirty-eight seconds later. Nothing in that run was broken. Every message
// arrived, and none of them changed a decision, because nothing downstream of the prompt
// ever asked whether they had been addressed.
//
// So the primitive is not "deliver the text". It is that the run cannot report success
// while an obligation is outstanding.
//
// Three rules are what make it an obligation rather than a suggestion.
//
// It is delivered on every turn it survives, not once. A steer folded into the transcript
// at the moment it arrived is one message in a history that gets pruned, compacted and
// reseeded, and the turn that finally claims completion may no longer be able to see it.
// Redelivery from the durable record is what makes surviving a reseed a property of the
// steer rather than a property of how the transcript happened to be trimmed.
//
// It is discharged by an account, and the account is judged. A run says how it addressed
// the redirect, and something other than the run rules on whether that answer addresses it.
// A steer nobody rules on could only be discharged by the run asserting it had complied,
// which is the same standard the whole ledger exists to replace. Where no judge is wired
// the goal stops and names the missing judge, rather than carrying an obligation it has no
// way to ever discharge (see steerrun.go).
//
// It cannot be withdrawn by the run. Once a reconcile has adopted a steer, dropping it or
// rewording it is refused as a terminal spec fault, exactly as for an invariant and for the
// same reason: the one party who must not be able to edit an obligation is the party under
// it, and the spec is not a surface only the operator can reach. Issuing another steer is
// always allowed, which is the route to correcting one that was wrong.
//
// A steer is not an amendment, and the difference is what it is judged against. Amending
// the objective rewrites what done means, so the grader then rules on the amended text and
// the redirect disappears into the definition of success. A steer leaves the definition of
// success alone and is checked on its own, which is why "you are using the wrong table,
// keep going" belongs here and not in the objective.

// steerMarkLen is how many hex characters of a steer's content hash form its fingerprint.
// Sixteen (64 bits) is far past collision range for the handful of redirects one run
// takes, and short enough to stay readable in a status dump.
const steerMarkLen = 16

// Steer errors. They separate a redirect that could never be delivered from one the run
// got out from under, because the two say different things: the first is an operator
// typing something incomplete, the second is the failure mode this exists for.
var (
	// ErrSteerIncomplete reports a steer missing its id or its instruction. A redirect
	// with nothing written in it cannot be delivered to the run or ruled on afterwards.
	ErrSteerIncomplete = errors.New("goal: steer needs an id and an instruction")
	// ErrSteerDuplicate reports two steers claiming the same id, which would make an
	// acknowledgement ambiguous about which redirect it answered.
	ErrSteerDuplicate = errors.New("goal: duplicate steer id")
	// ErrSteerWithdrawn reports a steer dropped or reworded after the run was handed it:
	// the obligation being edited by the party under it.
	ErrSteerWithdrawn = errors.New("goal: steer was withdrawn after the run was given it")
)

// Steer is one redirect issued to a running goal, in the operator's words.
type Steer struct {
	// ID names the redirect so an acknowledgement can answer this one and a status entry
	// can track it across reconciles. It is the operator's, like an invariant's, because a
	// content address is unwritable by hand and this is written by hand under time
	// pressure.
	ID string `json:"id"`
	// Instruction is what to do differently, in the operator's words. It is delivered to
	// the run verbatim and quoted back in a refusal, so it is read by both the model that
	// has to act on it and the person handed the stopped goal.
	Instruction string `json:"instruction"`
}

// SteerMark returns the fingerprint of a steer's content: the first steerMarkLen hex
// characters of a SHA-256 over its id and instruction, NUL-separated so no two different
// steers can be re-split into the same byte string. It is what makes the no-withdrawal
// rule enforceable: the status remembers what the redirect said when the run was given it,
// so a later spec that softens the wording is caught rather than accepted as the same
// redirect.
func SteerMark(st Steer) string {
	var b strings.Builder
	b.WriteString(st.ID)
	b.WriteByte(0)
	b.WriteString(st.Instruction)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:steerMarkLen]
}

// SteerState is the observed state of one redirect: the content it was adopted under, and
// the acknowledgement that discharged it if one has been accepted.
type SteerState struct {
	// ID is the steer this entry observes.
	ID string `json:"id"`
	// Mark is SteerMark of the steer as it read when the run was given it. A later spec
	// whose steer of the same id marks differently has reworded an obligation the run is
	// already under, and admission refuses it.
	Mark string `json:"mark"`
	// Acknowledged records that a judge accepted the run's account of how this redirect
	// was addressed. Until it is set the redirect is outstanding: it is redelivered every
	// turn and it refuses a completion claim.
	Acknowledged bool `json:"acknowledged,omitempty"`
	// Account is what the run said it did about the redirect, kept verbatim. It is the
	// part of the record worth reading later: whether a steer was honored is a question
	// about this sentence, and a bare acknowledged flag would answer it with "yes"
	// forever.
	Account string `json:"account,omitempty"`
	// AcknowledgedAt is when the acknowledgement was accepted. Nil while the redirect is
	// outstanding.
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
}

// Acknowledgement is a judge's finding that a run's account addresses one outstanding
// redirect, naming the redirect and how it was addressed. How is what gets recorded on the
// status and quoted in the run's record, so it says what the run did, not that it did
// something.
type Acknowledgement struct {
	ID  string `json:"id"`
	How string `json:"how"`
}

// SteerJudge rules on whether a run's account of its work addresses the redirects it is
// still under. It is handed the goal's record, its spec, the status this reconcile has
// built, the outstanding redirects and the run's own account, and returns one finding per
// redirect the account addresses.
//
// Silence is a refusal. A redirect the judge does not name stays outstanding, so a judge
// that answers half the question leaves the other half unpaid, and a judge that answers
// nothing discharges nothing. That direction is deliberate: the expensive error here is
// accepting an account that says nothing about the redirect, and the cheap one is asking a
// run to say again what it did.
//
// It is a port, not a policy. "Did this answer address that instruction" is a small
// classification and a good candidate for a cheap model tier, where a cheap model saying
// no keeps the obligation open, which is the safe direction. A judge that cannot answer
// returns an error rather than an empty finding, because "I could not tell" and "it does
// not address it" are different, and only the judge knows whether its failure is
// transient.
type SteerJudge interface {
	Acknowledged(ctx context.Context, r resource.Resource, spec Spec, status Status, outstanding []Steer, account string) ([]Acknowledgement, error)
}

// ValidateSteers refuses a set of redirects that could never be delivered or answered: one
// missing its id or instruction, or two claiming the same id. It runs at admission, before
// anything is dispatched.
func ValidateSteers(steers []Steer) error {
	seen := make(map[string]bool, len(steers))
	for i, st := range steers {
		if st.ID == "" || strings.TrimSpace(st.Instruction) == "" {
			return fmt.Errorf("%w: steer %d", ErrSteerIncomplete, i)
		}
		if seen[st.ID] {
			return fmt.Errorf("%w: %q", ErrSteerDuplicate, st.ID)
		}
		seen[st.ID] = true
	}
	return nil
}

// ValidateSteersGiven checks the redirects on the spec against the ones the status records
// the run as having been given: every one must still be present, and must still fingerprint
// to the mark it was given under. One that is gone, or that now reads differently, is the
// obligation being renegotiated, and it is refused.
//
// New redirects pass, and an acknowledged one is held to the same rule as an outstanding
// one. Deleting a discharged redirect would erase the account that discharged it, and the
// account is the only durable evidence that the operator was answered at all.
func (s Status) ValidateSteersGiven(steers []Steer) error {
	if len(s.Steers) == 0 {
		return nil
	}
	by := make(map[string]Steer, len(steers))
	for _, st := range steers {
		by[st.ID] = st
	}
	for _, given := range s.Steers {
		cur, ok := by[given.ID]
		if !ok {
			return fmt.Errorf("%w: %q was dropped", ErrSteerWithdrawn, given.ID)
		}
		if mark := SteerMark(cur); mark != given.Mark {
			return fmt.Errorf("%w: %q was reworded (given %s, now %s)", ErrSteerWithdrawn, given.ID, given.Mark, mark)
		}
	}
	return nil
}

// SyncSteers brings the status's record of the redirects into line with the spec, taking
// on any it has not seen before and preserving what is recorded about the rest. Taking one
// on is what commits it: from here ValidateSteersGiven will refuse to let it be dropped or
// reworded.
//
// A new redirect also clears the non-convergence count. That count is how many cycles in a
// row ended in the same refusal with nothing proven in between, and it stops a run that is
// being told nothing new (converge.go). An operator issuing a redirect is the run being
// told something new by the one party whose input the count was standing in for, so
// carrying the old count forward would stop the run for the absence of exactly what just
// arrived.
func (s *Status) SyncSteers(steers []Steer) {
	have := make(map[string]bool, len(s.Steers))
	for _, given := range s.Steers {
		have[given.ID] = true
	}
	for _, st := range steers {
		if have[st.ID] {
			continue
		}
		s.Steers = append(s.Steers, SteerState{ID: st.ID, Mark: SteerMark(st)})
		s.VerdictMark, s.VerdictRepeat, s.LastVerdict = "", 0, ""
	}
}

// OutstandingSteers returns the redirects the run is still under, in spec order: every one
// whose status entry does not carry an accepted acknowledgement.
//
// A redirect with no status entry counts as outstanding. Being taken on is what the run
// records about a redirect, never what discharges it, so a redirect the status has not
// caught up with is one nobody has answered, and reading it any other way would make a
// status write that has not landed yet into a way to skip an obligation.
func (s Status) OutstandingSteers(steers []Steer) []Steer {
	if len(steers) == 0 {
		return nil
	}
	done := make(map[string]bool, len(s.Steers))
	for _, given := range s.Steers {
		if given.Acknowledged {
			done[given.ID] = true
		}
	}
	var out []Steer
	for _, st := range steers {
		if !done[st.ID] {
			out = append(out, st)
		}
	}
	return out
}

// RecordAcknowledgements folds a judge's findings into the status and returns the
// redirects still outstanding afterwards, in the order they were offered.
//
// A finding naming a redirect that was not offered is ignored rather than trusted. The
// judge is handed the outstanding redirects and rules on those; a finding about anything
// else would discharge an obligation the judge was never asked about, including one
// already discharged, whose recorded account it would overwrite.
func (s *Status) RecordAcknowledgements(outstanding []Steer, acks []Acknowledgement, now time.Time) []Steer {
	addressed := make(map[string]string, len(acks))
	for _, a := range acks {
		addressed[a.ID] = a.How
	}
	by := make(map[string]int, len(s.Steers))
	for i, given := range s.Steers {
		by[given.ID] = i
	}
	var open []Steer
	for _, st := range outstanding {
		how, ok := addressed[st.ID]
		if !ok {
			open = append(open, st)
			continue
		}
		i, tracked := by[st.ID]
		if !tracked {
			// Not taken on yet, so there is nowhere durable to record the discharge. It
			// stays outstanding and is judged again on the pass that has its entry.
			open = append(open, st)
			continue
		}
		stamp := now
		s.Steers[i].Acknowledged = true
		s.Steers[i].Account = how
		s.Steers[i].AcknowledgedAt = &stamp
	}
	return open
}

// SteerBrief renders the redirects a run is still under as the turn's standing
// instruction, or "" when it is under none. It is what the executor folds into every turn
// so the obligation is in front of the model each time it decides what to do, and it
// states the discharge rule alongside the redirect, because a run that is told the rule
// can answer it and a run that is not will be refused for failing a test nobody described.
func SteerBrief(spec Spec, status Status) string {
	outstanding := status.OutstandingSteers(spec.Steers)
	if len(outstanding) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The operator has redirected this run. The objective and the definition of done are unchanged; these are corrections to how you get there:\n")
	for _, st := range outstanding {
		fmt.Fprintf(&b, "- %s\n", st.Instruction)
	}
	b.WriteString("\nFollow them from here. Before you finish, state for each one what you did about it. ")
	b.WriteString("A completion claim that leaves one unanswered is refused and the run stops.")
	return b.String()
}

// UnacknowledgedReason is the message a goal settles under when it reported completion
// with redirects still outstanding: what the operator asked for, and what the run said
// instead. Both halves are quoted, because the useful question afterwards is which of the
// two happened, and the account is the only place that can be read from.
func UnacknowledgedReason(outstanding []Steer, account string) string {
	quoted := make([]string, 0, len(outstanding))
	for _, st := range outstanding {
		quoted = append(quoted, fmt.Sprintf("%q", st.Instruction))
	}
	msg := fmt.Sprintf("completion reported with %d operator redirect(s) unaddressed: %s",
		len(quoted), strings.Join(quoted, "; "))
	if strings.TrimSpace(account) != "" {
		msg += fmt.Sprintf("; the run's account of finishing was %q", account)
	}
	return msg
}
