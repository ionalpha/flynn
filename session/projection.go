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
	case KindActionAdmitted:
		p.Admitted++
		if ev.Trust != "" {
			p.Containment = ev.Trust
		}
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
		p.Actions = upsertAction(p.Actions, ActionEntry{
			Call: ev.Call, Action: ev.Action, Trust: ev.Trust, State: ActionBlocked, Fault: ev.Fault,
		})
	case KindRecordSealed:
		// Verified outranks sealed: a re-seal never demotes a record already checked.
		if p.Record != RecordVerified {
			p.Record = RecordSealed
		}
	case KindRecordVerified:
		p.Record = RecordVerified
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
