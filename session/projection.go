package session

// RecordState is a run's record lifecycle, the value the status badge shows. A run
// records until it is sealed into a signed record, and a sealed record can be verified
// against its tiers. The states are ordered: a run only ever moves recording -> sealed
// -> verified.
type RecordState string

const (
	// RecordRecording is the live state: events are still being appended to the run.
	RecordRecording RecordState = "recording"
	// RecordSealed is set once the run's stream is sealed into a signed record.
	RecordSealed RecordState = "sealed"
	// RecordVerified is set once a sealed record has passed verification.
	RecordVerified RecordState = "verified"
)

// ActionState is where one governed action stands: running once admitted, done once
// it completes, blocked once it is refused. The states are terminal except running,
// which resolves to done or blocked when the action's outcome arrives.
type ActionState string

const (
	// ActionRunning is an admitted action that has not yet reported an outcome.
	ActionRunning ActionState = "running"
	// ActionDone is an admitted action that finished (cleanly, or with a fault it
	// carries).
	ActionDone ActionState = "done"
	// ActionBlocked is an action the dispatch waist refused.
	ActionBlocked ActionState = "blocked"
)

// ActionEntry is one governed action in the projection's ledger: what it was, the
// trust level it ran under, where it stands, and its fault class when it failed or
// was refused. The governance panel reads the ledger to show the run's recent
// boundary decisions, which the scalar counts summarize but cannot name.
type ActionEntry struct {
	// Call is the dispatch correlation id that pairs an admission with its outcome,
	// so a completion or rejection updates the entry its admission created rather
	// than adding a second row for the same action.
	Call int64
	// Action is the governed action's name (empty when the waist recorded none).
	Action string
	// Trust is the trust level the action ran under.
	Trust string
	// State is where the action stands: running, done, or blocked.
	State ActionState
	// Fault is the fault class of a blocked or failed action, empty otherwise.
	Fault string
}

// FanoutState is where a delegated child stands in the fan-out tree: running from the
// moment it is spawned until it folds back into its parent, then done or failed by the
// outcome it folded. The states are terminal except running.
type FanoutState string

const (
	// FanoutRunning is a spawned child that has not yet folded back into its parent.
	FanoutRunning FanoutState = "running"
	// FanoutDone is a child that folded back with a clean result.
	FanoutDone FanoutState = "done"
	// FanoutFailed is a child that folded back as an error.
	FanoutFailed FanoutState = "failed"
)

// FanoutChild is one delegated child in a run's fan-out tree: which goal spawned it,
// the sub-objective it was given, where it stands, how many turns it has taken, its own
// governance posture (trust level and blocked count, folded from the actions it ran), its
// seal state within the run's sealed record, and its folded answer once it finishes. The
// tree is flat with a Parent id on each node rather
// than nested, so a renderer builds the hierarchy and a deeper delegation (a child that
// itself spawns) needs no change here. Children are kept in spawn order.
type FanoutChild struct {
	// ID is the child goal's id, the value its own events carry in Goal.
	ID string
	// Parent is the id of the goal that spawned this child.
	Parent string
	// Objective is the sub-objective the parent delegated.
	Objective string
	// State is where the child stands: running, done, or failed.
	State FanoutState
	// Turns counts the model turns the child has completed so far.
	Turns int
	// Result is the child's folded answer once it is done or failed, empty while it
	// runs.
	Result string
	// Trust is the trust level of this child's most recent governed action, its own
	// containment posture (the per-child analogue of the run's Containment). Empty until
	// the child has an action admitted or rejected.
	Trust string
	// Blocked counts this child's governed actions the dispatch waist refused. A non-zero
	// Blocked is the signal that this child, not a sibling, hit a governance boundary.
	Blocked int
	// Seal is the child's record lifecycle within the run's sealed record: recording
	// while its events are still being appended, sealed once it has folded back and the
	// run's stream is signed (its events are then under the signed Merkle root, provable
	// with a per-event proof), verified once the run's sealed record passes verification.
	// A child still running when a seal lands stays recording, since its events are not
	// yet final. It is the per-child analogue of the run-level Record above.
	Seal RecordState
}

// maxLedger bounds the projection's action ledger to its most recent entries, so a
// long run's status stays a fixed size. The panel shows a tail of recent decisions,
// not the whole history (the sealed record holds that); older entries fall off the
// front as newer ones arrive.
const maxLedger = 64

