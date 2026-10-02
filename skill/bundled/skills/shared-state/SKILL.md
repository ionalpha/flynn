---
name: shared-state
description: Use when two or more code paths can touch the same state at once: goroutines or threads writing one variable or counter, two users editing the same record where one write overwrites the other, a retry after a timeout that may repeat a write, a handler given the same message twice, an update that gets lost when a background worker runs, work spread across processes or machines, or a mutex, channel, queue or lock being introduced. Covers the two questions that precede choosing a primitive, why an ordering edge kept in data (a version, a sequence number, a logical timestamp, an idempotency key) is invisible to every detector and compiler, retry safety when a mutation may already have been applied, why read-modify-write loses one of two writes across any boundary, and ordering events across machines without trusting a wall clock. One rule above the rest: a concurrency fix ships with a test that fails without it, since a race detector reports only races it observed.
metadata:
  flynnhq.com/title: Shared state and ordering
  flynnhq.com/tags: '["concurrency","ordering","races","idempotency","distributed"]'
---

# Shared state and ordering

## Two questions come before any primitive

Before reaching for a lock, a channel, a queue or a transaction, answer two things in
writing:

1. **What is shared?** The specific variable, field, row, file, counter or remote
   record that more than one thing can touch.
2. **What orders the accesses to it?** Name the thing that makes one access happen
   before the other. If you cannot name it, there is no order, and you have found the
   defect rather than a style question.

Call the answer to the second question the **ordering edge**. A race is a missing
edge. Everything below is about which kind of edge you need and how to prove you
added it.

Taking the questions in this order matters because the primitive answers only one of
two problems. A lock gives mutual exclusion: while I hold it, nobody else is inside
the same section. It says nothing about who goes first. A study of 105 real
concurrency bugs in four large open-source programs found roughly a third were
violations of an order the author intended and no lock expresses, and about a third
involved more than one variable at once. Adding a lock to an ordering bug produces a
program that is still wrong and now slower.

## The edge is in memory, or the edge is in the data

The two kinds behave nothing alike, and confusing them is the mistake that survives
review.

| | Edge in memory | Edge in the data |
|---|---|---|
| What it is | A mutex, a channel send, an atomic, a wait group, an actor hop | A version compared on write, a sequence number, a logical timestamp, an idempotency key |
| Who defines it | The language memory model | Code you wrote, and nothing else |
| Where it reaches | One address space | Anywhere the data goes |
| What sees it | A dynamic race detector, and in a few languages a type checker | Nothing |

An edge in memory is what the whole published literature on this subject teaches, and
it is a solved problem in the sense that tools help you. An edge in the data has to be
written down, compared and acted on by code you wrote, because no runtime knows that
a field is an ordering claim. Comparing a record's version on write is an ordering
edge. A logical timestamp compared on merge is an ordering edge. A sequence number on
an append-only log is an ordering edge. A wall clock reading is not one.

The moment shared state crosses a process boundary, every edge you have is of the
second kind, and no tool is coming to help. That is why the sections after the next
one are longer than the primitive selection you were expecting.

## What the detector saw is all the detector knows

A dynamic race detector such as the one behind `-race` builds a happens-before graph
over the execution that actually ran. Given synchronisation it understands, every
report it makes is a true race: there are no false positives worth arguing with. It
has false negatives without limit, because it can only find races on the code paths
that executed in that run, with the interleaving that run happened to take.

So a green run under the detector says one thing: nothing raced this time. It is not
a proof, and treating it as one is the most common way a concurrency fix ships broken.

The second limit is harder. The graph the detector builds ends at the address space,
so for an edge in the data there is nothing for it to instrument and nothing for it to
report. Two processes writing the same row are not a data race in any sense the tool
recognises, and the lost update is total.

There is a measured failure mode here worth naming, because it passes every check the
tools offer. Across 23 models on 115 concurrency problems graded by a model checker
rather than by a test run, 74 solutions failed because the program never created the
concurrency at all: it reached for a thread-safe collection and never started a
worker, or it finished before the threads could interact. The code named the right
primitive, compiled, ran, answered correctly, and was green under every detector,
because nothing in it ever ran at the same time as anything else. The same study found
that surface similarity to a correct solution does not predict concurrent correctness:
incorrect programs regularly scored higher on it than correct ones.

