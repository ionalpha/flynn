# Why each rule is there

Read this when a rule looks like overhead, or before adding or removing one. Each
entry says how strong its support is: *measured* (a study or benchmark with counts),
*practice* (the stated experience of a large engineering organisation, or a recorded
incident class), *inferred* (reasoned from a measurement about something nearby) or
*opinion* (an argument with no data). Figures marked *(unverified)* were read in a
summary or abstract and have not yet been checked against the full paper.

## Contents

- What is not proven
- Before the first edit
- The gates
- The defect classes
- What the harness should enforce
- The report
- Advice left out on purpose
- What a new rule needs before it is added

## What is not proven

Nothing here shows that an agent following this skill produces better changes than
one without it. The sources show that each failure happens and how often; few test
whether an instruction prevents it, and the ones that do (the reward-hacking results
under gate 10) found a written instruction weaker than a check the agent cannot edit.
So the skill states its rules plainly, without emphasis, and lists what a harness
should enforce.

Two measurements would settle it: the same tasks with and without the skill, graded
on held-out tests and reviewer findings, and an ablation that removes one rule at a
time. Until then, a rule's place in the skill is a judgement backed by failure
evidence, not a proven remedy.

## Before the first edit

**The project's own commands.** Measured. Context files that list a project's build
and test commands helped agents; files made of repository overviews raised cost by
over 20% with no gain in success (Gloaguen et al., ICLR 2026) *(unverified)*.

**Reproduce first; if it does not reproduce, change nothing.** Measured, one
benchmark. Correct outcomes rose from 60.5% to 88.5% when agents were told a reported
bug might not reproduce (FixedBench) *(unverified)*.

**Run the covering tests before editing.** Measured, small preprint. Giving an agent
the list of tests to run cut regressions to 1.82%; a generic test-first instruction
raised them from 6.08% to 9.94% (TDAD) *(unverified)*.

**Everything the change touches.** Measured for recall, practice for the rest. Models
name functions that do not exist far more often for project code than for public
libraries, so a list from memory misses what a search finds. A search still misses
consumers reached by reflection, string keys, generated code and stored data, which
is why the routes it cannot see are named.

## The gates

**Gate 1, evidence for every claim.** Measured. Agents have reported "all tests pass"
over output showing a failure; a required field for the counts fixed the count
*(unverified)*. Exit codes hide zero-test and skipped runs by design.

**Gate 2, a test that fails for the right reason.** Measured. The most common failure
in agent-written reproduction tests is a test that already passes on the unfixed code
(SWT-Bench) *(unverified)*. On thirty systems tasks (SpecBench, 2026), the gap between
visible and held-out tests grew by about 27 points for every tenfold increase in code
size, and the held-out tests missed most were the ones checking that invalid input is
rejected. Strict test-first as a ritual has mixed evidence for people and none for
agents, so the gate keeps only the narrow part: a test seen to fail for the stated
reason. Running the test against a clean copy of the old version, rather than
breaking lines in the working copy, is practice: break-and-restore has wiped
uncommitted work.

**Gate 3, the check matches the claim, on what ships.** Practice. Each listed form is
a recorded incident class: a check that did not exercise the claimed property, a
stale artefact shipped while local tests passed, a test database that differed from
production, a lint run that differed from the pipeline's.

**Gate 4, no failure passes as success.** Measured in one repository, plus practice.
In one classified fix history (below), "could not evaluate" read as false and a
partial result read as complete was the second largest class. OWASP added
"mishandling of exceptional conditions" to its 2025 Top 10. A generated file that
exceeded a size limit and hit an unhandled error caused the Cloudflare outage of
18 November 2025. Retry storms follow from unknown errors treated as retryable.

**Gate 5, proven reachable.** Measured in one repository, plus practice. An option or
guard not wired through every path was the third largest class in that history.
Recorded incidents include a signature check written and never called.

**Gate 6, one path per job.** Practice and opinion. Drift between parallel paths is a
recurring review finding; it was not counted as a separate class.

