---
name: performance-measurement
description: Use when something is meant to get faster, cheaper or smaller: a slow page, endpoint, query or job, a function someone wants optimised, a benchmark to write, a profile to read, a latency or memory or bundle number to bring down, or a belief that an edit has already sped something up. Treats the pair of measurements as the deliverable: a before number, an after number taken the same way, and a difference big enough to clear the run-to-run spread. Covers naming the share of total time a target holds before touching it, the seven conditions that make two timings comparable, keeping the benchmark honest when the timed region is what you are editing, one edit per comparison, and reverting whatever failed to beat the noise.
metadata:
  flynnhq.com/title: Performance measurement
  flynnhq.com/tags: '["performance","benchmarking","profiling","optimisation","measurement"]'
---

# Performance measurement

## The deliverable is a comparison

What you are building here is not a faster program. It is a difference between two
measurements, and that difference is worth exactly what the pair behind it is worth.
The code change is one input to the pair. It is usually the cheapest input, and it is
the only one anybody remembers to work on.

Measure first, profile, change, measure again: every model can already recite that,
because it is in every guide written for the last twenty years. Reciting it does not
work. On a benchmark of real performance-improving pull requests, the strongest agent
configuration produced patches that applied 88% of the time and were correct 78% of
the time, and those correct patches ran 2.26% faster where the human patch on the same
issue ran 10.85% faster. The loop was followed. The output was noise wearing a commit
message.

So the skill is not the loop. It is everything the loop leaves unspecified: which
quantity, whose share of the total, what makes the second number mean the same thing
as the first, and what the difference has to beat before it counts as one.

## Name the share before you name the fix

The first number is not a timing. It is a fraction: how much of the total the thing
you are about to change is responsible for. A tenfold win on 3% of the runtime returns
2.7% and costs whatever the rewrite costs forever.

So attribute the total before touching anything. A profile, a query log with times, a
trace with spans, a flame graph, a counter around the suspect region: any of them, so
long as what comes out is a share rather than an ordering. Write down what the biggest
contributor is and what percentage it holds. If nothing accounts for more than a small
slice, the answer is that this program has no hot spot, and the honest report says so
instead of picking the largest of the small ones.

This is the step that is measured to go wrong. Human patches on real optimisation
issues average around 131 lines across 4.3 files and 7.6 functions; model patches
cluster in one place, on a low-level data structure, and success falls as the number
of functions that have to move together rises. The local loop you can see is not the
share. Attribution is what tells them apart.

## What makes the second number comparable to the first

This is the part every published version reduces to the phrase "identical conditions".
It is not one condition, it is seven, and each one has inverted a published result
somewhere.

**One build recipe.** Same compiler, same flags, same optimisation level, same linked
artefacts in the same order. Link order and the size of the process environment are
both enough on their own to move measured time systematically, with nothing about the
program changed. One published study found a single benchmark where compiling at a
higher optimisation level measured anywhere from a 12% slowdown to a 9% speedup
depending only on setup. If the before number came from a debug build and the after
from a release build, you have measured your build system.

**One input.** Same data, same size, same distribution, same cache contents, same seed.
An input that grew between runs is a different question answered twice.

**One machine, in one state.** Same host, not a same-sized host. Nothing else running.
Power profile, thermal state and frequency scaling pinned or at least noted. Container
CPU and memory limits identical for both arms.

**Warm or cold, declared.** Say which you are measuring: first request against a cold
process and cold caches, or steady state after warm-up. Both are legitimate questions
and they are not the same question. Discard the warm-up iterations by a rule fixed in
advance, never by looking at which ones flatter the result.

**Interleaved, not batched.** Run baseline, candidate, baseline, candidate, in
randomised order, rather than all of one then all of the other. Anything that drifts
over the session (a warming machine, a noisy neighbour, a filling disk) lands on the
arm that ran second, and it lands there as your result.

**A repeat count fixed before the first run.** Decide how many iterations before you
see any of them. Stopping when the numbers look good is how a null result becomes a
win.

**The spread reported next to the difference.** A single timing is not a measurement.
Report the median and the spread, and for anything user-facing report a tail
percentile too, since the mean of a latency distribution hides the thing people
complain about.

None of this requires a clean room. Benchmarks on shared cloud hosts show run-to-run
variation ranging from a fraction of a percent to a hundred percent depending on the
workload and the instance; on the same instances, with the two arms interleaved in
randomised order and enough repeats, slowdowns of ten percent and smaller are still
detectable with confidence. Comparability is achievable on ordinary hardware. It is
just not free, and it is not the default.

## The harness is inside the system you are changing

Once a timed region exists and a number from it decides whether you are done, that
region becomes a target. This is not a hypothetical risk: the benchmark suites built
to grade automated optimisation ship a detector for deceptive patches, and the two
behaviours common enough to need one are memoising exactly what the performance test
asks for and editing the harness.