// Projection is the current status of a run, folded from its event stream: the facts
// an always-on status line and a governance panel read. It is a pure function of the
// events seen so far, so a live client, a replay, and a reattached client that folds
// from Seq 0 all compute the same status. It carries no presentation, only the run's
// state; a renderer decides how to show it (and combines it with UI-local facts a
// stream does not carry, such as the model's context-window size, to derive a
// context-fill percentage from Usage).
type Projection struct {
	// Objective is the run's opening goal; Result is its final answer once converged.
	Objective string
	Result    string

	// Turns counts the model turns completed. Usage is the run's cumulative token cost
	// across those turns, summed from each turn.completed.
	Turns int
	Usage Usage

	// Record is the run's record lifecycle for the badge.
	Record RecordState

	// Containment is the trust level of the most recent governed action, the run's
	// current containment posture. Empty until the first action is admitted.
	Containment string

	// Admitted, Completed, and Rejected count governed actions by outcome. Admitted
	// minus Completed minus the admitted-then-rejected races is the in-flight count; a
	// non-zero Rejected is the signal a run hit a governance boundary.
	Admitted  int
	Completed int
	Rejected  int

	// Actions is a bounded ledger of the run's most recent governed actions, newest
	// last, each carrying its name, trust level, state, and fault class. It names the
	// decisions the scalar counts above only tally, for a governance panel to show.
	// It is capped at maxLedger entries; older actions fall off the front.
	Actions []ActionEntry

	// Fanout is the run's delegated children in spawn order, each with its parent, its
	// sub-objective, where it stands, and its turn count. It is empty for a single
	// conversation and grows as a fan-out spawns children, so a live tree view reads
	// the run's delegation shape straight from the projection.
	Fanout []FanoutChild

	// Terminal reports whether the run reached a terminal event. Err is the stall
	// reason when it stalled, empty on a clean convergence.
	Terminal bool
	Err      string
}

// NewProjection returns the status of a run with no events yet: recording, and
// otherwise zero. Fold events into it with Reduce, or use Project over a slice.
func NewProjection() Projection { return Projection{Record: RecordRecording} }

// Reduce folds one event into a running Projection and returns the updated status. It
// is the single place a stream becomes status, shared by the live status line (folding
// each event as it arrives) and a replay (folding History). Unrecognized events leave
// the status unchanged, so the projection is stable as the event vocabulary grows.
func Reduce(p Projection, ev Event) Projection {
	switch ev.Kind {
	case KindSessionStarted:
		p.Objective = ev.Text
	case KindTurnCompleted:
		p.Turns++
		if ev.Usage != nil {
			p.Usage.InputTokens += ev.Usage.InputTokens
			p.Usage.OutputTokens += ev.Usage.OutputTokens
			p.Usage.CacheReadTokens += ev.Usage.CacheReadTokens
			p.Usage.CacheWriteTokens += ev.Usage.CacheWriteTokens
		}
		// A completed turn belonging to a spawned child advances that child's turn
		// count in the tree; a turn on the root goal matches no child and only bumps the
		// run total above.
		if ev.Goal != "" {
			p.Fanout = bumpChildTurns(p.Fanout, ev.Goal)
		}
	case KindChildSpawned:
		p.Fanout = append(append([]FanoutChild(nil), p.Fanout...), FanoutChild{
			ID: ev.Child, Parent: ev.Goal, Objective: ev.Text, State: FanoutRunning, Seal: RecordRecording,
		})
	case KindChildCompleted:
		p.Fanout = completeChild(p.Fanout, ev.Child, ev.Result, ev.IsError)
	case KindActionAdmitted:
		p.Admitted++
		if ev.Trust != "" {
			p.Containment = ev.Trust
		}
		p.Fanout = foldChildGovernance(p.Fanout, ev.Goal, ev.Trust, false)
		p.Actions = upsertAction(p.Actions, ActionEntry{
			Call: ev.Call, Action: ev.Action, Trust: ev.Trust, State: ActionRunning,
		})
	case KindActionCompleted:
		p.Completed++
		p.Actions = upsertAction(p.Actions, ActionEntry{
			Call: ev.Call, Action: ev.Action, Trust: ev.Trust, State: ActionDone, Fault: ev.Fault,
		})
	case KindActionRejected:
		p.Rejected++
		if ev.Trust != "" {
			p.Containment = ev.Trust
		}
		p.Fanout = foldChildGovernance(p.Fanout, ev.Goal, ev.Trust, true)
		p.Actions = upsertAction(p.Actions, ActionEntry{
			Call: ev.Call, Action: ev.Action, Trust: ev.Trust, State: ActionBlocked, Fault: ev.Fault,
		})
	case KindRecordSealed:
		// Verified outranks sealed: a re-seal never demotes a record already checked.
		if p.Record != RecordVerified {
			p.Record = RecordSealed
			p.Fanout = sealFoldedChildren(p.Fanout, RecordSealed)
		}
	case KindRecordVerified:
		p.Record = RecordVerified
		p.Fanout = sealFoldedChildren(p.Fanout, RecordVerified)
	case KindConverged:
		p.Terminal = true
		p.Result = ev.Text
	case KindStalled:
		p.Terminal = true
		p.Err = ev.Err
	default:
		// Assistant text, tool calls and results, and any later event kind carry no
		// status the badge or panel summarize; the transcript renders them directly.
	}
	return p
}

