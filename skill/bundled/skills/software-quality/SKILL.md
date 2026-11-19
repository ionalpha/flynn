---
name: software-quality
description: Use at the start of any engineering change to decide what it owes, and again before saying it is done, fixed, safe, ready to merge or ready for review. Also when someone asks for the quality skill or for work held to a high standard, when reviewing your own diff before handing it over, and when unsure which engineering skill applies. Gives the gates every change must pass in any language: a check that can fail, evidence for each claim, checks run on what users get, no failure that passes as success, code proven reachable, the smallest diff, nothing obsolete left behind. Says which gates a host such as Flynn enforces and which stay with you, and which narrower skill to load.
metadata:
  flynnhq.com/title: Software quality
  flynnhq.com/tags: '["quality","code quality","quality skill","self-review","definition of done","ready to merge","engineering standards"]'
---

# Software quality

Load this before the first edit and again before you report. Answer every gate with
a command you ran or a line you can quote; an answer from memory does not pass.
Scale the effort to how far the change reaches: a one-file fix owes a sentence per
gate, a change to stored data or a published interface owes each gate in full.

## Before the first edit

1. **The request in one sentence**, in the words of whoever asked. Every file you
   touch traces back to it.
2. **A check that can fail**: a command, a test, a request and its expected response,
   or a number before and after. Run it now and see it fail, or reproduce the
   symptom. If no such check exists, writing one is the first step of the work.
3. **How far the change reaches**: one file, one package, the whole program, people
   outside the repository, or stored data. Each step outward raises the evidence
   every later gate needs.
4. **Which skills apply**, from the table at the end. Load them now. A skill read at
   the end can only find faults in work already done.

## The gates

**1. Evidence for every claim.** For each thing you say works, name the command and
the line of output that shows it. Label everything else *read* (you looked at the
code) or *assumed* (you expect it and did not observe it). Read the output, not the
exit code: a run that executed zero tests, served a cached result or skipped the
case exits 0. Passing visible tests is weak evidence, because agents fit the tests
in front of them, so also test what the specification implies and nobody listed:
that invalid input is rejected, and that features work together.

**2. A test seen to fail.** A test that passed on its first run has not been shown to
test anything. Take the expected value from the specification, not from what the
code returns, and assert on outcomes. For a test written after the code, break the
line it covers, watch the test fail, then restore the line by editing it. A
version-control checkout would also discard uncommitted work beside it.

**3. Checked where users get it.** Name the target of each check (build output,
database, operating system, environment) and confirm it is the one that ships. A
green check on the wrong target ends the search. The usual forms: a stale artifact
served while a fresh build passes, a development database that differs from
production, a comparison where one side was not rebuilt, a local lint that differs
from the pipeline's. Iterate on the narrowest check, then run the command the
pipeline runs before you report. If that is red in code you did not touch, record
where, and say which of your claims it leaves unproven.

**4. No failure passes as success.** For every catch, default, fallback, retry and
skipped record you write, say who learns that it fired. If nobody does, return the
error or record it where someone reads it. A plausible fallback value looks like a
correct one, so nothing prompts anyone to look. A check that cannot decide refuses:
"could not evaluate" never becomes false, allowed or none found, and a partial or
page-capped result is never reported as complete. Match errors by type or code,
never by message text, and never parse output written for people.

**5. Proven reachable.** Before saying code validates, guards, records or shows
anything, name the call site or the test that reaches it. A guard written and never
called is worse than none, because everyone believes it is there. A new option or
flag reaches every path that should honour it, including resume, retry and the other
entry points. Every new write has a reader in the same change.

**6. One path for each job.** Before adding a function, endpoint, store or table,
search for the one that already does nearly this and extend it. Parallel paths drift,
and each later fix reaches only one of them. When you change what something means,
list every reader of it. When you convert a repeated pattern, find every copy first
and say how many you found.

**7. Read before you call.** Open each project-local function, type and flag at its
definition, and each library call whose behaviour you are recalling rather than
reading, in the version installed here. Models get rarely seen APIs wrong far more
often than common ones, and code local to one project is the rarest there is.
Resolve every new package name against its registry before it enters a manifest.

**8. The smallest change that does the job.** Tie each touched file to the request.
A problem found on the way goes in the report, not the diff. Keep restructuring and
behaviour changes in separate commits, so each can be reviewed and reverted alone.
Add no option, wrapper or abstraction for a need nobody has stated.

**9. Nothing obsolete left.** Remove what the change replaced: the old path, its
tests, its configuration, and every comment and document that describes it. When you
fix a rule that wrote bad data, repair the records it wrote or say why not.

Never edit, skip, weaken or delete a test, assertion, threshold or check to get a
pass, and never special-case the input a test uses. If a check is wrong, leave it
failing, say why in the report, and fix it as its own change.

