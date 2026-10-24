package goal

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/resource"
)

const (
	// RefusalRetryLimit is how many times one action may be refused by one rule before the
	// run stops. One retry is worth allowing (a gate's answer can change when a budget
	// window rolls or an approval lands); a third identical refusal is a run that is not
	// listening.
	RefusalRetryLimit = 3
	// RefusalRouteLimit is how many distinct actions one rule may refuse before the run
	// stops. Two is still likelier an honestly-scoped run meeting a grant it was not given;
	// by the third the run should be asking for authority and not looking for a way around.
	RefusalRouteLimit = 3
)

// Refusal is one action the waist refused, as read back off the run's record. Rule names
// what refused it (the fault code: capability_denied, containment_unavailable) and Action
// what was refused. The class is left out because several rules share one, and a count
// keyed on it would fold unrelated gates together and read a run that met three walls as
// one that worked around one.
type Refusal struct {
	Rule   string
	Action string
}

// RefusalProbe reads the refusals recorded for a goal on the run's durable record, in the
// order the waist refused them. It reads what happened and not what the run says
// happened: a run that routes around a gate reports each step as fine.
//
// A probe returns every refusal for the goal, including ones already seen. The
// reconciler derives its verdict from the whole record each pass and banks no count, so a
// tally cannot be lost to a crash or spent by a status write. A probe error is the
// reconciler's to classify; a read that failed for a moment must not read as a clean run.
type RefusalProbe interface {
	Refusals(ctx context.Context, r resource.Resource) ([]Refusal, error)
}

// RefusalVerdict is a run stopped by its own refusals: which rule kept refusing it, and
// the actions it was refused for, in first-refusal order.
//
// A refused gate is a verdict about the run. It catches a run that reformulates instead
// of stopping (rewording to clear a classifier, substituting an unhooked tool, splitting
// a disallowed action into allowed steps); each step defends itself and the trajectory
// does not, so the whole run's refusals are read. Two shapes count, per rule:
//
//   - Persistence: one action refused by one rule [RefusalRetryLimit] times. The run
//     asked again unchanged.
//   - Substitution: [RefusalRouteLimit] distinct actions refused by one rule. The run
//     came back by another door, or its grant is too narrow; it should stop and say so.
//
// A substitution that succeeds is invisible here; closing that is gate coverage.
type RefusalVerdict struct {
	// Rule is the gate that refused.
	Rule string
	// Actions are the distinct actions that rule refused, first-refusal order.
	Actions []string
	// Count is how many refusals that rule issued in total.
	Count int
	// Routed reports which shape fired: true for distinct actions reaching
	// RefusalRouteLimit, false for one action reaching RefusalRetryLimit.
	Routed bool
	// Repeated is the action that reached the retry limit, and Repeats how many times it
	// was refused. Both are set only on a retry verdict, whose message must name the one
	// action asked again and not every action the rule ever refused.
	Repeated string
	Repeats  int
}

// ReadRefusals returns the verdict a run's recorded refusals amount to, and whether there
// is one at all.
//
// A substitution is reported ahead of a persistent retry, as the more serious account of
// the same history. Within a shape the rule that refused most is taken, and a tie breaks
// by rule name so the same record always yields the same verdict.
func ReadRefusals(refusals []Refusal) (RefusalVerdict, bool) {
	type tally struct {
		actions []string        // distinct, first-refusal order
		perAct  map[string]int  // refusals per action
		seen    map[string]bool // membership for actions
		total   int             // refusals under this rule
	}
	rules := map[string]*tally{}
	for _, ref := range refusals {
		rule := strings.TrimSpace(ref.Rule)
		if rule == "" {
			// A refusal naming no rule cannot be attributed to a gate, and one bucket for all
			// of them would invent a rule that refused everything.
			continue
		}
		t := rules[rule]
		if t == nil {
			t = &tally{perAct: map[string]int{}, seen: map[string]bool{}}
			rules[rule] = t
		}
		t.total++
		act := strings.TrimSpace(ref.Action)
		if !t.seen[act] {
			t.seen[act] = true
			t.actions = append(t.actions, act)
		}
		t.perAct[act]++
	}

	names := make([]string, 0, len(rules))
	for name := range rules {
		names = append(names, name)
	}
	sort.Strings(names)

	var best RefusalVerdict
	found := false
	for _, name := range names {
		t := rules[name]
		routed := len(t.actions) >= RefusalRouteLimit
		// The most-refused action decides the retry shape, walked in first-refusal order
		// so the same record names the same one.
		repeated, repeats := "", 0
		for _, act := range t.actions {
			if t.perAct[act] > repeats {
				repeated, repeats = act, t.perAct[act]
			}
		}
		if !routed && repeats < RefusalRetryLimit {
			continue
		}
		v := RefusalVerdict{Rule: name, Actions: t.actions, Count: t.total, Routed: routed}
		if !routed {
			v.Repeated, v.Repeats = repeated, repeats
		}
		if !found || outranks(v, best) {
			best, found = v, true
		}
	}
	return best, found
}

// outranks reports whether a is the better account of a run than b: a substitution ahead
// of a retry, then the rule that refused most. The caller visits names in sorted order, so
// an exact tie keeps the earlier one.
func outranks(a, b RefusalVerdict) bool {
	if a.Routed != b.Routed {
		return a.Routed
	}
	return a.Count > b.Count
}

// RefusalReason is the stall message a refusal verdict carries. It says what was refused
// and by what, because the person it is handed back to must choose between widening the
// authority and abandoning the objective, and cannot from "blocked".
func (v RefusalVerdict) RefusalReason() string {
	if v.Routed {
		return fmt.Sprintf("stopped by %s: refused %d times across %d different actions (%s); "+
			"a gate that keeps refusing is an answer about this run, and going round it by another route is not progress",
			v.Rule, v.Count, len(v.Actions), strings.Join(v.Actions, ", "))
	}
	return fmt.Sprintf("stopped by %s: %s was refused %d times unchanged; "+
		"a gate that keeps refusing is an answer about this run, and asking it again is not progress",
		v.Rule, v.Repeated, v.Repeats)
}
