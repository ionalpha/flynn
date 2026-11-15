package mission

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/sandbox"
)

// DefaultPlannerMaxTokens is modest because a ledger is a short list; raise it with
// [WithPlannerMaxTokens].
const DefaultPlannerMaxTokens = 4096

// planSystem is separate from the build prompt so the whole call goes to deciding what the
// objective requires and how each item is checked. The ledger refuses an item with no
// verify clause, because a later step could only assert it.
//
// The output is a JSON array because a model is measurably more reluctant to overwrite a
// structured record than a paragraph.
const planSystem = `You are the planning phase of an autonomous agent. You do not do the work; you decide what the work is.

Expand the objective below into the concrete units of work it actually implies: the smallest set of items that, each proven, means the objective is met. For every item, give the check that proves it.

The check is run, not read. It must be a single shell command line, and it is treated as proof only when that command exits 0. Write the command, not a description of it: "go test ./..." is a check; "go test ./... passes" and "it looks done" are not. Reach for a command whose exit code already means the right thing (a test run, a build, a linter, grep for the thing you added, curl --fail against the endpoint), and compose with && when one item needs two conditions.

An item you cannot express as a command is usually an item phrased too vaguely to check. Sharpen it, or split it into parts that can be. A check that cannot be run does not prove its item: it is recorded as unproven.

A check reads what the work leaves behind: a file, a build, a passing test, a running service. An objective whose whole result is what you report back (run a command and say what it printed, answer a question, look something up) leaves nothing for a check to read, so it has no items. Never plan an item, or a file, that exists only so a check has something to find.

Respond with ONLY a JSON array, no prose before or after, no code fence. Each element is an object with exactly two string fields:
  - "item":   what the work is, in one clear sentence.
  - "verify": the shell command whose exit-0 proves the item is done.

Example:
[{"item":"Add a /health endpoint","verify":"curl --fail --silent http://localhost:8080/health"}]

If the objective needs no checkable work (it is already satisfied, it is not a work request, or its result is only what you report), respond with an empty array []. Do not invent filler items to look busy. Every item you return will be held to its verify clause.`

// plannerCorrections is the rounds before a refused plan is a transient failure. The second
// round is for a model that fixed one check and kept another.
const plannerCorrections = 2

// Planner is the model-backed [goal.Planner]. Without one a goal goes straight to building.
type Planner struct {
	model     llm.Model
	system    string
	maxTokens int
	sampling  *llm.Sampling

	// lookPath and goos are fields so a test can stand in for another host.
	tools    []string
	lookPath func(string) (string, error)
	goos     string

	admission *Admission
}

// Admission sends a refused check back to the model in the correction loop, so it is fixed
// while cheap instead of stalling a run.
type Admission struct {
	// Verifier dry-runs each grounded check on the untouched tree, in the run's sandbox.
	// Nil admits on grounding alone.
	Verifier goal.ItemVerifier
	// Recorder receives every verdict, refusals included.
	Recorder goal.AdmissionRecorder
	// Dir is where a check runs. Empty disables grounding.
	Dir string
	// Confined means commands cannot write outside Dir.
	Confined bool
}

// WithPlannerAdmission turns on [Admission]. Without it only checks that name a tool are
// refused.
func WithPlannerAdmission(a Admission) PlannerOption {
	return func(p *Planner) { p.admission = &a }
}

// PlannerOption configures a [Planner].
type PlannerOption func(*Planner)

// WithPlannerSystem overrides the standing prompt. Empty keeps the default.
func WithPlannerSystem(s string) PlannerOption {
	return func(p *Planner) {
		if s != "" {
			p.system = s
		}
	}
}

// WithPlannerMaxTokens overrides [DefaultPlannerMaxTokens]. Non-positive keeps it.
func WithPlannerMaxTokens(n int) PlannerOption {
	return func(p *Planner) {
		if n > 0 {
			p.maxTokens = n
		}
	}
}

// WithPlannerSampling pins decoding so a reproducible run plans as it builds.
func WithPlannerSampling(s *llm.Sampling) PlannerOption {
	return func(p *Planner) { p.sampling = s }
}

// WithPlannerTools names the run's tools so a check that calls one is refused: a shell
// cannot start a tool, so the goal would stall on a check that never ran.
func WithPlannerTools(names ...string) PlannerOption {
	return func(p *Planner) { p.tools = names }
}

// NewPlanner defaults the system prompt to the plan prompt plus [HostNote].
func NewPlanner(model llm.Model, opts ...PlannerOption) *Planner {
	p := &Planner{model: model, system: planSystem + "\n\n" + HostNote(), maxTokens: DefaultPlannerMaxTokens, lookPath: sandbox.LookPath, goos: runtime.GOOS}
	for _, o := range opts {
		o(p)
	}
	return p
}

var _ goal.Planner = (*Planner)(nil)

// Plan returns no items without error when there is nothing to plan, and the reconciler
// stalls the goal for it. Malformed or incomplete output is transient, so the model is
// sampled again before the goal stalls.
func (p *Planner) Plan(ctx context.Context, r resource.Resource) ([]goal.LedgerItem, error) {
	spec, err := goal.DecodeSpec(r)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "planner_spec_decode", err)
	}

	messages := []llm.Message{userTurn(p.prompt(spec), spec.Attachments)}
	for attempt := 0; ; attempt++ {
		resp, err := p.model.Generate(ctx, llm.Request{
			System:    p.systemPrompt(),
			Messages:  messages,
			MaxTokens: p.maxTokens,
			Sampling:  p.sampling,
		})
		if err != nil {
			return nil, err // the model classifies its own errors
		}

		items, err := parsePlan(resp.Message.TextContent())
		if err != nil {
			return nil, fault.Wrap(fault.Transient, "planner_bad_output", err)
		}
		code := "planner_unrunnable_check"
		refusal := p.refuseToolChecks(items)
		if refusal == "" && p.admission != nil {
			code = "planner_check_not_admitted"
			if refusal, err = p.admit(ctx, r, spec, items); err != nil {
				return nil, err
			}
		}
		if refusal == "" {
			return items, nil
		}
		if attempt == plannerCorrections {
			return nil, fault.New(fault.Transient, code, refusal)
		}
		messages = append(messages, resp.Message, userTurn(refusal, nil))
	}
}

