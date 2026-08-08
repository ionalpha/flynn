# Host boundary register

Flynn keeps its engine swappable by depending on an interface and letting whoever
embeds it supply the implementation. This file records, for every place that
happens, whether Flynn also ships something behind the interface and whether the
binary wires it.

The register exists because the swappability question and the standalone question
are different, and only the first one gets asked during review. A package that
defines a port and ships no producer still reads as correct: the boundary is clean,
the contract is small, a host can implement it. What it cannot do is work. Four
memory packages landed that way, and a goal capability sat fully built and unwired
for a release, because nothing in the repo asked the second question.

`ARCHITECTURE.md` describes the ports and why they exist. This is the inventory
behind them.

## Verdicts

Every row carries exactly one.

- **shipped**: Flynn has an implementation and the binary wires it. The interface
  stays, so a host replaces it and loses nothing. This is the target for anything
  Flynn can do with what it already holds: the model port, a sandbox, a store, a
  spine.
- **justified**: Flynn ships nothing on purpose, the reason is written in the doc
  comment, and the absence is visible. `goal.WithInvariantAudit` is the form to
  copy: a goal that states terms with no auditor stalls and says which auditor is
  missing, rather than running unaudited and finishing like a goal whose terms held.
- **gap**: neither. Every gap row has an open issue describing what to build. A gap
  with no issue is how one becomes permanent without anyone deciding it should.

The test behind a verdict: if the capability cannot be exercised by the `flynn`
binary plus a temp SQLite file with no host present, it is `justified` with a
written reason, or it is a `gap`.

## What a `justified` seam must do when it is absent

A written reason is half of it. The other half is that the absence is visible, and
the line is whether a reader of the finished run could tell the difference.

- **Outcome-affecting: stall or refuse, by name.** A seam whose absence changes what
  a run means never degrades quietly. `WithInvariantAudit` stalls with
  `InvariantAuditorMissing`, `WithUnitSpawner` with `UnitSpawnerMissing`, a declared
  plan-window ceiling with no source with `WindowSourceMissing`, and
  `memory/consolidate` refuses at construction with `ErrNoDistiller` rather than on a
  nightly job nobody is watching. Each has a test asserting the condition an operator
  would see, because the name is the sentence worth being able to say about the run
  afterwards.
- **Instrumentation: a documented no-op.** `mission.WithGenerationRecorder` falls
  back to `nopGenerationRecorder`, a nil `goal.Cleaner` has nothing to tear down, and
  a Hook with no `brakes.AnomalyDetector` still halts on the breakers it was
  configured with. These change no outcome, they say so in their doc comments, and
  each has a test that the absence costs nothing.

A missing-producer stall is recoverable, which is what makes stalling the right
answer rather than a refusal wearing a softer word. `Status.Unwired` marks a stall as
describing the loop rather than the run, and those are the only stalls a later
reconcile re-examines: wiring the thing the goal needed and reconciling again picks
the work back up, where a spent budget or a run that got nowhere stays settled. A new
`…Missing` stall belongs in `goal.unwiredStalls` on the commit that introduces it.

## Foundation

The backends a host genuinely owns. Every one has an in-process implementation for
tests and a durable one for the binary.

| Deferral | Shipped producer | Wired at | Verdict |
|---|---|---|---|
| `state.Provider` | `state.NewMemory`, `storage/sqlite.Store` | `cmd/flynn/runs.go` `openStore`, `agent.go` `New` | shipped |
| `state.SessionStore` / `SkillStore` / `MemoryStore` | `state` in-memory set, `storage/sqlite`, `skill.Store`, `memory.Store` | via `state.Provider` | shipped |
| `spine.Log` | `spine.MemoryLog`, `storage/sqlite` event log | `store.Log()` at every entry point | shipped |
| `spine.SnapshotCodec` | `chain.SnapshotSealer` | `cmd/flynn/spine.go` | shipped |
| `resource.Store` | `resource.NewMemory`, `storage/sqlite` resource store | `cmd/flynn/mission.go` `runtimeConfig` | shipped |
| `resource.SchemaCompiler` / `Validator` | `resource.builtinCompiler`, `resource.Registry` | `resource` kind registration | shipped |
| `jobs.Queue` / `Waker` | `jobs.MemoryQueue`, `storage/sqlite` job queue | `store.Jobs()`, `agent.go` | shipped |
| `llm.Model` | `llm/anthropic`, `llm/openai`, local serving | `provider.Resolve` | shipped |
| `sandbox.Sandbox` | `sandbox.Local`, `MicroVM`, `Remote` | `sandbox.NewLocal` at each assembly | shipped |
| `observe.Logger` / `Tracer` / `Meter` | `observe.slogLogger`, the `Nop` set | `observe.Default`, `cmd/flynn/main.go` | shipped |
| `secret.Source` | `secret.EnvSource`, `secret.chain`, vault store | `cmd/flynn/main.go` `credentialSource` | shipped |
| `dispatch.Admitter` | `capability.Admitter`, `dispatch.AllowAll` | `cmd/flynn/mission.go`, `learning.go` | shipped |
| `clock.Clock` / `Timing` | `clock.System`, `clock.Manual` | everywhere a clock is taken | shipped |

