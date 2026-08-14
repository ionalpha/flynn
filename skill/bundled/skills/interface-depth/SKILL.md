---
name: interface-depth
description: Use when a surface is awkward to use or keeps growing: a function with too many parameters, a type whose callers all repeat the same three steps, a class of twelve methods that does very little, a wrapper that forwards, an option added because one caller needed it, or a module whose tests pass only by standing in for what it calls. Making something easier to call is what depth means here, and depth is counted at the call site. The interface is every fact a caller must hold to make the call correctly, not the number of methods, and a module gets deeper when one of those facts is removed from every call site at once. Deepening buys less re-reading rather than better odds of getting the change right, so it is worth doing on a surface that is used often and not on a leaf that is called once.
metadata:
  flynnhq.com/title: Interface depth
  flynnhq.com/tags: '["design","interfaces","modularity","testing","refactoring"]'
---

# Interface depth

## Count the interface at the call site, not in the module

Open a file that calls the thing and read the call. Everything the author of that
line had to know, and could not have guessed, is the interface. The method list is a
small part of it, which is why a surface can look tidy in its own file and cost a
paragraph of understanding at each of forty places that use it.

Doing this in the other order is what produces a review that admires a module while
its callers all carry the same three lines. So before designing a surface, write the
call you want to exist, in the caller's file, with the caller's variables. Then build
the thing that makes that line correct. The call is the specification and the module
is the consequence.

For a surface that already exists, the same reading gives you the work list. Anything
that repeats across call sites is a fact the module declined to take, and it will
repeat again at the next one.

## The interface is a list of facts, and here are the kinds

Count these for the call you are about to write or change. They are what a caller
must hold, and each one is part of the surface whether or not it appears in a
signature.

| Kind | What it sounds like at the call site |
|---|---|
| Order | Something else must be called first, or after, or both. |
| Precondition | A value must already exist, be validated, be open, be non-empty. |
| Error work | The caller decides what a failure means and what to do about it. |
| Configuration | An argument with one right answer that every caller passes anyway. |
| Prohibition | Do not call it twice, concurrently, or after the other thing. |
| Cleanup | The caller owns something it must release, in the right order. |
| Internals | The caller has to know which backend, format, or cache is behind it. |

One method carrying six of these is a wider surface than four methods carrying none.
That is the whole reason to count facts rather than methods. The cost falls on the
caller, and the method list is not where it is written down.

Two of the kinds are worth naming as the usual culprits. Error work spreads a policy
decision across every call site, where it will be made differently in each. Order and
prohibition together are what a caller gets wrong at three in the morning, because
nothing refuses the mistake.

## Deepen by removing a fact, in this order

A module gets deeper when a caller stops having to know something. There are four
ways to take a fact away, and they are not equivalent. Try them in this order.

**Make the wrong call impossible.** Let the type refuse it. A constructor that
returns a value already in a usable state removes an ordering fact from every caller
at once. A type that cannot express the invalid combination removes a prohibition
nobody now has to remember. This is the only one of the four that cannot decay.

**Do the work inside.** The step every caller was going to take moves in: the
default gets chosen, the retry gets made, the resource gets released by the call that
acquired it. Ask what each call site does immediately before and immediately after
the call, and take whichever of the two is the same everywhere.

**Make the case not arise.** Change what the operation means so the caller has
nothing to handle. A lookup that returns an empty result rather than an absence error
deletes a branch from every caller. This is a change to what the surface promises, so
it is a decision about the contract and not a refactor.

**Write it down.** Last, and it does not remove the fact. A documented obligation is
still an obligation the caller carries. Prefer it only when the other three would
cost more than the fact does.

None of the four is "add a layer in front". Deepening usually deletes lines at the
call sites and adds a few inside the module, and if your change is doing the reverse
you are probably wrapping rather than deepening. The direction is worth watching
because of what it costs to get wrong: across generated code, the count of structural
problems tracks the number of lines produced closely enough that volume alone
predicts the damage.

## A call that forwards is a rename until you can finish the sentence

The sentence is: a caller of this no longer has to know ___. Finish it with something
specific, from the table above. If the honest ending is "which function it is really
calling", the layer is a second name for one thing, and it costs a reader a hop at
every call for nothing.

