---
name: structural-boundaries
description: Use when deciding where a change goes or whether a new boundary is worth adding: which file or package should hold new code, whether to create a module or extend one that exists, which way an import may point, whether an interface belongs here, how to keep a database or a vendor SDK out of the code that holds the rules, how to break an import cycle, and whether a proposed restructure into layers or ports and adapters should happen at all. Adding structure is the most common wrong move, so the default is to put the change where the code that changes with it already lives. A boundary counts as designed only when crossing it fails a check and a second implementation has crossed it; anything else is a folder name.
metadata:
  flynnhq.com/title: Structural boundaries
  flynnhq.com/tags: '["architecture","design","dependencies","modularity","refactoring"]'
---

# Structural boundaries

## Put the change where the code that changes with it already lives

Before deciding what to build, decide where it goes, and the answer is almost always
an existing file. Find the code that will have to change on the same day as yours:
the function that already parses this input, the package that already owns this
record, the test that already covers this path. Your change belongs beside it.

That is not a preference. Measurements of code written by models put the count of
structural problems in near-perfect correlation with the number of lines produced,
close enough that volume alone predicts the damage. A second finding names why the
usual advice backfires: told to be modular, a model splits the work across new files
without moving any of the reasons those files would change, and the result reads as
organised while every change still touches all of them.

So the instruction that follows from the evidence is the opposite of the one every
published treatment of architecture gives. Do not reach for a layer. Add the fewest
lines in the fewest places, and make each new file or package justify itself.

## Adding structure is the move that needs a reason

Four things get created too readily: a new file, a new package, an interface, and a
layer of indirection between two things that were talking to each other. Each is
cheap to write and expensive to remove, because removing one means finding every
caller that learned it.

Before creating any of them, say which of these is true:

- Something else already needs to change with it, and putting them together would
  mean one of them changing for two unrelated reasons.
- A second implementation exists now, in this repository, counting a test double
  that some test calls today.
- A rule already in force requires it, and you can name the rule.

None of the three is a matter of taste and all three are checkable. If none holds,
the code goes where the code it belongs with is, and this section is the whole
answer.

## A boundary exists when crossing it fails a check

A directory called `domain` next to a directory called `adapters` asserts nothing.
Neither does a comment, a naming convention, or a paragraph in a design document. All
of them describe an intention that the next change is free to violate, silently,
without anything going red.

What makes the difference is a rule that executes: a lint configuration, an import
test, a build tag, a compiler-enforced visibility. Written down that way, the
boundary refuses the crossing at the moment somebody attempts it, in the pull request
where it is cheap, and it keeps refusing for every author afterwards without any of
them reading the document.

This is also the answer to the strongest measured objection to structural advice. A
prompt telling a model to write clean, layered code cuts the excess in the first
iteration by about a third and then leaves the erosion running at the same rate as
before, because advice is consumed once and a check is consumed on every change.
When you add a boundary, add the rule in the same change. A boundary that ships
without one is a plan to have a boundary.

`references/enforcing.md` has the concrete form per ecosystem, and what to do when
the ecosystem has no tool for it.

## Two implementations, or it is a wrapper

An interface with exactly one implementation and no rule behind it usually forwards
its calls to that implementation, adding a name for something that already had one.
It costs a reader an extra hop for every call and buys nothing back.

Test it by deleting. Take the interface out, wire the caller to the concrete thing,
and see what got harder. If the answer is nothing, it was pass-through and should
stay deleted. If a test can no longer substitute anything, or a rule can no longer be
stated, put it back with that reason recorded.

A test double counts as the second implementation, but only one a test calls. A double
written in the same change that nothing exercises is the wrapper with extra files.

## Folders cannot express direction, and direction is the property

Nesting packages under `domain`, `application` and `infrastructure` changes import
paths and leaves the import graph exactly as it was. `infrastructure` can still
import `domain`, `domain` can still import `infrastructure`, and the layout gives no
indication that the second one happened.

The property worth having is direction: which packages may import which. State it as
a rank, where a package's rank is one more than the highest rank it imports, and the
rule is that nothing imports something of an equal or higher rank. Then put the rule
in the linter, where a violating import fails, rather than in the folder names, where
it cannot be expressed at all.

The crossing to watch for is the one that inverts. A primitive reaching up into
a domain package is what turns a graph that a reader can follow into one that nobody
can, and it is invisible in a diff that only shows the added line.

## An interface answers whether it can be replaced, not whether anything works

Depending on an interface makes the thing behind it replaceable. It does not make the
work possible, and only the first question gets asked in review, because the first is
the one a diff shows.

The result is a boundary that reads as excellent and does nothing. A package defines
its port, the contract is small, the direction is right, a host could implement it.
Nobody has, so the capability it fronts silently does not exist, and the code path
that needs it either stalls or does nothing at all without saying so. This is the
failure of every ports-and-adapters guide in circulation, none of which mentions it.

