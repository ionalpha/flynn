---
name: dependencies
description: Use when adding, choosing, pinning, upgrading or auditing a third-party package, library, SDK, action, or base image, and when deciding whether to write the thing yourself instead. Covers what a dependency costs across its whole life rather than at the install, which questions decide whether to take it, reading the licence before the code, who gets to choose the version and when, what a lockfile does and does not promise, upgrading so the breakage arrives in the test run rather than in production, and what to do when a package is abandoned. Carries one rule above the others: a package name or version written from memory is a guess, and it is resolved against the registry before it lands in a manifest.
metadata:
  flynnhq.com/title: Dependencies
  flynnhq.com/tags: '["dependencies","supply-chain","upgrades","licensing","security"]'
---

# Dependencies

## A name or version you did not look up is a guess

Two failures belong to whoever writes the manifest line, and both happen long before
any install-time protection runs.

The first is a package that does not exist. Across roughly 200,000 prompts, current
models invented package names in about 5 percent of answers, and 127 of those
invented names were produced identically by every model tested. Dozens of them were
still free for anyone to register. A name that several models agree on and no
registry holds is the most attractive target in the ecosystem, and no cooldown,
lockfile check or scanner protects you, because the package you asked for is the
attacker's package.

The second is a version recalled rather than read. Models attach explicit version
numbers most of the time, and in one benchmark between a third and a half of the
resulting dependency sets carried a known vulnerability, the great majority of which
had been published before the model's own training cut-off. The recalled version is
not random: it is the one that was common when the training data was collected, which
is exactly the population that has had years to accumulate advisories.

So the rule is mechanical. Before a name or a version enters a manifest, resolve it:
ask the registry what exists, what the current version is, and when it was published.
Write down what you read. If you cannot reach the registry, say that the version is
unverified rather than writing one that looks right.

## Price it as a subscription, not a purchase

Adding a dependency is not a one-off cost that ends when the install succeeds. You
will pay again, on a schedule you do not control:

| When you pay | What it costs |
|---|---|
| Every install | Its transitive tree, in bytes, build time, and things that can go wrong. |
| Every upgrade | Reading a changelog, running the suite, fixing what moved. |
| Every advisory | Someone reads it, decides whether you are affected, and acts within a deadline. |
| Every adjacent upgrade | Its constraints on your language runtime and its peers become yours. |
| When it is abandoned | You inherit the maintenance, or you do the removal you avoided at the start. |
| When you leave | Every place that learned its shape has to be rewritten. |

The only moment you can negotiate any of this is before it goes in. Afterwards, the
bill arrives whether or not you use the library more than once.

This is why the count of transitive dependencies is worth reading before the feature
list. A package with forty dependencies of its own is forty subscriptions, taken on
the recommendation of an author who was not thinking about your program.

## Deciding to take it: two questions that settle most cases

**What would you write instead, exactly?** Not "a date library", but the specific
function this program needs from it. If the honest answer is a few dozen lines you
could test in an afternoon, write them, because the subscription costs more than the
code. If the honest answer is a parser, a cryptographic primitive, a protocol
implementation, or anything where being subtly wrong is worse than being absent,
take the dependency and do not improvise. The line is not size, it is whether being
wrong is detectable by your own tests.

**Who maintains it, and what happens when they stop?** Look at what the project has
done in the last year rather than at its total popularity: releases, the age of the
oldest open bug that matters to you, whether one person holds the publish rights.
Then answer the question that decides the risk, which is what you would do if it
stopped tomorrow. Fork and carry it, replace it with a competitor, or write the
subset you use. If none of the three has an answer, you are taking a dependency you
cannot get out of, and that is worth knowing at the moment you add it rather than at
the moment it matters.

Record both answers where the dependency is declared or in the change that adds it. A
dependency that arrives with no recorded reason is one nobody can weigh later: it
gets ripped out by someone who could not tell what needed it, or kept for years
because nobody dared find out.

## Read the licence before the code

An unread licence is a refusal. Find the actual licence file in the actual version
you are installing, rather than the identifier a registry page displays, because the
two disagree often enough to matter and only one of them is the agreement.

Three things decide what happens next. What the licence requires when you distribute
the program, which is where copyleft terms bite and where the answer differs between
a service you host and a binary you ship. What it requires when you merely use it,
which for most permissive terms is attribution and nothing else. Whether the project
changed its licence at some version, which happens, and means the version you pin is
part of the legal answer.

