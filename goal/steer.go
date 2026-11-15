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

// steerMarkLen is 64 bits, past collision range for one run's redirects.
const steerMarkLen = 16

var (
	// ErrSteerIncomplete reports a steer missing its id or instruction.
	ErrSteerIncomplete = errors.New("goal: steer needs an id and an instruction")
	// ErrSteerDuplicate reports a shared id, which makes an acknowledgement ambiguous.
	ErrSteerDuplicate = errors.New("goal: duplicate steer id")
	// ErrSteerWithdrawn reports a steer dropped or reworded after the run was handed it.
	ErrSteerWithdrawn = errors.New("goal: steer was withdrawn after the run was given it")
)

// Steer is an operator redirect held apart from the objective, because an amendment would
// change what done means and lose the redirect. It is redelivered every turn from the
// durable record, so compaction cannot drop it. A [SteerJudge] rules on the run's account,
// never the run. Dropping or rewording one is [ErrSteerWithdrawn].
type Steer struct {
	// ID is operator-written, because a content address cannot be typed by hand.
	ID string `json:"id"`
	// Instruction is delivered verbatim and quoted in a refusal.
	Instruction string `json:"instruction"`
}

// SteerMark hashes id and instruction NUL-separated, so no two steers share bytes. The
// status keeps it from adoption, so softened wording is caught.
func SteerMark(st Steer) string {
	var b strings.Builder
	b.WriteString(st.ID)
	b.WriteByte(0)
	b.WriteString(st.Instruction)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:steerMarkLen]
}

// SteerState is one redirect's adoption mark and discharge.
type SteerState struct {
	ID string `json:"id"`
	// Mark is [SteerMark] at adoption; admission refuses a later mismatch.
	Mark string `json:"mark"`
	// Acknowledged unset keeps the redirect redelivered and refusing completion.
	Acknowledged bool `json:"acknowledged,omitempty"`
	// Account is kept verbatim, because a bare flag would say "yes" forever.
	Account string `json:"account,omitempty"`
	// AcknowledgedAt is nil while outstanding.
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
}

// Acknowledgement is recorded and quoted, so How says what the run did.
type Acknowledgement struct {
	ID  string `json:"id"`
	How string `json:"how"`
}

// SteerJudge returns one finding per redirect addressed. An unnamed redirect stays
// outstanding, because a wrong acceptance costs more than asking again. A judge that
// cannot answer returns an error, because only it knows if the failure is transient.
type SteerJudge interface {
	Acknowledged(ctx context.Context, r resource.Resource, spec Spec, status Status, outstanding []Steer, account string) ([]Acknowledgement, error)
}

// ValidateSteers runs at admission.
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

// ValidateSteersGiven holds acknowledged redirects too, because deleting one erases the
// only evidence the operator was answered. New redirects pass.
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

// SyncSteers adopts new redirects, after which [Status.ValidateSteersGiven] holds them. A
// new redirect clears the non-convergence count, because it is news to the run.
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

// OutstandingSteers counts a redirect with no status entry as outstanding, so a lost
// write cannot skip an obligation.
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

// RecordAcknowledgements ignores a finding for a redirect not offered, because it would
// discharge an unasked obligation or overwrite a recorded account.
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
			// No entry to record it in yet, so it is judged again later.
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

// SteerBrief states the discharge rule, because a run not told it is refused for a test
// nobody described.
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

// UnacknowledgedReason quotes both the redirect and the run's account, so a reader can
// see which happened.
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
