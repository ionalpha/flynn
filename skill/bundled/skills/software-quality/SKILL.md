---
name: software-quality
description: Use before any software change, in any language or stack, and again before saying it is done, fixed, safe, ready to merge or ready for review. Also when asked for the quality skill or for work held to a high standard, when reviewing your own diff before handing it over, and when unsure which engineering skill applies. Covers what to establish before the first edit, the gates every change passes, the defects to check for, what the harness should enforce, the report, and which narrower skill to load.
metadata:
  flynnhq.com/title: Software quality
  flynnhq.com/tags: '["quality","code quality","quality skill","self-review","definition of done","ready to merge","engineering standards"]'
---

# Software quality

Load this before the first edit and again before you report. Answer each gate with a command you ran or a line you can quote. Scale the effort to the change's reach: a one-file fix owes a sentence per gate; a change to stored data or a published interface owes each gate in full.

## Before the first edit

1. **The request**, in one sentence, in the requester's words. Every file you touch traces back to it.
2. **The project's own commands.** Read the repository's instruction and contributor files and its pipeline configuration for how it builds, tests and lints, and use those commands.
3. **A check that can fail**: a command, a test, a request with its expected response, or a number before and after. For a reported bug, reproduce it first. If it does not reproduce, change nothing and report that. Time one run; if it is slow, unclear, manual or flaky, load `feedback-loop`.
4. **The tests that cover the code you will change.** Find them and run them now, so a regression shows against a known result.
5. **Everything the change touches**, found by search, even in code you think you know: every caller, every reader of the data, every configuration, migration and stored or serialised form, and every test that covers it. Write how you found each one. A search is a lower bound: name the routes it cannot see (reflection, lookups by string, generated code, other repositories, stored data). The list sets how far the change reaches: one file, one module, the whole program, other systems or people, or stored data.
6. **The skills that apply**, from the table at the end. Load them now.

## The gates

1. **Evidence for every claim.** For each thing you say works, name the command and the output line that shows it. Mark everything else *read* (you looked at the code) or *assumed* (you did not observe it), and check an assumed fact before building on it. Read the output, not the exit code: zero tests run, a cached result and a skipped case all exit 0.

2. **A test that fails for the right reason.** For a change in behaviour: on the code before your change, the new test fails on its assertion, with a message that states the disagreement. A test that passes there, or fails on a build or setup error, proves nothing. Take expected values from the specification, never from what the code returns. Also test what the specification implies and the visible tests do not cover: invalid input rejected, values at a limit, features used together. For a test written after the code, run it against a clean copy of the previous version; never break and restore lines in the working copy. A change that keeps behaviour (a restructuring, a removal of dead code, a rename) adds no test of its own: the existing tests pass before and after with no edit to them, and the report says which tests cover the code it moved.

3. **The check matches the claim, on what ships.** Name what each check exercises and confirm it is the property you claim, on the code, build, database, configuration and platform users get. Rebuild both sides of any comparison. Iterate on the narrowest check, then run the pipeline's own command before you report. If that fails in code you did not touch, say where, and which claims it leaves unproven.

4. **No failure passes as success.** For every catch, default, fallback, retry and skipped record, name who learns it fired; if nobody does, return the error or log it where someone reads it. A check that cannot decide reports that, never false, allowed or none found. Report partial or truncated results as partial. Handle each expected error by name, including a limit being exceeded. Treat an unknown error as final, not retryable. Match errors by type or code, never by message text.

5. **Proven reachable.** Before saying code validates, guards, records or displays anything, name the call site or test that reaches it. A new option reaches every path that should honour it, including retry, resume and every entry point. Every new write has a reader in the same change.

6. **One path per job.** Before adding a function, endpoint, store or table, find the one that nearly does it and extend that. When you change what a value means, list every reader. When you convert a repeated pattern, find every copy first and give the count.

7. **Facts from the source.** Open each project function, type and setting at its definition before you call or change it. Check library behaviour against the installed version. For a new dependency, confirm the name in its registry and take the version from there. For an external service, send one real request and keep the response as a fixture.

8. **The smallest change that does the job.** Tie each touched file to the request; the tests from gate 2 are part of it. Put problems found on the way in the report, not the diff. Keep restructuring and behaviour changes in separate commits. Add no option, layer, wrapper or abstraction nobody asked for. A script or fixture you built to check your own work leaves the diff before you report, unless the project will run it again; a kept one goes in its own commit and under Changed in the report. Configuration and generated files are code and get the same checks.

9. **Nothing obsolete left.** Remove what the change replaced: the old path, its tests, its configuration, and every comment and document describing it. When you fix logic that wrote bad data, repair the data or say why not.

10. **Checks are not yours to change.** Never edit, skip, weaken or delete a test, assertion, threshold or check to get a pass, and never special-case the input a test uses. A failing check is never obsolete. If a check contradicts the request, or the task cannot be done as specified, stop and say so. Fix a wrong check as a separate change.

