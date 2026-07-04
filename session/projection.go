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
	case KindActionCompleted:
		p.Completed++
	case KindActionRejected:
		p.Rejected++
		if ev.Trust != "" {
			p.Containment = ev.Trust
		}
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
