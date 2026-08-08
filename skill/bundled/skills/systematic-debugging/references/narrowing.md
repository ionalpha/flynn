# Narrowing: the mechanical half

Read this once you have a witness and the cause is not yet obvious. Everything here
consumes the witness and returns a smaller question.

## Reduce the input

The procedure is a search, not an act of insight, and it terminates.

1. List the elements of the failing case: inputs, rows, fields, callers,
   configuration keys, environment variables, steps in the sequence.
2. Remove one. Run the witness.
3. Still failing, keep it out. Passing, put it back and mark that element as
   required.
4. Repeat until nothing left can come out.

Two refinements worth the effort when the case is large. Halve rather than step: drop
half the elements at once, and if it still fails you have removed half the search in
one run. And re-run the whole pass after the first one finishes, because removing one
element often makes another removable.

What you end with is a case in which each surviving part is necessary for the
failure. That is a statement about the bug rather than a smaller input, and it
usually names the cause on its own.

## Reduce the history

Applicable whenever a known-good state exists.

    git bisect start
    git bisect bad <the state that fails>
    git bisect good <the state that worked>
    git bisect run <the witness>

The witness must exit non-zero for the failure and zero otherwise, and it must be
runnable at every point in the range. Where an old point cannot build, exit 125 so
that point is skipped rather than judged.

The same search works outside version control. Dependency versions, configuration
revisions, data snapshots, and feature flag combinations are all ordered ranges with
a known-good end.

When the result is a merge or a rewrite of thousands of lines, the search has
narrowed the question and not answered it. Bisect inside it if the history allows, or
read that change with the witness in hand.

## Raise the rate of an intermittent failure

Measure the rate first, over a fixed number of attempts, so that anything you do next
has a number to compare against. A hundred attempts is usually enough to tell one in
two from one in twenty.

Then apply pressure, one lever at a time, re-measuring after each:

- Run the trigger in a tight loop, and several in parallel.
- Load the machine, so scheduling decisions change.
- Randomise the order of the suite, and run the suspect case both first and last.
- Sleep inside the window you suspect, which widens it from microseconds to
  something a scheduler will interleave.
- Shrink timeouts and pool sizes, so contention happens sooner.
- Run on one core, or on many, whichever the current environment is not.

Reach for a detector before any of this when the platform has one: a race detector, a
memory sanitiser, an undefined-behaviour sanitiser, or a scheduler you can seed. They
convert a failure that happens sometimes into one that happens on the run where the
mistake occurs.

When a case fails only after another case has run, the fault is shared state and the
question is which state. Run the suspect pair alone to confirm, then bisect the set
that runs before it.

## Instrument boundaries, not lines

In a system of several parts, one pass that records what entered each part and what
left it locates the failing part. This is a better first move than reading any single
part closely, because it costs one run and rules out most of the system.

Record the values, not the fact that a step happened. "Entered the pricing service"
tells you nothing; the tenant, the currency, and the amount tell you everything.

Mark every temporary line with one distinctive token, chosen so it appears nowhere
else in the tree, and remove them by searching for it before you finish.

## Read the run record

For a failure inside a previous agent run rather than inside a program:

    flynn runs
    flynn inspect <run-id>

The history is ordered and complete: each turn, each tool call, each result, and the
cost. Find the earliest turn where the run's belief and the actual state stop
agreeing. Everything after that point is consequence, and reading it first is how an
investigation ends up explaining a symptom several steps downstream of the mistake.

    flynn resume <run-id>

Resuming from the recorded state is the narrowing step for a run: change one thing
about the situation, resume, compare. The same one-variable rule applies.

Separate the two failures this exposes. If the run acted on something untrue, the
fault is in whatever supplied it and the fix belongs there. If the run had accurate
information and drew the wrong conclusion, the fault is in the procedure, and fixing
this one run leaves the next one to make the same move.
