package goal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// unitMarkLen is 64 bits of hex, past collision range for one goal's units and short
// enough to read in a status dump.
const unitMarkLen = 16

var (
	// ErrUnitIncomplete is refused because a unit with no check can never be proven and
	// would block every dependent forever.
	ErrUnitIncomplete = errors.New("goal: unit needs an id, an objective and a verify")
	// ErrUnitDuplicate is refused because an edge to that id would be ambiguous.
	ErrUnitDuplicate = errors.New("goal: duplicate unit id")
	// ErrUnitUnknownDependency reports a dangling edge.
	ErrUnitUnknownDependency = errors.New("goal: unit depends on an unknown unit")
	// ErrUnitCycle covers units on a cycle and units downstream of one.
	ErrUnitCycle = errors.New("goal: unit graph has a dependency cycle")
	// ErrUnitRewritten reports a dispatched unit altered or dropped.
	ErrUnitRewritten = errors.New("goal: unit was rewritten after it was dispatched")
	// ErrUnitUnknown reports a state change for a unit not in the graph.
	ErrUnitUnknown = errors.New("goal: unit not found")
	// ErrUnitNotDispatched reports a settle for a unit with no child, which has no outcome.
	ErrUnitNotDispatched = errors.New("goal: unit has not been dispatched")
)

// Unit is one node of fan-out planned in advance. Actions and Agent are intersected with
// the parent's grant, so a unit cannot widen authority.
//
//   - A bad graph is refused whole, before any child is created.
//   - A unit unblocks dependents by being proven; a failed or empty child leaves them
//     blocked.
//   - A dispatched unit is fingerprinted and cannot be rewritten. Appending is allowed.
type Unit struct {
	// ID is author-chosen, because a content address is unwritable by hand; [UnitMark]
	// supplies that.
	ID        string `json:"id"`
	Objective string `json:"objective"`
	// Verify is required, because an unprovable unit blocks everything downstream forever.
	Verify string `json:"verify"`
	// DependsOn must be proven before dispatch. Empty makes a root.
	DependsOn []string `json:"dependsOn,omitempty"`
	Actions   []string `json:"actions,omitempty"`
	// Agent names an archetype used in place of Actions. Empty runs an ad-hoc child.
	Agent string `json:"agent,omitempty"`
}

// UnitPhase is stored except Blocked, which [Status.UnitPhaseOf] derives from the edges.
type UnitPhase string

// The phases a unit moves through.
const (
	UnitPending    UnitPhase = "" // zero value, so a new entry starts here
	UnitBlocked    UnitPhase = "Blocked"
	UnitDispatched UnitPhase = "Dispatched"
	// UnitSettled unblocks dependents only when proven.
	UnitSettled UnitPhase = "Settled"
)