So every port carries one of three verdicts, written where the port is defined:

- **Shipped.** There is an implementation here and the program wires it. The interface
  stays so it can be replaced, and replacing it loses nothing.
- **Justified.** Nothing is shipped on purpose, the reason is in the doc comment, and
  the absence is visible at runtime. Something that needs the missing piece stops and
  names what it wanted, rather than proceeding as though it had one.
- **Staged.** It ships, it can be wired, and it is off while confidence is built. The
  note names the switch and the condition for turning it on.

The test that decides between them: run the program with nothing installed beyond
what it ships with, and see whether the capability works. If it cannot, the verdict is
justified with a written reason, or the boundary is covering a gap.

## Where a foreign type is allowed to stop

Code you did not write reaches you as types: a database row, an SDK's client object,
a framework's request, a generated stub. The decision is not whether to wrap it, which
is how the question is usually put. It is where those types stop being mentioned.

Pick that line explicitly and say it in one sentence: the SDK's types appear in this
one file and nowhere else. Then make it a rule that runs, since it is one of the
easiest rules to express, being a check on which packages may import the vendor's.

Two failures sit either side of the right answer. A vendor type threaded through
every function means an upgrade rewrites the program, and it is the failure the
literature is about. Wrapping everything the moment it arrives is the other, and it
costs a translation layer for an SDK that will never be swapped and a set of types
that duplicate the vendor's badly. Wrap what your rules are written in terms of, and
call the rest directly.

## Breaking a cycle without inventing a package

A cycle between two packages means they hold one thing that has been split in two
places. Three fixes, in the order to try them:

Move the code that both sides reach for into the one that is more depended on. This
is right more often than it is tried, and it removes a package rather than adding
one.

Invert the edge. The package that owns the rule declares the interface it needs and
the other implements it. This is correct when one side is the rule and the other is a
mechanism serving it, and wrong when they are peers, because then it produces an
interface nobody else will ever implement.

Extract the shared piece into its own package, last, and only when it is a coherent
thing you can name without saying "common", "shared" or "util". A package named for
its role in the cycle rather than for what it is becomes the next cycle.

## Read what is already enforced before adding to it

The structure you are changing was decided by someone else and part of it is already
written down as rules. Find them first: the lint configuration, the import tests, the
visibility markers the language provides, the build constraints. Those are the
boundaries with teeth, and they tell you more about the intended arrangement than any
document in the repository.

Then read the history rather than the tree. Files that keep appearing in the same
commits belong together whatever directory they are in, and a directory whose files
never change together is a grouping somebody imposed that the changes never bore out.
The command is a log over the paths you are about to touch, and it answers the
placement question directly.

Where the two disagree, the enforced rules win as a constraint and the history wins as
evidence. Say so if you find a rule the history has been fighting for a year, because
that is a finding worth reporting even when your change respects the rule.

## Refusing the restructure

A request to add a feature is not a request to restructure the code around it. When
the existing arrangement makes the feature awkward, the report is that it is awkward,
with the specific reason, and the feature still lands in the smallest change that
works.

A restructure is worth proposing separately when three conditions hold together: the
same difficulty has now appeared in several unrelated changes, you can name the rule
the new arrangement would enforce, and the move can be made in steps that each leave
the program working. Missing the third condition is what turns a restructure into a
rewrite that never lands.

When it does go ahead, it goes ahead by addition. Stand the new arrangement up beside
the old one, move callers a few at a time, and delete the old path only once nothing
reaches it. A change that moves everything at once cannot be reviewed and cannot be
undone by reverting one commit.

## Prove the change did not erode what it landed in

Structural damage does not fail a test suite, which is why it accumulates. Three
things are worth checking on the way out, and all three are cheap:

The rule you relied on still runs. If you placed code according to a boundary, run
the check that enforces it and see it pass, and if there was no check, say that the
boundary you placed against is unenforced.

Nothing was duplicated. Search for the distinctive name or literal you introduced and
confirm it appears once. Copying is the single most common way this class of code
degrades, and it is found by searching, not by remembering.

The count went the right way. Report the lines and files added against the change
requested. A one-line behaviour change that added four files and an interface is a
finding about your own work, and reporting it is better than waiting for review to.

## Refusals

- No new package, interface, or layer without one of the three reasons named above.
- No boundary added without the check that fails when it is crossed, or a statement
  that it is unenforced.
- No interface introduced with a single implementation and no test using a double.
- No port left with no implementation and no written verdict.
- No restructure bundled into a change that was asked for something else.
- No claim that a structure is layered, decoupled or clean when nothing executes
  the rule that would make it so.