## The goal reconciler

The reconciler takes its judgments as options. An option left unset changes what a
run can do, so this is the group where an unwired producer costs the most.

| Deferral | Shipped producer | Wired at | Verdict |
|---|---|---|---|
| `goal.StepExecutor` | `mission.Executor`, `driver.Router` | `cmd/flynn/mission.go` `assembleMission` | shipped |
| `goal.StopEvaluator` | `mission.Convergence` | same | shipped |
| `goal.Planner` | `mission.Planner` | same, planning runs only | shipped |
| `goal.ProgressProbe` | `progress.SpineProbe` | same | shipped |
| `goal.InvariantAuditor` | `evidence.CommandAuditor` | same | shipped |
| `goal.RefusalProbe` | `refusal.SpineProbe` | same | shipped |
| `goal.ItemVerifier` + `goal.Evidence` | `evidence.CommandVerifier`, `evidence.SpineEvidence` | same (planning runs); `fanout.go` always | shipped |
| `goal.UnitSpawner` | `orchestration.UnitFanout` | `cmd/flynn/fanout.go`, `agent.go` | shipped |
| `goal.Cleaner` | none | n/a | justified |
| `goal.WindowSource` | none | n/a | justified |
| `orchestration.Governor` | `dispatch.Dispatcher` (via `orchestration.UnitGovernor`) | `cmd/flynn/fanout.go`, `agent.go` | shipped |

`goal.Cleaner`: a nil cleaner means there is nothing external to tear down, which is
true of the standalone binary. Child goals are reaped through owner references, not
through this. Instrumentation-side of the line below: a delete with no cleaner
completes rather than hanging on a finalizer nobody will clear
(`TestGoalDeletionCompletesWithNoCleaner`).

`goal.WindowSource`: a plan window is a quota a host meters, and Flynn has no
equivalent to read. Every other spend bound (step budget, token and cost ceiling) is
enforced without it.

A goal that declares no `WindowFraction` asks nothing of the source and runs with
none wired, which is the standalone case. A goal that declares one and meets a
reconciler with no source stalls with `WindowSourceMissing` and names the ceiling
that went unmeasured. It used to run unbounded, and that was the one declared bound
in the register that could be passed over in silence: a run that finishes without its
ceiling having been checked is indistinguishable from one that stayed inside it, and
the operator who set the ceiling would read the second where the first happened.

`goal.UnitSpawner` was the plain case for this register: the producer existed, the
refusal when it was absent was honest (`UnitSpawnerMissing`), and the binary never
handed it over, so a goal carrying a unit graph stalled on a capability that was
already written. `orchestration.Governor` was the same gap seen from the other end,
its producer being the dispatcher itself. Both are now wired, from one call:
`orchestration.Units(spawner, orchestration.UnitGovernor(...))`, in the fan-out
assembly and in the `agent` facade.

Closing them moved the ledger loop too. A unit is settled from its child's ledger,
so the fan-out assembly wires `ItemVerifier`, `Evidence` and ledger convergence for
every goal on that path rather than only a planning one: without them a child
converges the moment the model says it is finished, and the unit fails as unproven
because the check the plan author wrote never got a turn to run.

## The conversation executor

| Deferral | Shipped producer | Wired at | Verdict |
|---|---|---|---|
| `mission.Tool` | `tools` set, `tools/github`, extension tools | `mission.WithTools` | shipped |
| `mission.ResultSummarizer` | `tools` bash, glob, grep, read | implicit on the tool | shipped |
| `mission.Fanout` | `orchestration.Spawner` | `cmd/flynn/fanout.go`, `agent.go` | shipped |
| `mission.Reporter` | `session.reporter` | `mission.WithObserver` | shipped |
| `mission.ApprovalPrompter` + `approval.Gate` | `cmd/flynn.approvalPrompter`, `approval` policy, `spinesink.ApprovalSink`, signer, nonce store | `cmd/flynn/approval_gate.go`, wired at every assembly | shipped |
| `mission.GenerationRecorder` | `nopGenerationRecorder` only | n/a | justified |
| `brakes.Switch` | `brakes.MemSwitch` | `cmd/flynn/fanout.go` `defaultBrakes` | shipped |
| `brakes.AnomalyDetector` | none | n/a | justified |

