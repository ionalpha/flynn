---
name: contract-design
description: Use when designing or changing anything other code depends on: an HTTP or RPC endpoint, an exported function or type, a schema, an event, a plugin interface, a flag, a configuration key. Covers which edits break callers and which do not, why the answer is different for what you accept and what you return, what a consumer may rely on in a payload it did not write, what an error has to tell a caller before they can act on it, how to find out whether a change breaks anyone instead of deciding that it does not, and how to design a first version so the second one can be an addition.
metadata:
  flynnhq.com/title: Contract design
  flynnhq.com/tags: '["api","contracts","compatibility","versioning","errors"]'
---

# Contract design

## Callers depend on what you did not promise

Hyrum's law is the observation that with enough callers, every observable behaviour
of your system is depended on by somebody, whatever the documentation says. The
ordering of a list you never promised to order, the text of an error, the fact that a
field is always present, the timing: each becomes load somebody is carrying.

The useful consequence is not caution, it is size. Every field you return, every case
you expose, every quirk a caller can see is a promise you now maintain. The cheapest
contract to keep is the smallest one you can publish, and the time to decide that is
before anyone is on the other side.

## Compatibility runs in the direction the value travels

"Is this change breaking" has no answer until you say which way the value moves. The
rule is short: you may loosen what you accept, and you may not weaken what you
guarantee.

| Direction | Free to do | Breaks callers |
|---|---|---|
| What you accept | Add an optional field. Accept a value you used to reject. Widen a type. Accept a new enum value. Relax a limit. | Require a new field. Reject something you used to accept. Tighten a validation or a limit. Narrow a type. Make an optional field mandatory. |
| What you return | Add a field, if consumers ignore what they do not recognise. Stop returning an error you used to return. Populate a field you already declared. | Remove or rename a field. Make an always-present field optional or nullable. Add a case to a closed set the consumer must exhaust. Change a format, a default, or an order somebody could observe. |

The row that surprises people is the last one, so here is the evidence. Three primary
specifications answer the same question, "may I add a value to an enum", three
different ways. Protocol Buffers says plainly that adding values to an enum is safe.
The Cargo book classifies adding an enum variant as a major change, because a caller
matching exhaustively no longer compiles. Google's AIP-180 splits it by direction: an
enum used only in requests may gain values freely, while one that appears in a
response has to have announced up front that it can.

All three are right about their own situation, and the reason they differ is the
reason for the table. A value moving towards you can only meet code you control. A
value moving away from you meets code that has already been written.

## Whether an addition is safe depends on the language

The general rule that additions are compatible is true of wire formats and false of
several type systems. That is the expensive half, because a type system refuses a
caller's build, and a caller whose build fails has been broken by you.

Go's own compatibility promise is the clearest illustration, because it lists the
exceptions to its own guarantee: an unkeyed struct literal may stop compiling when a
field is added, while a keyed one will not, and adding a method to a type can collide
with a method of the same name in a struct that embeds it. Rust is stricter still.
Adding a public field to a struct that has no private field is a major change, since
callers construct it by literal, and adding a defaulted method to a trait can create
an ambiguity at a call site that used to resolve.

So read the promise your own ecosystem makes before trusting the general rule. Where
the language offers a construct that keeps additions safe, use it in the first
version: a keyed literal, a constructor rather than an exported struct, an enum
marked as open, an interface nobody outside your package can implement.

## Read what you use, ignore the rest

A consumer that rejects a payload for containing something it did not expect turns
every compatible addition by the producer into an outage of its own making. The
specifications are explicit about this. RFC 9457, which defines the problem details
format for HTTP errors, requires that clients "MUST ignore any such extensions that
they don't recognize; this allows problem types to evolve". Proto3 keeps unknown
fields through a parse and a reserialise for the same reason.

This sits under the general advice to validate anything that arrives from outside,
and both are right about a different half. The resolution:

- Validate the fields you read. Types, ranges, authorisation, and anything you are
  about to act on or render.
- Ignore the fields you do not read. Never reject a payload for carrying something
  extra, and never fail a parse on an unrecognised member.
- Handle an unrecognised case rather than crashing on it. A status you have never
  seen should route to a default branch that says so, not to an assertion.
- Preserve what you pass on. If a payload travels through you, dropping the members
  you did not understand silently breaks the two systems either side of you.

The mirror of this rule is what you may rely on, and the answer is: only what was
promised. Not field order, not key order, not the text of a message, not an
identifier's internal structure, not the absence of a field, and not timing.

## An error must say whether retrying can help

A caller receiving a failure has three moves: try again, give up, or bring in a
person. An error that does not say which one applies has left the decision to the
caller, and the caller will make it by matching on the message text, which Hyrum's
law then turns into a contract you never meant to write and will break by fixing a
typo.

So classify, and put the classification in the payload rather than in the prose:

- A stable machine-readable code, chosen once and never reused for a different
  meaning. RFC 9457 does this with a URI in `type`; a short constant string does the
  same job inside a program.
- The class of reaction, which is the part that is usually missing. In this runtime
  that is `fault.Class`, and the values are the reactions rather than the causes:
  `transient`, `terminal`, `needs_approval`, `budget_exceeded`, `forbidden`,
  `cancelled`. The router, the governor, and the reconciler all branch on it and
  never on message text, which is what makes the classification worth writing.
