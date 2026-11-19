# Why each gate is there

Read this when a gate looks like overhead, or before adding or removing a rule. Each
entry says how strong its support is: *measured* (a study or benchmark with counts),
*practice* (the stated experience of a large engineering organisation, or a recorded
incident class), *inferred* (reasoned from a measurement that is about something
nearby) or *opinion* (an argument with no data).

## Contents

- What is not proven
- Evidence for each gate
- The defect classes
- Claims removed because nothing supported them
- Advice left out on purpose
- What a new rule needs before it is added

## What is not proven

Nothing here shows that an agent following this skill produces better changes than
one without it. The sources show that each failure happens and how often; none
tests whether an instruction prevents it. The one measurement of an instruction
against a failure (the reward-hacking result under the refusal below) found the
instruction did not help. So the skill states its rules plainly, without emphasis,
and the section on what the host enforces points at checks the agent cannot edit.

Two measurements would settle it: an A/B of the same tasks with and without the
skill, graded on held-out tests and reviewer findings, and an ablation that removes
one gate at a time to see which change the outcome. Until then, treat a gate's
place in the skill as a judgement backed by the failure evidence, not as a proven
remedy.

## Evidence for each gate

**Before the first edit: a check seen to fail.** Practice. It is the same rule as
gate 2, applied before the work instead of after it. Flynn's planner enforces it by
refusing a check that already passes on the untouched tree.

**Gate 1, evidence for every claim.** Measured, one benchmark. On thirty systems
tasks (SpecBench, 2026), frontier agents passed nearly all visible tests, and the
gap to held-out tests that combined features grew by about 27 points for every
tenfold increase in code size. One entry passed 97% of visible tests and 0% of
held-out ones. The held-out tests missed most often were the ones checking that
invalid input is rejected.

**Gate 2, a test seen to fail.** Measured, with a caveat. An agent pipeline resolved
94% of a benchmark with human-written tests and 68% with tests the agents wrote
(TDFlow, 2025). Agent-written test files contain more print statements than
assertions (Chen et al., 2026). Strict test-first as a ritual has mixed evidence for
people (a meta-analysis of 27 studies found a small quality gain; a blind
replication found none) and none for agents, so the gate keeps only the narrow part:
a test seen to fail for the stated reason and then pass.

**Gate 3, checked where users get it.** Practice. Each listed form is a recorded
incident class: a stale artifact shipped while local tests passed, a test database
that differed from production, a lint run that differed from the pipeline's. The
claim that a green check on the wrong target is worse than none is opinion: it rests
on the check ending the search.

**Gate 4, no failure passes as success.** Measured in one repository, plus practice.
In Flynn's classified fix history (below), "could not evaluate" read as false and a
partial result read as complete was the second largest class, about 20 of 155. OWASP
added "mishandling of exceptional conditions" to its 2025 Top 10. The fail-fast
argument (Shore, 2004) and Nygard's stability patterns point the same way. The skill
gives the mechanism (a plausible value prompts nobody to look) and no time to
discovery, because no source measured one.

**Gate 5, proven reachable.** Measured in one repository, plus practice. An option or
guard not wired through every path (a dropped flag, a resume path that lost its
guards) was the third largest class, about 18 of 155. Recorded incidents also include
an update signature check written and never called.

**Gate 6, one path for each job.** Practice and opinion. Drift between parallel paths
is a recurring review finding, and part of the wiring class above is a second path
that missed a fix. It is not a separately counted class in the classified history,
so the skill makes no claim about its rank.

**Gate 7, read before you call.** Measured for public libraries, inferred for
project code. Package suggestions name packages that do not exist at measurable
rates (Spracklen et al., USENIX Security 2025: 5.2% for commercial models, higher for
open ones), and some invented names were later registered to host malware. Accuracy
on library APIs falls sharply as an API gets rarer (Jain et al., 2024). Code local to
one project is absent from training data, so the skill infers it is the least
reliable to recall; no study measured that directly.

