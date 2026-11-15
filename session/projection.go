package session

// RecordState is a run's record lifecycle, shown on the status badge. A run
// only moves recording, then sealed, then verified.
type RecordState string

const (
	// RecordRecording is the live state: events are still being appended.
	RecordRecording RecordState = "recording"
	// RecordSealed is a stream sealed into a signed record.
	RecordSealed RecordState = "sealed"
	// RecordVerified is a sealed record that passed verification.
	RecordVerified RecordState = "verified"
)

// ActionState is where one governed action stands. Only running is
// non-terminal; it resolves to done or blocked when the outcome arrives.
type ActionState string

const (
	// ActionRunning is an admitted action with no outcome yet.
	ActionRunning ActionState = "running"
	// ActionDone is an admitted action that finished, cleanly or with the fault it
	// carries.
	ActionDone ActionState = "done"
	// ActionBlocked is an action the dispatch waist refused.
	ActionBlocked ActionState = "blocked"
)

// ActionEntry is one governed action in the ledger, so the governance panel can
// name decisions the scalar counts only tally.
type ActionEntry struct {
	// Call pairs an admission with its outcome, so the outcome updates the
	// admission's entry instead of adding a second row.
	Call int64
	// Action is the action's name; empty when the waist recorded none.
	Action string

	Trust string

	State ActionState
	// Fault is the fault class of a blocked or failed action.
	Fault string
}

// FanoutState is where a delegated child stands: running from spawn until it
// folds back into its parent, then done or failed.
type FanoutState string

const (
	// FanoutRunning is a child that has not folded back yet.
	FanoutRunning FanoutState = "running"
	// FanoutDone is a child that folded back with a clean result.
	FanoutDone FanoutState = "done"
	// FanoutFailed is a child that folded back as an error.
	FanoutFailed FanoutState = "failed"
)

// FanoutChild is one delegated child in a run's fan-out tree. The tree is flat,
// with a Parent id on each node, so a renderer builds the hierarchy and a
// deeper delegation needs no change here. Children are kept in spawn order.
type FanoutChild struct {
	// ID is the child goal's id, the value its events carry in Goal.
	ID string

	Parent string
	// Objective is the sub-objective the parent delegated.
	Objective string

	State FanoutState
	// Turns counts the model turns the child has completed.
	Turns int
	// Result is the folded answer once done or failed; empty while running.
	Result string
	// Trust is the trust level of the child's latest governed action, its own
	// containment posture. Empty until it has an action admitted or rejected.
	Trust string
	// Blocked counts the child's actions the waist refused, so a refusal is
	// attributed to this child and not a sibling.
	Blocked int
	// Seal is the child's state within the run's sealed record. A child sealed
	// while running stays recording, because its events are not final; once it has
	// folded back a seal covers its events under the signed Merkle root, provable
	// per event.
	Seal RecordState
}

// maxLedger caps the action ledger at its most recent entries, so a long run's
// status stays a fixed size. The sealed record holds the full history.
const maxLedger = 64

// maxEgress caps the egress ledger the same way; the counts cover the rest.
const maxEgress = 16

// EgressEntry is one netguard decision in the egress ledger, so the panel can
// name the recent destinations the counts only tally.
type EgressEntry struct {
	// Host is the resolved literal address the decision concerned.
	Host string

	Allowed bool
	// Reason is netguard's short reason for the verdict.
	Reason string
}

