package main

import (
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/memory/hybrid"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/skill/skilltool"
)

// driveConfig collects the optional levers a run is driven with. Its zero value (no
// budget, no resource caps) drives a run exactly as before, so a caller that passes
// no option is unaffected.
type driveConfig struct {
	budget    budgetpkg.Limits
	resLimits sandbox.ResourceLimits
	extAgent  *externAgent
	toolset   *boundToolset
	observe   func(session.Event)
	planning  bool
	proof     bool
	skills    *skilltool.Set
	gates     gateSetup
	// allowances are the irreversible actions outside the workspace this run was declared
	// to be allowed, carried onto the goal spec so the waist checks them and the
	// reconciler can tell an ask that has been answered from one that has not.
	allowances []goal.Allowance
	// terms are the invariants the run is held to, and stopCondition is what the operator
	// says being finished means. Both come off a goal spec file; empty leaves the run with
	// no stated terms and the default stop condition, which is every run that does not
	// pass one.
	terms         []goal.Invariant
	stopCondition string
	// emb is the embedding model recall ranks by, nil for the lexical order alone. It
	// is read by runLearningMission, which builds the run's memory, and not by drive:
	// the run's memory is assembled before the conversation is, and this is the option
	// set the entry points already thread through to it.
	emb hybrid.Embedder
}

// driveOption configures a run driven by drive.
type driveOption func(*driveConfig)

// withBudget caps a run's total spend: every model and tool call the run makes (and,
// in a fan-out, its children, which share one pool) is charged against the ceiling,
// and an action is refused once it is reached. A zero-limit budget is unlimited, so
// passing it leaves the run uncapped.
func withBudget(l budgetpkg.Limits) driveOption {
	return func(c *driveConfig) { c.budget = l }
}

// withMemoryEmbedder ranks the run's memory recall by meaning as well as words. A
// nil embedder is the default and leaves the durable store's own lexical order, so
// an install with no embedding model configured runs exactly as it did.
func withMemoryEmbedder(e hybrid.Embedder) driveOption {
	return func(c *driveConfig) { c.emb = e }
}

// withResourceLimits caps the host memory and process count of the commands a run's
// tools execute, on top of the always-on wall-clock and process-tree containment. The
// zero value applies no cap, so passing it leaves a run's commands unconstrained. See
// sandbox.ResourceLimits for the per-platform enforcement.
func withResourceLimits(r sandbox.ResourceLimits) driveOption {
	return func(c *driveConfig) { c.resLimits = r }
}

// withPlanning turns the planning phase on for the run: before the first build step the
// goal expands its objective into a visible ledger, each item carrying how it would be
// checked. It is opt-in per entry point rather than always-on because it changes what a
// run does first — a plan model call, then the ledger gate — so a command adopts it
// deliberately once its flow expects it. The `goal` command sets it; the other native
// paths run unplanned until they adopt it in turn.
func withPlanning() driveOption {
	return func(c *driveConfig) { c.planning = true }
}

// withLedgerProof makes the run's plan binding: each item's declared check is run and the
// goal will not report success over an item the record cannot show proof for.
//
// It is separate from withPlanning because the two carry different risk. Planning always
// runs the producer, so a run's items visibly flip to proven and a run's report can say
// how many were proven by execution; this adds the refusal on top, which is the part that
// can stop a run whose plan wrote checks no machine can run. It is staged, not optional:
// the refusal is the point of keeping a ledger, and a build that never turns it on has a
// plan that decides nothing.
func withLedgerProof() driveOption {
	return func(c *driveConfig) { c.proof = true }
}

// withExternalAgent drives the run through an external agent CLI backend (its own
// harness runs the loop) instead of a native model conversation. The model llm.Model
// passed to drive is unused in this mode: the external CLI drives its own model,
// selected by the agent's model string. Every tool call the CLI makes still comes back
// through the same dispatch waist, so governance is unchanged.
func withExternalAgent(ea *externAgent) driveOption {
	return func(c *driveConfig) { c.extAgent = ea }
}

