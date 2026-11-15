package governed

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/brakes"
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
)

// Option configures a run. An invalid option's error is returned by [Assemble]. No zero
// value weakens governance; the only opt-out is [Waive].
type Option func(*config)

// ExternalAgent is an agent CLI driving the loop. Its tool calls cross the dispatch
// waist, so grant, brakes and budget bound it; its model calls are unobserved and the
// caller declares that gap.
type ExternalAgent struct {
	// Model is handed to the CLI and set on the submitted goal.
	Model  string
	Driver driver.Driver
}

type config struct {
	err error

	workdir  string
	sandbox  sandbox.Sandbox
	shell    ShellPolicy
	shellSet bool
	system   string
	skills   *skilltool.Set
	runID    string
	plan     harness.Plan

	planning    bool
	ledgerProof bool

	approve  []string
	prompter mission.ApprovalPrompter
	outside  []string

	reporter mission.Reporter

	toolset    []mission.Tool
	toolGrant  capability.Grant
	hasToolset bool

	bridge    ToolBridge
	caps      []string
	hasCaps   bool
	external  *ExternalAgent
	fanout    *fanoutSpec
	driver    string
	brake     *brakes.Limits
	budget    *budgetpkg.Limits
	waivers   map[Guard]string
	waiveList []Waiver
}

type fanoutSpec struct {
	resolve driver.ModelResolver
}

func (c *config) fail(err error) {
	if c.err == nil {
		c.err = err
	}
}

// WithWorkDir roots the run's sandbox. Empty is an error; omit the option to use the
// process working directory.
func WithWorkDir(dir string) Option {
	return func(c *config) {
		if strings.TrimSpace(dir) == "" {
			c.fail(errors.New("governed: WithWorkDir needs a directory"))
			return
		}
		c.workdir = dir
	}
}

// WithSandbox runs over a host-owned sandbox, which the run does not close. The
// containment gate still refuses an action needing more isolation than it gives, and
// [Posture.HostSandbox] records it.
func WithSandbox(sb sandbox.Sandbox) Option {
	return func(c *config) {
		if sb == nil {
			c.fail(errors.New("governed: WithSandbox given a nil sandbox; use Waive(GuardSandbox, reason) to run without one"))
			return
		}
		c.sandbox = sb
	}
}

// WithSystem sets the system prompt framing every turn.
func WithSystem(system string) Option { return func(c *config) { c.system = system } }

// WithSkills adds the skill toolset. The caller keeps s to grade the run's reads, so a
// multi-turn session passes one set to each.
func WithSkills(s *skilltool.Set) Option { return func(c *config) { c.skills = s } }

// WithRunID names the run's stream and goal. Empty is a fresh id.
func WithRunID(id string) Option { return func(c *config) { c.runID = id } }

// WithPlan applies a model's scaffolding plan. The zero plan adds nothing.
func WithPlan(p harness.Plan) Option { return func(c *config) { c.plan = p } }

// WithPlanning plans a ledger before the first build step and runs each item's check in
// the run's sandbox.
func WithPlanning() Option { return func(c *config) { c.planning = true } }

// WithLedgerProof makes the settled ledger decide completion. It needs [WithPlanning];
// a fan-out always requires proof.
func WithLedgerProof() Option { return func(c *config) { c.ledgerProof = true } }

// WithApproval names the actions that pause for a person, and the prompter that asks.
// With a nil prompter the listed actions are refused, because nobody can be reached.
func WithApproval(actions []string, prompter mission.ApprovalPrompter) Option {
	return func(c *config) {
		c.approve = append(c.approve, actions...)
		c.prompter = prompter
	}
}

// WithAllowance names the irreversible actions that reach outside the workspace, which
// run only if the goal declares them.
func WithAllowance(outside []string) Option {
	return func(c *config) { c.outside = append(c.outside, outside...) }
}

