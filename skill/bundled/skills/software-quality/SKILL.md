---
name: software-quality
description: Use when someone asks for the quality skill by name or for work held to a high standard, at the beginning of an engineering task to decide what it owes before it is finished, before saying a change is done, finished or ready to merge, when reviewing your own diff before handing it over, and when unsure which engineering skill applies. Carries the gates every diff must clear in any language or framework: evidence for each claim, an account of what was not checked, checks run on the build that is released, failures that never look like success, the smallest diff, and no obsolete code kept. Condenses the central rule of each engineering skill in the pack and tells which to load when.
metadata:
  flynnhq.com/title: Software quality
  flynnhq.com/tags: '["quality","code quality","quality skill","self-review","definition of done","ready to merge","engineering standards"]'
---

# Software quality

Load this at the start of a change and again before you call it finished. At the
start it tells you what the change owes and which skill covers each part of it. At
the end it is the check you run on your own work before anyone else sees it.

Every rule here names the failure it prevents. Those failures are the ones that recur
in code written quickly, by a person or a model, and most of them share one shape:
something reported as working that nothing showed to work.

## At the start: what this change owes

Before the first edit, write down four things. They take a minute, and each one
changes what you do next.

1. **The request in one sentence**, in the words of whoever asked. Every file you
   touch should trace back to it.
2. **What finished looks like, as a check that can fail.** A command, a test, a
   request with an expected response, a number before and after. If you cannot name
   one, the work is not yet understood well enough to start.
3. **How far the change reaches**: private to one file, visible across the package,
   called by the wider program, published to people outside this repository, or
   stored data. The further it reaches, the more evidence every later step needs.
4. **Which skills apply**, from the table further down. Load them now. Their rules
   apply from the first edit, and a skill read at the end can only find faults in work
   already done.

## The gates

Each gate is a question you can answer with something you ran or a line you can
quote. An answer from memory does not pass.

**1. Every claim carries its evidence.** For each thing you say works, name the
command and what it printed. Mark everything else in those words: *read* (you looked
at the code but did not run it) or *assumed* (you expect it and did not observe it).
Read the output, not the exit code. A green run that executed zero tests, served a
cached result, skipped the case or never reached the assertion is not a pass. Treat
a passing suite you can see as weak evidence: agents write code that satisfies the
tests in front of them, and on larger changes the gap to tests they cannot see grows
large. So test what the specification implies and nobody listed, above all the
rejection of invalid input and features used together.

**2. Say what you did not check.** End the report with what did not run and why: the
platform you could not reach, the integration with no fixture, the migration never
applied to an empty database, the path that only runs in production. A reader can
weigh a gap they were told about. A gap nobody mentions reads as covered.

**3. Check what users get, where they get it.** Name the target of every check: which
build output, which database, which operating system, which environment, which
packages. Then confirm it is the one that ships. A green check on the wrong target is
worse than none, because it ends the search. The usual forms: a test against a
development database that differs from production; a stale build artifact served
while a fresh one passes locally; a comparison where only one side was rebuilt; a
path checked from the wrong host or operating system; a local lint run that differs
from the one the pipeline runs. Run the command the pipeline runs before you say
done; if you cannot, say so.

**4. A failure is loud.** For every catch, default, fallback, retry and skipped
record you write, say who finds out when it fires and how soon. If the answer is
nobody, return the error or record it where someone looks. A fallback that produces
a plausible result turns a failure you would see in a minute into one found weeks
later, far from its cause. The same family: an error count in a reply that nobody
reads, a success status that discarded the payload, a timeout recorded as a verdict,
a cached failure with no way to clear, human-formatted output parsed as data, and an
error matched by its message text. Fail at once on a bug; give a defined, named
response to bad input and to a dependency that is down.

**5. Prove it is reached.** Before saying code protects, validates, records or shows
anything, name the call site that reaches it, or the test that does. A signature
check written and never called is worse than none, because everyone believes it is
there. Every new write needs a reader in the same change, and every new input needs
a test that it arrives where it is used.

**6. One path for each job.** Before adding a function, endpoint, store, renderer or
table, search for the one that already does nearly this and extend it. A second route
to the same result needs a stated reason, because parallel paths drift apart and each
later fix reaches only one of them. When you change what something means, list every
reader of it before calling the change finished. When you convert a pattern that
appears in several places, search for all of them first and say how many you found.