## A concurrency change ships with a test that fails without it

This is the rule the skill exists to enforce. The failing test comes before the fix,
it has to fail for the reason you believe it does, and it stays in the suite
afterwards as the thing that catches the regression. A fix that was only ever asserted is
indistinguishable from a fix that does nothing, and both of them are green.

The same 105-bug study found that 92% of those bugs could be triggered reliably by
forcing an order among no more than four memory accesses. The interleaving you need is
small. You can construct it.

**When the edge is in memory**, the test has to make the racy access happen, not hope
for it. Start the two goroutines, hold them at a barrier, release them together, run
it under the detector, and repeat it enough times that the schedule varies. If the
test passes on the broken code, it is not testing the race; make the window wider,
add the barrier, or drive the two sides explicitly.

**When the edge is in the data**, there is no detector, so determinism is the whole
method. This runtime is built for it and the pieces are already in the tree:

- `clock.Manual` (`clock/clock.go`) is the injected clock every component reads time
  through. It moves only when the test moves it, and the timers it hands out fire only
  when it passes their deadline, so a backoff, a lease expiry or a timeout happens on
  the line where you advanced the clock. No sleeping, and the same schedule every run.
- `testkit.FaultPlan` and `testkit.FailOnCall` (`internal/testkit/chaos.go`) inject a
  failure at exactly the call you name, counting calls rather than guessing at timing.
  That is how you write "the write succeeded and the response was lost" as a test
  rather than as a comment.
- A `rapid` state machine over the event log runs long random action sequences against
  a model and checks the invariant after every step, and it shrinks a failure to the
  minimal sequence that reproduces it.
- `reconcile/chaos_test.go` is the finished shape: a property draws a run of transient
  failures, the manual clock is driven through each backoff, and the assertion is that
  the controller converges and stops exactly once.

State in the change what the test does when the fix is reverted. "Fails at the
assertion on line 40 with two writes and one recorded" is the evidence. "Added a
mutex" is not.

## Read, then modify, then write, is how updates get lost

Read a value, decide something from it, write the result. Between the read and the
write, someone else did the same, and one of the two updates is gone with no error
anywhere. This one shape covers a struct field, a database row, a stored resource, a
file, a counter and a remote API, and the fix is the same at every scale: **the write
carries what the read saw**.

That means compare-and-set, not assignment. Send the version you read and let the
store refuse the write if it has moved. In this tree that refusal is
`resource.ErrConflict`, and `resource/retry.go` is what the loop around it looks like
when it is written properly: read, mutate, put, and on a conflict re-read and try the
mutation again against the new state.

Two details in that file are the ones people leave out. The retry is bounded
(`maxConflictRetries`), because an unbounded conflict loop under contention is a
livelock that looks like a hang. And exhaustion returns an error that still matches
`ErrConflict`, so a caller that was going to handle contention can still tell that is
what happened.

Where the store has no version, the same idea appears as a conditional write: an
`If-Match` header, a `WHERE version = ?` clause, a compare-and-swap, a unique
constraint. Where none of those exist, you have no edge and you must stop pretending
otherwise: serialise the writers, or accept and document that the last writer wins.

## A retry is a second attempt at something that may already have happened

When a call to change something fails, exactly one question decides whether retrying
is safe: did the request reach the other side? Three outcomes, and they are not
interchangeable.

| What failed | Did it apply? | Retry |
|---|---|---|
| The connection was refused, or the request was never sent | No | Safe |
| The request was sent and the response never arrived, or timed out | Unknown | Only if the operation is idempotent |
| The other side answered with a refusal | No | Never, until the input changes |

The middle row is the whole subject. A read timeout after the bytes went out is not a
failure of the operation; it is a failure to learn the outcome. Retrying it repeats a
mutation that may already have been applied.

This is why failures in this runtime are typed rather than matched by string.
`fault.Class` (`fault/fault.go`) says how a caller must react: `Transient` is worth
retrying, `Terminal` must not be, and `Cancelled`, `Forbidden`, `NeedsApproval` and
`BudgetExceeded` each mean a different thing to a caller. `fault.Classify` treats an
unclassified error as `Terminal`, so retrying is something a failure is explicitly
granted rather than something it inherits by being unrecognised. Preserve that
default. An error that reaches the router unclassified is not a candidate for a retry
loop.

