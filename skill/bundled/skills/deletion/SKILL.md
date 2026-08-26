---
name: deletion
description: Use when removing anything that already exists: dead code, an unused function or package, a duplicate, a feature nobody calls, an option, a flag, a dependency, or the leftovers an earlier change left behind and nobody has cleaned up. Covers how much evidence a removal needs, which grows with how far the thing reaches, why a search of the tree is not proof that nothing runs it, which of two copies should die and what stops the next one, retiring a surface other people call, and what to leave alone. Carries one refusal above all others: a test, an assertion, an error path, or a check may never be removed, weakened or skipped as a way of getting a run to green.
metadata:
  flynnhq.com/title: Deletion
  flynnhq.com/tags: '["deletion","refactoring","dead-code","duplication","maintenance"]'
---

# Deletion

## Removing is a change of behaviour, and it gets the same care

Rewriting something for clarity leaves the system doing what it did. Removing
something changes what the system contains, and if you were wrong about who needed
it, the failure appears somewhere you are not looking, in code you did not read, at a
time you are not there.

That is an argument for evidence, not for timidity. Most of what accumulates in a
codebase genuinely has nobody left who needs it, removing it is the highest-value
change available, and it is the change least often made. What follows is how to be
sure enough to act.

## The evidence a removal needs grows with how far the thing reaches

One standard for everything is either paralysis or vandalism. Match the work to the
reach:

| What you are removing | What justifies it |
|---|---|
| A local variable, an unreachable branch, a private symbol | The compiler, the linter, or a search of the one file. Do it and move on. |
| Something visible across a package | A search of the repository, plus the suite green. |
| Something the wider program calls | The above, plus the dynamic paths in the next section checked. |
| A published surface others depend on | A caller audit you can state, a deprecation with a period attached, and a version. Removing it without that is not deletion, it is breaking someone. |
| Persisted data, a column, a stored field | Not deletion. It is a migration, it is irreversible, and it belongs to whoever owns that data. |

Say which row you are in when you report the change. A removal from row one needs no
explanation, and a removal from row four with the explanation of a row one is the
failure this table exists to prevent.

## A search proves absence in this tree, and nothing else

No hits does not mean no callers. The paths a search misses are the ones that fail in
production rather than in the suite:

- A name assembled at runtime from configuration, a route table, or a string.
- Reflection, dynamic dispatch, or an interface satisfied structurally.
- Generated code, and anything a build step writes.
- Another repository, a plugin, a script somebody runs by hand, a scheduled job.
- Serialised state that names the thing: a saved record, a queued message, a
  persisted event written by an older version.

The last one deserves its own line. Data written before your change outlives the code
that wrote it, so a type that nothing constructs today may still arrive tomorrow from
a queue or a log.

Two things resolve this better than more searching. Look at what has executed: where
the system records its runs, the history says whether this path was ever taken, which
is evidence where a missing search hit is only an argument. And where nothing
records it, remove it in a way that reports rather than one that hides: leave the
entry point in place and failing loudly for one release, then remove it once nothing
has hit it.

## Start with what you added

The first candidates are yours. Debug output, a helper written for a case that ended
up handled elsewhere, a parameter every caller passes the same value for, a branch
covering a situation the final design cannot reach, the second way of doing something
that survived because the first still worked.

These need no caller audit and no deprecation. Nobody outside your change knows they
exist, and they are the cheapest lines in the codebase to remove because you still
remember why they are there. An hour later that is no longer true, and a week later
they are somebody's puzzle.

## Delete the copy, then stop the next one

When the same logic exists twice, keep the one with the callers, the tests and the
history, and delete the other. If the survivor is missing something the copy had, move
that across first, in its own step, so the diff shows a behaviour change rather than
hiding it inside a deletion.

Then close the path. Deduplicating without leaving anything behind means the next
author writes the third copy in good faith, and the rate at which duplication
accumulates is not something a review pass keeps up with. When you extract the shared
thing, add the rule that fails the pattern it replaced: a lint rule, an import rule, a
test that asserts the count. The removal and the rule land together or the removal is
temporary.

