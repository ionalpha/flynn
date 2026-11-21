package main

import (
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/internal/projectctx"
	"github.com/ionalpha/flynn/memory/hybrid"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/skill/skilltool"
)

// driveConfig holds a run's optional levers. Its zero value (no budget, the strictest
// shell policy) opens nothing, so leaving an option out is always safe.
type driveConfig struct {
	budget   budgetpkg.Limits
	shell    governed.ShellPolicy
	extAgent *externAgent
	toolset  *boundToolset
	observe  func(session.Event)
	planning bool
	proof    bool
	skills   *skilltool.Set
	gates    gateSetup
	// allowances are the irreversible actions outside the workspace this run may take. They
	// ride the goal spec, so the waist checks them and the reconciler can tell an answered
	// ask from an open one.
	allowances []goal.Allowance
	// terms are the invariants the run is held to; stopCondition is the operator's meaning
	// of finished. Both come from a goal spec file; empty means no terms and the default
	// stop condition.
	terms         []goal.Invariant
	stopCondition string
	// emb is the embedding model recall ranks by; nil ranks lexically. runLearningMission
	// reads it, because the run's memory is assembled before the conversation; drive does
	// not.
	emb hybrid.Embedder
	// project is the trust decision for the repository's instruction files. The zero value
	// means none found, and adds, taints and records nothing.
	project projectctx.Decision
	// selection is the skills the run is offered and the tools it is granted. Nil leaves
	// both at their defaults and adds nothing to the record.
	selection *selectionPlan
}

type driveOption func(*driveConfig)

// withBudget caps a run's total spend. Every model and tool call, including a fan-out's
// children, which share one pool, is charged, and an action is refused at the ceiling.
// A zero-limit budget is unlimited.
func withBudget(l budgetpkg.Limits) driveOption {
	return func(c *driveConfig) { c.budget = l }
}

// withMemoryEmbedder ranks memory recall by meaning as well as words. A nil embedder
// keeps the store's lexical order, so an install with no embedding model is unchanged.
func withMemoryEmbedder(e hybrid.Embedder) driveOption {
	return func(c *driveConfig) { c.emb = e }
}

// withShellPolicy limits the commands a run's tools execute: memory and process count
// (see [sandbox.ResourceLimits] for per-platform enforcement), the network, and the
// data directory and credential locations. The zero [governed.ShellPolicy] keeps all of
// them closed.
func withShellPolicy(p governed.ShellPolicy) driveOption {
	return func(c *driveConfig) { c.shell = p }
}

// withPlanning has the goal expand its objective into a visible ledger before the first
// build step, each item carrying how it would be checked. It is opt-in per entry point
// because it changes what a run does first (a plan model call, then the ledger gate).
// The `goal` command sets it.
func withPlanning() driveOption {
	return func(c *driveConfig) { c.planning = true }
}

// withLedgerProof makes the plan binding: each item's declared check runs, and the goal
// will not report success over an item the record cannot prove.
//
// It is separate from withPlanning because the risk differs. Planning always runs the
// producer, so items flip to proven and the report counts them; this adds the refusal,
// which can stop a run whose plan wrote checks no machine can run. Without it a failing
// item stays unproven and the run moves on (see goal.LedgerState).
func withLedgerProof() driveOption {
	return func(c *driveConfig) { c.proof = true }
}

// withExternalAgent drives the run through an external agent CLI, whose harness runs
// the loop and its own model; drive's llm.Model is unused. Every tool call still crosses
// the dispatch waist, so governance is unchanged.
func withExternalAgent(ea *externAgent) driveOption {
	return func(c *driveConfig) { c.extAgent = ea }
}

// withToolset drives the run over a caller-supplied toolset and grant in place of the
// sandboxed working-tree tools. A pull-request review uses it to hold exactly its
// archetype's authority: no shell, no filesystem, and a grant that is the complete list
// the waist admits. A toolset run never reads or writes the tree.
func withToolset(t *boundToolset) driveOption {
	return func(c *driveConfig) { c.toolset = t }
}

// withEventObserver calls fn on every session event as the run streams. A caller reads
// an outcome (a submitted review verdict) off the recorded events, so what it acts on
// and what the record says cannot differ.
func withEventObserver(fn func(session.Event)) driveOption {
	return func(c *driveConfig) { c.observe = fn }
}

// withSkills gives the run the skill toolset: recall offers a skill by name and
// description, and the model calls skill_read for the body. A caller that recalls skills
// into the prompt must pass a toolset over the same store, or the offer names a missing
// tool.
//
// It takes the toolset because the caller reads back the skills the run loaded, which
// the outcome is credited to.
func withSkills(s *skilltool.Set) driveOption {
	return func(c *driveConfig) { c.skills = s }
}

// withApproval requires a person to authorize each named action before the run takes
// it, resolving the pause through prompter. It is an operator's second gate above the
// capability grant and sandbox.
//
// A nil prompter is the non-interactive case and fails closed: the listed action is
// refused. Naming no actions leaves the run ungated.
func withApproval(actions []string, prompter mission.ApprovalPrompter) driveOption {
	return func(c *driveConfig) {
		c.gates.approve, c.gates.prompter = actions, prompter
	}
}

// withAllowance marks the irreversible actions that reach outside the workspace and
// declares which this run may take. An undeclared marked action is refused at the waist
// and the goal pauses with the ask, so the person who wrote the objective decides.
//
// The lists are set together: marking with nothing declared stops at the first marked
// action, and declaring an unmarked action authorizes nothing gated. A goal spec file can
// narrow a declaration to a target, which --allow cannot; declaredAllowances converts the
// flag's names.
func withAllowance(outside []string, declared []goal.Allowance) driveOption {
	return func(c *driveConfig) {
		c.gates.outside = outside
		c.allowances = declared
	}
}

// withGoalSpec states the run's terms and, if the file gives one, its stop condition.
// The terms are audited before the stop condition on every reconcile, and a breach
// settles the goal whatever the run was about to claim.
//
// It is one option because a caller that set the terms but dropped the stop condition
// would run against a definition of done nobody wrote. An allowance from the file goes
// through withAllowance.
func withGoalSpec(spec goalSpecFile) driveOption {
	return func(c *driveConfig) {
		c.terms = spec.Invariants
		c.stopCondition = spec.StopCondition
	}
}

// boundToolset pairs a toolset with the grant that bounds it, so drive never receives a
// toolset without its confining authority.
type boundToolset struct {
	tools []mission.Tool
	grant capability.Grant
}

// budgeted reports whether a ceiling is set on any axis.
func (c driveConfig) budgeted() bool { return c.budget.Tokens > 0 || c.budget.Cost > 0 }
