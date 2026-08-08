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
| `goal.ItemVerifier` + `goal.Evidence` | `evidence.CommandVerifier`, `evidence.SpineEvidence` | same, planning runs only | shipped |
| `goal.UnitSpawner` | `orchestration.UnitFanout` | nowhere | gap |
| `goal.Cleaner` | none | n/a | justified |
| `goal.WindowSource` | none | n/a | justified |
| `orchestration.Governor` | `dispatch.Dispatcher` | nowhere | gap |

`goal.Cleaner`: a nil cleaner means there is nothing external to tear down, which is
true of the standalone binary. Child goals are reaped through owner references, not
through this.

`goal.WindowSource`: a plan window is a quota a host meters, and Flynn has no
equivalent to read. The doc comment says a nil source leaves that one axis
unbounded. Every other spend bound (step budget, token and cost ceiling) is enforced
without it. The wording is worth re-checking, since a bound that is declared and not
enforced is quieter than a stall.

`goal.UnitSpawner` is the plain case for this register: the producer exists, the
refusal when it is absent is honest (`UnitSpawnerMissing`), and the binary never
hands it over, so a goal carrying a unit graph stalls on a capability that is
already written. `orchestration.Governor` is the same gap seen from the other end.
Its producer is the dispatcher itself, and the only call that takes one is
`orchestration.Units`, which nothing outside the package calls. Wiring the unit
fan-out closes both rows.

## The conversation executor

| Deferral | Shipped producer | Wired at | Verdict |
|---|---|---|---|
| `mission.Tool` | `tools` set, `tools/github`, extension tools | `mission.WithTools` | shipped |
| `mission.ResultSummarizer` | `tools` bash, glob, grep, read | implicit on the tool | shipped |
| `mission.Fanout` | `orchestration.Spawner` | `cmd/flynn/fanout.go`, `agent.go` | shipped |
| `mission.Reporter` | `session.reporter` | `mission.WithObserver` | shipped |
| `mission.ApprovalPrompter` + `approval.Gate` | `cmd/flynn.approvalPrompter`, `approval` policy, sink, signer, nonce store | nowhere | gap |
| `mission.GenerationRecorder` | `nopGenerationRecorder` only | nowhere | gap |
| `brakes.Switch` | `brakes.MemSwitch` | `cmd/flynn/fanout.go` `defaultBrakes` | shipped |
| `brakes.AnomalyDetector` | none | n/a | justified |

`approval`: both halves are written. `mission.WithApprovalPrompter` takes the
prompter and the signer, `cmd/flynn/approval.go` implements the prompter against the
shell's live prompt, and the `approval` package has its policy, sink, signer and
nonce store. No caller connects them, in the binary or in the `agent` facade, so a
run that should pause for a human never pauses.

`mission.GenerationRecorder`: the only implementation discards. A standalone run
therefore keeps no record of the decoding envelope each model call ran under, which
is a piece of the record the rest of the verifiability story assumes is there.

`brakes.AnomalyDetector`: the doc comment says the default is no detector and the
configured breakers carry in-process detection, so an absent one narrows the signal
rather than removing the halt.

## Learning and memory

| Deferral | Shipped producer | Wired at | Verdict |
|---|---|---|---|
| `learn.Distiller` | `learn.ModelDistiller` under `NewGovernedDistiller` | `cmd/flynn/learning.go` `governedDistiller` | shipped |
| `learn.Verifier` | `learn.SandboxVerifier` under `NewGovernedVerifier` | `cmd/flynn/learning.go` `governedVerifier` | shipped |
| `memory/consolidate.Distiller` | none (`DistillerFunc` is the adapter, not a producer) | nowhere | gap |
| `memory/digest.Pusher` | `memory/ridealong.Surfacer` | nowhere | gap |
| `memory/curate` write policy | `curate.Wrap` | nowhere | gap |
| `memory/guard.PromotionReader` | every memory store | via the store | shipped |
| `memory/ridealong` anchors | n/a, the vocabulary is the host's | n/a | justified |

`learn` is the pattern the other two should follow. The interface stays a port, the
model-backed implementation ships beside it, the governed wrapper puts its model
call through the dispatch waist, and the binary wires the pair.

`memory/consolidate` made the same call and stopped at the interface. The reasoning
in its doc comment holds: a lesson is a language judgment and the package should
keep no model. What is missing is the sibling that has one, the way `evidence`
supplies what `goal` refuses to.

`memory/ridealong`: an anchor is an opaque `{Kind, ID}` pair and nothing here
resolves one, which is deliberate and documented. Whether the binary anchors
anything of its own (a run, a file, a skill) is a separate question.

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
