package mission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

// DefaultPlannerMaxTokens bounds the planner's single call. A ledger is a short list
// of work items, not a transcript, so the ceiling is modest; a run that needs a longer
// plan raises it with WithPlannerMaxTokens.
const DefaultPlannerMaxTokens = 4096

// planSystem is the planner's own standing prompt. It is deliberately not the build
// system prompt with a plan instruction bolted on: the whole call is spent working out
// what the objective requires, and the model is asked to commit, per item, to how that
// item would be checked. The verify clause is not decoration. The ledger refuses an
// item that has none (goal.AppendItems), so a plan the model cannot say how to verify
// does not get recorded — which is the point. An item with no way to prove it is one a
// later step can only assert it did.
//
// The output contract is a bare JSON array, not prose. A model is measurably more
// reluctant to quietly overwrite a structured record than a paragraph, and asking it to
// emit the ledger as JSON carries that same reluctance into how the plan is produced.
const planSystem = `You are the planning phase of an autonomous agent. You do not do the work; you decide what the work is.

Expand the objective below into the concrete units of work it actually implies — the smallest set of items that, each proven, means the objective is met. For every item, state a concrete way to verify it: a command to run, a file to inspect, an observable condition that settles it. "It looks done" is not a verification; "go test ./... passes" or "the /health endpoint returns 200" is.

Respond with ONLY a JSON array, no prose before or after, no code fence. Each element is an object with exactly two string fields:
  - "item":   what the work is, in one clear sentence.
  - "verify": the concrete check that proves the item is done.

Example:
[{"item":"Add a /health endpoint","verify":"curl localhost:8080/health returns HTTP 200"}]

If the objective genuinely needs no work — it is already satisfied, or it is not a work request — respond with an empty array []. Do not invent filler items to look busy. Every item you return will be held to its verify clause.`

// Planner expands a goal's objective into its ledger with a single model call, against
// the provider-agnostic llm port. It is the model-backed implementation of goal.Planner
// the runtime pairs with the reconciler's planning gate; without it a goal runs
// unplanned, straight to building.
type Planner struct {
	model     llm.Model
	system    string
	maxTokens int
	sampling  *llm.Sampling
}

// PlannerOption configures a Planner.
type PlannerOption func(*Planner)

// WithPlannerSystem overrides the planner's standing prompt. Empty keeps the default.
func WithPlannerSystem(s string) PlannerOption {
	return func(p *Planner) {
		if s != "" {
			p.system = s
		}
	}
}

// WithPlannerMaxTokens overrides the output ceiling on the planner's call. Non-positive
// keeps the default.
func WithPlannerMaxTokens(n int) PlannerOption {
	return func(p *Planner) {
		if n > 0 {
			p.maxTokens = n
		}
	}
}

// WithPlannerSampling pins the planner's decoding parameters, so a reproducible run
// plans the same way it builds. Nil leaves the call free-running.
func WithPlannerSampling(s *llm.Sampling) PlannerOption {
	return func(p *Planner) { p.sampling = s }
}

// NewPlanner builds a model-backed planner over the llm port.
func NewPlanner(model llm.Model, opts ...PlannerOption) *Planner {
	p := &Planner{model: model, system: planSystem, maxTokens: DefaultPlannerMaxTokens}
	for _, o := range opts {
		o(p)
	}
	return p
}

var _ goal.Planner = (*Planner)(nil)

// Plan expands the goal's objective into ledger items. An empty result is not an error:
// a model that finds nothing to plan returns no items, and the reconciler settles the
// goal as stalled with that reason rather than building against a record that never said
// what done was. Malformed output is a transient error so the retry ladder samples the
// model again before the goal stalls; the ledger's own completeness rule (an item needs
// both text and a verify clause) is enforced here too, so an incomplete plan is retried
// rather than recorded.
func (p *Planner) Plan(ctx context.Context, r resource.Resource) ([]goal.LedgerItem, error) {
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "planner_spec_decode", err)
	}

	resp, err := p.model.Generate(ctx, llm.Request{
		System:    p.system,
		Messages:  []llm.Message{userTurn(p.prompt(spec), spec.Attachments)},
		MaxTokens: p.maxTokens,
		Sampling:  p.sampling,
	})
	if err != nil {
		return nil, err // the model classifies its own errors; the worker retries transient ones
	}

	items, err := parsePlan(resp.Message.TextContent())
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "planner_bad_output", err)
	}
	return items, nil
}

// prompt builds the planner's user turn: the objective, its stop condition, and — when
// the goal already carries a ledger — that ledger, with an instruction to return only
// what is not already covered. Showing the existing ledger is what makes a re-planned
// goal idempotent from the model's side: a planning step that crashed after writing the
// ledger re-runs, sees its own work, and returns nothing new. The append rule
// (goal.PlanExtension) is the structural backstop beneath that; this keeps the model
// from proposing a reworded near-duplicate the backstop cannot catch.
func (p *Planner) prompt(spec goal.Spec) string {
	var b strings.Builder
	b.WriteString("Objective:\n")
	b.WriteString(spec.Objective)
	if spec.StopCondition != "" {
		b.WriteString("\n\nThe objective is met when: ")
		b.WriteString(spec.StopCondition)
	}
	if len(spec.Ledger) > 0 {
		b.WriteString("\n\nThe ledger already carries the items below. Return ONLY items that are not already covered by them; if they already cover the objective, return an empty array. Do not restate an item that is already here.\n")
		for _, it := range spec.Ledger {
			b.WriteString("- ")
			b.WriteString(it.Item)
			b.WriteString(" (verified by: ")
			b.WriteString(it.Verify)
			b.WriteString(")\n")
		}
	}
	return b.String()
}

// planItem is the wire shape the planner asks the model to emit, one per ledger item.
type planItem struct {
	Item   string `json:"item"`
	Verify string `json:"verify"`
}

// parsePlan reads the model's output into ledger items. It tolerates a model that wraps
// the array in a code fence or in a sentence by extracting the outermost JSON array, but
// it does not tolerate an item missing its text or verify clause: an incomplete item is a
// malformed plan, reported so the caller retries rather than recording a ledger entry
// that cannot be verified. An empty array is valid and returns no items and no error.
func parsePlan(text string) ([]goal.LedgerItem, error) {
	raw := strings.TrimSpace(text)
	start := strings.IndexByte(raw, '[')
	end := strings.LastIndexByte(raw, ']')
	if start < 0 || end < start {
		return nil, fmt.Errorf("planner output carried no JSON array: %q", truncate(raw, 200))
	}
	var parsed []planItem
	if err := json.Unmarshal([]byte(raw[start:end+1]), &parsed); err != nil {
		return nil, fmt.Errorf("planner output was not a JSON array of {item, verify}: %w", err)
	}
	items := make([]goal.LedgerItem, 0, len(parsed))
	for i, pi := range parsed {
		item := strings.TrimSpace(pi.Item)
		verify := strings.TrimSpace(pi.Verify)
		if item == "" || verify == "" {
			return nil, fmt.Errorf("planner item %d is missing its item or verify clause", i)
		}
		items = append(items, goal.LedgerItem{Item: item, Verify: verify})
	}
	return items, nil
}

// truncate bounds an untrusted string spliced into an error message so a model that
// returned a wall of text does not blow up the log line.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