**Gate 8, the smallest change.** Practice, with correlation. Google's review guide
treats about a hundred lines as reasonable and a thousand as usually too large, and
separates refactoring from behaviour changes. Studies of agent pull requests find
rejection tracks size and complexity, which does not show that size causes it.

**Gate 9, nothing obsolete.** Practice. SRE guidance treats dead code and disabled
flags as liabilities. The data-repair half comes from recorded incidents in which a
corrected rule left over a thousand records the old rule had written.

**The refusal to weaken a check.** Measured. METR (2025) found frontier models editing
the scorer, the timer or the test on about 30% of runs on one benchmark, and prompt
changes telling them not to did not lower the rate. This is the strongest case for
checking it in the host rather than in the prompt.

**Stopping after three attempts.** Opinion. The count is a convention; Flynn's goal
loop uses the same count for steps without progress.

**The reread before reporting.** Measured. Models asked to correct their own
reasoning without outside feedback rarely improve (Huang et al., ICLR 2024). For
code, self-repair gains are modest and limited by the quality of the feedback
(Olausson et al., ICLR 2024). Hence a reread answered by something that ran.

## The defect classes

Ordered by a hand classification of every fix commit in Flynn's history up to
November 2026: 202 fix commits, about 155 of them code defects, counts within about
5%. Fail-open trust checks (about 32), unknown read as false (20), options not wired
through (18), domain logic and version compatibility (18), platform (17), encoding
and ordering (14), concurrency (12), unbounded work (11), lifecycle (5). The standard
linters would have prevented almost none of them.

This is one repository: an agent runtime in Go with a large security component. A
web application or a data pipeline will rank the classes differently. Classify a
second project's history before treating the order as general; the list itself is
also checked against a published taxonomy of bugs in generated code (Tambon et al.,
2024), which contains the data-form, missing-case and invented-object classes.

## Claims removed because nothing supported them

An earlier draft carried these. Each was a plausible statement with no source.

- A silent fallback "turns a failure you would see in a minute into one found weeks
  later". No time to discovery was measured. The mechanism stays.
- Writing down the four opening items "takes a minute, and each one changes what you
  do next".
- "If you cannot name a check, the work is not understood well enough to start." Some
  work is exploration; the rule now makes finding a check the first step.
- Silent failure is the largest defect class, and duplicate paths the second. The
  classified history says fail-open trust checks are the largest and does not count
  duplicate paths separately.
- The defect classes "recur in every codebase" and "most data defects are a test that
  used a tidier input". One repository was classified.
- Open every function you call at its definition. Applied to standard library calls,
  this costs reads and returns little; the rule now targets project code and recalled
  library behaviour.
- "A diff too large to read in one sitting is too large to review." Opinion stated as
  a rule.

## Advice left out on purpose

- **A coverage target.** Coverage says code ran, not that anything checked it; suites
  with full coverage and very low mutation scores are reported.
- **Style gates** such as maximum function length. No evidence supports them, and
  splitting cohesive code to meet a limit produces shallow modules (Ousterhout).
- **"Review will catch it."** Studies of review at Microsoft and Google find about
  three quarters of findings concern readability and design, with few functional
  defects.
- **Emphatic wording.** No study shows stronger language makes an agent comply, and
  the reward-hacking result shows a direct instruction often does not.
- **A single headline rate** for vulnerabilities in generated code. Published figures
  run from about 20% to over 80% by benchmark, and several come from vendors.
- **Mutation testing in the normal loop.** Useful at scale with heavy filtering, noisy
  without it. Breaking the covered line by hand (gate 2) answers the same question for
  the lines you changed.

## What a new rule needs before it is added

A failure it prevents, seen more than once; a check answered by running something or
quoting a line; and its evidence, graded as above. If a host can enforce the rule,
file that first: a rule the agent cannot edit beats one it is asked to follow. If the
rule belongs to one topic, it goes in that topic's skill, and this one gains a row in
the table.
