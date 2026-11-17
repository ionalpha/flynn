package session

import (
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/usage"
)

// Kind classifies a session event. A renderer switches on it to draw the live
// transcript.
type Kind string

const (
	// KindSessionStarted is the first event, carrying the objective.
	KindSessionStarted Kind = "session.started"
	// KindTurnStarted marks the start of one model turn.
	KindTurnStarted Kind = "turn.started"
	// KindAssistant carries the model's natural-language text for a turn.
	KindAssistant Kind = "assistant.message"
	// KindToolCall is the model requesting a tool, with its arguments.
	KindToolCall Kind = "tool.call"
	// KindToolResult is the outcome of a tool call.
	KindToolResult Kind = "tool.result"
	// KindTurnCompleted marks the end of a turn, with why the model stopped.
	KindTurnCompleted Kind = "turn.completed"
	// KindChildSpawned records a delegation: Goal spawned Child with the
	// sub-objective in Text. The fan-out tree is built from these records, so it
	// needs no separate store.
	KindChildSpawned Kind = "child.spawned"
	// KindChildCompleted records Child finishing, with its answer in Result and
	// IsError set on failure. It closes the edge KindChildSpawned opened.
	KindChildCompleted Kind = "child.completed"
	// KindConverged is terminal success, carrying the final answer.
	KindConverged Kind = "session.converged"
	// KindStalled is terminal failure (budget exhausted or a step failed
	// terminally), carrying the reason.
	KindStalled Kind = "session.stalled"

	// KindActionAdmitted is the dispatch waist admitting a governed action (a tool,
	// the model call, a delegation). The waist records on the run's own stream, so
	// admissions interleave with their turns. The stored type stays
	// dispatch.start, which the sealer signs and a verifier checks.
	KindActionAdmitted Kind = "action.admitted"
	// KindActionCompleted is a governed action finishing, with a fault class when
	// it errored. Projected from dispatch.end.
	KindActionCompleted Kind = "action.completed"
	// KindActionRejected is the waist refusing an action, with the denial's fault
	// class. Projected from dispatch.rejected.
	KindActionRejected Kind = "action.rejected"

	// KindRecordSealed marks the stream sealed into a signed record. Projected from
	// spine.record.
	KindRecordSealed Kind = "record.sealed"
	// KindRecordVerified marks a sealed record checked against its tiers. It is
	// emitted when verification runs, so a replay reproduces the badge's
	// transitions.
	KindRecordVerified Kind = "record.verified"

	// KindEgressDecision is one netguard dial-time decision: the destination host,
	// allowed or blocked, and a short reason. It sits beside the run's governed
	// actions, so one panel shows both kinds of decision.
	KindEgressDecision Kind = "net.egress"

	// KindApprovalRequested is the run pausing a privileged action to ask a person:
	// the action, scope, host, preview and expiry. It is recorded before the prompt
	// opens, so a client that attaches late sees every open request. Written by the
	// system.
	KindApprovalRequested Kind = "approval.requested"
	// KindApprovalDecision is how a request ended, or the gate's own verdict. A
	// record with an Outcome and a closed [approval.Reason] resolves the request
	// named by RequestID; the gate's verdict has neither. Written by a person when
	// Reason is allowed or denied, and by the system otherwise.
	KindApprovalDecision Kind = "approval.decision"
	// KindPromptEnded closes one user prompt: PromptID, Stop class, Reason, Steps
	// and Usage. Written once per prompt, by the system.
	KindPromptEnded Kind = "prompt.ended"
	// KindClientAttached records a client connecting: the negotiated protocol
	// major, capabilities, transport and, where the OS reports one, the peer
	// process. The client's name and version are a claim and decide nothing.
	// Written by the system.
	KindClientAttached Kind = "client.attached"
	// KindBudgetExtended records a person raising the spend ceiling: before, after
	// and the total raised so far. Written by a person.
	KindBudgetExtended Kind = "budget.extended"

	// KindProjectContext is the trust decision about a repository's instruction
	// files (AGENTS.md, CLAUDE.md), so the sealed record shows which instructions
	// the model was given. Allowed reports a load as untrusted context, Reason the
	// basis (flag, remembered, prompt, unattended), and Text one file per line with
	// its content hash.
	KindProjectContext Kind = "project.context"
)

// Event types the dispatch waist and the sealer write onto a run's stream,
// projected into the vocabulary above. They mirror dispatch's constants and the
// sealer's record type; TestDecodeGovernanceEvents fails if they drift.
const (
	typeDispatchStart    = "dispatch.start"
	typeDispatchEnd      = "dispatch.end"
	typeDispatchRejected = "dispatch.rejected"
	typeRecordSealed     = "spine.record"
	typeEgress           = "net.egress"

	// The approval sink writes these with flat payload keys, so they are decoded
	// here. TestApprovalRecordsRoundTrip fails if either side renames a key or a
	// type.
	typeApprovalRequested = "approval.requested"
	typeApprovalDecision  = "approval.decision"

	typeProjectContext = "project.context"
)