## Never remove the check that stands between you and green

A failing test, an assertion, a type constraint, a validation, an error path, a lint
rule: none of these may be removed, weakened, skipped, marked expected-to-fail, or
have its input adjusted, as a route to a passing run. This holds when the check looks
wrong, when it looks flaky, and when removing it would be defensible on other grounds,
because at the moment a run is failing you are the least reliable judge of that.

This refusal is not a matter of manners. Measured on tasks built so that passing
requires contradicting the specification, a frontier model took the shortcut on around
three quarters of them, and telling a model not to helped one model a great deal and
another barely at all. What worked on every model was keeping the tests out of reach.
So treat your own reasoning here as the weak control it has been shown to be, and
treat the rule as absolute rather than as a factor to weigh.

Three moves belong to the same family and are refused for the same reason: special
casing the input the test uses, hardcoding the value it expects, and moving the
assertion to something weaker that still passes.

What to do instead: if the check is genuinely wrong, say so, leave it failing, and
report what it asserts against what the specification says. A wrong check is a
finding, and it is one the person you are working for wants.

## What survives the pass

Leave alone anything whose reason you cannot reconstruct. A guard that looks
unnecessary, a retry that looks superstitious, a sort before an operation that does
not obviously need one: each of these is sometimes exactly what it appears to be, and
sometimes the fix for an incident nobody wrote down. Read the history of the line
before removing it. If the commit that introduced it explains itself, you have your
answer either way; if it does not, that absence is a reason to keep it and say so.

Error handling is not complexity. Neither is a validation you cannot see a caller
violating, nor a branch for a case that has not happened yet but is reachable.

And an abstraction with one implementation is a candidate rather than a verdict. Take
it out, wire the caller directly, and see whether anything got harder. If a test can
no longer substitute anything, or a rule can no longer be stated, it was doing work.

## Retiring something that still works

A capability nobody uses costs something every day it stays: it is in the tests, in
the build, in the documentation, in the surface every reader has to understand before
they can be sure it is not involved.

Retire it in the open. Announce it where its callers will see it, make it warn while
it still works, give the warning a date, and remove it after that date rather than
before. Where the callers are inside this repository, retire them first and delete
last, so the change never leaves the program in a state where something calls what is
gone.

Where you cannot find out who calls it, say that instead of guessing. An unknown
caller count is a fact about the situation, and it is the reason the deprecation
period exists.

## Removal lands as its own change

Do not remove things during work you were asked to do for another reason. A change
that adds a feature and deletes four things is hard to review, harder to revert, and
the deletion is what breaks. Note what you found and remove it in a change that says
so in its title.

The same restraint applies to scope. Code you were not asked to touch, that nothing in
your task depends on, is not yours to tidy on the way past, however plainly it could
be better.

## Prove it

Three things, and none of them is expensive:

Run the suite before and after, and confirm no test file was edited between the two
runs. That is checkable from the diff, and it is the single most useful fact about a
change that removes things.

Say what search you ran and where you looked. "No callers outside tests, searched the
repository and the two dependent services" is evidence. "Appears unused" is not.

Report the count and the row. Lines and files removed, and which row of the evidence
table this was. A large removal from row one and a small removal from row four are
different changes, and the number alone tells the reviewer the wrong one.

## Refusals

- No test, assertion, error path, type constraint or check removed, weakened or
  skipped to get a run to green, and no input special-cased to the same end.
- No removal of an exported surface without a stated caller audit and a deprecation.
- No deletion of persisted data or a stored field as part of a code change.
- No removal of code whose reason cannot be reconstructed, without saying that is why
  it is going.
- No deduplication that leaves nothing to stop the next copy.
- No unrequested tidying of code the task did not touch.
- No claim that something is unused when the evidence is a search with no hits and the
  thing is reachable dynamically.