// systemPrompt names the run's tools up front so a correction round is not needed to.
func (p *Planner) systemPrompt() string {
	system := p.system
	if len(p.tools) > 0 {
		system += "\n\nYour own tools (" + strings.Join(p.tools, ", ") + ") are functions you call, not programs: a check is run by a shell, which cannot start them, so a check that names one can never pass. " +
			"An action you take through a tool is proven by that tool's recorded result. Plan the check on what the action leaves behind instead."
	}
	if p.admission != nil {
		system += "\n\nEvery check is run once, before any work, on the tree as it is, and is admitted only if it fails then. A check that already passes, that the shell cannot parse, or that runs a program the host lacks is sent back to you. " +
			"A check may name a file only if it exists now or the objective asks for it; do not invent a file for a check to find."
		if p.admission.Confined {
			system += " Commands are confined to the working directory: a check cannot name a path outside it that does not already exist, such as a file in the host's temp directory."
		}
	}
	return system
}

// refuseToolChecks returns "" when a shell can start every check. On Windows a POSIX shell
// is refused even when installed, because Git's bash fails to start under the confined token
// and reports 256 for every line.
func (p *Planner) refuseToolChecks(items []goal.LedgerItem) string {
	var b strings.Builder
	for _, it := range items {
		for _, name := range goal.NamedCommands(it.Verify, p.tools) {
			if p.runsAsProgram(name) {
				continue
			}
			fmt.Fprintf(&b, "- %q is checked by %q, which is the name of one of your own tools and not a program a shell on this host can start.\n", it.Item, name)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "These checks cannot run, so they would prove nothing and the goal would stall on them:\n" + b.String() +
		"A check is a shell command, and a tool is not one. An action taken through a tool is proven by the tool's recorded result, so do not plan a check that calls it. " +
		"Plan a command that tests what the action leaves behind (a file that exists, a test that passes, a program that exits 0). " +
		"Return the whole JSON array again, with every check fixed."
}

// admit runs the free text rules first and dry-runs only a check that passes them, because
// a check naming a file nobody made fails for that reason alone. The dry run uses the
// verifier and sandbox the run will, so a refusal here is a stall avoided. Every verdict is
// recorded. A cancelled context is the only error.
func (p *Planner) admit(ctx context.Context, r resource.Resource, spec goal.Spec, items []goal.LedgerItem) (string, error) {
	a := p.admission
	ground := goal.Ground{Dir: a.Dir, Confined: a.Confined, Objective: spec.Objective + "\n" + spec.StopCondition, LookPath: p.lookPath}
	var b strings.Builder
	for _, it := range items {
		v := goal.AdmissionVerdict{Item: goal.ItemID(it.Item, it.Verify), Admitted: true}
		var reasons []string
		for _, prob := range goal.CheckGrounding(it.Verify, ground) {
			v.Admitted, v.Refusal = false, prob.Refusal
			reasons = append(reasons, prob.Reason)
		}
		if v.Admitted && a.Verifier != nil {
			verdict, err := a.Verifier.VerifyItem(ctx, r, it)
			if err != nil {
				return "", err
			}
			v.DryRun = verdict.Executed
			if refusal, reason := goal.DryRunRefusal(it.Verify, verdict); refusal != "" {
				v.Admitted, v.Refusal = false, refusal
				reasons = append(reasons, reason)
			} else if !verdict.Executed {
				v.Reason = "admitted without a dry run: " + strings.TrimSpace(verdict.Detail)
			}
		}
		if !v.Admitted {
			v.Reason = strings.Join(reasons, " ")
			fmt.Fprintf(&b, "- %q, checked by %q (%s): %s\n", it.Item, it.Verify, v.Refusal, v.Reason)
		}
		if a.Recorder != nil {
			if err := a.Recorder.RecordAdmission(ctx, r, v); err != nil {
				return "", err
			}
		}
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "These checks were not admitted, so the goal has not started:\n" + b.String() +
		"A check is admitted only when it names files that exist or that the objective asks for, can run on this host as written, and fails now, on the tree as it is, because the work is not done. " +
		"Return the whole JSON array again, with every check fixed.", nil
}

func (p *Planner) runsAsProgram(name string) bool {
	if p.goos == "windows" && (name == "bash" || name == "sh") {
		return false
	}
	_, err := p.lookPath(name)
	return err == nil
}

// prompt shows any existing ledger, so a planning step that crashed after writing it sees
// its own work and returns nothing new. goal.PlanExtension rejects exact repeats; this
// stops a reworded near-duplicate it cannot catch.
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

type planItem struct {
	Item   string `json:"item"`
	Verify string `json:"verify"`
}

// parsePlan extracts the outermost JSON array, so a fence or sentence around it is
// tolerated. An item missing its text or verify clause is an error so the caller retries.
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

// truncate bounds untrusted model text spliced into an error.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
