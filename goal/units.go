package goal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// unitMarkLen is how many hex characters of a unit's content hash form its fingerprint:
// 64 bits, past collision range for the units one goal carries and short enough to read
// in a status dump.
const unitMarkLen = 16

// Unit-graph errors are distinct because they say different things about the graph's
// author: a malformed unit could never run, a cycle does not terminate, and a rewrite
// edits a committed unit while a child is running it.
var (
	// ErrUnitIncomplete reports a unit missing its id, objective or verify clause. A unit
	// with no declared check can never be proven, so it would block every dependent
	// forever.
	ErrUnitIncomplete = errors.New("goal: unit needs an id, an objective and a verify")
	// ErrUnitDuplicate reports two units claiming the same id, which would make an
	// edge to that id ambiguous.
	ErrUnitDuplicate = errors.New("goal: duplicate unit id")
	// ErrUnitUnknownDependency reports an edge naming a unit the graph does not carry.
	ErrUnitUnknownDependency = errors.New("goal: unit depends on an unknown unit")
	// ErrUnitCycle reports units that can never become ready because they sit on, or
	// downstream of, a dependency cycle.
	ErrUnitCycle = errors.New("goal: unit graph has a dependency cycle")
	// ErrUnitRewritten reports a unit being altered or dropped after work was already
	// spent on it.
	ErrUnitRewritten = errors.New("goal: unit was rewritten after it was dispatched")
	// ErrUnitUnknown reports a state change naming a unit the graph does not carry.
	ErrUnitUnknown = errors.New("goal: unit not found")
	// ErrUnitNotDispatched reports a settle for a unit no child was ever created for,
	// so there is no outcome it could be reporting.
	ErrUnitNotDispatched = errors.New("goal: unit has not been dispatched")
)

// Unit is one node of a goal's fan-out graph: a child's objective, the way that child's
// work is to be proven, and the units that must be proven before it may start. Actions
// and Agent are the child's authority, intersected with the parent's grant, so a unit
// cannot hand a child authority the running goal does not hold.
//
// A unit graph is plan-driven fan-out, known in advance (mission's spawn tool is for
// fan-out discovered as the run goes). Three rules make it a record, not a suggestion:
//
//   - It is refused whole at admission: a cycle, a dangling edge or a duplicate id is
//     rejected before any child is created.
//   - A unit unblocks its dependents by being proven, not by finishing. A child that
//     exited, failed or produced nothing leaves them blocked.
//   - A unit in flight cannot be rewritten: it is fingerprinted when first dispatched
//     and a later graph that alters it is refused. Appending units is allowed.
type Unit struct {
	// ID names the unit so an edge can point at it. It is the author's, not a content
	// address, which is unwritable by hand; Mark supplies that (see [UnitMark]).
	ID string `json:"id"`
	// Objective is what the child is asked to achieve, in the plan author's words.
	Objective string `json:"objective"`
	// Verify is the declared way to prove this unit. It is required: an unprovable unit
	// does not fail, it blocks every unit downstream of it forever.
	Verify string `json:"verify"`
	// DependsOn names the units that must be proven before this one is dispatched. Empty
	// makes the unit a root.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Actions is the ad-hoc set of tool actions the child may take, narrowed against
	// the parent's grant.
	Actions []string `json:"actions,omitempty"`
	// Agent names an archetype to run the child as, whose system prompt and
	// capabilities configure it instead of Actions. Empty runs an ad-hoc child.
	Agent string `json:"agent,omitempty"`
}

// UnitPhase is where one unit of the graph stands. Pending, Dispatched and Settled
// are durable facts about what has been spent; Blocked is derived from the edges and
// is never stored (see Status.UnitPhaseOf).
type UnitPhase string

// The phases a unit moves through.
const (
	// UnitPending is a unit nothing has been spent on yet. It is the zero value, so a new
	// state entry starts here.
	UnitPending UnitPhase = ""
	// UnitBlocked is a pending unit with a dependency not yet proven. It is derived and
	// never stored, so it cannot drift from the edges.
	UnitBlocked UnitPhase = "Blocked"
	// UnitDispatched is a unit whose child goal has been created and is running.
	UnitDispatched UnitPhase = "Dispatched"
	// UnitSettled is a unit whose child finished. Only a proven one unblocks its
	// dependents.
	UnitSettled UnitPhase = "Settled"
)

