package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/spine"
)

// This file is the binary's only caller of the governed-run assembly. Everything that
// makes a run governed is composed by package governed, which a host embeds the same way;
// what is here is the translation from the command line's levers to that package's
// options. internal/rigor fails the build if a composition step is declared back in this
// package.

// gateSetup is what an entry point asks for from the two gates above the run's capability
// grant: the actions that need a person to authorize them and the prompter that asks, and
// the actions whose effects leave the workspace and cannot be undone.
//
// The approval fields are independent: actions with no prompter is the non-interactive run
// that refuses rather than proceeds, and a prompter with no actions is an interactive
// session where nothing is asked to pause.
type gateSetup struct {
	approve  []string
	prompter mission.ApprovalPrompter
	// outside names the actions that reach outside the workspace irreversibly. One of them
	// runs only if the goal declares it, and a run that reaches an undeclared one is paused
	// with the ask rather than handed a refusal it would look for a way around.
	outside []string
}

// fanoutConfig enables the goals engine on a one-shot run: the model may delegate
// self-contained sub-goals to concurrent, governed child agents, and each child is routed
// to the model its bound Agent archetype pins (resolveModel turns a named model into a
// client). A nil *fanoutConfig leaves a run as a single conversation.
type fanoutConfig struct {
	resolveModel driver.ModelResolver
}

// assembly is everything one governed run is built from.
type assembly struct {
	model   llm.Model
	plan    harness.Plan
	workdir string
	system  string
	rstore  resource.Store
	jq      jobs.Queue
	log     spine.Log
	skills  *skilltool.Set
	runID   string
	// shell is what the run's commands are kept away from. A toolset run has no shell and
	// ignores it.
	shell    governed.ShellPolicy
	planning bool
	proof    bool
	gates    gateSetup
	budget   budgetpkg.Limits
	fanout   *fanoutConfig
	ext      *externAgent
	toolset  *boundToolset
}

// toolsetWaiver is the recorded reason a review-style run has no sandbox.
const toolsetWaiver = "caller-supplied toolset and grant: the run holds no shell or filesystem tool, so there is no working tree to confine"

// options translates the assembly into the governed package's options. The binary's
// defaults are the package's defaults, so the only things said here that change a run's
// posture are the toolset's explicit sandbox waiver and the shell policy the operator's
// flags describe.
func (a assembly) options() []governed.Option {
	opts := []governed.Option{
		governed.WithSystem(a.system),
		governed.WithPlan(a.plan),
		governed.WithApproval(a.gates.approve, a.gates.prompter),
		governed.WithAllowance(a.gates.outside),
	}
	if a.workdir != "" {
		opts = append(opts, governed.WithWorkDir(a.workdir))
	}
	if a.skills != nil {
		opts = append(opts, governed.WithSkills(a.skills))
	}
	if a.runID != "" {
		opts = append(opts, governed.WithRunID(a.runID))
	}
	if a.planning {
		opts = append(opts, governed.WithPlanning())
	}
	if a.proof {
		opts = append(opts, governed.WithLedgerProof())
	}
	if a.budget.Tokens > 0 || a.budget.Cost > 0 {
		opts = append(opts, governed.WithBudget(a.budget))
	}
	switch {
	case a.toolset != nil:
		opts = append(opts,
			governed.WithToolset(a.toolset.tools, a.toolset.grant),
			governed.Waive(governed.GuardSandbox, toolsetWaiver))
	case a.ext != nil:
		opts = append(opts, governed.WithExternalAgent(governed.ExternalAgent{Model: a.ext.model, Driver: a.ext.driver}))
	case a.fanout != nil:
		opts = append(opts, governed.WithFanout(a.fanout.resolveModel))
	}
	if a.toolset == nil {
		// A toolset run has no shell, and the package refuses a policy it cannot apply.
		opts = append(opts, governed.WithShellPolicy(a.shell))
	}
	return opts
}

// assemble builds the governed run the assembly describes.
func (a assembly) assemble(ctx context.Context) (*governed.Run, error) {
	return governed.Assemble(ctx, a.model, governed.Backends{Resources: a.rstore, Jobs: a.jq, Log: a.log}, a.options()...)
}

// stringList is a flag.Value that accumulates every occurrence of a repeatable flag,
// so `--require-approval bash --require-approval write` names two actions rather
// than the second overwriting the first.
type stringList struct{ values []string }

// String renders the accumulated values for the flag package's usage output.
func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(s.values, ",")
}

// Set appends one occurrence. A blank value is kept out rather than turned into an
// action named the empty string, which would list a requirement nothing can satisfy.
func (s *stringList) Set(v string) error {
	if v = strings.TrimSpace(v); v != "" {
		s.values = append(s.values, v)
	}
	return nil
}

// declaredAllowances turns the operator's list of allowed action names into the standing
// authorizations carried on the goal spec. Blank entries are dropped and duplicates
// collapse, so declaring one action twice is one authorization.
//
// Every declaration made this way is action-level: it authorizes the action against
// whatever it is attempted on. A declaration narrowed to one target is expressible on the
// spec and is not offered here, because nothing on the CLI's own path binds a target for
// the gate to match it against, and a flag whose narrower form silently authorizes nothing
// would be worse than not having the form.
func declaredAllowances(actions []string) []goal.Allowance {
	seen := map[string]bool{}
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		if name := strings.TrimSpace(a); name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]goal.Allowance, 0, len(names))
	for _, name := range names {
		out = append(out, goal.Allowance{Action: name})
	}
	return out
}

// checkAllowedAreMarked refuses an --allow name that no --irreversible marks. The
// declaration authorizes an action only where the run marks it, so an unmatched name
// authorizes nothing and reads as if it had: usually a misspelling of the action the
// operator meant to let through.
func checkAllowedAreMarked(outside, allowed []string) error {
	marked := map[string]bool{}
	for _, name := range outside {
		marked[strings.TrimSpace(name)] = true
	}
	for _, name := range allowed {
		if name = strings.TrimSpace(name); name != "" && !marked[name] {
			return fmt.Errorf("--allow %s names an action no --irreversible marks, so it would authorize nothing: --allow only lifts the hold --irreversible puts on an action, and does not widen what the run's normal grant permits; add --irreversible %s, or check the spelling", name, name)
		}
	}
	return nil
}

// unrecordedAttested reports how many of the external harness's events could not be
// written to the record, and why.
func unrecordedAttested(ea *externAgent) (int, error) {
	if ea == nil {
		return 0, nil
	}
	return governed.UnrecordedAttested(ea.driver)
}