The same rule covers copying: code adapted from a source whose licence you have not
read does not go into the tree. Attribution obligations survive copying, and they
survive rewording.

## Who chooses the version, and when

A version range is a delegation. Under a resolver that picks the newest match, a
range says the publisher decides which version you run and gets to make that decision
after you have stopped paying attention. Under a resolver that selects the minimum
version satisfying every requirement, the same range is a floor and nothing more, and
new releases change nothing until someone asks for them.

So find out which of those your ecosystem does before arguing about pins. Then choose
deliberately: hand the choice to the publisher when the cost of being a version
behind exceeds the cost of an unreviewed change, and keep it when it does not.

A lockfile records the resolution, and that is all it does. It makes today's install
reproduce tomorrow, so two machines build the same program and a bisect means
something. It does not say the pinned versions are safe, or current, or that anyone
looked at them. Commit it, install from it in CI with the flag that refuses to update
it, and never edit one by hand: a lockfile edited by anything other than the tool
that owns it is a claim about hashes nobody verified.

## Installing runs code, and the question is who vouches

In most ecosystems, installing a package executes that package's code on the machine
doing the install, with that machine's privileges and its credentials in the
environment. This is not theoretical: a worm spread through exactly that mechanism
across hundreds of packages in September 2025, using each victim's publishing
credentials to infect the next.

Two defaults follow. Install with lifecycle scripts disabled unless a specific
package needs them, and grant the exception by name. Do the install where a
compromise is contained rather than on the machine holding your keys, which for a
run means the sandbox, not the developer's session.

Signatures are worth demanding, and worth understanding precisely. A verified
signature tells you which artefact you got and who built it. It tells you nothing
about what that artefact does, and a correctly signed compromised release is the
normal shape of this attack rather than an exotic one. Verification narrows who you
are trusting; it does not remove the trust.

That is why the identity you pin matters more than the fact of signing. Pin the thing
that actually built the artefact, the release workflow and its issuer, rather than a
name that can be transferred, renamed, or handed to a new owner.

## Upgrade so the breakage lands in the test run

The order is: read what changed, run against the new version, then commit the
manifest. Doing it the other way round means the lockfile change and the discovery of
what broke are in the same commit, and it is why upgrades get reverted wholesale
instead of fixed.

Read the changelog for the versions you are crossing, not just the newest one, and
read it for what was removed and what changed behaviour, which is where the breakage
lives. Then run the suite against the new version before the manifest change lands.
If it is green, commit the manifest and the lockfile together with the range of
versions crossed in the message.

When something fails, stop batching. One package at a time is slow and it is the only
way to know which one did it. Cross major versions one at a time as well, since the
migration notes are written for each step and not for the jump.

Two upgrades deserve different handling. A security patch is not a discretionary
upgrade: take the smallest version step that clears the advisory, and if it does not
apply to how you call the library, say so with the reason rather than skipping
silently. A major framework upgrade is a project, not a chore, and it belongs in its
own change with its own plan.

## Abandonment is the cost nobody budgets

A dependency that has stopped moving is not stable. It is a maintenance obligation
that has been transferred to you without a notification. The signals worth acting on
are a security advisory nobody is fixing, a runtime version it will not support, and
maintainers who have said they are done.

Decide once, explicitly, between three answers: pin it and carry it knowingly with a
note saying so, vendor the subset you use into your own tree where your tests cover
it, or replace it. What is not an answer is leaving it and hoping, because the
decision then gets made for you by whichever advisory arrives first.

Vendoring is a real option and it is underused. Copying two hundred lines you
understand, with the licence header intact and a comment saying where it came from,
is often cheaper than an unmaintained subscription and always cheaper than an
emergency.

## What to report when you add or upgrade one

State the things a reviewer cannot get from the diff:

- The name and version you resolved, and that you read it today rather than recalled
  it.
- Why this package rather than writing it, in one sentence.
- The licence, by identifier, and where you read it.
- How many dependencies came with it.
- For an upgrade: the versions crossed, what the changelog said would break, and that
  the suite ran green against the new version before the manifest changed.

## Refusals

- No package name or version in a manifest that was not resolved against the registry
  in this session.
- No dependency added without a recorded reason and an identified licence.
- No code copied from a source whose licence has not been read.
- No lockfile edited by hand, and no install in CI that is allowed to update one.
- No upgrade committed before the suite has run against the new version.
- No claim that a signature makes a package safe.
- No dependency added to avoid writing a function whose correctness your own tests
  would catch.