// WithReporter receives the run's conversational events alongside the spine, which
// still records them.
func WithReporter(r mission.Reporter) Option {
	return func(c *config) {
		if r == nil {
			c.fail(errors.New("governed: WithReporter given a nil reporter"))
			return
		}
		c.reporter = r
	}
}

// WithToolset runs tools under grant with no sandbox, so the run loses the verifier,
// auditor and containment gate and needs Waive(GuardSandbox, reason). An unrestricted
// grant is refused.
func WithToolset(tools []mission.Tool, grant capability.Grant) Option {
	return func(c *config) {
		if len(tools) == 0 {
			c.fail(errors.New("governed: WithToolset needs at least one tool"))
			return
		}
		if grant.Unrestricted() {
			// An unrestricted grant is indistinguishable from no policy.
			c.fail(errors.New("governed: WithToolset needs a grant that names its actions, not an unrestricted one"))
			return
		}
		c.toolset, c.toolGrant, c.hasToolset = tools, grant, true
	}
}

// WithExtensions adds the extensions' tools and grants exactly their names. A tool that
// reuses an offered name fails assembly, because it would inherit that grant.
func WithExtensions(b ToolBridge) Option {
	return func(c *config) {
		if b == nil {
			c.fail(errors.New("governed: WithExtensions given a nil bridge"))
			return
		}
		c.bridge = b
	}
}

// WithCapabilities narrows the grant to names plus the model call. A name outside the
// toolset grants nothing callable.
func WithCapabilities(names ...string) Option {
	return func(c *config) {
		c.caps, c.hasCaps = append([]string(nil), names...), true
	}
}

// WithExternalAgent hands the loop to an external agent CLI. A policy the external
// loop cannot enforce (approval or allowance actions) is an error, never ignored.
func WithExternalAgent(ea ExternalAgent) Option {
	return func(c *config) {
		if ea.Driver == nil {
			c.fail(errors.New("governed: WithExternalAgent needs a driver"))
			return
		}
		c.external = &ea
	}
}

// WithFanout lets the model delegate sub-goals to governed child agents. resolve serves
// a child whose archetype pins a model; nil keeps children on the host model.
func WithFanout(resolve driver.ModelResolver) Option {
	return func(c *config) { c.fanout = &fanoutSpec{resolve: resolve} }
}

// WithDriver selects the default run loop by registry name. An unknown name fails at
// assembly.
func WithDriver(name string) Option {
	return func(c *config) {
		if _, err := driver.Default().Resolve(name); err != nil {
			c.fail(err)
			return
		}
		c.driver = name
	}
}

// WithBrakeLimits replaces the default brake. A non-positive cap or window is an error,
// because it would never trip.
func WithBrakeLimits(l brakes.Limits) Option {
	return func(c *config) {
		if l.MaxActions <= 0 || l.Window <= 0 {
			c.fail(fmt.Errorf("governed: WithBrakeLimits needs a positive MaxActions and Window, got %d per %s", l.MaxActions, l.Window))
			return
		}
		c.brake = &l
	}
}

// WithBudget sets the spend ceiling [Assemble] opens, keyed by run id. A resumed run
// keeps its existing pool. Without it the hook is wired but unlimited.
func WithBudget(l budgetpkg.Limits) Option {
	return func(c *config) { c.budget = &l }
}

// Waive opts out of g with a reason for an auditor. It is recorded as a
// [WaivedEventType] event before anything else, and [Assemble] fails if that write
// fails, so a weaker run is never unrecorded.
func Waive(g Guard, reason string) Option {
	return func(c *config) {
		if !g.valid() {
			c.fail(fmt.Errorf("governed: cannot waive unknown guard %q", string(g)))
			return
		}
		if strings.TrimSpace(reason) == "" {
			c.fail(fmt.Errorf("governed: waiving %s needs a reason", g))
			return
		}
		if c.waivers == nil {
			c.waivers = map[Guard]string{}
		}
		c.waivers[g] = reason
		c.waiveList = append(c.waiveList, Waiver{Guard: g, Reason: reason})
	}
}
