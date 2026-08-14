---
name: reading-unfamiliar-code
description: Use before the first edit to unfamiliar code: someone else's package, a file you have just opened, a codebase or repository you have not worked in, or a request to understand how an existing behaviour is produced, which part of the tree produces it, or which callers a function has. Covers the three claims an edit depends on, that every symbol it invokes is real, that one concrete input has been traced from the entry point to its effect, and that every other site observing the state you will alter has been enumerated, with each answer marked read, ran or assumed, and no edit allowed to rest on an assumed one. Carries the measured finding that a model's recall of a project's own functions is far worse than its recall of public libraries, so names local to code you did not write are opened at their definitions rather than remembered.
metadata:
  flynnhq.com/title: Reading unfamiliar code
  flynnhq.com/tags: '["comprehension","codebase","reading","onboarding","tracing"]'
---

# Reading unfamiliar code

## What you recall about this repository is worth a fraction of what you recall about the world

Measured across four models on the same task: when generated code called an API from
a third-party library, about 13 percent of those calls named something that did not
exist. When the same models called a function belonging to the project they were
working inside, the figure was about 85 percent. Model size barely moved it.

The reason is ordinary. Popular libraries appear thousands of times in training data
with their real signatures attached. This repository appears zero times. There is no
felt difference between remembering a signature and inventing one, which is why
reading a larger volume of code does not fix it.

Finding the right place to change is no better: across a benchmark of real kernel
bugs, the best agent tested picked the correct file first 41.6 percent of the time.

So the discipline is mechanical: a recalled thing may not stand in for a read one.

## Every claim carries how you know it

Before the edit, write down the claims the edit depends on and mark each one:

- **read**: you opened the definition in this tree, this session, and saw it.
- **ran**: you made the code execute and watched what it did.
- **assumed**: you have not checked.

Assumed claims are unavoidable, since there is always more code than time. The rule is
narrower than banning them: no edit may rest on one. When you find that yours does,
either go and settle the claim or stop and say which claim would settle it.

The mark is the whole mechanism. "I understand this code" cannot be audited, by you or
by anyone reviewing you. "The retry count comes from the config struct, assumed" can,
and writing the word assumed beside something you were about to build on is usually
enough to send you to the file.

Three claims earn their marks on nearly every change.

## Claim one: the names are real

List every symbol the edit will call, pass to, or alter: functions, types, fields,
constants, configuration keys, error values, table and column names. Open each one
where it is defined. Not a search hit, not a call site: the definition.

Read four things off it. The signature as it stands, including what comes back on
failure. What it does with the value you intend to hand it, which is often not what
its name implies. What it assumes has already happened, ordering and initialisation
included. Whether a second symbol with a similar name exists, which is how the wrong
one ends up called.

A symbol you can name but did not open is assumed, however sure it feels. This is the
step the numbers above describe, and it is the cheapest of the three.

## Claim two: one concrete input, followed the whole way

Choose a single real input the code handles: one request, one row, one message, one
command line. Follow it from where it enters the process to where it has its effect,
and write down every hop it makes.

Prefer making it happen to reading about it. Run the entry point with that input. Stop
it in a debugger. Add a temporary line that prints what arrived, marked with a token
distinctive enough to remove in one search. A path you executed beats a path you
inferred, because inference is where the branch you did not notice gets skipped.

One path is enough, and one is the point. It is the difference between a description
you recognise and a chain you can state.

The test suite is the second-best source here and frequently the fastest. A test that
drives this path is a worked example with the setup already written, and its fixtures
tell you what a real input looks like.

Where the path cannot be made to run, say which hop you could not observe, and treat
everything downstream of it as assumed.

## Claim three: who else observes what you are about to change

This is the claim a hurried edit never reaches, and it is why a change that passed its
own tests breaks something in a file nobody opened.

Enumerate, for the thing you will change: every caller, every test asserting on it,
every serialised form it appears in, and every consumer that arrives by a route a
search of the tree will not reveal. Reflection, injection by name, dynamic dispatch,
generated code, string-keyed registries, configuration files, stored data, a message
some other service parses. A search of the source is a lower bound on that set and
never a proof.

Then answer the question that sizes the change: which of them observe the behaviour
you are altering rather than merely the name. A caller passing an untouched value
through is cheap. A caller depending on the ordering, the error type, the null case or
the timing is where the change grows, and finding three of those is a reason to
redesign the edit rather than to write it larger.

## Name what you did not find out

The read ends with a short list of what stayed dark, written down and handed on: the
branch never reached, the flag whose default you could not locate, the caller living
in a repository you cannot see, the behaviour you could only infer.

An unknown that is written down bounds the change. The same unknown left unwritten
becomes an assumption nobody knows was made, and in the diff it is indistinguishable
from something that was checked.

## Write the model where the next run will find it

This read is expensive and it evaporates when the session ends. Leave behind, in the
change itself or in whatever notes the project keeps, the claims and their marks, the
traced path in the order of its hops, and the unknowns.

Hold it against the code rather than letting it replace the code. A description
written last month and not re-derived since is a claim marked assumed, whoever wrote
it. That applies in full to what the repository already contains: comments, design
documents and diagrams are witnesses about the past and not evidence about the
present, and correcting one you found to be wrong is part of finishing the read.

## What this runtime can do about it

`--fanout` on a goal runs concurrent child agents, each routed to the model its
archetype pins, all folded into one verifiable record. The three claims divide cleanly
among three children: one opens every definition, one traces the path, one enumerates
who observes it. Their answers come back to you and the pages of file content they had
to read do not, which is what keeps a thorough read affordable.

Executing unfamiliar code is a priced and bounded act here. A run carries
`--max-cost`, `--max-tokens`, `--max-memory` and `--max-processes`, and
`--irreversible` names the actions that must stop for approval before they happen.
That is what makes running it the default source of evidence rather than the brave
option.

The read itself is recorded. `flynn runs` lists what has run, and the event history of
any one of them comes back in order from `flynn inspect <run-id>`, so what a run had
genuinely opened before its first write is recoverable afterwards, by you or by a
reviewer. `flynn spine export` seals that record for someone outside.

The model also outlives the run. `/remember` pins a fact so later runs recall it,
`flynn memory consolidate` distils a series of them, and a run picked up again with
`flynn resume <run-id>` starts from the state it recorded. A second session on the
same code should begin where the first one stopped.

## There is no automatic check for this one

Skills in this pack can carry a shell command that re-grades them as the environment
changes. The condition to grade here is about ordering inside a run: that the
definitions were opened and a path was traced before the first write to that package.
No shell command can observe it, because the command runs outside the run it would
have to inspect.

So the check is manual, and it is the marks themselves. Before the first edit, the
claims exist with a mark each and none the edit rests on says assumed. After the run,
`flynn inspect` shows whether the reads came before the writes. A filled-in check
field would have looked better here and asserted nothing.

## Refusals

- No call to a symbol from this project that was not read at its definition.
- No edit whose correctness depends on a claim still marked assumed.
- No claim to understand a path that was neither executed nor traced hop by hop.
- No change to something shared before its observers have been enumerated.
- No silent unknown: whatever was not found out is written down.
- No comment, diagram or document treated as evidence of present behaviour.
- Nothing added to observe a value survives the trace that needed it.
