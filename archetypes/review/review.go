// Package review is the agent's pull-request reviewer: the standing instruction it
// works from, and the exact authority it holds while it works.
//
// A reviewer is not a new kind of run. It is the ordinary tool-using loop pointed at
// a narrow toolset, so this package registers no driver of its own. What makes a
// reviewer a reviewer is the pair below: the instruction that tells it what a
// finding is, and the grant that decides what it may do. Both are values, so both
// can be read, diffed, and asserted on in a test.
//
// The grant is the interesting half. A reviewer may fetch a pull request, comment on
// it, and submit a verdict. It may not run a command, read the working tree, write a
// file, or delegate to a child run, because those actions are absent from the grant
// and the dispatch waist refuses an action the grant does not name. That refusal is
// structural: it holds whatever the model is talked into asking for.
//
// This directory, archetypes/, is the home of the archetypes Flynn ships: each
// package is one authored agent (its instruction and its authority) built on the
// Agent kind that internal/archetype defines. The engine lives there; the content
// lives here.
//
// The grant only binds once a caller puts it on the context. An unbound context is
// permissive by design, the zero-config default of package capability, so assembling
// a reviewer without binding its grant produces an unconstrained run that looks
// identical from the outside. Bind it, and assert the refusal.
package review

import (
	"slices"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/internal/archetype"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/tools/github"
)

// ArchetypeName is the reviewer's name as a stored Agent resource.
const ArchetypeName = "reviewer"

// SystemPrompt is the reviewer's standing instruction.
//
// It is written against one failure mode. A model asked to review a diff will find
// something to say about every hunk, because saying nothing feels like failing the
// task. The result is a wall of observations that buries the one comment that
// mattered, and a reviewer nobody reads. So the instruction is mostly about what not
// to post: no finding without a file, a line, and a concrete failure it causes. The
// tool enforces the same rule, and refuses a finding that lacks one.
const SystemPrompt = `You are reviewing a pull request. Your job is to find defects that would survive review, not to comment on every change.

Start by fetching the pull request. Read the whole diff before you say anything.

Post a finding only when you can name the defect and the failure it causes. A finding needs a file, a line, and a concrete scenario: the inputs or the state, and the wrong output, the crash, or the corruption that results. If you cannot write that scenario, you do not have a finding. Say nothing.

These are defects worth a comment:
- The code is wrong for an input it will actually receive.
- A boundary is unchecked, and crossing it is reachable.
- An error is swallowed, and the caller proceeds on a value that is not there.
- A resource leaks, a lock is held across a call that can block, or two goroutines touch the same field.
- A change breaks a caller the diff does not show.
- A security property the surrounding code relies on is broken.

These are not:
- Style, naming, and formatting. A linter owns those.
- A suggestion you cannot tie to a failure.
- Praise, summaries of what the diff plainly does, or restatements of the title.
- Hypotheticals guarded by "if this is not handled elsewhere". Find out, or drop it.

Do not repeat a finding you have already posted. The fetch tells you which findings are already on the pull request; posting one again is a duplicate, and the tool updates the existing comment rather than adding a second.

A finding lives on the line it concerns and nowhere else. There is no summary comment, and nothing you say goes on the pull request's main thread. A reader sees the comment on the code, replies to it there, and resolves it there.

When you have posted your findings, submit a verdict. You give it one sentence: what you concluded. The tool links every finding you posted underneath it, so listing them yourself only says the same thing twice. Do not summarise the diff, do not restate the findings, do not praise. Request changes when you posted a finding that should block the merge. Otherwise comment. Approving is a claim that you read the whole change and found nothing that should block it, so approve only when that is true. If the diff reached you truncated, you did not read the whole change, and the tool will refuse the approval.`

// capabilities is the reviewer's complete authority: the three review tools it
// calls, and the model call the loop makes on its way to calling them.
//
// Everything absent is refused. There is no shell action, no filesystem action, and
// no spawn, so a reviewer cannot run a command, read a file outside the diff it was
// handed, or delegate the work to a child run with a wider grant. Adding a name here
// widens what a reviewer may do, which is why the list is small and the test that
// pins it is not a formality.
var capabilities = []string{
	"github_pr_fetch",
	"github_comment",
	"github_submit_review",
	mission.ActionModelGenerate,
}

// Capabilities returns the action names a reviewer is granted, in sorted order. The
// caller receives a copy, so a mutation cannot widen a reviewer's authority through
// the returned slice.
func Capabilities() []string {
	out := slices.Clone(capabilities)
	slices.Sort(out)
	return out
}

// Grant returns the capability grant a reviewer runs under. Bind it onto the run's
// context with capability.Into, or the dispatch waist has no policy to consult and
// admits everything.
func Grant() capability.Grant { return capability.NewGrant(capabilities...) }

// Archetype returns the reviewer as an Agent resource spec: the standing prompt, the
// exact authority, the loop, and the model. Storing it as a resource is what makes a
// reviewer's authority readable after the fact, rather than a property of whichever
// code path happened to assemble the run.
//
// The loop is the general-purpose tool-using one. A review is a conversation that
// calls tools and stops, which is what that loop already is; a bespoke review loop
// would differ from it only in name.
//
// An empty model defers to the host's configured model, so a reviewer can run on a
// frontier model or a local one without touching this spec. Package harness adapts
// the scaffolding to whichever it gets.
func Archetype(model string) archetype.Spec {
	return archetype.Spec{
		System:       SystemPrompt,
		Capabilities: Capabilities(),
		Model:        model,
		Driver:       driver.NameDefault,
	}
}

// Tools builds the reviewer's toolset against one repository. The names it exposes
// are exactly the capabilities the grant names; a tool the grant omits is refused at
// the waist even though the toolset offers it, and a capability with no tool behind
// it can never be exercised. TestCapabilitiesMatchTheToolset pins them together.
func Tools(cfg github.Config) ([]mission.Tool, error) {
	set, err := github.New(cfg)
	if err != nil {
		return nil, err
	}
	return set.Tools(), nil
}