`approval`: both halves were written and nothing connected them, so a run that
should have paused for a person never paused. They are connected now, and the
policy that decides which actions pause is the operator's: `--require-approval
<action>`, repeatable, empty by default.

Empty by default is a decision, not an omission. A run's standing controls are its
capability grant and its sandbox, which apply on every path including the ones
nobody is watching; approval is the second gate above them, for the actions a
particular operator wants to see before they happen. A default that paused
something would be a default that deadlocks every run with no one to ask.

The other half of that decision is what a run with a policy and no prompter does.
It refuses the listed action rather than taking it: the interactive shell installs
the modal prompt, and every other path (a one-shot `flynn goal`, a served
conversation) carries the gate without one. Every decision, granted or denied, is
recorded on the run's own stream as an `approval.decision` event, so it is sealed
with the rest of what the run did.

`mission.GenerationRecorder`: the only implementation discards, and it stays that
way. Nothing in the binary calls `mission.WithSampling`, so every model call a Flynn
run makes is free-running and the envelope a recorder would write is the zero
envelope on every call: not pinned, no seed, no temperature. Recording that once per
model call would put a constant on the sealed stream and call it evidence, and the
fact it encodes is one fact about the run rather than one per turn.

What would change the answer is pinning the sampling, not wiring the recorder. Pin
it and the envelope stops being a constant and becomes the half of a generation's
identity the serving layer does not carry. That is a decision about Flynn's default
decoding posture and it comes first; a recorder wired ahead of it records only the
absence of one. The reason is written on `mission.GenerationRecorder` itself, so the
next reader does not have to re-derive it from the call sites.

`brakes.AnomalyDetector`: the doc comment says the default is no detector and the
configured breakers carry in-process detection, so an absent one narrows the signal
rather than removing the halt.

## Learning and memory

| Deferral | Shipped producer | Wired at | Verdict |
|---|---|---|---|
| `learn.Distiller` | `learn.ModelDistiller` under `NewGovernedDistiller` | `cmd/flynn/learning.go` `governedDistiller` | shipped |
| `learn.Verifier` | `learn.SandboxVerifier` under `NewGovernedVerifier` | `cmd/flynn/learning.go` `governedVerifier` | shipped |
| `memory/consolidate.Distiller` | `memory/distil.ModelDistiller` under `NewGoverned` | `cmd/flynn/memory_cmd.go` `consolidateDistiller` | shipped |
| `memory/digest.Builder` | n/a, the digest is Flynn's own | `cmd/flynn/memorystack.go` `newMemoryStack`, built at wake | shipped |
| `memory/digest.Pusher` | `memory/ridealong.Surfacer` | `digest.New`'s default, via `newMemoryStack` | shipped |
| `memory/curate` write policy | `curate.Wrap` | `cmd/flynn/memorystack.go` `newMemoryStack` | shipped |
| `memory/guard.PromotionReader` | every memory store | via the store | shipped |
| `memory/ridealong.Surfacer` (pull) | `memoryStack.skillNotes` behind `skilltool.Notes` | `cmd/flynn/memorystack.go`, into `skilltool.New` at `learning.go` and `repl.go` | shipped |
| `memory/ridealong` anchors | `state.SkillAnchor`, written by the curator from `Outcome.SkillsRead` | `learn.Curator.Curate` | shipped |

`learn` is the pattern the other two should follow. The interface stays a port, the
model-backed implementation ships beside it, the governed wrapper puts its model
call through the dispatch waist, and the binary wires the pair.

`memory/consolidate` made the same call and stopped at the interface. The reasoning
in its doc comment holds: a lesson is a language judgment and the package should
keep no model. The sibling that has one is now `memory/distil`, which stands to
`consolidate` as `evidence` does to `goal`, and the pass is exercised end to end
over a temp database in `storage/sqlite/distil_test.go`.

The binary wires it as `flynn memory consolidate`. Consolidation is a command
rather than something a run does on its way past, because it is the one piece of
memory work that is nobody's turn: distilling five failures into a lesson spends
model calls on material the current run is not about, and hanging that off a
session would charge whichever conversation happened to be open.

The rest of the memory stack is wired at `cmd/flynn/memorystack.go`, in one
assembly rather than three calls in three places, because the pieces only work
together. The digest offers what the write path curated: a subject whose fact was
superseded has one standing answer to offer, where the raw store has every version
anyone ever wrote and no way to say which is current. A digest over an uncurated
store would push contradictions at every reader unasked.

`memory/ridealong` is wired on the push side: it is the digest's pusher, so a
memory that reaches a reader unasked is counted and the run's prime scope marked
in the same step. That is what gives the decay policy a usage signal to read.

The pull side rides on `skill_read`. A memory lesson is anchored to the skills the
run that produced it loaded, and loading a skill surfaces what was learned while
working from it, framed as background and counted as a use. Both ends are Flynn's:
it issues the skill's id, and `skill_read` is its own tool, so the loop closes with
no host present. `cmd/flynn/ridealong_wiring_test.go` runs it over two missions and
one store.

A skill was chosen over the other referents Flynn holds. A file path is cheaper to
write and higher-traffic, and a memory about a path is worth less than one about a
procedure: what somebody learned the last time they applied a procedure is exactly
what the next reader about to apply it wants and has no query to ask for. A run id
anchors a lesson to the one run that will never read it again. One mechanism
exercised on a real read beats two half-wired ones, so only this one is wired.

`state.AnchorKindSkill` is the one anchor kind this codebase names, and it does not
weaken the rule above it. An anchor stays an opaque `{Kind, ID}` pair that nothing
resolves; the vocabulary is still the host's for every kind a host refers with. A
skill is not another system's record. It is a row in Flynn's own store, under an id
Flynn issued, which is the whole test for what belongs on this side of the boundary.

The anchor is written from the skills the run loaded, not the ones it was offered.
Loading is an act the run chose, which is the best evidence available that it was
working on that procedure, and the caller already holds the list because
reinforcement is credited from it. It is a proxy for aboutness rather than a
judgment about it: a run that loads five procedures anchors its lesson to all five,
and the surfacing cap is what keeps that from becoming a reader's problem.

## Channels, extensions, external agents

| Deferral | Shipped producer | Wired at | Verdict |
|---|---|---|---|
| `inbox.Source` / `Sink` | `internal/source/signalcli`, `internal/source/telegram` | `cmd/flynn/serve.go` `serveChannels` | shipped |
| `inbox.Enqueuer` | `jobs` queues | `inbox` ingest | shipped |
| `driver.Driver` | `driver.defaultDriver`, `singleShotDriver`, `externagent.Driver` | `driver.NewRouter` | shipped |
| `externagent.Spawner` / `Adapter` / `Process` | `SandboxSpawner`, `Claude`, `Codex` | `cmd/flynn/externagent.go` | shipped |
| `extension.Resolver` / `Launcher` / `ToolSource` | `ReleaseResolver`, `SourceResolver`, `SandboxLauncher`, `Loader` | `cmd/flynn/extensions.go` | shipped |
| `extension.HostSigner` / `SignerChannel` | `RoutedSigner`, `mcpSignerChannel` | `cmd/flynn/extensions.go` | shipped |
| `extension.HostFetcher` | `HTTPHostFetcher` | `cmd/flynn/extensions.go` | shipped |
| `controlplane.Authenticator` | `TokenAuthenticator`, `PossessionAuthenticator`, `DelegationAuthenticator`, `DenyAll` | `cmd/flynn/serve.go` | shipped |
| `controlplane.SeedVault` | vault store | `cmd/flynn/spine.go`, `playbook.go` (`LoadOrCreateIdentity`) | shipped |
| `chain.RootSigner` / `SnapshotSigner` | `chain.Ed25519RootSigner` | `cmd/flynn/spine.go` | shipped |
| `chain.NodeStore` / `CheckpointStore` | `storage/sqlite` merkle store | durable record path | shipped |
| `harness.ProfileSource` | `harness.StaticProfiles`, `internal/profilestore.Source` | `cmd/flynn/modelrun.go` | shipped |

## Deferrals that are not interfaces

Places a doc comment hands the work to whoever embeds Flynn, without a port.

| Deferral | What Flynn does alone | Verdict |
|---|---|---|
| `internal/archetype`: an Agent declares capabilities by name, resolving them to implementations is the host's job | the binary resolves its own toolset against the declared names | justified |
| `archetypes/review`: an empty model field defers to the host's configured model | the binary's configured model applies | justified |
| `state.Scope` levels (instance, project, workspace) | defined in Flynn's own terms, not a host's | justified |

## How this list is derived

Membership is mechanical. Only the verdict column is a judgment.

1. Exported interfaces declared in the public band (every package outside
   `internal/`, excluding `_test.go` files), together with the named types in the
   module whose method set covers each one.
2. Every `WithX` option and `Config` field whose absence changes what a run can do.
3. Doc comments that hand work to a host or a caller, which is what puts the last
   table here.

A producer counts as wired only when a non-test call site in `cmd/flynn`, `agent.go`
or `runtime` passes it in. An implementation that exists and is never handed over is
a gap, not a shipped default, because the difference is invisible to a reader of the
package and decisive for an operator.

The derivation is meant to become a check that fails when a deferral has no row, when
a row names something that no longer exists, or when a gap row has no open issue. A
register kept by hand decays; this one is written so it does not have to be.