// Event is one record on a session's stream. Seq is monotonic within a session
// and Time is when it was recorded; the other fields are set according to Kind
// and zero otherwise.
type Event struct {
	Seq   int64           `json:"seq"`
	Time  time.Time       `json:"time"`
	Kind  Kind            `json:"kind"`
	Actor spine.ActorType `json:"actor"`

	// Goal is the id of the goal the event belongs to: a delegated child's id under
	// fan-out, so concurrent children stay distinct on the shared stream. Empty for
	// the run itself (session-level events and a single conversation's root).
	Goal string `json:"goal,omitempty"`
	// Child is the spawned child's id (child.spawned); its parent is Goal.
	Child string `json:"child,omitempty"`
	// Turn is the 1-based model turn; 0 on session-level events.
	Turn int `json:"turn,omitempty"`
	// Text is the objective (started), the model's message (assistant) or the final
	// answer (converged).
	Text string `json:"text,omitempty"`
	// Tool is the tool name (tool.call, tool.result).
	Tool string `json:"tool,omitempty"`

	ToolUseID string `json:"toolUseID,omitempty"`
	// Input is the tool's JSON arguments (tool.call).
	Input json.RawMessage `json:"input,omitempty"`
	// Result is the tool's output text (tool.result).
	Result string `json:"result,omitempty"`
	// IsError reports a failed tool call (tool.result).
	IsError bool `json:"isError,omitempty"`
	// StopReason is why the turn ended (turn.completed).
	StopReason string `json:"stopReason,omitempty"`
	// Usage is the turn's token cost (turn.completed), so a live UI and a replay
	// both show per-turn spend and cache hits.
	Usage *Usage `json:"usage,omitempty"`
	// Err is the stall reason (stalled).
	Err string `json:"error,omitempty"`

	// Action is the governed action's name (action.*): a tool, the model call or a
	// spawn. Tool names only a model-requested tool.
	Action string `json:"action,omitempty"`
	// Call pairs an action.admitted with the action.completed or action.rejected
	// that answers it, so a panel can show the actions in flight.
	Call int64 `json:"call,omitempty"`
	// Trust is the action's trust level (action.*), which sets the containment a
	// gate requires to admit it.
	Trust string `json:"trust,omitempty"`
	// Fault is the fault class on a refused or failed action: how a caller should
	// react (forbidden, budget_exceeded, needs_approval, transient, ...). Empty on
	// a clean completion.
	Fault string `json:"fault,omitempty"`
	// Code names the rule that refused or failed the action (capability_denied,
	// containment_unavailable, ...). Several gates share a Fault class, so the
	// class alone does not say what refused. Empty on a clean completion and on an
	// unclassified failure.
	Code string `json:"code,omitempty"`

	// Host is the literal address netguard resolved before deciding (net.egress).
	Host string `json:"host,omitempty"`
	// Allowed reports a permitted dial (net.egress) or loaded instruction files
	// (project.context). False with Host set is a blocked destination.
	Allowed bool `json:"allowed,omitempty"`
	// Reason says why netguard allowed (public, allowlisted) or refused (private or
	// reserved address, ...) a dial (net.egress), or the basis of a
	// project.context decision.
	Reason string `json:"reason,omitempty"`

	// RequestID pairs an approval.requested with the approval.decision that
	// resolves it. Empty on the gate's own verdict.
	RequestID string `json:"requestId,omitempty"`
	// Scope is the scope path an approval was asked for (approval.*), identical on
	// the request and every record that refers to it.
	Scope string `json:"scope,omitempty"`
	// Grant is the narrower target a person granted (approval.decision), when they
	// narrowed it. A signature covers it.
	Grant string `json:"grant,omitempty"`
	// Preview is the line shown beside an approval prompt (approval.requested),
	// built by approval.Preview: at most [approval.PreviewLimit] bytes with no
	// control characters.
	Preview string `json:"preview,omitempty"`
	// Tainted reports that the request follows content the run took from outside,
	// so a client can warn before approval (approval.requested).
	Tainted bool `json:"tainted,omitempty"`
	// ExpiresAt is when an open request declines (approval.requested).
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// Outcome is allow or deny (approval.decision).
	Outcome approval.Outcome `json:"outcome,omitempty"`
	// FeedbackPresent reports that the person attached feedback to a denial
	// (approval.decision). The text goes to the model and is not recorded.
	FeedbackPresent bool `json:"feedbackPresent,omitempty"`
	// Client is the attached client that answered an approval or that attached
	// (approval.decision, client.attached). Empty when the system closed the
	// request.
	Client string `json:"client,omitempty"`

	// PromptID names the prompt a prompt.ended closes.
	PromptID string `json:"promptId,omitempty"`
	// Stop classifies how a prompt ended (prompt.ended). Check [Stop.Valid] before
	// branching on a value read from a record.
	Stop Stop `json:"stop,omitempty"`
	// Steps is the number of steps a prompt took (prompt.ended).
	Steps int `json:"steps,omitempty"`

	// Attachment describes the attaching client (client.attached).
	Attachment *ClientAttachment `json:"attachment,omitempty"`
	// Budget is the ceiling change (budget.extended).
	Budget *BudgetExtension `json:"budget,omitempty"`
}