// upsertAction folds one action event into the ledger and returns the updated ledger,
// leaving the input untouched (the reducer is pure, so a caller holding the pre-fold
// projection keeps its slice). An entry whose Call matches an existing one updates it
// in place, so an admission followed by its completion or rejection is a single row
// that changes state rather than two rows for one action; a Call of zero (a waist that
// records no correlation id) always appends. The ledger is capped at maxLedger, oldest
// dropped first.
func upsertAction(ledger []ActionEntry, e ActionEntry) []ActionEntry {
	if e.Call != 0 {
		for i := range ledger {
			if ledger[i].Call == e.Call {
				out := append([]ActionEntry(nil), ledger...)
				// A completion carries no action name on some waists; keep the name
				// the admission recorded rather than blanking it.
				if e.Action == "" {
					e.Action = out[i].Action
				}
				if e.Trust == "" {
					e.Trust = out[i].Trust
				}
				out[i] = e
				return out
			}
		}
	}
	out := append(append([]ActionEntry(nil), ledger...), e)
	if len(out) > maxLedger {
		out = out[len(out)-maxLedger:]
	}
	return out
}

// bumpChildTurns increments the turn count of the child whose id is goal, returning the
// updated slice and leaving the input untouched (the reducer is pure). A goal that names
// no known child, the run's root, leaves the tree unchanged.
func bumpChildTurns(children []FanoutChild, goal string) []FanoutChild {
	for i := range children {
		if children[i].ID == goal {
			out := append([]FanoutChild(nil), children...)
			out[i].Turns++
			return out
		}
	}
	return children
}

// foldChildGovernance attributes one governed action to the child whose id is goal,
// updating that child's trust posture and, when the action was blocked, its blocked
// count. It returns the updated slice and leaves the input untouched (the reducer is
// pure). A goal that names no known child, the run's root, leaves the tree unchanged, so
// a root action folds only into the run-level containment above.
func foldChildGovernance(children []FanoutChild, goal, trust string, blocked bool) []FanoutChild {
	if goal == "" {
		return children
	}
	for i := range children {
		if children[i].ID == goal {
			out := append([]FanoutChild(nil), children...)
			if trust != "" {
				out[i].Trust = trust
			}
			if blocked {
				out[i].Blocked++
			}
			return out
		}
	}
	return children
}

// sealFoldedChildren advances the seal state of a run's folded children when the run's
// record advances, returning the updated slice and leaving the input untouched (the
// reducer is pure). On a seal, a child that has folded back has its events under the
// signed root, so it moves to sealed; a child still running is left recording, since its
// events are not yet final and a later seal segment will cover them. On a verify, every
// folded child moves to verified. A run with no children leaves the tree unchanged.
func sealFoldedChildren(children []FanoutChild, rec RecordState) []FanoutChild {
	if len(children) == 0 {
		return children
	}
	out := append([]FanoutChild(nil), children...)
	for i := range out {
		if out[i].State == FanoutRunning {
			continue // a running child's events are not final; it stays recording
		}
		switch rec {
		case RecordSealed:
			if out[i].Seal == RecordRecording {
				out[i].Seal = RecordSealed
			}
		case RecordVerified:
			out[i].Seal = RecordVerified
		case RecordRecording:
			// Never called with recording (a run does not un-seal); leave the child as is.
		}
	}
	return out
}

// completeChild flips the child named by id from running to its folded outcome (failed
// when isErr, else done) and records its result, returning the updated slice and leaving
// the input untouched. A completion for an unknown child leaves the tree unchanged.
func completeChild(children []FanoutChild, id, result string, isErr bool) []FanoutChild {
	for i := range children {
		if children[i].ID == id {
			out := append([]FanoutChild(nil), children...)
			out[i].State = FanoutDone
			if isErr {
				out[i].State = FanoutFailed
			}
			out[i].Result = result
			return out
		}
	}
	return children
}

// Project folds a whole event slice into the run's status, the replay counterpart of
// Reduce: History or a resume picker reads a run's status by projecting its recorded
// events. It seeds from NewProjection, so an empty slice is a recording run with no
// activity yet.
func Project(evs []Event) Projection {
	p := NewProjection()
	for _, ev := range evs {
		p = Reduce(p, ev)
	}
	return p
}