- A human-readable message, which is for the person reading a log and is not part of
  the contract.

Classify at the boundary where the foreign error arrives, in the adapter that knows
what a 429 or a closed connection means in that protocol. Anything deeper in the
program that has to work out whether a failure was transient is doing it from less
information than the code that received it.

Calling a failure retryable creates one more obligation. If you tell a caller they
may repeat a write, take an idempotency key and make the repeat return the first
outcome instead of performing a second one. Without that, "transient" means "charge
them twice", and the caller who follows your contract is the one who gets hurt.

## Let the differ decide, not your reading

The question of whether an edit breaks a caller is mechanically decidable in most
ecosystems, and the tools exist: `oasdiff` for OpenAPI, with a flag that fails a
build on a breaking-level change, `buf breaking` for Protocol Buffers,
`cargo-semver-checks` and `cargo-public-api` for Rust, `apidiff` for Go, japicmp and
revapi for compiled Java. Run one against the previous release, and read what it
says rather than deciding for yourself.

Two reasons this beats judgement. The first is that the taxonomy above is longer than
anyone holds in their head, and the cases people miss are exactly the ones a differ is
written to catch. The second is that a version number carries no information: a
systematic review of breaking changes in software ecosystems, published in May 2026,
reports from its primary studies that 67% of Maven artifacts have shipped at least
one semantic versioning violation over their history. The bump is a claim. The diff
is evidence.

Where no differ exists for your surface, make one. Commit a generated listing of the
public surface as a file in the repository, regenerate it in the build, and let the
review show the change. A pull request that alters that file and says nothing about
compatibility is a question the reviewer now knows to ask.

## Green proves only the callers in this checkout

This is where an agent goes wrong, and it goes wrong invisibly. Asked to change a
function's signature, the obvious move is to change it and fix everything that no
longer compiles, and the run comes back green. What that proves is that the callers
inside this working copy were updated. It says nothing at all about callers outside
it, and a checkout is all you can see.

So before changing anything, say how far it reaches, and let that pick the move:

| Reach | What to do |
|---|---|
| Private to a file or package | Change it. The compiler is the whole audit. |
| Exported, callers all in this repository | Change it and update them in the same commit. Say how many you changed. |
| Published: another repository, a released library, a running client, a stored record | You cannot see the callers. Add rather than change. |

The additive move, when you cannot see who is on the other side: introduce the new
name or shape beside the old one, make the old one delegate to it so there is a
single implementation, mark the old one deprecated with a date and a pointer to the
replacement, and leave it working. Removing it later is a separate change with its
own evidence standard, and the `deletion` skill owns that step.

This section is reasoning rather than a measured finding, and it is worth saying so.
Nothing published measures how often an agent treats the callers it can see as all of
them. The rule follows from what a checkout is.

## Design the first version so the second is additive

Most breaking changes are paid for at design time, by a decision that left no room.
The ones worth spending thought on:

- Return an object, never a bare array at the top level. An array has nowhere to put
  the next field, and paging, totals, and warnings are all next fields.
- Give an enum an explicit unspecified value and say in the documentation that values
  will be added. That is the difference between consumers writing a default branch
  and consumers writing an exhaustive switch you cannot extend.
- Keep the shape you accept separate from the shape you return, even when they start
  out identical. They diverge the first time you add a server-generated field, and by
  then they are one type used by both.
- Make identifiers opaque. The moment a caller can parse structure out of an id, its
  format is a contract, and you cannot change how ids are issued.
- Page with an opaque cursor rather than an offset. An offset promises a stable order
  and a stable count, which are two promises nobody intended to make.
- Never reuse a name, a field number, or a code for a different meaning. Reserve it
  instead. Old callers and stored records still carry the old meaning, and a reused
  name makes them wrong rather than absent.
- Publish the least you can. A field returned because it happened to be in the struct
  is a promise acquired by accident.

## What to say when you hand it over

Four things, and they are short:

Which direction changed, and which row of the table it falls in. "Added an optional
request field, loosening what we accept" is the whole compatibility argument for that
change, and it is checkable.

What the differ said. Name the tool and paste the verdict. If none was available, say
which surface listing you regenerated instead.

The reach, and for an exported change, how many callers you updated and where you
looked for them. An unknown caller count is a fact worth reporting, not a gap to fill
with an assumption.

For anything breaking, the migration a caller has to perform, in the imperative, with
the date the old path stops working. A deprecation with no date is an intention.

## Refusals

- No breaking change to a published surface without a version bump or a stated
  migration and a date for existing callers.
- No new required field on a request that already has callers, and no new case in a
  closed set that you return.
- No payload rejected, and no parse failed, because it carried a field you do not
  read.
- No error that leaves a caller unable to tell whether retrying can help, and no
  branching on the text of a message to work it out.
- No failure reported as retryable on a write that is not idempotent.
- No claim that a change is compatible on the strength of a green build in this
  checkout, when the surface is published beyond it.
- No name, field number, or error code reused for a new meaning after it has been
  retired.
- No breaking change carried in a commit that was about something else.
