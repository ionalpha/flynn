package goal

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/resource"
)

// A refused gate is a verdict about the run, not an obstacle on the way to finishing it.
//
// The failure this exists for: a gate fires, and the run reformulates instead of stopping.
// It rewords to clear a classifier, substitutes an equivalent tool that is not hooked,
// splits one disallowed action into individually allowed steps. Every step defends itself
// in isolation, the trajectory does not, and at the end the cleared gate reads as resolved.
// Nothing in a step-by-step account of the run catches it, because no step is the problem.
//
// The waist is what makes it observable. Every governed action passes one chokepoint, so
// every refusal lands on one stream with the rule that refused it and the action it
// refused, and a whole run's refusals can be read together. A harness that governs at each
// call site has nowhere to ask this question from.
//
// Two shapes are counted, and they are counted per rule, because the rule is what the run
// is being told no by.
//
//   - Persistence: the same action refused by the same rule, over and over. The run was
//     told no and asked again unchanged.
//   - Substitution: several different actions refused by one rule. The run was told no and
//     came back by another door.
//
// Substitution is the one the report names and it is the harder of the two to be sure
// about, because two actions refused by one gate can also be an honestly-scoped run that
// needs two things it was not given. That is why the threshold is three rather than two,
// and why the outcome is the same either way: a run that has been refused by one gate
// three different ways should stop and say so, whether it was probing for a way through or
// discovering that its grant is too narrow. Both are the run's author's to resolve, and
// neither is something to keep working around.
//
// What this cannot see: a substitution that succeeds. If the route the run tries second is
// not hooked, the waist records an admitted action and nothing about it is distinguishable
// from ordinary work. This detector finds the run that kept pushing on gates, which is the
// recorded signature; closing the other half is a question about gate coverage, not about
// what a record can be read to say.
//
// This is deliberately not what the non-convergence detector does. That one stops a run
// that is producing steps and getting nowhere. This one stops a run that is getting
// somewhere, by a route it was refused.

const (
	// RefusalRetryLimit is how many times one action may be refused by one rule before
	// the run stops. Being told no and asking again unchanged is worth one retry (a
	// gate's answer can legitimately change when a budget window rolls or an approval
	// lands), and no more: a third identical refusal is a run that is not listening.
	RefusalRetryLimit = 3
	// RefusalRouteLimit is how many distinct actions one rule may refuse before the run
	// stops. Three, not two, because two is where an honestly-scoped run bumping into a
	// grant it was not given is still the likelier reading. By the third door the run
	// should be asking for authority rather than looking for a way around the answer.
	RefusalRouteLimit = 3
)

// Refusal is one action the waist refused, as read back off the run's record. Rule names
// what refused it (the fault code: capability_denied, containment_unavailable) and Action
// names what was refused. The class is deliberately not here: several rules share one
// class, and a count keyed on the class would fold unrelated gates together and read a run
// that met three different walls as a run that worked around one.
type Refusal struct {
	Rule   string
	Action string
}

// RefusalProbe reads the refusals recorded for a goal on the run's durable record, in the
// order the waist refused them. It reads what happened rather than what the run says
// happened, which is the whole point: a run that routes around a gate reports each step as
// fine, and the refusals are the part of the history it does not get to narrate.
//
// A probe returns every refusal for the goal, including ones already seen: the reconciler
// derives its verdict from the whole record each pass rather than banking a count, so a
// tally cannot be lost to a crash or spent by a status write. A probe error is the
// reconciler's to classify; a read that failed for a moment must not read as a clean run.
type RefusalProbe interface {
	Refusals(ctx context.Context, r resource.Resource) ([]Refusal, error)
}

// RefusalVerdict is a run stopped by its own refusals: which rule kept refusing it, and
// the actions it was refused for, in the order they were first refused.
type RefusalVerdict struct {
	// Rule is the gate that refused.
	Rule string
	// Actions are the distinct actions that rule refused, first-refusal order.
	Actions []string
	// Count is how many refusals that rule issued in total.
	Count int
	// Routed reports which shape fired: true when distinct actions reached
	// RefusalRouteLimit (the run came back by another door), false when one action
	// reached RefusalRetryLimit (the run asked again unchanged).
	Routed bool
	// Repeated is the action that reached the retry limit, and Repeats how many times it
	// was refused. Both are set only on a retry verdict: the message has to name the one
	// action that was asked again rather than every action the rule ever refused, which
	// would describe a different run.
	Repeated string
	Repeats  int
}

// ReadRefusals returns the verdict a run's recorded refusals amount to, and whether there
// is one at all.
//
// The rules are ranked, and the ranking is stable rather than arbitrary: a substitution is
// reported ahead of a persistent retry, because it is the more serious account of the same
// history and a run doing both is better described by it. Within a shape, the rule that
// refused most is taken, and a tie is broken by rule name so the same record always yields
// the same verdict.
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
			// A refusal that names no rule cannot be attributed to a gate, and folding
			// every such refusal into one bucket would invent a rule that refused
			// everything. It is counted by nobody rather than counted wrongly.
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
		// so the same record always names the same one.
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
// of a retry, then the rule that refused most. Names are already in sorted order at the
// call site, so an exact tie keeps the earlier one and the verdict is deterministic.
func outranks(a, b RefusalVerdict) bool {
	if a.Routed != b.Routed {
		return a.Routed
	}
	return a.Count > b.Count
}

// RefusalReason is the stall message a refusal verdict carries. It says what was refused
// and by what, because the run is being handed back to a person who has to decide between
// widening the authority and abandoning the objective, and neither decision can be made
// from "blocked".
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
