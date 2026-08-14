---
name: test-first
description: Use before you write the code for any new or changed behaviour, and when you write the test for it: a feature, an endpoint, business rules, a validation, an edge case, a behaviour-preserving refactor, or anything someone asked to have work differently. Answers whether to write the test before the code or after, and covers implementing one behaviour per red green refactor cycle, what the red run has to produce before it counts for anything, the test that passed the first time it was run and so proves nothing, the test that failed on a missing import rather than on an assertion, the test failure whose output names no assertion and no values, where an expected value has to come from, what may and may not change between red and green, what to do when there is no place to observe the behaviour from, and how the pair of runs is kept here so red before green is read off the record instead of claimed.
metadata:
  flynnhq.com/title: Test first
  flynnhq.com/tags: '["testing","tdd","verification","evidence","implementation"]'
---

# Test first

## The red run exists to produce a disagreement

A test is a prediction: given this input, the system produces that result. Before
the implementing change, the system has to contradict the prediction, and the
contradiction has to be printed in values you can read. Two numbers, two strings,
two statuses, and the name of the assertion that put them side by side.

That printed contradiction is the whole product of the red run. Not the ordering,
not the red colour of the output, not an exit code. Ordering is easy to obey and
proves nothing on its own: a test written before the code and never run, or run
and misread, sits in the tree looking exactly like one that did its job.

So the question at the red step is never whether the test ran first. It is what
the run printed, and whether that text is the system disagreeing with the
prediction rather than something else that also comes out red.

## Three ways a red run produces nothing

**It agreed all along.** The command comes back green on unchanged code. The
prediction is about behaviour that already exists, or the assertion is weak enough
that anything satisfies it, or the test never reached the code it names. Whatever
the cause, the outcome is a test that will pass forever and can never report a
break. This one is worth suspecting first, because green on the unchanged code is
easy to read as progress. Nothing was in the way, so the work must be going well.

**It broke instead of disagreeing.** An import that does not resolve, a symbol
that does not exist yet, a fixture that is missing, a syntax error, a
configuration the runner refused. The command failed. The prediction was never
evaluated, so the system has not said anything about it. Fix whatever stopped the
run and run it again. Until an assertion is reached, the red step has not started.

Watch the boundary case here: in a language that will not compile with a call to a
function that does not exist, the first run of a genuinely new behaviour is always
this. That run is scaffolding, not evidence. Add the smallest declaration that
lets the file build, returning whatever an unimplemented thing returns, and run
again for a real disagreement.

**It disagreed illegibly.** The command failed, an assertion was reached, and the
output names nothing: a bare truth check, six unlabelled assertions in one test
body so the report says only which line, an equality between two structures
printed as two walls of text. You can see that something is wrong and cannot see
what. Later, when this test breaks on somebody else's change, they will read the
same output with less context than you have now. Split the test until each failure
identifies itself, or give the assertion the message it needs.

The first and third cases both come back with a verdict that looks decisive, and
neither has told you anything. Reading the actual text of the failure is what
separates them.

## Write the expectation in values

Before the body of the test, say the two things out loud: the input the system
gets, and the exact result it should produce. A concrete result, written out. Not
"returns something sensible", not "does not error": `4.05`, `"tag:urgent"`, `409`,
an empty list, an error naming the field.

Where the result comes from decides whether the test can disagree at all, and it
goes wrong without producing a single failure. A disagreement needs two
independent parties. If the expected side is produced by
calling the same code the test is examining, or by a helper that shares its
arithmetic, or by recording whatever came out the first time and freezing it, then
there is only one party in the room and it always agrees with itself. Such a test
turns green the moment the code runs at all, whatever the code does, and it will
stay green through the bug it was written to catch.

Get the expected value from somewhere the implementation cannot reach: worked out
by hand, taken from the specification, copied from the request that prompted the
work, computed by a different method, quoted from the person who asked. When you
cannot produce it that way, you do not yet know what the behaviour is, and that is
the finding to report rather than a gap to fill with whatever the code emits.

## One prediction, then the code for it

Take a single behaviour per cycle, all the way through: predict, disagree,
implement, agree. Then the next.