**7. Read before you write.** Open every function, type, flag and package you call at
its definition, in the version installed here. A name you remember is a guess. Models
invent calls to code local to a project far more often than calls to well-known
libraries, and they suggest packages that do not exist, so a recalled name is the
least reliable thing in the change.

**8. Witness the fault before fixing it, and see the new test fail.** A failing test,
a reproduced symptom or a log line exists before the first edit, and the fix is
checked against that same witness. A test that passed on its first run has not been
shown to test anything. Work its expected value out from the specification, not from
what the code currently returns. Assert on outcomes; a test that only prints a value
proves nothing. When a test was written after the code, show that it can fail: run it
against the code before the change, or break the line it covers from a committed
checkpoint, see it fail, and undo the break in the editor. Never undo such a break
with a version control command that overwrites the file, because that also discards
any uncommitted work beside it.

**9. The smallest change that does the job.** List the files you touched and tie each
to the request. A problem you notice on the way goes in the report as a finding, not
into the diff. Keep a restructuring and a behaviour change in separate commits, so
each can be reviewed and reverted on its own. Do not add a parameter, option, layer or
abstraction for a need nobody has stated yet. Large diffs get worse review and are
rejected more often, whoever wrote them.

**10. Remove what the change made obsolete.** Ask what this change replaces, then
remove the old path, its tests, its configuration and every comment and document that
describes it, in the same change. A comment describing behaviour that no longer
exists misleads more than no comment. When you fix a rule that wrote bad data, the
fix also repairs the records the old rule wrote, or says why it does not.

One refusal sits above the gates. Never edit, weaken, skip or delete a test, an
assertion, a threshold or a check to get a run to pass, and never special-case the
input a test uses. Agents take this shortcut often, and an instruction not to has
done little to stop them, so the rule is absolute. If a check is wrong, leave it
failing, say why in the report, and fix the check as its own change.

## What fast-written code gets wrong

These defect classes recur in every codebase with a history, and each has a check
that takes minutes. Go through the ones your change touches.

- **Data shape.** Run the code on the shapes real data has: a null where the type
  says there is none, an empty collection, a duplicate, two rows tied on a sort key, a
  child that arrives before its parent, a file in another encoding, a deleted
  record still referenced, every sort and filter option a query accepts, and the
  invalid input it must reject. Most data defects are a test that used a tidier
  shape than production has.
- **Lifecycle.** Everything started is stopped on every exit path: success, error,
  cancellation and timeout. Timers, subscriptions, streams, child processes, file
  handles and goroutines or threads. Stopping a parent stops its children. A
  watchdog runs on its own clock, not on the output it watches.
- **Identity.** Who or what an action is for is passed explicitly. Defects follow
  when it is taken from ambient state: the current directory, a path string, an
  in-memory map a restart empties, the first row of an unordered query, the address
  of the proxy in front of you, or whichever session happens to be active.
- **Partial failure.** The process can stop between any two lines. After each step,
  ask what state a crash there leaves, whether a retry repeats a write that already
  happened, whether an operation that was sent but never confirmed is reported as
  done, and whether cleanup still runs once its context is cancelled. Validate before
  an irreversible step, never after it. A read error is not proof of absence.
- **Destruction.** A delete or overwrite checks its invariant at the entry point, and
  any confirmation states the real reach ("delete this item and its 14 children").
  A tool that can put state into a bad shape ships with the way back out.
- **Bounds.** Every cache, queue, buffer, log, retry loop, wait and result set has a
  limit, and the behaviour at the limit is defined. Every call across a process or
  network boundary has a timeout.
- **Unrequested work.** New behaviour that runs on a timer, in the background or on
  startup, or that spends money, is off until someone turns it on, and every entry
  point checks the same switch.
- **Security relevance.** Any change that touches input from outside, authentication,
  authorisation, files, queries, shell commands, templates or outbound network calls
  is a security change, whatever the request said. Load `untrusted-input`.

## The rule each skill carries, and when to load it

The rule is the one to remember if you read nothing else. Load the skill for the
method behind it.