To make the unknown case safe, give the operation an **idempotency key** derived from
the request, not generated per attempt, and have the receiving side record the key
with the result before answering. A repeat then returns the stored result rather than
performing the change again. The key belongs at the adapter boundary, once, so every
caller of that adapter gets the property instead of each one reinventing it.

The same applies inbound. A message bus that promises at-least-once delivery will hand
you the same message twice, and a bus that is at-most-once will silently drop one.
Neither is fixed by asking for exactly-once, which no transport can give you across a
failure. Make the handler idempotent and the delivery guarantee stops being the
interesting question.

A study of 104 concurrency bugs across four widely deployed distributed systems found
that 63% of them appear only when a fault is present: a crash and reboot, a network
partition, a delayed message, a disk error. Testing the happy path at high
concurrency does not reach any of them, which is what fault injection at the ports is
for.

## Re-read the state; do not act on the notification

When work is triggered by an event, acting on the payload of that event assumes it
arrived, arrived once, and arrived in order. Across a process boundary none of those
is given.

Take the durable identity from the notification and read the current state yourself.
Then the handler behaves the same whether the event arrived once, twice or out of
order, because it acts on what is true now rather than on what was true when the
notification was written. That is why a reconciler here is handed only a `Ref` and
must re-read the live resource (`reconcile/manager.go`), and why the manager
re-enqueues every live resource on a `DefaultResync` interval: the bus is at-most-once,
so a lost hint has to be recovered by resync rather than turning into work that never
happens.

The cost is real and worth paying knowingly. You do more reads, and you must make the
handler safe to run when nothing changed.

## Across machines, a wall clock is not an order

Two nodes comparing `time.Now()` are comparing two independently wrong numbers. Clocks
skew, and they step backwards when they are corrected. A record written later can
carry an earlier timestamp, and last-writer-wins on wall time then discards the newer
write with no error.

A hybrid logical clock is the standard answer and this tree has one. `hlc.Time`
(`hlc/hlc.go`) is a wall reading in milliseconds plus a logical counter. `Now` returns
a value strictly after every earlier one from that clock, and `Observe(remote)` takes
the maximum of local, remote and physical time before ticking, which is what carries
causality across instances: after you have seen a remote event, everything you produce
sorts after it, whatever your local clock believes. Ties resolve by comparing wall,
then counter, then writer id, so every node reaches the same answer without asking.
The physical clock is injected, so a replay reproduces the same timestamps.

Two further habits belong with it. **Keep one copy.** State here projects from a
single append-only log (`spine/spine.go`) whose `Seq` is monotonic within a stream and
whose events are never mutated or deleted, carrying `CausationID` for causal replay
and `OriginInstanceID` from the first version so multi-instance sync never forces an
identifier change. Two mutable copies of the same fact will diverge, and reconciling
them afterwards is a harder problem than ordering was. **Decide the merge rule
explicitly.** Last-writer-wins is a legitimate choice, and it silently discards a
concurrent update. Say so where the rule lives, or use a structure that merges without
loss.

## What to report when you change concurrent code

- What is shared, in the specific: the field, row or record.
- What orders the accesses to it now, and whether that edge is in memory or in the
  data.
- The test that fails without the fix, and what it does when the fix is reverted.
- For anything retried: which failures are retried, why they are safe to repeat, and
  where the idempotency key is recorded.
- For anything ordered across processes: what supplies the order, and what happens to
  a concurrent write that loses.

## Refusals

- No concurrency fix without a test that fails on the unfixed code.
- No treating a green run under a race detector as proof that a race is absent.
- No retry of a mutation whose failure mode is "the outcome is unknown" unless the
  operation is idempotent.
- No unclassified error retried, and no retry loop without a bound.
- No read then modify then write across a boundary where the write does not carry what
  the read saw.
- No wall-clock comparison used to order events produced on different machines.
- No lock added to fix a bug whose ordering edge was never named.
- No sleep used to make a test pass; move the injected clock instead.

For the failure shapes and how each one is tested, see
[the taxonomy](references/taxonomy.md). When the symptom is a test that fails
intermittently and the cause is unknown, narrow it first with the debugging skill, and
come back here once you know what is shared.
