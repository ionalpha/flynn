# The failure shapes, and how each one is tested

There are fewer shapes than there are bugs. Each entry below names what is shared,
which ordering edge is missing, how it presents when it goes wrong, and the test that
fails before the fix. Find your symptom in the third column and read the fourth.

## Shapes with the edge in memory

**Lost update on a shared variable.** Two goroutines read a counter, a map entry or a
struct field, compute from it, and write back. One result disappears. Presents as a
total that is too low, a cache that occasionally forgets an entry, or a flag that
resets. Test: start both writers behind a barrier, release together, repeat a few
hundred times under the race detector, and assert the final value. On the unfixed code
the detector reports the pair of accesses and the assertion also fails on its own.

**Check then act.** Read a condition, then act on it, with a gap between. Presents as
two of something that should be unique: two files created, two sends of the same
notification, a directory removed twice. Test: hold both callers at the point after
the check and release them together; assert the effect happened once. A lock around
the check alone does not fix it, since the whole check-and-act has to be one section.

**Concurrent map or slice access.** Reading a map while another goroutine writes it.
Presents in Go as a hard runtime crash rather than a wrong answer, which at least is
loud. Test: a loop writing while a loop reads, under the detector. Note that
`sync.Map` and a mutex are answers to different questions; a mutex around a
read-modify-write of one entry is what the lost-update shape needs.

**Iteration or closure capture.** A goroutine started inside a loop that reads the
loop's variable, or a shared buffer reused across iterations. Presents as work done
several times on the same item and never on others. Test: run the loop with a large
enough body, collect what each goroutine saw, and assert the collected set equals the
input set.

**Lock-order inversion.** Two paths take the same two locks in opposite orders.
Presents as a hang under load, never in a unit test. Test: two goroutines, each taking
the locks in one of the two orders, released together, with a timeout on the whole
test so a deadlock reports as a failure rather than as a hung run. The durable fix is
a documented lock order, not a timeout.

**Missed wakeup.** A waiter checks a condition and then waits, while the signaller
sets the condition and signals in the window between. Presents as a worker asleep with
work queued. Test: force the interleaving by signalling before the waiter waits, and
assert the waiter still proceeds. The fix is to hold the same lock over the condition
check and the wait, or to use a channel whose buffering keeps the notification.

**Leaked goroutine or task.** Work started with no exit path: no cancellation, no
receiver for its send, no bound on its lifetime. Presents as memory and file
descriptors climbing over hours, and as a test suite that slows down. Test: count live
goroutines before and after the unit of work, with the leak checker running at the end
of the test.

## Shapes with the edge in the data

None of these is visible to a race detector. Every test below is written with the
injected clock and a deterministic fault plan rather than with real timing.

**Lost update across a store.** Two processes read the same record, mutate different
fields, and write back whole. Presents as an edit silently reverting minutes later,
which is usually reported as a user error. Test: read the record twice into two
variables, write both, and assert that the second write is refused because the version
moved. On the unfixed code both writes succeed and the first mutation is gone.

**Duplicate delivery.** The same message or the same request arrives twice, because
the transport is at-least-once, because a client retried, or because a response was
lost. Presents as a double charge, a doubled counter, or two records where the natural
key allows two. Test: call the handler twice with the same message and assert the
effect happened once and the second call returned the recorded result.

**The retry over an unknown outcome.** The request went out, the response never came
back, and the caller retried. Presents identically to duplicate delivery and is found
in a different place, because the defect is in the caller's classification of the
failure. Test: inject the failure at exactly the call after the effect using a fault
plan, then assert the total number of effects is one.

**Out-of-order arrival.** Two updates about the same entity arrive in the opposite
order to the order they were made, because they took different paths or because one
was retried. Presents as an older value overwriting a newer one, intermittently and
usually for one entity. Test: deliver the two updates in each of the two orders and
assert the same final state both times.

**Lost notification.** A change hint is dropped and the work it would have triggered
never happens. Presents as a stuck record: everything else moved on, this one stayed.
Test: drop the hint, advance the injected clock past the resync interval, and assert
the work happened anyway. A system that fails this test has no safety net, whatever
its retry policy says.

**Partial failure after a fault.** A sequence of steps was interrupted by a crash,
a restart or a partition, leaving some steps applied. Presents as a record in an
impossible combination of states. Test: inject the fault after each step in turn,
restart the component, and assert it converges to a valid state from every one of
those points. The measurement to keep in mind is that most distributed concurrency
bugs in real systems appear only when a fault is present, so this is the test that
finds them.

**Clock skew across nodes.** Two machines order events by comparing wall-clock
readings. Presents as the newer write losing, at a rate that tracks how far apart the
clocks have drifted. Test: give the two writers manual clocks set minutes apart, write
in a known real order, and assert the later write wins. A logical clock passes this;
`time.Now()` cannot.

**Lease or timeout expiry during use.** A holder believes it still owns something
after its lease has expired elsewhere. Presents as two owners doing the same exclusive
job. Test: advance the injected clock past the expiry between the acquisition and the
use, and assert the holder notices before acting.

## Two things that are not fixes

**Adding a sleep.** It changes the probability of the interleaving and nothing about
the ordering, so the test that passes with it is a test that will fail again on a
slower machine. Where the sleep is in a test, replace it with an advance of the
injected clock. Where it is in production code, it is an ordering edge that was never
written down.

**Widening a lock until the symptom goes.** A lock held across a call into other code
is how lock-order inversions get created, and a lock held across an operation that
crosses a process boundary converts a slow dependency into a stalled service. Name the
shared state, take the lock over exactly the accesses to it, and if the edge you need
turns out to be in the data, no amount of widening will produce it.