// Projection is a run's status folded from its event stream. It is a pure
// function of the events seen, so a live client, a replay and a client that
// reattaches from Seq 0 compute the same status. It carries no presentation; a
// renderer combines it with UI-local facts such as the model's context window
// to derive, for example, context fill from Usage.
type Projection struct {
	// Objective is the run's opening goal; Result is its final answer once
	// converged.
	Objective string
	Result    string

	// Turns counts completed model turns. Usage sums their token cost from each
	// turn.completed.
	Turns int
	Usage Usage

	Record RecordState

	// Containment is the trust level of the latest governed action. Empty until the
	// first admission.
	Containment string

	// Admitted, Completed and Rejected count governed actions by outcome. Admitted
	// minus Completed minus the admitted-then-rejected races is the in-flight
	// count.
	Admitted  int
	Completed int
	Rejected  int

	// Actions holds the latest governed actions, newest last, capped at
	// [maxLedger].
	Actions []ActionEntry

	// EgressAllowed and EgressBlocked count dials netguard permitted and refused.
	// Both stay zero for a run with no netguard-gated egress (a loopback model), so
	// the egress row stays empty.
	EgressAllowed int
	EgressBlocked int

	// Egress holds the latest egress decisions, newest last, capped at
	// [maxEgress].
	Egress []EgressEntry

	// Fanout lists delegated children in spawn order. It is empty for a single
	// conversation.
	Fanout []FanoutChild

	// Terminal reports a terminal event. Err is the stall reason; empty on
	// convergence.
	Terminal bool
	Err      string
}

// NewProjection returns the status of a run with no events: recording, and
// otherwise zero.
func NewProjection() Projection { return Projection{Record: RecordRecording} }

// Reduce folds one event into p. It is the one place a stream becomes status,
// shared by the live status line and a replay. Unrecognized events leave p
// unchanged, so new event kinds do not disturb it.
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
		// A root turn matches no child and only bumps the run total.
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
	case KindEgressDecision:
		if ev.Allowed {
			p.EgressAllowed++
		} else {
			p.EgressBlocked++
		}
		p.Egress = appendEgress(p.Egress, EgressEntry{Host: ev.Host, Allowed: ev.Allowed, Reason: ev.Reason})
	case KindRecordSealed:
		// A re-seal never demotes a verified record.
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
		// Assistant text, tool calls and results carry no status; the transcript
		// renders them.
	}
	return p
}

// upsertAction returns ledger with e folded in, leaving the input untouched
// because the reducer is pure. An entry with a matching Call is updated in
// place, so an action is one row that changes state; Call zero (no correlation
// id) always appends. The oldest entries drop past [maxLedger].
func upsertAction(ledger []ActionEntry, e ActionEntry) []ActionEntry {
	if e.Call != 0 {
		for i := range ledger {
			if ledger[i].Call == e.Call {
				out := append([]ActionEntry(nil), ledger...)
				// Some waists send no name on completion; keep the admission's.
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

// appendEgress returns ledger with e appended, leaving the input untouched.
// The oldest entries drop past [maxEgress].
func appendEgress(ledger []EgressEntry, e EgressEntry) []EgressEntry {
	out := append(append([]EgressEntry(nil), ledger...), e)
	if len(out) > maxEgress {
		out = out[len(out)-maxEgress:]
	}
	return out
}

// bumpChildTurns increments child goal's turn count, leaving the input
// untouched. The root names no child and changes nothing.
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

// foldChildGovernance attributes a governed action to child goal: its trust
// posture and, when blocked, its blocked count. The input is untouched. A root
// action matches no child and folds only into the run-level containment.
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

// sealFoldedChildren advances folded children when the run's record advances,
// leaving the input untouched. On a seal, folded children move to sealed and
// running ones stay recording, since a later seal segment covers their events.
// On a verify, every folded child moves to verified.
func sealFoldedChildren(children []FanoutChild, rec RecordState) []FanoutChild {
	if len(children) == 0 {
		return children
	}
	out := append([]FanoutChild(nil), children...)
	for i := range out {
		if out[i].State == FanoutRunning {
			continue
		}
		switch rec {
		case RecordSealed:
			if out[i].Seal == RecordRecording {
				out[i].Seal = RecordSealed
			}
		case RecordVerified:
			out[i].Seal = RecordVerified
		case RecordRecording:
			// A run never un-seals, so recording is not passed here.
		}
	}
	return out
}

// completeChild sets child id to failed (isErr) or done with its result,
// leaving the input untouched. An unknown id changes nothing.
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

// Project folds a whole event slice, starting from [NewProjection]; a replay or
// a resume picker uses it on recorded events.
func Project(evs []Event) Projection {
	p := NewProjection()
	for _, ev := range evs {
		p = Reduce(p, ev)
	}
	return p
}