| When | The rule | Load |
|---|---|---|
| Before editing code you have not read, or tracing how a behaviour is produced | No edit rests on a recalled name: open each symbol at its definition, trace one real input to its effect, list who else observes the state you change, and mark each claim read, ran or assumed. | `reading-unfamiliar-code` |
| A failure whose cause is not proven | Nothing changes until something fails in front of you. Fix where the wrong value is born, not where it surfaced. After three fixes that each revealed a new problem, stop and report the design question. | `systematic-debugging` |
| New or changed behaviour | The red run must print a disagreement in readable values from an assertion that was reached. The expected value comes from outside the code. The test file does not change between red and green. | `test-first` |
| Removing anything | The evidence a removal needs grows with its reach, and a search with no hits proves absence in this tree only. A removal lands as its own change. | `deletion` |
| Changing something other code or users depend on | Loosen what you accept, never weaken what you return. Let a compatibility checker decide instead of reading the diff. A green build proves only the callers in this checkout. | `contract-design` |
| A function or type that is awkward to call or keeps gaining options | Count the facts a caller must hold. A new layer must finish the sentence "a caller no longer has to know ___", or it is a rename. | `interface-depth` |
| Deciding where code goes, or whether to add a module, package or layer | Put the change where the code that changes with it already lives. A boundary exists only when crossing it fails a check. | `structural-boundaries` |
| Naming a thing, or one concept going by several words | Search for the word the project already uses before inventing one, and say which words you tried. A name that no longer describes the behaviour is a defect. | `domain-language` |
| Two code paths that can touch the same state | First say what is shared and what orders the accesses. A concurrency fix ships with a test that fails without it. A write carries what its read saw. | `shared-state` |
| Calling a system you do not run | No call written from memory: read the current documentation for your version and keep one real response. Their payloads stop at one adapter. An unclassified failure is not retried. | `external-integration` |
| A value from outside the program, or a credential | Only a conversion that can fail raises trust, and it must run on the value actually used. A secret is a type with one exit. Where trust cannot be established, remove the ability to act. | `untrusted-input` |
| Adding, upgrading or copying in third-party code | Resolve every name and version against the registry before it lands in a manifest. Read the licence first. Record where copied code came from. | `dependencies` |
| Making something faster, cheaper or smaller | The deliverable is a before and after taken the same way, with a difference that clears the run-to-run spread. Name the share of the total the target holds before touching it. | `performance-measurement` |

## When two rules seem to disagree

- **Unknown fields.** Refuse unknown fields in input your own callers send you; that is
  where a typo or an attack hides. Ignore unknown fields in a response from a service
  you depend on, so its next additive release does not break you. The rule depends on
  the direction the data travels.
- **Narrow or full checks.** Run the narrowest check that covers your change while you
  work, because it is fast and its failures are yours. Run the full gate the pipeline
  runs before you call it finished. If the full gate is red in code you did not touch,
  record where, do not fix it in this change, and say which of your claims it leaves
  unproven.
- **Test before or after.** The evidence supports one narrow rule and not the strict
  ritual: a test exists that was seen to fail for the stated reason and then pass.
  Writing it first is the cheapest way to see that, which is why `test-first`
  defaults to it.
- **When to stop.** Three attempts at one problem that each uncover a new one means
  the approach is wrong. Stop, and report what you tried and what each attempt
  revealed, rather than stacking a fourth patch.

## Before you hand it over

Reread the diff as a reviewer who did not write it. Rereading alone finds little:
models rarely correct their own reasoning without outside feedback, and get better
only when something external answers. So answer each line below with a command you
ran or a line you can quote.

- Every changed line traces to the request (gate 9), and nothing the change made
  obsolete remains (gate 10).
- Every new error path reaches a person or a log someone reads (gate 4), and every new
  check has a caller (gate 5).
- Each new test was seen to fail (gate 8), and no existing test, assertion or
  threshold was weakened to pass.
- The defect classes above that this change touches were each checked.
- The full gate the pipeline runs passed on this change, or the report says why not.

A diff too large to read in one sitting is too large to review. Split it.

## What the report says

Lead with the result, then:

- **Changed**: what changed and why, one line per file or group of files.
- **Ran**: each command, with the part of its output that shows the result.
- **Not checked**: what did not run, and why.
- **Assumed**: what you believe and did not observe.
- **Found on the way**: problems outside the request, left as findings for someone to
  pick up.

## Refusals

- No claim of done, fixed, safe or faster without the command that showed it.
- No test, assertion, threshold or check weakened, skipped or deleted to get a pass.
- No silent fallback, swallowed error or default that hides a missing value.
- No second implementation of something that already exists, without a stated reason.
- No call, flag or package name written from memory.
- No unrelated edits folded into the change.

The reasons and sources behind each gate, and the popular advice left out on purpose,
are in [references/evidence.md](references/evidence.md).