The rule is that the measurement has to keep exercising the work a real caller causes.
Concretely, these are refusals rather than judgement calls:

- No cache keyed on the input the benchmark happens to use, or on anything the
  benchmark alone can produce.
- No work hoisted out of the timed region that has not also left the request. Moving a
  computation to process start makes the timer smaller and the first user slower.
- No shrinking of the workload, the iteration count, the data set or the assertions to
  make the number move.
- No edit to the harness, the timer, the fixture or the threshold in the same change as
  the optimisation. If the harness is genuinely wrong, fix it first, in its own change,
  and re-establish the baseline afterwards.
- No result kept that does not survive an input the harness has never seen. Run the
  candidate once against a second workload, of a different size or shape, before
  believing it.

A change that is faster only under the measurement is worse than no change, because it
consumes the budget and leaves a maintenance cost with the alarm already disconnected.

## One change per comparison

A batch of five edits and a 30% improvement tells you that the five together did 30%.
It does not tell you that any of them helped, and in practice two are usually
carrying it while one is a regression paid for by the others. Measure them one at a
time and you learn which to keep. Measure them together and you keep all five forever,
including the ones that cost.

Where a change genuinely has to move several files at once to mean anything, that is
one change. The test is whether the pieces are independently revertible, not how many
lines there are.

## When the difference does not clear the spread

Revert it. This is the rule that gets skipped, and it is the one with the compounding
cost.

Set the bar before the run: the difference has to exceed the run-to-run spread you
measured on the baseline by a stated margin, or it is not a result. A 3% improvement
on data that varies by 8% is a number you generated, not a change you made.

What survives a revert is the knowledge, so record the attempt: what was tried, what
it measured, and that it was reverted. Otherwise the next run does the same work, gets
the same non-result, and possibly keeps it.

A neutral change is not free and is not harmless. It is complexity someone maintains,
reads, and works around, bought with nothing. The same applies to a change that is
faster and unreadable when the share it holds was small.

## A wrong answer has no speed

Correctness is a precondition of the number, not a trade-off against it. Run the
existing suite against the candidate before the timing means anything, and where the
optimisation changes an algorithm, add a test that pins the output against the old
implementation over a range of inputs rather than one example.

Watch specifically for the optimisations that are correct on the benchmark's input and
wrong elsewhere: a cache with no invalidation, a fast path with an unchecked
precondition, a parallel version with a race that the timing run happens not to hit, a
reordering that changes floating-point results. Each of these measures beautifully.

## Both arms belong on the record, not in the summary

In this runtime, a measurement is an action rather than an assertion, and that is what
makes a before-and-after claim checkable by someone who was not present.

A declared check runs through the same admission path as every other tool call, under
its own action name, and its verdict is appended to the run's event stream with the
full output hashed onto the event. So the baseline command and the after command are
two events in one ordered log, with sequence numbers. That the baseline was taken
before the change is a property of the record rather than a claim in a paragraph, and
a comparison against a baseline that was never run has nowhere to hide.

`flynn inspect <run-id>` prints that history back, so a reviewer reads the baseline as
it was taken, including the command, the environment and the output, rather than a
later description of it. Grading works from the same recorded effects and never from
the run's narration, and it is deterministic, so a performance claim can be re-graded
months later without rerunning anything.

The guard step is already enforced rather than recommended. Packages on the declared
hot-path list must carry a Go benchmark function or `go test ./...` fails, so a path
that has once been measured cannot quietly lose its measurement in a later refactor.
Add the package to that list in the same change that optimises it.

Where the work runs under the isolation tier, the vCPU count, memory ceiling, process
cap and wall-clock limit are declared before the guest starts, which turns "the same
machine" from something you remember into something both arms are stated to have run
under. Put the same declaration on both.

## What to report

Say what a reviewer cannot recover by reading the diff:

- The quantity measured, and the share of the total it held before the change.
- The before number and the after number, each with its spread and its repeat count.
- The conditions both arms ran under: build, input, host, warm or cold, interleaved or
  not.
- Whether the difference cleared the spread, and by what margin.
- What was tried and reverted.
- That the suite passed against the candidate.
- What you did not measure, if anything about the claim rests on it.

## Refusals

- No optimisation before a baseline has been run and recorded.
- No claim of improvement without an after number taken under the conditions the
  before number was taken under.
- No comparison across different builds, inputs, hosts, or warm states.
- No change to the harness, the timer or the workload in the change being measured.
- No cache or fast path keyed on what the benchmark uses.
- No batch of independent edits reported as one improvement.
- No change kept whose difference sits inside the spread.
- No timing reported for a candidate whose tests have not passed.
- No target chosen without knowing what share of the total it holds.