// Usage is one turn's token cost, copied from the model port's accounting so a
// session consumer need not import the model package. InputTokens includes
// tokens served from cache and CacheReadTokens is that subset, so the hit rate
// is CacheReadTokens over InputTokens.
type Usage struct {
	InputTokens      int `json:"inputTokens,omitempty"`
	OutputTokens     int `json:"outputTokens,omitempty"`
	CacheReadTokens  int `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
}

// Tokens is u as a [usage.Tokens], the one place token figures are defined, so a
// consumer asks it for a total or a cache hit rate instead of recomputing either.
func (u Usage) Tokens() usage.Tokens {
	return usage.FromLLM(llm.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	})
}

// Add returns the sum of u and o, accumulated through [usage.Tokens.Add].
func (u Usage) Add(o Usage) Usage {
	t := u.Tokens().Add(o.Tokens())
	return Usage{
		InputTokens:      int(t.Input.N),
		OutputTokens:     int(t.Output.N),
		CacheReadTokens:  int(t.CacheRead.N),
		CacheWriteTokens: int(t.CacheWrite.N),
	}
}

// payloadKey holds the whole event body as one JSON string, which survives any
// spine backend unchanged: the in-memory log shares maps and a durable log
// re-encodes the payload as JSON.
const payloadKey = "event"

// toAppend renders the event as a spine append on stream. The log assigns Seq
// and Time; the body carries everything else.
func (e Event) toAppend(stream string) spine.AppendInput {
	body, _ := json.Marshal(e)
	return spine.AppendInput{
		Stream:  stream,
		Type:    string(e.Kind),
		Actor:   e.Actor,
		Time:    e.Time,
		Payload: map[string]any{payloadKey: string(body)},
	}
}

// fromSpine rebuilds a session event, taking Seq, Time and Actor from the log
// and the body from the payload.
func fromSpine(se spine.Event) Event {
	e := decodeBody(se)
	e.Seq = se.Seq
	e.Time = se.Time
	e.Actor = se.Actor
	return e
}

// decodeBody builds the body by stored type. Conversation events are folded
// under [payloadKey]; governance and record events keep the waist's and
// sealer's own payload shapes, which are read without changing the stored
// record, so signing and verification still see the originals.
func decodeBody(se spine.Event) Event {
	switch se.Type {
	case typeDispatchStart:
		return governanceEvent(KindActionAdmitted, se)
	case typeDispatchEnd:

		return governanceEvent(KindActionCompleted, se)
	case typeDispatchRejected:
		return governanceEvent(KindActionRejected, se)
	case typeRecordSealed:
		return Event{Kind: KindRecordSealed}
	case typeEgress:
		return egressEvent(se)
	case typeApprovalRequested:
		return approvalRequestedEvent(se)
	case typeApprovalDecision:
		return approvalDecisionEvent(se)
	case typeProjectContext:
		return projectContextEvent(se)
	default:
		var e Event
		if s, ok := se.Payload[payloadKey].(string); ok {
			_ = json.Unmarshal([]byte(s), &e)
		}
		e.Kind = Kind(se.Type)
		return e.sanitized()
	}
}

// governanceEvent reads the waist's payload (action, call, trust, error_class,
// error_code, goal). The call id comes back from a durable log as a float, so
// it goes through [asInt64].
func governanceEvent(kind Kind, se spine.Event) Event {
	e := Event{Kind: kind}
	e.Action, _ = se.Payload["action"].(string)
	e.Trust, _ = se.Payload["trust"].(string)
	e.Fault, _ = se.Payload["error_class"].(string)
	e.Code, _ = se.Payload["error_code"].(string)
	e.Goal, _ = se.Payload["goal"].(string)
	e.Call = asInt64(se.Payload["call"])
	return e
}

// egressEvent reads the egress sink's payload (host, verdict, reason). The
// verdict is the string "allowed" or "blocked" on the wire and maps to Allowed.
func egressEvent(se spine.Event) Event {
	e := Event{Kind: KindEgressDecision}
	e.Host, _ = se.Payload["host"].(string)
	e.Reason, _ = se.Payload["reason"].(string)
	if v, _ := se.Payload["verdict"].(string); v == "allowed" {
		e.Allowed = true
	}
	return e
}

// projectContextEvent reads a project-file trust decision. The verdict is a
// string on the wire and maps to Allowed.
func projectContextEvent(se spine.Event) Event {
	e := Event{Kind: KindProjectContext}
	e.Reason, _ = se.Payload["basis"].(string)
	e.Text, _ = se.Payload["files"].(string)
	if v, _ := se.Payload["verdict"].(string); v == "loaded" {
		e.Allowed = true
	}
	return e
}

// asInt64 reads an int64 (in-memory log) or float64 (a durable log decodes JSON
// numbers as floats), and returns 0 for anything else.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}