// withToolset drives the run over a caller-supplied toolset and grant instead of
// the sandboxed working-tree tools. This is how a specialised run (a pull-request
// review) holds exactly the authority its archetype declares: the toolset carries
// no shell and no filesystem, and the grant it comes with is the complete list of
// what the waist admits. The working directory is untouched; a toolset run never
// reads or writes the tree.
func withToolset(t *boundToolset) driveOption {
	return func(c *driveConfig) { c.toolset = t }
}

// withEventObserver invokes fn on every session event as the run streams, in
// addition to rendering. A caller uses it to read an outcome off the run's own
// recorded events (a submitted review verdict, say) rather than re-deriving it
// out of band, so what the caller acts on and what the record says cannot differ.
func withEventObserver(fn func(session.Event)) driveOption {
	return func(c *driveConfig) { c.observe = fn }
}

// withSkills gives the run the skill toolset, which is what serves a skill's
// procedure: recall offers a skill by name and description, and the model calls
// skill_read to get the body. Without it a run is offered nothing and can read
// nothing, so a caller that recalls skills into the prompt must pass a toolset over
// the same store it recalled from, or the offer names a tool that is not there.
//
// The toolset rather than the store, because the caller wants it back afterwards:
// it holds the list of skills the run actually loaded, which is what the run's
// outcome is credited to.
func withSkills(s *skilltool.Set) driveOption {
	return func(c *driveConfig) { c.skills = s }
}

// withApproval requires a person to authorize each of the named actions before the run
// takes it, and resolves that pause through prompter. It is opt-in per action rather than
// on by default: a run's standing controls are its capability grant and its sandbox, and
// approval is the second gate an operator adds above them for the actions they want to
// see before they happen.
//
// A nil prompter is the non-interactive case and it is fail-closed: with nobody to ask,
// the listed action is refused rather than taken. Naming no actions leaves the run
// ungated, which is the default.
func withApproval(actions []string, prompter mission.ApprovalPrompter) driveOption {
	return func(c *driveConfig) {
		c.gates.approve, c.gates.prompter = actions, prompter
	}
}

// withAllowance marks the actions that reach outside the workspace and cannot be undone,
// and declares which of them this run may take. A marked action the run did not declare is
// refused at the waist and the goal is paused with the ask, so the decision lands with the
// person who wrote the objective rather than with the run that read it.
//
// The two lists are set together because neither means anything alone: marking with nothing
// declared is a run that stops the first time it reaches one, and declaring an action
// nothing marked authorizes what was never gated. Marking nothing is the default and leaves
// the run exactly as it was.
//
// The declarations are already structured because a goal spec file can narrow one to a
// target, which --allow cannot express; declaredAllowances turns the flag's action names
// into the same form.
func withAllowance(outside []string, declared []goal.Allowance) driveOption {
	return func(c *driveConfig) {
		c.gates.outside = outside
		c.allowances = declared
	}
}

// withGoalSpec states the run's terms and, where the file gives one, its stop condition.
// The terms are what the run may not trade away to finish: they are audited before the
// stop condition is consulted on every reconcile, and a breach settles the goal whatever
// the run was about to claim.
//
// It is one option rather than two because the two halves of a spec file arrive together
// and a caller that set the terms while dropping the operator's stop condition would run
// a goal against a definition of done nobody wrote. An allowance from the same file is
// folded in through withAllowance, which owns that list.
func withGoalSpec(spec goalSpecFile) driveOption {
	return func(c *driveConfig) {
		c.terms = spec.Invariants
		c.stopCondition = spec.StopCondition
	}
}

// boundToolset is a toolset paired with the grant that bounds it. They travel
// together so a caller cannot hand drive a toolset while forgetting the authority
// that is supposed to confine it: the default-permissive trap this pairing exists
// to avoid.
type boundToolset struct {
	tools []mission.Tool
	grant capability.Grant
}

// budgeted reports whether a ceiling is set on any axis.
func (c driveConfig) budgeted() bool { return c.budget.Tokens > 0 || c.budget.Cost > 0 }
