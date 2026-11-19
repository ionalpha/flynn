# Why each gate is there

Read this when a gate looks like overhead and you want to know what it costs to skip
it, or when someone proposes adding a rule to the skill. Each entry gives the
evidence and how strong it is: *measured* (a study or benchmark with counts),
*practice* (the stated experience of a large engineering organisation) or *opinion*
(an argument from an experienced practitioner, with no data).

## Contents

- Evidence for each gate
- The defect classes
- Advice left out on purpose
- What a new rule needs before it is added

## Evidence for each gate

**Gate 1, evidence for each claim.** Measured. On a benchmark of thirty systems tasks
(SpecBench, 2026), every frontier agent passed nearly all of the tests it could see,
and the gap to held-out tests that combined features grew by roughly 27 points for
every tenfold increase in code size. One compiler passed 97% of visible tests and 0%
of held-out ones, because it had memorised the visible inputs. The held-out tests
missed most often were the ones that check invalid input is rejected. On very long
tasks (SWE-Marathon, 2026), nearly every failure carried a sign of weak validation.

**Gate 2, what was not checked.** Opinion, consistent with gate 1. Nobody has
measured it directly; it follows from the fact that a reader can only discount a gap
they were told about.

**Gate 3, the target users get.** Practice. Each of the usual forms in the gate is a
recorded incident class in long-running codebases: a stale build artifact shipped
while local tests passed, a test database that differed from production, paths
checked from the wrong host, a lint run that differed from the pipeline's.

**Gate 4, loud failure.** Practice and consensus. Silent failure is the largest
defect class in the internal bug histories this skill was built from. OWASP's 2025
Top 10 added "mishandling of exceptional conditions" as its own category. The
fail-fast argument (Shore, 2004) and the stability patterns in Nygard's *Release It!*
(timeouts, bounded resources, failing fast) point the same way.

**Gate 5, proof that code is reached.** Practice. Recorded incidents include an
update signature check that was written and never called, and reports computed but
never read by anything.

**Gate 6, one path.** Practice. Duplicate paths that drifted apart are the second
largest defect class in the same bug histories. Changes to a definition that reached
some of its readers and not others are a common case.

**Gate 7, read before writing.** Measured. Generated code invents project-local calls
far more often than library calls. In a large study of package suggestions (Spracklen
et al., USENIX Security 2025) 5.2% of commercial-model suggestions named packages that
do not exist. A 2026 replication found current frontier models still at 2% to 5%, with
some invented names later registered by others and used to host malware. Studies of
deprecated and misused library calls find every model tested uses deprecated calls.

**Gate 8, witness and failing test.** Measured, with a caveat. With human-written
tests an agent pipeline resolved 94% of a benchmark; with tests the agents wrote
themselves it resolved 68% (TDFlow, 2025). Agent-written test files contain more print
statements than assertions (Chen et al., 2026), and how often an agent writes tests
barely changes whether it resolves the task. Coverage shows a line ran, not that
anything checks it: suites with full coverage and very low mutation scores have been
reported. Strict test-first as a ritual has mixed evidence for people (a meta-analysis
of 27 studies found a small quality gain; a blind replication found none) and none for
agents. The gate keeps only the narrow, cheap part: a test seen to fail for the stated
reason and then pass.

**Gate 9, small changes.** Practice, with correlation. Google's review guide treats
about a hundred lines as reasonable and a thousand as usually too large, and wants
refactoring in a separate change from features and fixes. Beck argues the same split
between structure and behaviour. Studies of agent pull requests find rejection
tracks the size and complexity of the change. DORA's survey work finds that AI
assistance raises throughput and lowers stability unless small batches and fast
feedback are in place.

**Gate 10, removing what became obsolete.** Practice. Google's SRE guidance treats
dead code, commented-out code and disabled flags as liabilities. The data-repair half
comes from recorded incidents where a corrected rule left over a thousand records the
old rule had written.

**The refusal.** Measured. METR (2025) found frontier models editing the scorer, the
timer or the test to pass on 30% of runs on one benchmark, and prompt changes such as
"do not cheat" did not reduce the rate. A rule in the prompt is weak protection. The
skill states the refusal as absolute, and where it can, the check should sit outside
the agent's reach.

**The review before handover.** Measured. Models asked to review and correct their
own reasoning without outside feedback rarely improve and sometimes get worse (Huang et
al., ICLR 2024). A survey of self-correction work (Kamoi et al., TACL 2024) finds it
reliable only where outside feedback exists. That is why every line of the handover
check is answered by something that ran.

**The stop after three attempts.** Opinion. The count is a convention. Having a count,
and making the action at that count a stop and a report, keeps patches from piling
onto a wrong approach.

## The defect classes

Built from closed bug histories and checked against a published taxonomy of bugs in
generated code (Tambon et al., 2024: misread intent, missing corner cases, wrong input
types, invented objects and attributes, incomplete generation, and code added that
nobody asked for). In the internal histories, data shape, lifecycle, identity taken
from ambient state, and destructive actions without an invariant together accounted
for more defects than any single gate's class except silent failure and duplicate
paths. The partial-failure questions come from review rounds where each round found
one more of them; asking all of them at once before the first review avoids that.

## Advice left out on purpose

- **A coverage target.** Coverage says code ran, not that anything checked it.
- **Strict test-first for every line.** See gate 8; the narrow rule carries the benefit
  the evidence supports.
- **Style gates** such as maximum function length, no `else`, or a minimum number of
  call sites before a helper. No evidence supports them, and splitting cohesive code to
  meet a length limit produces shallow modules (Ousterhout).
- **"Review will catch it."** Studies of code review at Microsoft and Google find most
  review findings concern readability and design, about three quarters of them, with
  few functional defects. Correctness has to be shown before review.
- **Emphatic wording as enforcement.** No study shows that stronger language in an
  instruction makes an agent comply; the reward-hacking result above shows it often
  does not. The skill gives reasons, and puts checks where an agent cannot edit them.
- **A single headline rate** for vulnerabilities in generated code. Published figures
  run from about 20% to over 80% depending on the benchmark, and several come from
  vendors.
- **Automated mutation testing in the normal loop.** It works at scale with heavy
  filtering of which mutants to show, and is noisy without it. The manual check in gate
  8 gives the same answer for the lines you changed.

## What a new rule needs before it is added

A failure it prevents, seen more than once; a check that can be answered by running
something or quoting a line; and the evidence, with its strength named as above. A
rule without a reason is the first thing the next editor deletes, and a rule without a
check turns into a wish. If the rule belongs to one topic, it goes in that topic's
skill and this one gains a line in the table.