Writing a batch of tests up front and then implementing against the batch loses
most of what this method is for. Predictions written before anything exists are
guesses about the shape of an interface nobody has used yet, and a dozen of them
commit you to that shape before the first one has been through the code. Each
completed cycle changes what the next prediction should say. Spend that.

Keep the implementing change small enough that the disagreement it resolves is
obviously the one you recorded. When you find yourself writing code no current
prediction requires, stop: it has no red run behind it, so nothing here covers it,
and it is being added on the strength of a guess about later.

## Where the prediction is observed

A prediction has to be made at a place where the behaviour is visible from
outside: an exported function, a handler, a command, a message consumed, a row
written. Observed there, the test survives the code underneath being rearranged,
which is most of its value over the years it exists.

Reaching inside for something private buys a disagreement that is easy to produce
and worth little. It fails on tomorrow's rename and stays quiet when the behaviour
itself goes wrong, which is precisely backwards.

If no such place exists, that is a finding about the design and it outranks the
test. Say so, and say what would have to exist: a return value instead of a
mutation, an argument instead of a global read, a seam where a dependency is
handed in. Report it rather than routing around it with a test that reaches into
internals and pretends the coverage means something.

## Green: the same command, unchanged

Run the identical command, and read the pass with the same care you read the
failure. What ran green has to be the thing that ran red, character for character.

Between the two runs, the test file does not change. Not the assertion, not the
expected value, not the input, not a skip marker, not a loosened comparison.
Editing the test between red and green means the disagreement you resolved is not
the one you recorded, and the pair of runs no longer says what it appears to say.
If the red run showed the prediction itself was wrong, that is a new prediction:
correct it, run it red again on the unchanged code, and start the pair over.

Then run the wider suite. A behaviour that arrives by breaking a neighbour is not
finished, and the neighbour's test is a prediction somebody made for a reason.

## What this does not cover

Configuration with no behaviour in it, generated files, copy changes, a
dependency bump that alters nothing you own, an exploratory sketch you are going
to throw away. Predict nothing, and say plainly that nothing was predicted.

Chasing a bug that is already happening is a different job with a different order
of operations. Reproduce it, narrow it, and only then does the narrowed case
become a prediction that this method applies to.

## The pair of runs is kept here

In this runtime a goal's ledger holds the units of work, and each unit carries the
declared command that proves it. The item's identity is a hash of its text
together with that command (`goal/ledger.go`), and the ledger is append-and-mark
only: an item cannot be rewritten or removed, so narrowing the check after the
fact is refused as a regression rather than passing as an edit. An item with no
declared check is refused outright, because it could only ever be asserted.

Write the check into the item before you build. That is the ordering rule in a
form nothing later in the run can undo.

Running it goes through the same governed path as any other action
(`goal.verify.item`, in `evidence/evidence.go`), and the verdict is appended to
the run's own event stream as `item.verified` (`chain/evidence.go`). A failing
verdict is recorded rather than dropped, carrying the exit code and a hash of the
output. Each verdict is stamped executed or asserted by whatever ran it, and there
is no path by which a model writes that field itself.

So the red step is an event: this exact command, verdict false, executed, at a
known position in the run. The green step is the same command's identity, verdict
true, executed, later in the same stream. `flynn inspect <run>` replays them in
order and `flynn spine verify <run>` reports the record's ground-truth tier.

Red before green stops being something the run says about itself and becomes
something a reader who was not there can check.

## The check

For each behaviour changed in this run:

1. A verdict exists for its declared check, executed, verdict false, before the
   first edit to the implementing code.
2. The output behind that verdict shows an assertion that was reached and a
   comparison of values, not a failure to build, resolve, load or start.
3. A later verdict exists for the same item identity, executed, verdict true.
4. Nothing between the two changed the test.

Where an item reaches green with no failing verdict recorded before it, the test
attached to that item is of unknown value, whatever its coverage number says.

## Refusals

- No implementing code before the run that should fail has been run and read.
- No expected value derived from the code under test, its helpers, or its current
  output.
- No test edited between its failing run and its passing one.
- No prediction weakened, skipped or deleted to reach green.
- No claim that a check ran where nothing ran it.
- No coverage reported as evidence in place of a failure that was observed.