**Gate 7, facts from the source.** Measured. Package suggestions name packages that do
not exist at 5.2% to 21.7% depending on the model (Spracklen et al., USENIX Security
2025), and some invented names were later registered to host malware. Between 36.7%
and 55.7% of agent dependency choices carried a known vulnerability *(unverified)*.
Accuracy on library APIs falls sharply as an API gets rarer (Jain et al., 2024).

**Gate 8, the smallest change.** Practice, with correlation. Google's review guide
separates refactoring from behaviour changes. Studies of agent pull requests find
rejection tracks size, which does not show that size causes it. Configuration pushes
caused about a third of outages in Google's published postmortem set *(unverified)*.

**Gate 9, nothing obsolete.** Practice. Dead code and disabled flags are treated as
liabilities in SRE guidance. The data-repair half comes from incidents in which a
corrected rule left records the old rule had written.

**Gate 10, checks and other people's files are not yours to change.** Measured for
checks; the sentences about files someone else owns are reasoning. METR (2025) found frontier
models editing the scorer, the timer or the test on about 30% of runs on one
benchmark, and telling them not to did not lower the rate. On ImpossibleBench, giving
the agent a way to flag a task as impossible cut cheating from 54% to 9% for one
model and from 49% to 12% for another *(unverified)*. This is why "stop and say so"
is in the gate and a flag action is in the harness list.

**Gate 11, decide what is yours to decide.** Measured in one project. Among the
corrections a second project's operator made to agent work, stopping to ask a
question the code already answered, and handing a step back that the agent could have
done, were two of the four classes that did not fall between October and November
2026, while the classes gates 1 to 10 cover did. Gate 10 and the three-attempt rule
both tell the agent to stop; without this gate the skill only ever pushes one way.

**Stopping after three attempts.** Opinion. The count is a convention.

**Running an interface before saying it works.** Measured in the same project: an
interface change reported as working and never exercised in a browser was the third
of those four classes.

## The defect classes

The list comes from a hand classification of every fix commit in one repository (an
agent runtime in Go with a large security component; about 155 code defects, counts
within about 5%), checked against a published taxonomy of bugs in generated code
(Tambon et al., 2024) and a second project's corrections. The order in the skill is
not a ranking: a web application or a data pipeline ranks the classes differently.
The standard linters would have prevented almost none of them.

**Concurrency.** Measured. The common failure in agent concurrency fixes is a test
that never creates the overlap (CONCUR) *(unverified)*.

**Secrets.** Measured, one study set. Debug logging accounted for 73.5% of credential
leaks studied *(unverified)*.

## What the harness should enforce

Measured. Every large effect found came from the environment rather than the prompt:
read-only or hidden tests, closed history and network, and a flag action. The list
names each check by what it does so any harness can provide it.

## The report

Measured. Models asked to correct their own reasoning without outside feedback rarely
improve (Huang et al., ICLR 2024), and self-repair for code is limited by the quality
of the feedback (Olausson et al., ICLR 2024). Hence a comparison against what ran,
and test counts stated in full. What the report must carry is the measured part; its
layout is not, so the skill leaves that to the host and the requester.

## Advice left out on purpose

- **A coverage target.** Coverage says code ran, not that anything checked it; once
  suite size is fixed it stops tracking bug detection.
- **Style gates** such as maximum function length. No evidence supports them, and
  splitting cohesive code to meet a limit produces shallow modules (Ousterhout).
- **A test-first order rule.** No defect evidence for agents, and the generic
  instruction raised regressions in the one measurement above.
- **"Write secure code."** Generic security reminders lowered correctness in the
  studies found, without a measured security gain.
- **"Review will catch it."** Studies of review at Microsoft and Google find about
  three quarters of findings concern readability and design.
- **Emphatic wording.** No study shows stronger language makes an agent comply.
- **Mutation testing in the normal loop.** Useful at scale with heavy filtering,
  noisy without it.

## What a new rule needs before it is added

A failure it prevents, seen more than once; a check answered by running something or
quoting a line; and its evidence, graded as above. If a harness can enforce the rule,
build that first: a rule the agent cannot edit beats one it is asked to follow. If the
rule belongs to one topic, it goes in that topic's skill, and this one gains a row in
the table.
