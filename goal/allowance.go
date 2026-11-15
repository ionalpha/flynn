package goal

import (
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/allowance"
)

// Allowance is the spec form of [allowance.Declaration]. An unattended run cannot answer a
// prompt and a bare refusal invites routing around it ([RefusalVerdict]), so the goal
// parks with an [AllowanceAsk] instead.
//
// The record names the refused action without its target, so any declaration of the
// action clears the ask. The gate still refuses an undeclared target.
type Allowance struct {
	// Action is matched exactly.
	Action string `json:"action"`
	// Target empty covers any target, the only form for a call site that names none.
	Target string `json:"target,omitempty"`
}

// Declarations converts allowances for [allowance.Into].
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

// AllowanceCovers ignores targets, because the record cannot answer more (see
// [Allowance]).
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

// AllowanceAsk is a run parked on an undeclared irreversible action.
type AllowanceAsk struct {
	Action string
	// Attempts tells one attempt from a run that kept trying.
	Attempts int
}

// ReadAllowanceAsk names the first undeclared refused action, so the ask names what stopped
// the run first. A refusal the spec now declares is no ask, so a goal resumes without its
// history being edited.
func ReadAllowanceAsk(refusals []Refusal, alls []Allowance) (AllowanceAsk, bool) {
	counts := map[string]int{}
	first := ""
	for _, ref := range refusals {
		if strings.TrimSpace(ref.Rule) != allowance.CodeRequired {
			continue
		}
		act := strings.TrimSpace(ref.Action)
		if act == "" || AllowanceCovers(alls, act) {
			// An unattributable refusal is not folded into an action nothing tried.
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

// AskReason is written for the person who must answer it.
func (a AllowanceAsk) AskReason() string {
	msg := fmt.Sprintf("paused: %s reaches outside the workspace and cannot be undone, "+
		"and this run was not given it", a.Action)
	if a.Attempts > 1 {
		msg += fmt.Sprintf(" (refused %d times)", a.Attempts)
	}
	return msg + "; declare an allowance for it on the goal to let the run continue, " +
		"or leave it undeclared and the run is over"
}
