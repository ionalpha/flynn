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

// Option configures a run. Options accumulate: an invalid one records its error and
// [Assemble] returns it, so a bad option is reported once with its cause instead of
// being dropped. Nothing about an option's zero value weakens governance; the only
// opt-out is [Waive].
type Option func(*config)

// ExternalAgent is an external agent CLI that drives the run's loop in place of a
// native model conversation. Every tool call it makes crosses the same dispatch waist,
// so the grant, brakes and budget bound it; its inner model calls stay unobserved and
// are declared as a provenance gap by the caller.
type ExternalAgent struct {
	// Model is the model string the CLI is handed, set on the submitted goal.
	Model string
	// Driver builds the episode loop.
	Driver driver.Driver
}

type config struct {
	err error

	workdir  string
	sandbox  sandbox.Sandbox
	resLimit sandbox.ResourceLimits
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

// WithSandbox runs over a sandbox the host owns instead of creating a confined local
// one. The run does not close it. It is still subject to the containment gate, so a
// host sandbox that provides weaker isolation than an action's trust needs refuses the
// action rather than running it, and [Posture.HostSandbox] records that the run did not
// build its own confinement.
func WithSandbox(sb sandbox.Sandbox) Option {
	return func(c *config) {
		if sb == nil {
			c.fail(errors.New("governed: WithSandbox given a nil sandbox; use Waive(GuardSandbox, reason) to run without one"))
			return
		}
		c.sandbox = sb
	}
}

// WithResourceLimits caps the host memory and process count of the commands the run's
// tools execute. It applies only to the sandbox the run builds.
func WithResourceLimits(l sandbox.ResourceLimits) Option {
	return func(c *config) { c.resLimit = l }
}

// WithSystem sets the system prompt framing every turn.
func WithSystem(system string) Option { return func(c *config) { c.system = system } }

// WithSkills adds the skill toolset to the run's tools. The caller keeps the set to
// grade the run against its reads, so a session spanning turns hands one set to each.
func WithSkills(s *skilltool.Set) Option { return func(c *config) { c.skills = s } }

// WithRunID names the run's stream and goal. Empty is a fresh id.
func WithRunID(id string) Option { return func(c *config) { c.runID = id } }

// WithPlan applies a model's scaffolding plan, so a weaker model is driven with the
// support it needs. The zero plan of a strong model adds nothing.
func WithPlan(p harness.Plan) Option { return func(c *config) { c.plan = p } }

// WithPlanning expands the objective into a visible ledger before the first build step
// and closes the loop with a verifier that runs each item's declared check in the run's
// sandbox.
func WithPlanning() Option { return func(c *config) { c.planning = true } }

// WithLedgerProof makes the settled ledger, not the model's final answer, decide
// whether a goal that carries one is done. It has effect with [WithPlanning]; a fan-out
// always requires proof.
func WithLedgerProof() Option { return func(c *config) { c.ledgerProof = true } }

// WithApproval names the actions that pause for a person, and the prompter that asks.
// An empty actions list with a prompter is an interactive session where nothing is
// listed. Actions with a nil prompter is the non-interactive run: the listed actions
// are refused, because a run that cannot reach a person does not proceed as though it
// had.
func WithApproval(actions []string, prompter mission.ApprovalPrompter) Option {
	return func(c *config) {
		c.approve = append(c.approve, actions...)
		c.prompter = prompter
	}
}

// WithAllowance names the actions whose effects leave the workspace and cannot be
// undone. One of them runs only where the goal declared it in advance.
func WithAllowance(outside []string) Option {
	return func(c *config) { c.outside = append(c.outside, outside...) }
}

// WithReporter receives the run's conversational events in addition to the spine. The
// spine always records them; a reporter cannot replace that record.
func WithReporter(r mission.Reporter) Option {
	return func(c *config) {
		if r == nil {
			c.fail(errors.New("governed: WithReporter given a nil reporter"))
			return
		}
		c.reporter = r
	}
}

// WithToolset runs a caller-supplied toolset under grant instead of the sandboxed
// working-tree tools. The grant is the complete authority the waist consults. There is
// no sandbox, so the run also loses the sandbox-bound verifier, auditor and containment
// gate; that is a weaker posture and requires Waive(GuardSandbox, reason). A grant that
// admits every action is refused, so the caller always names what the tools may do.
func WithToolset(tools []mission.Tool, grant capability.Grant) Option {
	return func(c *config) {
		if len(tools) == 0 {
			c.fail(errors.New("governed: WithToolset needs at least one tool"))
			return
		}
		if grant.Unrestricted() {
			// An unrestricted grant admits every action, which is the one grant that cannot be
			// told apart from having no policy. The caller names what the tools may do.
			c.fail(errors.New("governed: WithToolset needs a grant that names its actions, not an unrestricted one"))
			return
		}
		c.toolset, c.toolGrant, c.hasToolset = tools, grant, true
	}
}

// WithExtensions adds the tools the loaded extensions contribute, and grants exactly
// their names. An extension tool is permitted by the same grant as any other tool, and
// the extension's own credential checks still apply at the waist. A tool that reuses the
// name of one the run already offers fails assembly: it would inherit that name's grant.
func WithExtensions(b ToolBridge) Option {
	return func(c *config) {
		if b == nil {
			c.fail(errors.New("governed: WithExtensions given a nil bridge"))
			return
		}
		c.bridge = b
	}
}

// WithCapabilities narrows the grant to exactly these actions plus the model call, so
// an archetype run is the least-privilege boundary its Agent resource declares. It
// narrows only: a name outside the offered toolset grants nothing the run could call.
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

// WithFanout lets the model delegate self-contained sub-goals to concurrent, governed
// child agents. resolve turns a named model into a client for a child whose archetype
// pins one; nil restricts children to the host model.
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

// WithBrakeLimits replaces the default rate breaker. A non-positive cap or window is an
// error: a limit that never trips would be no brake while reading as one.
func WithBrakeLimits(l brakes.Limits) Option {
	return func(c *config) {
		if l.MaxActions <= 0 || l.Window <= 0 {
			c.fail(fmt.Errorf("governed: WithBrakeLimits needs a positive MaxActions and Window, got %d per %s", l.MaxActions, l.Window))
			return
		}
		c.brake = &l
	}
}

// WithBudget sets the spend ceiling [Assemble] opens for the run, so it is in force on
// every path that starts the run. A run with no ceiling is unchanged: its budget hook is
// wired but a pool with no budget is unlimited. The ceiling is the run's pool, keyed by its
// id: a pool already open under that id (a resumed run) is kept as it stands.
func WithBudget(l budgetpkg.Limits) Option {
	return func(c *config) { c.budget = &l }
}

// Waive records an explicit opt-out of g, with the reason an auditor will read. It is
// the only way to run with less than the binary's guards. The waiver is written to the
// run's stream as a [WaivedEventType] event before anything else, and [Assemble] fails
// if that write fails, so a weaker run is never unrecorded.
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