11. **Decide what is yours to decide.** Settle from the request, the code and the project's conventions whatever they settle, take the conventional default for the rest, and name each default in the report. Stop for the requester only when the choice is theirs and changes the work: a check that contradicts the request, two readings of the request that lead to different changes, or a step that cannot be undone or that others will see. Finish every step the request covers before you report; a step you could have done is not handed back as a suggestion.

If three attempts at one problem each uncover a new problem, stop and report what each one revealed and what you would try next.

## Defects to check

Check each one your change touches, with a test or a search.

- **Checks that fail open.** An authentication, permission, sandbox or allow-list check that admits when its input is missing, empty or unreadable. Test the missing case as well as the wrong one.
- **Data forms.** Null, empty, duplicates, tied sort keys, a child before its parent, other encodings and time zones, timestamps sorted as text, deleted records still referenced, every filter and sort a query accepts.
- **Stored data.** A schema or format change runs on the records that already exist, and the previous version still reads the new ones.
- **Compatibility.** For anything others depend on (an API, schema, file format, command output, event): accepting more is safe; returning less or requiring more breaks callers outside this repository. Use a compatibility checker where one exists.
- **Concurrency.** Two paths touching the same state: name what orders them, and write a test that makes them overlap. A clean race-detector run on a test that never overlaps shows nothing.
- **Lifecycle.** Everything started stops on success, error, cancellation and timeout: timers, subscriptions, connections, background tasks, child processes, file handles.
- **Identity.** Pass who or what an action is for explicitly. Never infer it from the current directory, a path, in-memory state a restart clears, the first row of an unordered query, or the active session.
- **Partial failure.** For a stop between any two steps, say what state it leaves, whether a retry repeats a write, and whether an unconfirmed request is reported as done. Validate before any irreversible step.
- **Bounds.** Every cache, queue, buffer, retry loop, wait, file and result set has a limit and a tested path for exceeding it. Every network or cross-process call has a timeout.
- **Platform.** Paths, line endings, case sensitivity, permissions, locale and process handling differ by operating system and runtime. Check each one the change ships to, or name the ones you did not.
- **What people see.** Each screen, message or response handles loading, empty, error and oversized content. Run a change to an interface in the running application and use it the way a person would, before saying it works: a build that passes and a unit test that renders it are not that check. Where nothing can run it, say so under Not checked.
- **Secrets.** Credentials never reach logs, error messages, URLs or output, debug logging included.
- **Destructive and unrequested work.** A delete states its full reach ("this item and its 14 children") and checks its preconditions where it is entered. Scheduled, startup and paid work stays off until someone turns it on.

## What the harness should enforce

Written rules lower these failures and do not remove them. Where your harness provides one of these, use it and cite its record; where it does not, do the step by hand and say so in the report.

- Tests, fixtures and scorers read-only during the change.
- The new test run on the old and the new code, with tests executed, passed and skipped counted.
- The definition of done fixed before work starts.
- New dependencies resolved against the registry before they are added.
- Network and repository history closed unless the task needs them.
- A compatibility checker against the last release.
- An action that marks a task impossible as specified, for a person to decide.
- The final report compared against the commands actually run.

## The report

Compare the diff against the request and each gate, using what you ran. Then lead with the result:

- **Changed**: what and why, one line per file or group of files.
- **Ran**: each command and the output line that shows its result; for tests, the counts run, passed, failed and skipped in the final run.
- **Not checked**: what did not run, and why.
- **Assumed**: what you believe and did not observe.
- **Found on the way**: problems outside the request, left for someone to pick up.

## Which skill to load

| When | The rule to keep if you load nothing else | Load |
|---|---|---|
| Editing code you have not read | Open each symbol at its definition and trace one concrete input to its effect. | `reading-unfamiliar-code` |
| A failure whose cause is unproven | Change nothing until it fails in front of you; fix where the wrong value is first produced. | `systematic-debugging` |
| New or changed behaviour | See the new test fail on its assertion, not on a build or setup error. | `test-first` |
| Removing anything | A search with no hits covers this repository only; make the removal its own change. | `deletion` |
| Changing what others depend on | Accept more, never return less, and run a compatibility checker. | `contract-design` |
| An awkward function or type | A new wrapper must finish "a caller no longer has to know ___". | `interface-depth` |
| Where code goes | Put it beside the code that changes with it. | `structural-boundaries` |
| Naming | Use the word the project already uses before inventing one. | `domain-language` |
| Shared state | Name what is shared and what orders the accesses; ship a test that fails without the fix. | `shared-state` |
| A system you do not run | Read the current documentation for your version and keep one real response as a fixture. | `external-integration` |
| Input from outside, or a credential | Parse it into a value that rejects bad input, and use only the parsed value. | `untrusted-input` |
| Third-party code | Take every name and version from the registry, and read the licence first. | `dependencies` |
| Faster, cheaper, smaller | Measure before and after the same way, harness untouched; the difference must exceed the run-to-run spread. | `performance-measurement` |
| A check that is slow, unclear, manual or flaky, or a step done three times | Time the loop; build the tool when the runs left cost more than building it. | `feedback-loop` |

The evidence for each rule is in [references/evidence.md](references/evidence.md).
