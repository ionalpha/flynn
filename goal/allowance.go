package goal

import (
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/allowance"
)

// Allowance is one standing authorization on the goal spec: an action the run may take
// even though it reaches outside the workspace irreversibly, optionally narrowed to a
// target. It is the spec's form of allowance.Declaration, which is what the waist checks.
//
// An irreversible action outside the workspace is never inferred from the objective: it
// is declared in advance by the person who wrote the objective, or the run stops and
// asks. Nobody is watching a goal-mode run, so a confirmation prompt would hang it, and
// handing the model a refusal invites it to route around the gate (see
// [RefusalVerdict]). So the goal parks with an [AllowanceAsk] naming the action, and
// resumes once the spec declares it. The pause is derived from the record on every
// pass, which is what makes declaring the allowance release it.
//
// A refusal on the record names the action and not its target, so any declaration of
// that action silences the ask, including one narrowed to a different target. The waist
// still refuses the undeclared target; what is lost is the pause, not the protection.
//
// A declaration naming no action is refused by the spec schema, before a goal carrying one
// is ever stored: it would authorize nothing while reading like it authorizes something,
// which is the worst thing a line on a spec can do.
type Allowance struct {
	// Action is the dispatch action name being authorized, matched exactly.
	Action string `json:"action"`
	// Target narrows the authorization to one target the acting call site names. Empty
	// authorizes the action against any target, which is the widest form and the only one
	// available where a call site names no target.
	Target string `json:"target,omitempty"`
}

// Declarations renders the spec's allowances in the form the waist reads, for the
// composition that binds a run's authority before it dispatches anything.
func Declarations(alls []Allowance) []allowance.Declaration {
	if len(alls) == 0 {
		return nil
	}
	out := make([]allowance.Declaration, 0, len(alls))
	for _, a := range alls {
		out = append(out, allowance.Declaration{Action: a.Action, Target: a.Target})
	}
	return out
}

// AllowanceCovers reports whether the spec declares the named action at all, whatever
// target any declaration narrows it to. It is the coverage question the record can actually
// answer: see the note above on what a refusal does not carry.
func AllowanceCovers(alls []Allowance, action string) bool {
	action = strings.TrimSpace(action)
	if action == "" {
		return false
	}
	for _, a := range alls {
		if strings.TrimSpace(a.Action) == action {
			return true
		}
	}
	return false
}

// AllowanceAsk is a run parked on an undeclared irreversible action: what it tried to do,
// and how many times it tried.
type AllowanceAsk struct {
	// Action is the action the waist refused for want of a declaration.
	Action string
	// Attempts is how many times the run was refused for that action, so the person
	// handed the goal can tell one attempt from a run that kept trying.
	Attempts int
}

// ReadAllowanceAsk returns the ask a run's recorded refusals amount to, and whether there
// is one. It is the first action refused for want of a declaration that the spec still does
// not declare, in the order the waist refused them, so the ask names what stopped the run
// first rather than whichever refusal happens to sort earliest.
//
// A refusal for an action the spec now declares is not an ask. That is what lets a paused
// goal resume: the author adds the declaration, the same record now reads as answered, and
// the run carries on without the refusal having to be edited out of its history.
func ReadAllowanceAsk(refusals []Refusal, alls []Allowance) (AllowanceAsk, bool) {
	counts := map[string]int{}
	first := ""
	for _, ref := range refusals {
		if strings.TrimSpace(ref.Rule) != allowance.CodeRequired {
			continue
		}
		act := strings.TrimSpace(ref.Action)
		if act == "" || AllowanceCovers(alls, act) {
			// An unattributable refusal is counted by nobody rather than folded into a
			// bucket that would name an action nothing tried.
			continue
		}
		if first == "" {
			first = act
		}
		counts[act]++
	}
	if first == "" {
		return AllowanceAsk{}, false
	}
	return AllowanceAsk{Action: first, Attempts: counts[first]}, true
}

// AskReason is the message a paused goal carries. It is written to the person who has to
// answer it, so it says which action needs declaring and what declaring it means, rather
// than reporting that the run was blocked.
func (a AllowanceAsk) AskReason() string {
	msg := fmt.Sprintf("paused: %s reaches outside the workspace and cannot be undone, "+
		"and this run was not given it", a.Action)
	if a.Attempts > 1 {
		msg += fmt.Sprintf(" (refused %d times)", a.Attempts)
	}
	return msg + "; declare an allowance for it on the goal to let the run continue, " +
		"or leave it undeclared and the run is over"
}
