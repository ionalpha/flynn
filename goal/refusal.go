package goal

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/resource"
)

const (
	// RefusalRetryLimit is refusals of one action by one rule before a stop. A retry can
	// succeed when a window rolls or an approval lands; a third is a run not listening.
	RefusalRetryLimit = 3
	// RefusalRouteLimit is distinct actions one rule may refuse. Two is likely a narrow
	// grant; by three the run should ask for authority.
	RefusalRouteLimit = 3
)

// Refusal is one recorded refusal. Rule is the fault code, such as capability_denied. The
// class is omitted because rules share classes, and counting by class would read three
// unrelated gates as one gate routed around.
type Refusal struct {
	Rule   string
	Action string
}

// RefusalProbe reads the durable record in refusal order, because a run routing around a
// gate reports every step as fine. It returns every refusal each time, so no count is
// banked where a crash or status write could lose it. A failed read is never a clean run.
type RefusalProbe interface {
	Refusals(ctx context.Context, r resource.Resource) ([]Refusal, error)
}

// RefusalVerdict reads the whole trajectory, because each step of a reformulating run
// looks defensible alone. Per rule it fires on [RefusalRetryLimit] refusals of one action
// or [RefusalRouteLimit] distinct actions. A substitution that succeeds is invisible here.
type RefusalVerdict struct {
	Rule string
	// Actions are distinct, in first-refusal order.
	Actions []string
	Count   int
	// Routed is true for the distinct-action limit.
	Routed bool
	// Repeated and Repeats are set only on a retry verdict, so its message names one
	// action.
	Repeated string
	Repeats  int
}

// ReadRefusals ranks a substitution above a retry as more serious, then the rule that
// refused most, breaking ties by name so a record always yields the same verdict.
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
			// One bucket for unnamed rules would invent a rule that refused everything.
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
		// First-refusal order, so the same record names the same action.
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

// outranks keeps the earlier of an exact tie, because the caller visits names sorted.
func outranks(a, b RefusalVerdict) bool {
	if a.Routed != b.Routed {
		return a.Routed
	}
	return a.Count > b.Count
}

// RefusalReason names the rule and actions, because the reader must choose between
// widening authority and abandoning the objective.
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