When three attempts at one problem each uncover a new problem, stop and report what
each attempt revealed instead of adding a fourth patch.

## Defects that recur

Go through the ones your change touches; each takes a test or a search. The first is
the largest class in the fix history this list was checked against.

- **Trust checks that fail open.** A sandbox, authentication or allow-list check that
  admits when its input is missing, empty or unreadable. Test the missing case as
  well as the wrong one. For input from outside the program, load `untrusted-input`.
- **Data forms.** Null where the type says none, empty collections, duplicates, rows
  tied on a sort key, a child before its parent, another encoding, timestamps sorted
  as text, a deleted record still referenced, every sort and filter option a query
  accepts.
- **Platform.** Paths, line endings, case sensitivity, permissions and process APIs
  differ by operating system. Check each one the change ships to, or say which you
  did not.
- **Concurrency and lifecycle.** Everything started is stopped on success, error,
  cancellation and timeout: timers, subscriptions, streams, child processes, handles,
  goroutines and threads. A watchdog runs on its own clock. Two paths touching the
  same state: load `shared-state`.
- **Identity.** Who or what an action is for is passed explicitly, never taken from
  the current directory, a path string, an in-memory map a restart empties, the first
  row of an unordered query, or whichever session is active.
- **Partial failure.** The process can stop between any two lines. Ask what state a
  stop there leaves, whether a retry repeats a write, and whether a request sent but
  never confirmed is reported as done. Validate before an irreversible step.
- **Bounds.** Every cache, queue, buffer, retry loop, wait and result set has a limit
  with defined behaviour at the limit. Every call across a process or network
  boundary has a timeout.
- **Destruction and unrequested work.** A delete states its full reach ("this item
  and its 14 children") and checks its invariant at the entry point. Work that runs on
  a timer, at startup or spends money is off until someone turns it on.

## What the host enforces

A rule in a prompt is weak protection; a check the agent cannot edit is stronger.
Find out which gates your host enforces and spend your effort on the rest.

In Flynn:

- `flynn goal` writes a plan ledger before building. The host runs each item's check
  on the untouched tree and sends back any check that already passes, does not parse
  or calls a program the host lacks. The red half of gate 2 is enforced there.
- With `--require-proof`, and always for a fan-out unit, the goal reports success
  only over items whose check the host ran and recorded as passing. Cite that record
  instead of pasting output.
- A check is fixed once planned; it cannot be argued for or rewritten at claim time.
- Three steps without progress, or the same refusal twice, stop the run.
- Every command and its output is on the run's record (`flynn spine`), so the Ran
  section of your report can be checked against it.

A plain conversation or `flynn run` has none of these, and every gate is yours. No
host checks these yet: that the passing run executed at least one test, that no
test, assertion or skip marker was weakened during the build, and that the check ran
on the target users get.

## The report

Before writing it, reread the diff as a reviewer and answer each gate with something
you ran. Rereading without running anything finds little; models seldom repair their
own code without outside feedback such as a failing test. Then lead with the result:

- **Changed**: what and why, one line per file or group of files.
- **Ran**: each command, with the part of its output that shows the result.
- **Not checked**: what did not run and why. A gap nobody mentions reads as covered.
- **Assumed**: what you believe and did not observe.
- **Found on the way**: problems outside the request, left for someone to pick up.

## Which skill to load

| When | The rule to keep if you load nothing else | Load |
|---|---|---|
| Editing code you have not read | Open each symbol at its definition and trace one concrete input to its effect. | `reading-unfamiliar-code` |
| A failure whose cause is unproven | Change nothing until it fails in front of you; fix where the wrong value is born. | `systematic-debugging` |
| New or changed behaviour | The red run prints a readable disagreement from an assertion that was reached. | `test-first` |
| Removing anything | A search with no hits proves absence in this tree only; a removal is its own change. | `deletion` |
| Changing what others depend on | Accept more, never return less, and let a compatibility checker decide. | `contract-design` |
| An awkward function or type | A new wrapper must finish "a caller no longer has to know ___". | `interface-depth` |
| Where code goes | Put it beside the code that changes with it; a boundary exists only when crossing it fails a check. | `structural-boundaries` |
| Naming | Search for the word the project already uses before inventing one. | `domain-language` |
| Shared state | Say what is shared and what orders the accesses; the fix ships with a test that fails without it. | `shared-state` |
| A system you do not run | Read current documentation for your version and keep one recorded response. | `external-integration` |
| Input from outside, or a credential | Only a conversion that can fail raises trust, on the value the code then uses. | `untrusted-input` |
| Third-party code | Resolve every name and version against the registry; read the licence first. | `dependencies` |
| Faster, cheaper, smaller | A before and after taken the same way, with a difference larger than the run-to-run spread. | `performance-measurement` |

The evidence behind each gate, what is not proven, and the advice left out on
purpose are in [references/evidence.md](references/evidence.md).