// UnitState is the observed state of one unit, keyed to it by id. It makes a fan-out
// resumable: a run that crashes with three units dispatched resumes knowing those
// children exist, and spawns neither twice nor never.
type UnitState struct {
	ID    string    `json:"id"`
	Phase UnitPhase `json:"phase,omitempty"`
	// ChildID is the goal the unit was dispatched as, recorded so a resumed run finds the
	// child it already created.
	ChildID string `json:"childID,omitempty"`
	// Mark is the unit's content fingerprint at dispatch (see [UnitMark]). A later graph
	// whose unit of the same id fingerprints differently is a rewrite of work in flight,
	// and is refused.
	Mark string `json:"mark,omitempty"`
	// Proven records that the unit's declared check passed and the evidence gate admitted
	// it; Evidence references the verification. An edge waits on this, not on Phase.
	Proven   bool   `json:"proven,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// SettledAt is when the child finished, and Failure is why it finished without
	// proving anything. Failure is empty on a proven unit.
	SettledAt *time.Time `json:"settledAt,omitempty"`
	Failure   string     `json:"failure,omitempty"`
}

// UnitMark returns the fingerprint of a unit's content: the first unitMarkLen hex
// characters of a SHA-256 over every field that decides what the child does. Fields
// are NUL-separated and the separator is also used between list elements, so no two
// different units can be re-split into the same byte string.
//
// DependsOn is included deliberately. Re-pointing a dispatched unit's edges does not
// change what its child was asked to do, but it changes what the graph claims was
// proven before that child started, and the graph is the record of exactly that.
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

// ValidateUnits refuses a unit graph that could never run: a unit missing its id,
// objective or verify clause; two units claiming one id; an edge to a unit that does
// not exist; or a cycle. It runs before anything is dispatched, so a bad graph is a
// terminal spec fault and not a fan-out abandoned halfway with children running.
//
// There is no separate reachability check: with no dangling edges, a unit unreachable
// from every root is exactly a unit on or downstream of a cycle, which the cycle check
// already reports. An empty graph is valid and means the goal does not fan out.
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

// unreachable returns the ids of the units no dependency order can ever admit, in graph
// order. It is Kahn's algorithm run to exhaustion: whatever it cannot emit is on a cycle
// or downstream of one, and naming the whole set serves the author better than naming
// one edge. Edges must already be checked for dangling targets.
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
	// A unit is queued exactly once (a root above, or on the one decrement that takes its
	// count to zero), so no guard against emitting twice is needed.
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

// SyncUnits brings the status's per-unit state into line with the graph on the spec:
// every unit gains a pending entry, in graph order, and existing state is carried across
// untouched. A graph that grew mid-run thus has state for its new units, none arriving
// dispatched or proven.
//
// State for a unit no longer in the graph is dropped. That cannot lose a dispatch:
// [Status.ValidateDispatched] refuses a graph that dropped a unit anything was spent on.
func (s *Status) SyncUnits(units []Unit) {
	s.Units = syncStateByID(s.Units, units,
		func(st UnitState) string { return st.ID },
		func(u Unit) string { return u.ID },
		func(id string) UnitState { return UnitState{ID: id} })
}

// syncStateByID rebuilds per-item state to line up with items: state for a listed item
// is carried across, a new item gets a fresh one, state for a gone item is dropped, and
// an empty list clears the state. SyncLedger and SyncUnits are both this, and nothing is
// created but a zero state, so a sync can never fabricate a proof or a dispatch.
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

// ValidateDispatched checks the graph on the spec against the units the status records
// work against: a dispatched unit must still be present and still fingerprint to the
// mark it carried when its child was created. That makes the no-rewrite rule enforced
// and not merely requested.
//
// It is narrower than the ledger's prefix rule on purpose. The ledger is what completion
// is judged against, so even reordering it edits the definition of done. A unit graph is
// a dispatch plan: a unit nothing has been spent on is not a commitment, so a graph may
// gain units, lose pending ones and reorder. It may not change the meaning of work in
// flight.
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
			continue // never dispatched: not yet a commitment
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

// UnitPhaseOf reports where a unit stands, resolving the one derived phase: a pending
// unit with an unproven dependency is Blocked. It is computed on every read and never
// stored, because two representations of where the run is can disagree.
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

// UnmetDependencies returns the ids of the dependencies of a unit that are not yet
// proven, in the order the unit declares them. It is what a blocked unit is waiting
// for, and it is what a stall on a graph that cannot finish has to be able to name.
//
// Unproven, not unsettled: a dependency whose child ran and failed is unmet forever,
// which is what stops a failure propagating into its dependents as though it had
// succeeded.
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

// ReadyUnits returns the units that may be dispatched now: pending, with every
// dependency proven, in graph order. It is a pure function of the graph and the
// recorded state, so a resumed run computes the same answer as the run that crashed.
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

// DispatchedUnits returns the ids of the child goals currently running units, in
// graph order, so a parent can poll exactly the children it created.
func (s Status) DispatchedUnits() []string {
	var ids []string
	for _, st := range s.Units {
		if st.Phase == UnitDispatched && st.ChildID != "" {
			ids = append(ids, st.ChildID)
		}
	}
	return ids
}

// MarkUnitDispatched records that a child goal was created for a unit, stamping the
// unit's fingerprint so a later rewrite of it is refused. It reports [ErrUnitUnknown]
// for a unit the status has no state for. Re-dispatching a dispatched or settled unit is
// refused and not ignored: a second dispatch means a second child, and accepting it
// silently is how a resumed run doubles a fan-out.
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

// MarkUnitProven settles a unit as proven, with the evidence reference and the time its
// child finished. Only a dispatched unit may settle: a proof for a unit no child was
// created for is a claim about work that never ran. Settling a settled unit is a no-op
// that keeps the first outcome, so the record says when the unit settled and not when
// it was last re-asserted.
func (s *Status) MarkUnitProven(id, evidence string, now time.Time) error {
	return s.settleUnit(id, now, func(st *UnitState) {
		st.Proven, st.Evidence, st.Failure = true, evidence, ""
	})
}

// MarkUnitFailed settles a unit that finished without proving anything, recording why.
// A failed unit stays settled and unproven, so every unit downstream of it stays
// blocked: a failure does not propagate into its dependents wearing a success.
func (s *Status) MarkUnitFailed(id, failure string, now time.Time) error {
	return s.settleUnit(id, now, func(st *UnitState) {
		st.Proven, st.Evidence, st.Failure = false, "", failure
	})
}

// RefuseUnit settles a unit whose child could never be created, recording the refusal.
// It is the one way a unit settles without having been dispatched: a spawn refused for
// good (past the delegation depth, outside the grant, out of budget) is that unit's
// outcome and not an error to retry forever. A unit since dispatched or settled is left
// alone, so a late refusal cannot unseat a child that exists.
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

// settleUnit is the shared body of the two settle paths: find the unit, refuse one
// that was never dispatched, keep a settlement that already happened, and stamp the
// outcome.
func (s *Status) settleUnit(id string, now time.Time, outcome func(*UnitState)) error {
	for i := range s.Units {
		if s.Units[i].ID != id {
			continue
		}
		switch s.Units[i].Phase {
		case UnitSettled:
			return nil
		case UnitPending, UnitBlocked:
			// Blocked is never recorded, so it cannot be read back here; it is listed with
			// Pending because no child was created, so there is no outcome to report.
			return fmt.Errorf("%w: %q", ErrUnitNotDispatched, id)
		case UnitDispatched: // the only phase a settlement may arrive for
		}
		at := now
		s.Units[i].Phase = UnitSettled
		s.Units[i].SettledAt = &at
		outcome(&s.Units[i])
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnitUnknown, id)
}

// UnitsSettled reports whether the graph has units and every one of them is settled
// proven. An empty graph is not settled: a goal with nothing planned has not
// finished, it has not started, which is the same rule the ledger holds.
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

// UnitStalled reports whether the graph can no longer make progress even though it
// has not finished: nothing is running, and nothing is ready. That is what a graph
// looks like once a unit has failed, and it is a distinct outcome from a run that is
// merely slow. Without it a parent would park on children that no longer exist and
// wait out its own budget to discover it.
//
// The reason names the units that will never be reached and what each is waiting
// for, so the stall is legible without re-deriving the graph by hand.
func (s Status) UnitStalled(units []Unit) (bool, string) {
	if len(units) == 0 || s.UnitsSettled(units) {
		return false, ""
	}
	for _, st := range s.Units {
		if st.Phase == UnitDispatched {
			return false, "" // still running
		}
	}
	if len(s.ReadyUnits(units)) > 0 {
		return false, "" // more to dispatch
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

// unitState returns the recorded state for a unit id.
func (s Status) unitState(id string) (UnitState, bool) {
	for _, st := range s.Units {
		if st.ID == id {
			return st, true
		}
	}
	return UnitState{}, false
}

// failureText keeps a stall message readable when a child settled unproven without
// saying why, which a child that simply produced nothing does.
func failureText(failure string) string {
	if failure == "" {
		return "no verification was recorded"
	}
	return failure
}