// UnitState lets a crashed fan-out resume without spawning a child twice or never.
type UnitState struct {
	ID      string    `json:"id"`
	Phase   UnitPhase `json:"phase,omitempty"`
	ChildID string    `json:"childID,omitempty"`
	// Mark is the [UnitMark] at dispatch; a later mismatch is a refused rewrite.
	Mark string `json:"mark,omitempty"`
	// Proven is set only by the evidence gate, and an edge waits on it instead of Phase.
	Proven   bool   `json:"proven,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Failure is empty on a proven unit.
	SettledAt *time.Time `json:"settledAt,omitempty"`
	Failure   string     `json:"failure,omitempty"`
}

// UnitMark hashes every field with NUL separators, so no two units re-split into the
// same bytes. DependsOn is included, because re-pointing edges changes what the graph
// claims was proven before the child started.
func UnitMark(u Unit) string {
	var b strings.Builder
	b.WriteString(u.ID)
	b.WriteByte(0)
	b.WriteString(u.Objective)
	b.WriteByte(0)
	b.WriteString(u.Verify)
	b.WriteByte(0)
	b.WriteString(u.Agent)
	for _, d := range u.DependsOn {
		b.WriteByte(0)
		b.WriteString(d)
	}
	b.WriteByte(0)
	for _, a := range u.Actions {
		b.WriteByte(0)
		b.WriteString(a)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:unitMarkLen]
}

// ValidateUnits runs before any dispatch, so a bad graph is a spec fault and never a
// fan-out abandoned halfway. With no dangling edges, the cycle check also covers
// reachability. An empty graph is valid.
func ValidateUnits(units []Unit) error {
	if len(units) == 0 {
		return nil
	}
	byID := make(map[string]Unit, len(units))
	for i, u := range units {
		if strings.TrimSpace(u.ID) == "" || strings.TrimSpace(u.Objective) == "" || strings.TrimSpace(u.Verify) == "" {
			return fmt.Errorf("%w: unit %d (%q)", ErrUnitIncomplete, i, u.ID)
		}
		if _, dup := byID[u.ID]; dup {
			return fmt.Errorf("%w: %q", ErrUnitDuplicate, u.ID)
		}
		byID[u.ID] = u
	}
	for _, u := range units {
		for _, d := range u.DependsOn {
			if _, ok := byID[d]; !ok {
				return fmt.Errorf("%w: %q depends on %q", ErrUnitUnknownDependency, u.ID, d)
			}
		}
	}
	if stuck := unreachable(units); len(stuck) > 0 {
		return fmt.Errorf("%w: %s can never become ready", ErrUnitCycle, strings.Join(stuck, ", "))
	}
	return nil
}

// unreachable runs Kahn's algorithm to exhaustion and names every unit it cannot emit,
// which helps the author more than one edge. Edges must already resolve.
func unreachable(units []Unit) []string {
	remaining := make(map[string]int, len(units))
	for _, u := range units {
		remaining[u.ID] = len(u.DependsOn)
	}
	dependents := make(map[string][]string, len(units))
	for _, u := range units {
		for _, d := range u.DependsOn {
			dependents[d] = append(dependents[d], u.ID)
		}
	}
	queue := make([]string, 0, len(units))
	for _, u := range units {
		if remaining[u.ID] == 0 {
			queue = append(queue, u.ID)
		}
	}
	// A unit is queued once, on the decrement that reaches zero, so no dedup is needed.
	emitted := make(map[string]bool, len(units))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		emitted[id] = true
		for _, dep := range dependents[id] {
			remaining[dep]--
			if remaining[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	var stuck []string
	for _, u := range units {
		if !emitted[u.ID] {
			stuck = append(stuck, u.ID)
		}
	}
	return stuck
}

// SyncUnits drops state for a removed unit. That cannot lose a dispatch, because
// [Status.ValidateDispatched] refuses dropping a dispatched unit.
func (s *Status) SyncUnits(units []Unit) {
	s.Units = syncStateByID(s.Units, units,
		func(st UnitState) string { return st.ID },
		func(u Unit) string { return u.ID },
		func(id string) UnitState { return UnitState{ID: id} })
}

// syncStateByID creates only zero state, so a sync can never fabricate a proof or a
// dispatch.
func syncStateByID[S, I any](have []S, items []I, stateID func(S) string, itemID func(I) string, fresh func(string) S) []S {
	if len(items) == 0 {
		return nil
	}
	by := make(map[string]S, len(have))
	for _, st := range have {
		by[stateID(st)] = st
	}
	out := make([]S, 0, len(items))
	for _, it := range items {
		if st, ok := by[itemID(it)]; ok {
			out = append(out, st)
			continue
		}
		out = append(out, fresh(itemID(it)))
	}
	return out
}

// ValidateDispatched is looser than the ledger's prefix rule, because only spent work is
// a commitment. A graph may gain, drop pending and reorder units; a dispatched unit must
// stay and keep its mark.
func (s Status) ValidateDispatched(units []Unit) error {
	if len(s.Units) == 0 {
		return nil
	}
	marks := make(map[string]string, len(units))
	for _, u := range units {
		marks[u.ID] = UnitMark(u)
	}
	for _, st := range s.Units {
		if st.Mark == "" {
			continue
		}
		mark, ok := marks[st.ID]
		if !ok {
			return fmt.Errorf("%w: %q was dropped from the graph", ErrUnitRewritten, st.ID)
		}
		if mark != st.Mark {
			return fmt.Errorf("%w: %q was dispatched as %s and now fingerprints to %s", ErrUnitRewritten, st.ID, st.Mark, mark)
		}
	}
	return nil
}

// UnitPhaseOf derives Blocked on every read, because a stored copy could disagree.
func (s Status) UnitPhaseOf(units []Unit, id string) UnitPhase {
	st, ok := s.unitState(id)
	if !ok {
		return UnitPending
	}
	if st.Phase != UnitPending {
		return st.Phase
	}
	if len(s.UnmetDependencies(units, id)) > 0 {
		return UnitBlocked
	}
	return UnitPending
}

// UnmetDependencies is in declared order. A failed dependency is unmet forever, so a
// failure never reaches dependents as success.
func (s Status) UnmetDependencies(units []Unit, id string) []string {
	proven := make(map[string]bool, len(s.Units))
	for _, st := range s.Units {
		proven[st.ID] = st.Proven
	}
	var unmet []string
	for _, u := range units {
		if u.ID != id {
			continue
		}
		for _, d := range u.DependsOn {
			if !proven[d] {
				unmet = append(unmet, d)
			}
		}
		break
	}
	return unmet
}

// ReadyUnits is pure over the graph and record, so a resumed run gets the answer the
// crashed run would have.
func (s Status) ReadyUnits(units []Unit) []Unit {
	proven := make(map[string]bool, len(s.Units))
	phase := make(map[string]UnitPhase, len(s.Units))
	for _, st := range s.Units {
		proven[st.ID], phase[st.ID] = st.Proven, st.Phase
	}
	var ready []Unit
	for _, u := range units {
		if phase[u.ID] != UnitPending {
			continue
		}
		blocked := false
		for _, d := range u.DependsOn {
			if !proven[d] {
				blocked = true
				break
			}
		}
		if !blocked {
			ready = append(ready, u)
		}
	}
	return ready
}

// DispatchedUnits returns child goal ids, so a parent polls only the children it created.
func (s Status) DispatchedUnits() []string {
	var ids []string
	for _, st := range s.Units {
		if st.Phase == UnitDispatched && st.ChildID != "" {
			ids = append(ids, st.ChildID)
		}
	}
	return ids
}

// MarkUnitDispatched stamps the [UnitMark]. A second dispatch is refused, because it
// means a second child and a doubled fan-out on resume.
func (s *Status) MarkUnitDispatched(u Unit, childID string) error {
	for i := range s.Units {
		if s.Units[i].ID != u.ID {
			continue
		}
		if s.Units[i].Phase != UnitPending {
			return fmt.Errorf("goal: unit %q is already %s", u.ID, s.Units[i].Phase)
		}
		s.Units[i].Phase = UnitDispatched
		s.Units[i].ChildID = childID
		s.Units[i].Mark = UnitMark(u)
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnitUnknown, u.ID)
}

// MarkUnitProven needs a dispatched unit, because a proof with no child is a claim about
// work that never ran. A repeat keeps the first outcome and time.
func (s *Status) MarkUnitProven(id, evidence string, now time.Time) error {
	return s.settleUnit(id, now, func(st *UnitState) {
		st.Proven, st.Evidence, st.Failure = true, evidence, ""
	})
}

// MarkUnitFailed leaves the unit unproven, so everything downstream stays blocked.
func (s *Status) MarkUnitFailed(id, failure string, now time.Time) error {
	return s.settleUnit(id, now, func(st *UnitState) {
		st.Proven, st.Evidence, st.Failure = false, "", failure
	})
}

// RefuseUnit is the one settle without dispatch, because a spawn refused for good is an
// outcome and not a retry. A unit no longer pending is left alone, so a late refusal
// cannot unseat a child that exists.
func (s *Status) RefuseUnit(id, reason string, now time.Time) error {
	for i := range s.Units {
		if s.Units[i].ID != id {
			continue
		}
		if s.Units[i].Phase != UnitPending {
			return nil
		}
		at := now
		s.Units[i].Phase = UnitSettled
		s.Units[i].SettledAt = &at
		s.Units[i].Failure = reason
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnitUnknown, id)
}

func (s *Status) settleUnit(id string, now time.Time, outcome func(*UnitState)) error {
	for i := range s.Units {
		if s.Units[i].ID != id {
			continue
		}
		switch s.Units[i].Phase {
		case UnitSettled:
			return nil
		case UnitPending, UnitBlocked:
			// Blocked is never stored; it is listed so the switch is exhaustive.
			return fmt.Errorf("%w: %q", ErrUnitNotDispatched, id)
		case UnitDispatched:
		}
		at := now
		s.Units[i].Phase = UnitSettled
		s.Units[i].SettledAt = &at
		outcome(&s.Units[i])
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnitUnknown, id)
}

// UnitsSettled is false for an empty graph, because a goal with nothing planned has not
// started. The ledger holds the same rule.
func (s Status) UnitsSettled(units []Unit) bool {
	if len(units) == 0 || len(s.Units) == 0 {
		return false
	}
	for _, st := range s.Units {
		if !st.Proven {
			return false
		}
	}
	return true
}

// UnitStalled reports an unfinished graph with nothing running or ready, so a parent
// never parks on children that no longer exist. The reason names each stuck unit and
// what it waits on.
func (s Status) UnitStalled(units []Unit) (bool, string) {
	if len(units) == 0 || s.UnitsSettled(units) {
		return false, ""
	}
	for _, st := range s.Units {
		if st.Phase == UnitDispatched {
			return false, ""
		}
	}
	if len(s.ReadyUnits(units)) > 0 {
		return false, ""
	}
	var reasons []string
	for _, st := range s.Units {
		switch {
		case st.Proven:
			continue
		case st.Phase == UnitSettled:
			reasons = append(reasons, fmt.Sprintf("%s failed: %s", st.ID, failureText(st.Failure)))
		default:
			reasons = append(reasons, fmt.Sprintf("%s blocked on %s", st.ID, strings.Join(s.UnmetDependencies(units, st.ID), ", ")))
		}
	}
	return true, "unit graph cannot proceed: " + strings.Join(reasons, "; ")
}

func (s Status) unitState(id string) (UnitState, bool) {
	for _, st := range s.Units {
		if st.ID == id {
			return st, true
		}
	}
	return UnitState{}, false
}

// failureText covers a child that produced nothing and gave no reason.
func failureText(failure string) string {
	if failure == "" {
		return "no verification was recorded"
	}
	return failure
}
