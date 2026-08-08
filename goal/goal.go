// Package goal is the agent's first desired-state kind: a Goal declares an
// objective and a stop condition (the desired state), and a reconciler drives it
// toward that condition by dispatching work steps and observing progress (the
// observed state). It is the agent's own execution model expressed on the generic
// resource + reconcile foundation: declarative, level-triggered, crash-resumable,
// and budget-bounded, rather than an imperative do-step-do-step loop that loses
// the thread on failure.
package goal

import (
	"encoding/json"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

const (
	// GroupVersion is the Goal kind's API group and version.
	GroupVersion = "goal.ionagent.io/v1alpha1"
	// Kind is the resource kind name.
	Kind = "Goal"
	// Finalizer is the key the reconciler adds to a goal so it gets a chance to
	// clean up owned work (child goals, runs, worktrees) before the goal is removed.
	Finalizer = "goal.ionagent.io/cleanup"
	// StepJobKind is the job kind a dispatched goal step is enqueued under.
	StepJobKind = "goal.step"
	// PlanJobKind is the job kind a goal's planning step is enqueued under. It rides
	// the same queue and worker as a build step and is distinguished by kind, so
	// planning inherits the lease, crash-resume and retry ladder a step already has
	// rather than growing a second execution path.
	PlanJobKind = "goal.plan"
	// VerifyJobKind is the job kind a goal's item-verification step is enqueued under.
	// Like planning it rides the same queue and worker as a build step, so a
	// verification inherits the lease, crash-resume and retry ladder rather than
	// growing a third execution path, and is distinguished only by kind.
	VerifyJobKind = "goal.verify"
)

// Phase is a coarse, human-facing lifecycle summary of a goal, derived from its
// conditions (a convenience projection, not the source of truth).
type Phase string

// The phases a goal moves through, from accepted to a terminal converged or
// stalled state.
const (
	PhasePending   Phase = "Pending"   // accepted, no step run yet
	PhasePlanning  Phase = "Planning"  // expanding the objective into a ledger, before any building
	PhaseRunning   Phase = "Running"   // a step is in flight or more are queued
	PhaseConverged Phase = "Converged" // the stop condition is satisfied
	PhaseStalled   Phase = "Stalled"   // out of budget or a step failed terminally
)

// Standard condition types (kstatus-style: abnormal conditions are present and
// True only when something noteworthy holds).
const (
	CondReady       = "Ready"       // True once the goal has converged
	CondReconciling = "Reconciling" // True while the controller is actively working
	CondStalled     = "Stalled"     // True when progress has stopped abnormally
)

var specSchema = json.RawMessage(`{
  "type": "object",
  "required": ["objective", "stopCondition"],
  "properties": {
    "objective": {"type": "string", "minLength": 1},
    "stopCondition": {"type": "string", "minLength": 1},
    "maxSteps": {"type": "integer", "minimum": 0},
    "grant": {"type": "array", "items": {"type": "string"}},
    "depth": {"type": "integer", "minimum": 0},
    "budgetPool": {"type": "string"},
    "budget": {
      "type": "object",
      "properties": {
        "tokens": {"type": "integer", "minimum": 0},
        "cost": {"type": "number", "minimum": 0},
        "windowFraction": {"type": "number", "minimum": 0, "maximum": 1}
      },
      "additionalProperties": false
    },
    "ledger": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "item", "verify"],
        "properties": {
          "id": {"type": "string", "minLength": 1},
          "item": {"type": "string", "minLength": 1},
          "verify": {"type": "string", "minLength": 1}
        },
        "additionalProperties": false
      }
    },
    "units": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "objective", "verify"],
        "properties": {
          "id": {"type": "string", "minLength": 1},
          "objective": {"type": "string", "minLength": 1},
          "verify": {"type": "string", "minLength": 1},
          "dependsOn": {"type": "array", "items": {"type": "string", "minLength": 1}},
          "actions": {"type": "array", "items": {"type": "string"}},
          "agent": {"type": "string"}
        },
        "additionalProperties": false
      }
    },
    "invariants": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["id", "statement"],
        "properties": {
          "id": {"type": "string", "minLength": 1},
          "statement": {"type": "string", "minLength": 1},
          "check": {"type": "string"}
        },
        "additionalProperties": false
      }
    },
    "allowances": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["action"],
        "properties": {
          "action": {"type": "string", "minLength": 1},
          "target": {"type": "string"}
        },
        "additionalProperties": false
      }
    },
    "system": {"type": "string"},
    "driver": {"type": "string"},
    "model": {"type": "string"},
    "attachments": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["mediaType", "data"],
        "properties": {
          "mediaType": {"type": "string", "minLength": 1},
          "data": {"type": "string"}
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}`)

// Spec is a goal's desired state: what to achieve, the condition that means it is
// done, an optional ceiling on how many steps may be spent trying, and the
// capabilities the goal is authorized to use.
type Spec struct {
	Objective     string `json:"objective"`
	StopCondition string `json:"stopCondition"`
	MaxSteps      int    `json:"maxSteps,omitempty"`
	// Grant is the exact set of action names this goal may take, carried on the goal
	// itself so authority travels with the work rather than being fixed at the
	// executor. One executor can then drive goals of differing authority (a parent at
	// full grant, a delegated child narrowed to a subset), and a child's grant is set
	// to a subset of its parent's so a delegation can never widen authority. Empty
	// defers to the executor's default grant, so an ungoverned standalone run is
	// unchanged.
	Grant []string `json:"grant,omitempty"`
	// Depth is how many delegation hops separate this goal from a root goal: a root
	// is 0 and a spawned child is its parent's depth plus one. A fan-out spawner
	// refuses to create a child past a maximum depth, so a chain of agents spawning
	// agents cannot recurse without bound.
	Depth int `json:"depth,omitempty"`
	// BudgetPool is the run id whose budget this goal charges and reserves against.
	// A fan-out shares one pool: every descendant inherits the root's pool, so the
	// whole graph is bounded by a single ceiling rather than a budget per goal. Empty
	// means the goal is its own pool (a standalone root).
	BudgetPool string `json:"budgetPool,omitempty"`
	// Budget bounds the goal by what it spends, alongside MaxSteps' bound on how many
	// steps it takes: a ceiling on total tokens, cost, and share of the plan window,
	// checked at the same reconcile point as MaxSteps and enforced against the spend
	// recorded on BudgetPool. A step is the wrong unit for cost (a step that reads a
	// file and a step that runs a full-codebase gather differ by an order of
	// magnitude), so this bounds the thing that actually varies. The zero value bounds
	// nothing, so a goal that sets no budget is governed by MaxSteps exactly as before.
	Budget SpendBudget `json:"budget,omitempty"`
	// System is the standing system prompt this goal runs under, carried on the goal
	// so a delegated child can run as a different agent than its parent (its prompt
	// baked in by the spawner from the bound Agent). Empty defers to the executor's
	// default prompt, so a standalone run is unchanged.
	System string `json:"system,omitempty"`
	// Driver names the run loop this goal uses (resolved from the driver registry), and
	// Model names the model it runs on. They are carried on the goal so a delegated
	// child can run a different loop and model than its parent (set by the spawner from
	// the bound Agent). Empty defers to the host default loop and model, so a standalone
	// run is unchanged.
	Driver string `json:"driver,omitempty"`
	Model  string `json:"model,omitempty"`
	// Attachments are images seeded onto the goal's opening user turn. The
	// objective is the opening turn's text; the attachments are its images, so
	// a goal can open on a picture the way a composer prompt can. They are the
	// model port's own image type (bytes inline, like the rest of a
	// conversation in the checkpoint), so no translation is needed to open the
	// turn. Empty on every text-only goal, so the text path serializes
	// identically and is unchanged.
	Attachments []llm.Image `json:"attachments,omitempty"`
	// Ledger is the goal's objective expanded into the units of work it implies,
	// each carrying its own declared way to verify it. It is desired state, which is
	// why it lives here and not in status: it is what the goal is committed to
	// doing, and the per-item proven/unproven observation of it is Status.Ledger.
	// The planner phase writes it before any building starts, and it is
	// append-and-mark-only from then on (see ledger.go). Empty on a goal that runs
	// without a planner, which is every goal composed before planning is wired.
	Ledger []LedgerItem `json:"ledger,omitempty"`
	// Units is the goal's fan-out written down in advance: a graph of child units with
	// dependency edges, admitted in dependency order as governed children (see
	// units.go). It is desired state for the same reason the ledger is, and it is the
	// plan-driven counterpart to the model deciding to spawn mid-conversation: the
	// decomposition is already agreed, so it arrives written rather than rediscovered.
	// A malformed graph (a cycle, an edge to a unit that does not exist) is refused at
	// admission, before a child is created. Empty on every goal that does not fan out
	// from a plan, and such a goal behaves exactly as it did before.
	Units []Unit `json:"units,omitempty"`
	// Invariants are the terms of the run: what must stay true while the goal works,
	// as against the stop condition's one thing that must become true (see
	// invariant.go). They are desired state, and they are the one part of it the run
	// may never trade against progress: an auditor rules on them before the stop
	// evaluator is asked anything, a breach settles the goal whatever the evaluator
	// was about to say, and a term already adopted cannot be dropped or reworded.
	// Empty on a goal that states no terms, and such a goal behaves exactly as it did
	// before.
	Invariants []Invariant `json:"invariants,omitempty"`
	// Allowances are the irreversible actions outside the workspace this run is
	// authorized to take (see allowance.go). They are the standing form of a decision
	// nobody will be present to make: a run that reaches an undeclared one is paused with
	// the ask rather than refused into a model that would look for another route. Empty
	// on a goal that declares none, and such a goal can take no such action at all, which
	// is the default and the point.
	Allowances []Allowance `json:"allowances,omitempty"`
}

// SpendBudget is a goal's spend ceiling on three axes. Tokens and Cost cap the total
// the goal may spend on its budget pool; WindowFraction caps the share of the plan
// window the run may consume, in [0,1] (0.5 is half the window). A zero field is no
// bound on that axis, so the zero SpendBudget bounds nothing.
//
// Window share matters where the scarce resource is not money but a subscription's
// plan window: there the argument is to stop at a percentage of the weekly window
// rather than at a dollar figure. Flynn enforces the fraction but does not source the
// window data; an app supplies that through the reconciler's WindowSource, and the
// bound has no effect until one is wired.
//
// These ceilings are the agent's own, so crossing one stops the goal with a named
// reason. That is a different outcome from hitting a provider's own limit (a pause
// that resumes when the provider resets) or a transient error (retried with backoff),
// and it deliberately does not share their code path: see the reconciler's spendGuard.
type SpendBudget struct {
	Tokens         int64   `json:"tokens,omitempty"`
	Cost           float64 `json:"cost,omitempty"`
	WindowFraction float64 `json:"windowFraction,omitempty"`
}

// IsZero reports whether the budget bounds nothing on any axis, so the reconciler can
// skip the spend guard entirely for a goal that sets no ceiling.
func (b SpendBudget) IsZero() bool {
	return b.Tokens == 0 && b.Cost == 0 && b.WindowFraction == 0
}

// InFlight records a dispatched step not yet observed complete, so a re-reconcile
// observes the running work instead of launching a duplicate.
type InFlight struct {
	JobID     string    `json:"jobID"`
	StartedAt time.Time `json:"startedAt"`
	// Kind is the job kind dispatched (StepJobKind or PlanJobKind), so the observing
	// pass knows what it is observing. A planning step is not building, so it does
	// not spend the build budget; without this the reconciler cannot tell the two
	// apart once the job is done and the reservation is being cleared. Empty on a
	// reservation written before planning existed, which reads as a build step.
	Kind string `json:"kind,omitempty"`
}

// Status is a goal's observed state.
type Status struct {
	Phase Phase `json:"phase,omitempty"`
	// ObservedSpecHash is the resource.SpecHash the reconciler last acted on, so a
	// reconcile is a no-op while the spec is unchanged and the goal has settled.
	ObservedSpecHash string      `json:"observedSpecHash,omitempty"`
	Steps            int         `json:"steps,omitempty"`
	InFlight         *InFlight   `json:"inFlight,omitempty"`
	Conditions       []Condition `json:"conditions,omitempty"`
	Message          string      `json:"message,omitempty"`
	// Checkpoint is opaque progress a worker persists mid-step so a step that
	// crashes resumes from here instead of restarting. It is owned by the step
	// executor; the reconciler never interprets it.
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	// WaitingSince marks the goal as parked: its last step reported ErrWaiting (no
	// progress, waiting on external state such as a fan-out's running children),
	// stamped with when the worker recorded it. While set, the reconciler does not
	// dispatch a step, evaluate the stop condition, or count the wait against the
	// step budget. A settling child clears it (the prompt wake); the reconciler's
	// recheck fallback clears it after a bounded delay if that wake is lost.
	WaitingSince *time.Time `json:"waitingSince,omitempty"`
	// Planned records that the planning phase has run to completion. It is separate
	// from a non-empty Ledger because the two answer different questions: a planner
	// that ran and produced nothing is a planned goal with an empty ledger, which is
	// a stall, not an invitation to plan again on the next pass.
	Planned bool `json:"planned,omitempty"`
	// Ledger is the observed state of Spec.Ledger, one entry per planned item and in
	// the same order. Every entry starts unproven, so a goal begins from a record
	// that says nothing is done. Completion is a transition here.
	Ledger []LedgerState `json:"ledger,omitempty"`
	// Units is the observed state of Spec.Units, one entry per unit and in the same
	// order: which units have had a child goal created for them, which of those
	// children have finished, and which finished having proven their unit. It is what
	// makes a fan-out resumable, so a run that crashes with children in flight comes
	// back knowing they exist rather than creating them a second time.
	Units []UnitState `json:"units,omitempty"`
	// Invariants is the observed state of Spec.Invariants: which terms the run has
	// adopted, what each said when it was adopted, how often it has been audited, and
	// any breach found. It is durable because both halves of the rule need to outlive
	// a reconcile: the adopted wording is what a later spec's rewording is caught
	// against, and a recorded breach is what stops the goal converging on a pass that
	// runs no audit of its own.
	Invariants []InvariantState `json:"invariants,omitempty"`
	// VerifyPending marks that a build step has completed and the current ledger item's
	// declared check has not been run since. It is what alternates the run between
	// building and verifying: the reconciler sets it when it observes a build step and
	// clears it when it observes the verification, so exactly one check runs per build
	// step rather than one per reconcile tick. It is durable because the alternation has
	// to survive a crash: a run that restarted here would otherwise build twice and
	// verify once, and the item's state would lag the work by a step.
	VerifyPending bool `json:"verifyPending,omitempty"`
	// ItemFeedback is the detail of the last verification that ran and did not pass, so
	// the next build step is told what its own declared check reported rather than being
	// asked to guess why the item is still open. The executor surfaces it and the
	// reconciler clears it once the item is proven. Empty when the current item has no
	// failed check behind it.
	ItemFeedback string `json:"itemFeedback,omitempty"`
	// IdleStreak, ProgressMark and LastActivity drive no-progress detection
	// (progress.go). ProgressMark is the fingerprint of substantive work observed after
	// the last step that made any, LastActivity is a short description of what the most
	// recent step did (so a no-progress stall can name it), and IdleStreak counts the
	// consecutive steps since ProgressMark last changed. They are the observed state of
	// "is this run still getting anywhere", computed over the durable record rather than
	// the working tree.
	IdleStreak   int    `json:"idleStreak,omitempty"`
	ProgressMark string `json:"progressMark,omitempty"`
	LastActivity string `json:"lastActivity,omitempty"`
	// ProgressNudge is the warning the reconciler wants the next step handed when the
	// goal is stalling but not yet stopped (see ProgressWarning). The executor surfaces
	// it to the agent — a goal told it is stalling sometimes un-stalls — and clears it
	// once delivered. Empty when the goal is making progress.
	ProgressNudge string `json:"progressNudge,omitempty"`
	// LastVerdict, VerdictMark and VerdictRepeat drive non-convergence detection
	// (converge.go). LastVerdict is the refusal the run was last given verbatim, kept so
	// the stall can quote it; VerdictMark is the normalized form that refusal was compared
	// on, together with how much of the ledger stood proven at the time; and VerdictRepeat
	// counts the consecutive build-and-check cycles that have ended in that same refusal.
	// They are the observed state of "is this run still being told something new", which is
	// a different question from whether it is busy.
	LastVerdict   string `json:"lastVerdict,omitempty"`
	VerdictMark   string `json:"verdictMark,omitempty"`
	VerdictRepeat int    `json:"verdictRepeat,omitempty"`
}

// Condition is one standard status condition (the shared resource.Condition).
type Condition = resource.Condition

// RegisterKind registers the Goal kind so goals can be stored and admitted like
// any other resource.
func RegisterKind(reg *resource.Registry) error {
	return reg.Register(resource.Kind{
		APIVersion: GroupVersion,
		Name:       Kind,
		Schema:     specSchema,
		Singular:   "goal",
		Plural:     "goals",
	})
}

// DecodeSpec reads the typed spec from a resource.
func DecodeSpec(r resource.Resource) (Spec, error) { return resource.DecodeSpec[Spec](r) }

// DecodeStatus reads the typed status from a resource.
func DecodeStatus(r resource.Resource) (Status, error) { return resource.DecodeStatus[Status](r) }

// statusHead is the subset of Status the reconciler reads before it commits to doing
// work: the phase and observed spec hash that decide the no-op skip. Decoding into it
// skips over the opaque Checkpoint (which carries the whole transcript) instead of
// copying it, so the periodic resync of already-settled goals no longer materializes
// the transcript on every pass just to find it has nothing to do.
type statusHead struct {
	Phase            Phase  `json:"phase,omitempty"`
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`
}

// decodeStatusHead reads only the scalar status fields the no-op skip needs, without
// copying the Checkpoint. The full DecodeStatus runs only once the reconcile is going
// to act on the goal.
func decodeStatusHead(r resource.Resource) (statusHead, error) {
	var h statusHead
	if len(r.Status) == 0 {
		return h, nil
	}
	return h, json.Unmarshal(r.Status, &h)
}

// Encode marshals the status for writing back onto a resource.
func (s Status) Encode() (json.RawMessage, error) { return resource.EncodeStatus(s) }

// SetCondition upserts c by type, stamping LastTransitionTime only when the
// status value actually changes (so a no-op reconcile does not churn the time).
func (s *Status) SetCondition(c Condition, now time.Time) {
	s.Conditions = resource.SetCondition(s.Conditions, c, now)
}

func hasFinalizer(fz []string, key string) bool {
	for _, f := range fz {
		if f == key {
			return true
		}
	}
	return false
}

func removeFinalizer(fz []string, key string) []string {
	out := fz[:0:0]
	for _, f := range fz {
		if f != key {
			out = append(out, f)
		}
	}
	return out
}