This catches the common shape, which is a method that passes its arguments on
unchanged to a method of the same shape. It should not catch the one-line function
that does have an ending: choosing an argument the caller would otherwise supply,
turning two calls into one, holding a name that the rest of the code is written in
terms of. Those remove a fact, and the sentence finishes.

Where the wrapper is an interface with a single implementation, the question is
whether the boundary should exist at all, which is a different judgement and belongs
with placement rather than here.

## Depth buys less re-reading, not a better chance of being right

The one controlled measurement to date paired codebases that matched in architecture
and dependencies and differed in structural quality, then ran 660 agent trials over
them. Task success did not move. Token use fell 7 to 8 percent and the number of
times a file had to be revisited fell 34 percent.

Read that as the honest case for this work. Deepening a surface does not make the
next change more likely to be correct; it makes the surface cheaper to use, every
time it is used. Two things follow.

Spend the effort where the reading happens: the module with forty call sites, the one
that comes up in every session, the one whose callers keep repeating a step. A leaf
called once is not worth redesigning, and a request to add behaviour to it is not an
invitation to.

Report the benefit in the terms the evidence supports. "This removes the retry
decision from 28 call sites" is checkable. "This makes the code more robust" is not,
and is a claim the measurement does not support.

## A stand-in for something in your own process is evidence of a shallow surface

When a test cannot exercise a module without substituting the collaborators that
module calls, those collaborators have become part of what a caller must understand.
The test is telling you where the interface really is, and it is further out than the
signature suggests.

This is worth watching precisely because it is the failure agents produce most. Over
1.2 million commits from 2025, commits authored by coding agents added mocks to tests
36 percent of the time against 26 percent for everyone else, and touched test files
23 percent of the time against 13 percent. The mock is quicker to write than the
question of why the module could not be run.

Substituting a genuine external, a payment provider or a mail service, is not the
signal. Substituting something in the same process is. When you find one, the fix is
usually to move the collaborator's work inside the module and test the outcome
through the call the real caller makes.

## Make the surface prove it is the whole surface

The claim to check is that the behaviour is reachable from outside. In Go, put the
test in the external test package: a file declaring `package thing_test` can reach
only what `thing` exports, so the suite compiles only if the exported surface is
enough. Every other ecosystem has the same move: import through the package's public
entry point rather than a deep path, and let the import fail rather than reaching for
an internal name.

Run it, and one of two things happens. It compiles, and the interface is real. It
does not, and you have found the exact name a caller would need and cannot have.
Either outcome is worth more than an opinion about the design, and it takes a minute.

Keep the internal tests you need for the parts that are genuinely hard to drive from
outside. The rule is that at least one test crosses the surface a caller crosses, not
that every test must.

## An argument added for one caller is that caller's problem, moved

An option that exists because a single call site needed it has widened the surface
for everybody who did not. The next one arrives the same way, and after five of them
the signature is a record of the history of its callers rather than a description of
what the thing does.

Two better answers. Give that caller the general operation and let it do the special
part itself, at its own call site, where the specialisation belongs. Or find the more
general operation that covers both cases without naming either, which is the same
move as removing a fact and usually shortens the signature.

The trap on the other side is building for callers who do not exist. Keep the shape
general and the implementation exactly as capable as today's callers require. A
parameter for a case nobody has is a fact every caller must read past, plus code
nobody runs.

## Report what a caller stopped having to know

A depth change is finished when you can say which fact left the interface and how
many call sites stopped carrying it. Both halves are countable, and neither is a
matter of taste.

Give the numbers the change actually produced: facts removed, call sites simplified,
lines added inside against lines deleted outside. If the lines went up in both
places, say so, because that is the signature of a layer rather than a deepening and
it is better found by you than in review.

`references/depth.md` works three examples end to end, including one where the right
answer was to leave the surface alone.

## Refusals

- No claim that a module is deep without naming a fact its callers no longer hold.
- No forwarding method, function, or class that leaves every obligation in place.
- No new parameter, option, or flag introduced for one call site.
- No stand-in in a test for a collaborator inside the same process, unless the reason
  it cannot be run is written down.
- No deepening of a surface nobody reads, and no restructure of one because a feature
  request touched it.
- No report of a design improvement in terms the change cannot evidence.
