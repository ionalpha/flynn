# Host boundary register

Flynn depends on interfaces (ports) and lets whoever embeds it supply the
implementation. This file lists every port in the public packages and records two
things for each: whether Flynn has its own implementation (the producer), and whether
the `flynn` binary wires it. [ARCHITECTURE.md](../ARCHITECTURE.md) explains the ports;
this is the inventory.

A port with a clean contract and no producer passes review and still does nothing in
the standalone binary. The register makes that visible.

## Verdicts

Every row carries exactly one.

- **shipped**: Flynn has an implementation and the binary wires it. A host can still
  replace it. This is the target for anything Flynn can do with what it already has:
  the model port, a sandbox, a store, a spine.
- **justified**: Flynn has no implementation on purpose, the reason is in the doc
  comment, and the absence is visible. `goal.WithInvariantAudit` is the example to
  copy: a goal that states terms with no auditor stalls and names the missing auditor.
- **staged**: Flynn has the implementation, the binary can wire it, and it is off by
  default. A staged row names **the switch** that turns it on and **the promotion
  condition** under which it becomes the default, specific enough to check later.
  "Revisit this" is not a promotion condition. The binary must show the state, so an
  operator can see that the capability exists and is off.
- **gap**: none of the above. Every gap row links an open issue describing what to
  build.

The test: if the `flynn` binary plus a temp SQLite file, with no host, cannot exercise
the capability, it is `justified` with a written reason, or it is a `gap`.

A default-off flag is not automatically `staged`. Most of the binary's off-by-default
flags are the intended end state: `--endpoint-local`, `--api-expose` and
`review --approve` widen access, and they stay off. `runtime.Config.AllowAssertedEvidence`
is the same one level down: off means a check has to have run. `--fanout`,
`--no-learn` and `--plain` select a mode. A row is `staged` only when off-by-default is
temporary, which today is two rows.

## How a `justified` port behaves when absent

The reason is written down, and the absence shows in the finished run.

- **Changes the outcome: stall or refuse, by name.** `WithInvariantAudit` stalls with
  `InvariantAuditorMissing`, `WithUnitSpawner` with `UnitSpawnerMissing`,
  `WithSteerJudge` with `SteerJudgeMissing`, a declared plan-window ceiling with no
  source with `WindowSourceMissing`, and `memory/consolidate` refuses at construction
  with `ErrNoDistiller`. Each has a test asserting the condition an operator sees.
- **Instrumentation: a documented no-op.** `mission.WithGenerationRecorder` falls back
  to `nopGenerationRecorder`, a nil `goal.Cleaner` has nothing to tear down, and a Hook
  with no `brakes.AnomalyDetector` still halts on its configured breakers. Each says so
  in its doc comment and has a test that the absence changes no outcome.
- **Timing only: say what is lost, and prove the outcome is not.** `goal.WithHalt` is
  the one port in this group. With none wired, an operator's kill still settles the
  goal and no further step is dispatched; the stop just does not reach the step already
  running. The doc comment says so, and a test asserts the run stops either way.

A missing-producer stall is recoverable. `Status.Unwired` marks a stall as caused by
the loop's wiring, not by the run, and only those stalls are re-examined by a later
reconcile: wire the missing producer, reconcile again, and the work resumes. A spent
budget or a run that made no progress stays settled. A new `*Missing` stall goes in
`goal.unwiredStalls` in the commit that adds it.

## Foundation

Backends a host can own. Each has an in-process implementation for tests and a durable
one for the binary.

| Port | Producer | Wired at | Verdict |
|---|---|---|---|
| `state.Provider` | `state.NewMemory`, `storage/sqlite.Store` | `cmd/flynn/runs.go` `openStore`, `agent.go` `New` | shipped |
| `state.SessionStore` / `SkillStore` / `MemoryStore` | `state` in-memory set, `storage/sqlite`, `skill.Store`, `memory.Store` | via `state.Provider` | shipped |
| `spine.Log` | `spine.MemoryLog`, `storage/sqlite` event log | `store.Log()` at every entry point | shipped |
| `spine.SnapshotCodec` | `chain.SnapshotSealer` | `cmd/flynn/spine.go` | shipped |
| `resource.Store` | `resource.NewMemory`, `storage/sqlite` resource store | `governed/assemble.go` | shipped |
| `resource.SchemaCompiler` / `Validator` | `resource.builtinCompiler`, `resource.Registry` | `resource` kind registration | shipped |
| `jobs.Queue` / `Waker` | `jobs.MemoryQueue`, `storage/sqlite` job queue | `store.Jobs()`, `agent.go` | shipped |
| `llm.Model` | `llm/anthropic`, `llm/openai`, local serving (`localmodel.Host` starts the server, `llm/openai` talks to it) | `provider.Resolve`, `cmd/flynn/modelrun.go` `newLocalRunner` | shipped |
| `sandbox.Sandbox` | `sandbox.Local`, `sandbox.MicroVM` (`BootMicroVM`); `sandbox.Remote` (`NewRemote`) is the adapter for a host-supplied `sandbox.Transport` and is unused until one exists | `sandbox.NewLocal` at each assembly | shipped |
| `observe.Logger` / `Tracer` / `Meter` | `observe.slogLogger`, the `Nop` set | `observe.Default`, `cmd/flynn/main.go` | shipped |
| `secret.Source` | `secret.EnvSource`, `secret.chain`, vault store | `cmd/flynn/main.go` `credentialSource` | shipped |
| `dispatch.Admitter` | `capability.Admitter`, `dispatch.AllowAll` | `governed/assemble.go`, `governed/learning.go` | shipped |
| `allowance.Policy` | `allowance.Actions` | `governed/allowance.go`, from `--irreversible` | shipped |
| `clock.Clock` / `Timing` | `clock.System`, `clock.Manual` | everywhere a clock is taken | shipped |

`allowance.Policy` marks nothing until the operator names an action with
`--irreversible`. Dispatch sees an action's name and never its arguments, and one name
covers both a command that lists a directory and one that deletes data, so the binary
cannot work out which actions are irreversible on its own.

A host that wants a model running on the same machine uses `localmodel` and needs no
port for it. `localmodel.Open` takes a data directory and returns a host that
classifies a source, installs a runtime behind the advisory gate, fetches weights
against a catalog digest, serves them inside the sandbox on a loopback port behind a
per-launch token, and stops them. The guards have no switch. A model server a host
starts itself has none of them, so the package accepts no server binary, address,
chat template, runtime build or lower floor. The served runtime keeps outbound network
access under the local sandbox, so the loopback pin and the token keep the endpoint
private.

The host decides three things, and each can only tighten. `RaiseFloor` raises the
advisory floor for one `Host`; it never lowers, and does not change another `Host`.
`Config.MinContainment` states the least isolation the host accepts (kernel
confinement unless it says `"process"`), and `Serve` refuses a sandbox that provides
less and reports the level it launched under. A server found already running is held
to the same floor and isolation as a new one, and stopped if it falls short. Before the
token is sent, the listener must refuse a request with no credential and one with a
made-up credential, and accept one with a probe key only the launched runtime was
given.

## The goal reconciler

The reconciler takes its judgments as options. An unset option changes what a run can
do.

| Port | Producer | Wired at | Verdict |
|---|---|---|---|
| `goal.StepExecutor` | `mission.Executor`, `driver.Router` | `governed/assemble.go` `Assemble` | shipped |
| `goal.StopEvaluator` | `mission.Convergence` | same | shipped |
| `goal.Planner` | `mission.Planner` | same, planning runs only | shipped |
| `goal.ProgressProbe` | `progress.SpineProbe` | same | shipped |
| `goal.InvariantAuditor` | `evidence.CommandAuditor` | same; the fan-out build too | shipped |
| `goal.SteerJudge` | `evidence.ModelSteerJudge` | same; the fan-out build too | shipped |
| `goal.Halter` | `brakes.Hook` | same brake the executor dispatches under; the fan-out build too | shipped |
| `goal.RefusalProbe` | `refusal.SpineProbe` | same | shipped |
| `goal.ItemVerifier` + `goal.Evidence` | `evidence.CommandVerifier`, `evidence.SpineEvidence` | same (planning runs); the fan-out build always | shipped |
| `goal.UnitSpawner` | `orchestration.UnitFanout` | `governed/assemble.go`, `agent.go` | shipped |
| `goal.Cleaner` | none | n/a | justified |
| `goal.WindowSource` | none | n/a | justified |
| `orchestration.Governor` | `dispatch.Dispatcher` (via `orchestration.UnitGovernor`) | `governed/assemble.go`, `agent.go` | shipped |
| `runtime.Config.RequireLedgerProof` | `goal.EvidenceGate`, built by `runtime.New` | `governed/assemble.go` behind `governed.WithLedgerProof` (`--require-proof`); the fan-out build always, for a unit's child | staged |

`RequireLedgerProof` is staged:

- **Switch:** `--require-proof` on `flynn goal`. It is always on for a unit's child: a
  unit settles from its child's ledger, and a child that converged on the model's word
  alone would fail the unit as unproven.
- **Promotion condition:** it becomes the default on a host whose sandbox can contain
  semi-trusted work, once a planned run proves every ledger item through an executed
  check. Where a check cannot run, turning it on would stall every planned goal for a
  reason unrelated to the goal.

`TestThePromotionConditionForLedgerProof` plans a goal whose item carries a check that
exits 0, drives it through the binary's assembly, and reads the verdict. It runs on
every CI run:

| Platform | Sandbox containment | The planned item |
|---|---|---|
| `windows-latest` | kernel-confined | proven by its executed check |
| `macos-latest` | kernel-confined | proven by its executed check |
| `ubuntu-latest` | process-jail | unproven: `containment_gate: containment_insufficient` |

An item's check is model-authored, so it runs as semi-trusted work. GitHub's Ubuntu
runner forbids unprivileged user namespaces, the sandbox falls back to a process jail,
and the containment gate refuses the check before it runs. Where the host can contain
the work, every planned item was proven.

The open decision: make the default depend on the host's containment (a run already
reports which mode it is in), or leave `--require-proof` as the operator's switch.

Verification itself is always on. On every planned goal, each item's check runs in the
run's sandbox and its verdict goes on the record. The flag adds the refusal that reads
those verdicts. A run prints which of the two applies before it starts (`ledgerLine`
in `cmd/flynn/run.go`).

`goal.Cleaner`: a nil cleaner means there is nothing external to tear down, which is
true of the standalone binary. Child goals are removed through owner references. A
delete with no cleaner completes instead of waiting on a finalizer
(`TestGoalDeletionCompletesWithNoCleaner`).

`goal.WindowSource`: a plan window is a quota a host meters, and Flynn has nothing
equivalent to read. Every other spend bound (step budget, token and cost ceiling) works
without it. A goal that declares no `WindowFraction` runs without a source. A goal that
declares one, on a reconciler with no source, stalls with `WindowSourceMissing` and
names the unmeasured ceiling. Otherwise a run that finished unchecked would look the
same as one that stayed inside its ceiling.

`goal.UnitSpawner` and `orchestration.Governor` are wired from one call,
`orchestration.Units(spawner, orchestration.UnitGovernor(...))`, in the fan-out assembly
and in the `agent` facade. A unit settles from its child's ledger, so the fan-out
assembly wires `ItemVerifier`, `Evidence` and ledger convergence for every goal on that
path. Without them a child would converge as soon as the model said it was done, and
the unit would fail as unproven.

## The conversation executor

| Port | Producer | Wired at | Verdict |
|---|---|---|---|
| `mission.Tool` | `tools` set, `tools/github`, extension tools | `mission.WithTools` | shipped |
| `mission.ResultSummarizer` | `tools` bash, glob, grep, read | implicit on the tool | shipped |
| `mission.Fanout` | `orchestration.Spawner` | `governed/assemble.go`, `agent.go` | shipped |
| `mission.Reporter` | `session.reporter` | `mission.WithObserver` | shipped |
| `mission.ApprovalPrompter` + `approval.Gate` (with `approval.Policy`, `approval.Signer`, `approval.NonceStore`) | `cmd/flynn.approvalPrompter`, `approval.Requirements`, `spinesink.ApprovalSink`, `approval.Ed25519Signer`, `approval.MemStore` | `governed/approval.go`, wired at every assembly | shipped |
| `mission.GenerationRecorder` | `nopGenerationRecorder` only | n/a | justified |
| `brakes.Switch` | `brakes.MemSwitch` | `governed/assemble.go`, wired at every assembly | shipped |
| `brakes.AnomalyDetector` | none | n/a | justified |

`approval`: the operator chooses which actions pause with `--require-approval <action>`,
repeatable and empty by default. A run's standing controls are its capability grant
and its sandbox, which apply on every path. Approval is an extra gate for the actions
an operator wants to see first. A default that paused something would deadlock every
run with nobody to ask.

A run with a policy and no prompter refuses the listed action. The interactive shell
installs the prompt; every other path (a one-shot `flynn goal`, a served conversation)
has the gate without one. Every decision, granted or denied, is recorded on the run's
stream as an `approval.decision` event and sealed with the rest of the run.

`mission.GenerationRecorder`: the only implementation discards. Nothing in the binary
calls `mission.WithSampling`, so every model call is unpinned and a recorder would
write the same empty envelope (no seed, no temperature) on every call. Pinning the
sampling would change that, and it comes first; the reason is on
`mission.GenerationRecorder` itself.

`brakes.AnomalyDetector`: the default is no detector. The configured breakers still
detect in process, so an absent detector narrows what triggers a halt and does not
remove the halt.

## Learning and memory

| Port | Producer | Wired at | Verdict |
|---|---|---|---|
| `learn.Distiller` | `learn.ModelDistiller` under `NewGovernedDistiller` | `governed/learning.go` `Distiller` | shipped |
| `learn.Verifier` | `learn.SandboxVerifier` under `NewGovernedVerifier` | `governed/learning.go` `SkillVerifier` | shipped |
| `memory/consolidate.Distiller` | `memory/distil.ModelDistiller` under `NewGoverned` | `cmd/flynn/memory_cmd.go` `consolidateDistiller` | shipped |
| `memory/digest.Builder` | n/a, the digest is Flynn's own | `governed/memory.go` `NewMemoryStack`, built at wake | shipped |
| `memory/digest.Pusher` | `memory/ridealong.Surfacer` | `digest.New`'s default, via `newMemoryStack` | shipped |
| `memory/curate` write policy | `curate.Wrap` | `governed/memory.go` `NewMemoryStack` | shipped |
| `memory/guard.PromotionReader` | every memory store | via the store | shipped |
| `memory/ridealong.Surfacer` (pull) | `memoryStack.skillNotes` behind `skilltool.Notes` | `governed/memory.go`, into `skilltool.New` at `learning.go` and `repl.go` | shipped |
| `memory/ridealong` anchors | `state.SkillAnchor`, written by the curator from `Outcome.SkillsRead` | `learn.Curator.Curate` | shipped |

`learn` is the pattern: the interface stays a port, the model-backed implementation sits
beside it, a governed wrapper sends its model call through `dispatch`, and the binary
wires the pair.

`memory/consolidate` keeps no model; writing a lesson is a language judgment.
`memory/distil` is the sibling that has one, as `evidence` is to `goal`. The pass runs
end to end over a temp database in `storage/sqlite/distil_test.go`. The binary runs it
as `flynn memory consolidate`, a separate command: distilling past failures spends
model calls on material the current run is not about, and running it inside a session
would charge that session.

The rest of the memory stack is assembled in one place, `governed/memory.go`.
The digest offers what the write path curated: a subject whose fact was superseded has
one current answer, while the raw store holds every version. A digest over an
uncurated store would push contradictions at every reader.

`memory/ridealong` is the digest's pusher. A memory pushed to a reader is counted, and
the run's scope is marked in the same step; the decay policy reads those counts.

The pull side uses `skill_read`. A lesson is anchored to the skills its run loaded, and
loading a skill brings up what was learned while working from it, framed as background
and counted as a use. Flynn issues the skill id and owns `skill_read`, so this works
with no host. `cmd/flynn/ridealong_wiring_test.go` runs it over two missions and one
store.

Skills are the only anchor. What someone learned the last time they applied a
procedure is what the next person applying it wants, and they have no query to ask for
it. A file path is a weaker anchor, and a run id anchors a lesson to a run that never
reads it again.

`state.AnchorKindSkill` is the one anchor kind this codebase names. An anchor is still
an opaque `{Kind, ID}` pair that nothing resolves, and every other kind's vocabulary
belongs to the host. A skill is a row in Flynn's own store under an id Flynn issued.

The anchor is written from the skills the run loaded, not the ones it was offered: the
run chose to load them, and reinforcement already reads that list. A run that loads
five procedures anchors its lesson to all five, and the surfacing cap limits what a
reader sees.

## Channels, extensions, external agents

| Port | Producer | Wired at | Verdict |
|---|---|---|---|
| `inbox.Source` / `Sink` | `internal/source/signalcli`, `internal/source/telegram` | `cmd/flynn/serve.go` `serveChannels` | shipped |
| `inbox.Enqueuer` | `jobs` queues | `inbox` ingest | shipped |
| `driver.Driver` | `driver.defaultDriver`, `singleShotDriver`, `externagent.Driver` | `driver.NewRouter` | shipped |
| `externagent.Spawner` / `Adapter` / `Process` | `SandboxSpawner`, `Claude`, `Codex` | `cmd/flynn/externagent.go` | shipped |
| `extension.Resolver` / `Launcher` / `ToolSource` | `ReleaseResolver`, `SourceResolver`, `SandboxLauncher`, `Loader` | `governed/extensions.go`, `cmd/flynn/extensions.go` | shipped |
| `extension.HostSigner` / `SignerChannel` | `RoutedSigner`, `mcpSignerChannel` | `cmd/flynn/extensions.go` | shipped |
| `extension.HostFetcher` | `HTTPHostFetcher` | `cmd/flynn/extensions.go` | shipped |
| `controlplane.Authenticator` | `TokenAuthenticator`, `PossessionAuthenticator`, `DelegationAuthenticator`, `DenyAll` | `cmd/flynn/serve.go` | shipped |
| `controlplane.SeedVault` | vault store | `cmd/flynn/spine.go`, `playbook.go` (`LoadOrCreateIdentity`) | shipped |
| `chain.RootSigner` / `SnapshotSigner` | `chain.Ed25519RootSigner` | `cmd/flynn/spine.go` | shipped |
| `chain.NodeStore` / `CheckpointStore` | `storage/sqlite` merkle store | durable record path | shipped |
| `harness.ProfileSource` | `harness.StaticProfiles`, `internal/profilestore.Source` | `cmd/flynn/modelrun.go` | shipped |

## Other ports

| Port | Producer | Wired at | Verdict |
|---|---|---|---|
| `bus.Bus` | `bus.NewMemory` | `runtime.New`, built alongside the store when none is supplied | shipped |
| `dispatch.EventSink` | `internal/spinesink.Sink`, `dispatch.MemorySink`, `dispatch.DiscardSink` | `governed/assemble.go` at every assembly | shipped |
| `dispatch.Hook` | `brakes.Hook`, `budget.Hook`, `approval.Gate`, `capability.ContainmentGate` | `governed/assemble.go` | shipped |
| `inbox.Worker` | `cmd/flynn.goalWorker` | `cmd/flynn/serve.go` | shipped |
| `externagent.Recorder` | `cmd/flynn.attestedSink` | `cmd/flynn/externagent.go` | shipped |
| `extension.Point` | `extension.Registry`'s registered handlers | `governed/extensions.go`, `cmd/flynn/extensions.go` | shipped |
| `governed.ToolBridge` | `extension.Loader`, through `Tools()` | `governed.WithExtensions` | shipped |
| `extension.Conn` | `extension.sessionConn` over a sandbox session | `extension.SandboxLauncher` | shipped |
| `sandbox.ContainerDriver` | `sandbox.NewContainerDriver` over an `OCIEngine` | `RegisterContainerDriver` at init, for Docker and Podman | shipped |
| `sandbox.Machine` / `Driver` / `Serving` | `sandbox.commandMachine`, the per-platform drivers registered in `init`, `sandbox.containerServing` | `sandbox.BootMicroVM`, container exec | shipped |
| `sandbox.Transport` | none (only the test fake in `sandbox/remote_test.go`) | `sandbox.NewRemote` | justified |
| `memory/hybrid.Embedder` | `llm/embed.Client` over any OpenAI-compatible embeddings endpoint | `cmd/flynn/embedder.go`, behind `FLYNN_EMBED_MODEL` | staged |

`sandbox.Transport`: a remote sandbox backend (E2B, Daytona, Modal) is an account the
operator has and Flynn does not, so Flynn includes no transport and the binary never
builds a `Remote`. `Remote` is the adapter a host's transport plugs into. It adds its
own default-deny path check on top of whatever the backend enforces. With no
transport, the binary uses the local and container backends.

`memory/hybrid.Embedder` is staged. `llm/embed.Client` speaks the embedding wire format
that hosted providers and local servers share, and `newMemoryStack` wraps the durable
store in `hybrid` when an embedding model is named.

- **Switch:** `FLYNN_EMBED_MODEL`, a provider:model spec resolved through the same
  credential chain as the chat model. Unset by default, which ranks recall by words. It
  is set per install, not per run, because a corpus is embedded under one model.
- **Promotion condition:** it becomes the default when the catalog has an embedding
  model Flynn provisions and serves itself, so ranking by meaning needs no account and
  no endpoint. Before that, every install without an API key would print a notice about
  a capability it cannot have.

`/memory` says what recall is ranked by and names the embedding model if there is
one. A spec that does not resolve is reported once and dropped: the run keeps its
memory and ranks by words, and `hybrid.Store` says so with `ErrNotFused`, as the plain
store does.

## Optional capabilities

An interface a producer may also implement, found by type assertion instead of being
injected. Not implementing one is supported; the table gives the fallback.

| Capability | Implemented by | Fallback when it is not |
|---|---|---|
| `chain.FlushNodeStore` | the durable node store | a checkpoint is signed without forcing buffered nodes first, so a crash can leave proof nodes short of the size the checkpoint claims |
| `resource.KeyLister` | both bundled backends | the reconcile resync lists records instead of keys, copying every record of the kind to enqueue names |
| `resource.AnyScopeGetter` | a backend with a name index | the caller falls back to `ListAll` and a name scan |
| `sandbox.Contained` | `sandbox.Local` with confinement, the container and microVM backends | the sandbox is treated as the weakest tier, so an unknown tier is never assumed to contain more than it proves |
| `mission.TrustedWork` | the shell tool and every tool that runs model-authored content | the tool is the agent's own trusted code and runs at any tier |
| `extension.SelfPolicing` | `extension.RoutedSigner` | the host signer signs whatever it is handed, and the decision about what may be signed is nobody's |

## Interfaces that are not ports

- `bus.Subscription`, `clock.Timer`, `observe.Span`, `observe.Counter`,
  `observe.Histogram`: handles a port returns. Whoever implements `bus.Bus`,
  `clock.Timing` or `observe.Meter` implements these with it.
- `reconcile.Reconciler`: Flynn implements it and the reconcile loop consumes it.
  There is nothing for a host to supply.
- `controlplane.Watcher`: a read model Flynn implements (`PollWatcher`) and does not
  depend on. The served watch endpoint tails the resource stream directly. The
  interface gives an embedder list, get and watch as one read model; a streaming
  transport would be a new implementation of it.

## Host deferrals without an interface

Places where a doc comment leaves the work to whoever embeds Flynn, with no port.

| Deferral | What Flynn does alone | Verdict |
|---|---|---|
| `internal/archetype`: an Agent declares capabilities by name, resolving them to implementations is the host's job | the binary resolves its own toolset against the declared names | justified |
| `archetypes/review`: an empty model field defers to the host's configured model | the binary's configured model applies | justified |
| `state.Scope` levels (instance, project, workspace) | defined in Flynn's own terms, not a host's | justified |

## The governed assembly

A run is governed when its sandbox, grant, dispatch hooks, spine sink, auditors and
probes are composed together. One function does that, `governed.Assemble`, and
`cmd/flynn` is one caller of it. `TestBinaryDoesNotComposeAGovernedRun` in
`internal/rigor` fails the build if the binary starts building hooks or a sink itself,
so an embedder and the binary cannot drift apart. The root `agent.Config` reaches the
same function. It takes the host's `Resources`, `Jobs` and `Log` (all three or none), a
`Sandbox`, `Secrets`, `Prompter`, `Reporter`, `Extensions`, `ExternalAgent`, and a
`Policy` holding the list-valued settings (`Tools`, `RequireApproval`, `Waivers`).
The lists sit behind a pointer so `Config` stays comparable. `GoalResult`, beside
`Goal`, returns the run id, the stop reason and the record along with the text.

The defaults are the governed ones: a zero option set gets the sandbox with its
containment gate, the approval gate, the brakes and the budget, the spine sink, the
auditor and the probes. A run gets less only through `governed.Waive(guard, reason)`.
The package exports no `Without...` function, a test scans for one, and a waiver with
no reason is refused. Each waiver is written to the run's stream as a
`governance.waived` event, and `Assemble` then writes a `governance.posture` event
listing the guards the run has and the budget and brake limits in force, so the sealed
record carries the posture and a reader does not depend on the host's account of it.
Both are written before `Assemble` returns, so they precede the run's first action on
every path that starts one (`Run.Submit`, `Run.Resume`, `Run.Do`, or a caller driving
`Run.Runtime` itself), and they precede `session.started`: a reader looks for the
first session event, not the first event. The spend ceiling from `WithBudget` is
opened in `Assemble` for the same reason.

A nil or zero option never means no guard. A nil sandbox, a nil model without an
external agent, an unrestricted grant on `WithToolset` and an approval policy with no
prompter each refuse by name or deny; `TestOptionsNeverSilentlyWeaken` and
`TestApprovalWithoutAPrompterDenies` hold them. The approval gate is always installed
and passes everything when the policy is empty, so the posture records
`approval: true` with an empty action list. An external agent loop has no approval gate
on its path, so its posture records `approval: false`. A `Kit`'s dispatcher refuses a
call whose context carries no grant, where the capability package alone would admit
it.

`WithApproval` and `WithAllowance` refuse a name the run's grant does not offer: a gate
on an action that never occurs looks like protection and gives none. An external agent
loop also refuses `WithCapabilities` and `WithExtensions`, since it brings its own
tools and the grant could not bound them. `Config.Driver` with `Policy.Tools` is
refused because the driver would be dropped. A resumed run opens the budget pool its
first assembly created, so the posture states that pool's limits, not the ones passed
to the later `WithBudget`.

The assembly takes the grant, the dispatch hooks, the approval policy, the egress
allow-list and the sandbox policy only from what the host passes in or from
host-owned configuration (the binary's flags and environment). It reads no policy from
the workspace, which the agent can write. The one file the operator hands the run is
`--goal-file`, read once from the path the operator named, before the run starts.
`TestWorkspacePolicyFilesDoNotWidenTheGrant` writes a policy-shaped file into the
workspace and shows the grant and posture are unchanged.

For an embedder: a run built from its own `tools` set (`WithToolset`) has no working
tree to confine, so it states `Waive(GuardSandbox, ...)` and the waiver is recorded. A
host-supplied sandbox is the host's to close; a sandbox the assembly built is closed
with the run.

## Standalone acceptance

The rows above say a producer exists and is wired. `cmd/flynn/standalone_acceptance_test.go`
tests that each capability works from the binary, one pass per capability, over a temp
data directory with a scripted model and no host. The store is SQLite on disk, the
sandbox is the local one with confinement, every action goes through `dispatch`, and
the record is the run's own spine. Only the model is scripted.

| Capability | Pass |
|---|---|
| A goal runs, acts through the sandbox and converges | `TestStandaloneAGoalRunsThroughTheSandboxAndConverges` |
| A stated term is audited, and a breach stops the run | `TestStandaloneAStatedTermIsAuditedAndABreachStopsTheRun` |
| A goal carrying a unit graph dispatches its units as governed children | `TestFanoutAssemblyRunsAUnitGraph` |
| A converged run is distilled into a skill and a memory, with the skill's check run in the sandbox | `TestStandaloneAConvergedRunIsDistilledWithItsCheckRun` |
| A session wakes with a digest, and the push is counted | `TestStandaloneASessionWakesWithADigestAndCountsThePush` |
| A repeated failure accumulates as a series and consolidates into a lesson | `TestStandaloneASeriesConsolidatesIntoALesson` |
| A run seals and its record verifies from the store alone | `TestStandaloneARunSealsAndItsRecordVerifies` |
| The ride-along closes over two runs and one store | `TestRideAlongClosesTheLoopAcrossTwoRuns` |

The invariant pass starts from a goal spec file (`--goal-spec`), the way an operator
sets `goal.Spec.Invariants`, and asserts that the breach names the term. It assembles
and submits the way `flynn goal` does but skips `drive`, which cancels the run context
as soon as the conversation converges and would race the audit. Everything else on the
path is the binary's own.

### Stating the terms of a run

`--goal-spec <file>` takes JSON. It is the only way to set `goal.Spec.Invariants` from
the command line:

```json
{
  "objective": "upgrade the http client and keep the suite green",
  "stopCondition": "the client is upgraded and the suite passes",
  "invariants": [
    {
      "id": "public-api",
      "statement": "the exported API of ./client does not change",
      "check": "./dev/apidiff ./client"
    }
  ],
  "allowances": [{"action": "shell", "target": "prod"}]
}
```

A check is a shell command, and quoting one inside a flag is error-prone, hence a
file. The file decodes into its own struct, not `goal.Spec`: the model, step budget,
grant and unit graph come from the run or from flags. Unknown fields are refused, so a
typo such as `"invariant"` fails instead of producing a run held to nothing.

A term with no `check` is judged by the model auditor from the run's record, the weaker
of the two auditors. Before it starts, the run prints which terms have a check.

A check runs as semi-trusted work and needs a sandbox that can contain it. On a host
with only a process jail (a GitHub Actions runner, which forbids unprivileged user
namespaces) the containment gate refuses it, the auditor reports `Terminal`, and the
run stops saying the check could not run. `TestTheAuditorRunsAChecksCommandInTheRealSandbox`
runs the wired auditor against the real sandbox and accepts either answer the host can
give.

A host can find out before a run whether it can contain confined work.
`flynn sandbox check` (and [`sandbox.CheckConfinement`](../sandbox/readiness.go) for a
host embedding Flynn) starts a real confined no-op child and returns a
[`sandbox.Readiness`](../sandbox/readiness.go): a pass, or a named cause (a service
unit's `RestrictNamespaces=`, disabled user namespaces, an AppArmor user-namespace
restriction, a syscall filter) with the change that fixes it and the container or
microVM tier as the alternative. A Local that asks for confinement runs that check
before its first launch and refuses with the same result. No flag, environment
variable or configuration overrides a refusal, and an outcome the check cannot
classify refuses.

The default is fail closed. `WithDefaultConfinement` never runs a command unconfined:
a host that cannot confine gets a refusal from `Exec`, `Capture`, `Stream`, `Serve` and
`Session`. Running at the process-jail floor is a separate choice the embedder makes in
code, `WithBestEffortConfinement(notice)`. Each launch it lets through is counted
(`ConfinementFallbacks`) and passed to the notice, which goes to standard error when
nil. It is never offered for a Local that denies the network, governs egress, hides
paths or holds a read grant the floor cannot honour.

An embedder hides a runner's own files from the commands it launches with
`WithUnreadablePaths`: the paths read as empty inside the sandbox on Linux and are
denied on macOS. A Local that cannot do this (on Windows, without `WithReadOnlyFS`, or
with a grant under a hidden path) refuses with `ErrReadDeny`, and
`sandbox.ReadDenySupported` reports the platform's answer before a launch refuses.
The binary applies it, with `WithNetworkDenied`, to the shell of `flynn run`,
`flynn serve` and the goal worker: the data directory, the runner's runtime directory
and the account's credential locations are hidden, and the command has no network.
`--allow-network` and `--allow-credential-reads` lift each, and the run's stream
records which. On Windows the binary sets
`ShellPolicy.AllowCredentialReadsWhereUnhidable`, so the shell runs with the
credentials readable and the posture event names the platform as the reason;
`--require-credential-hiding` keeps the refusal. An embedder that builds its own Local
gets none of this unless it asks, and the zero `ShellPolicy` refuses on Windows.

`deploy/systemd/flynn-runner.service` and `deploy/apparmor/flynn` are the unit and
profile that let the sandbox run on a hardened Linux host; `deploy/README.md` explains
each line.

## How this list is derived

Membership is mechanical. Only the verdict is a judgment.

1. Exported interfaces in the public packages (every package outside `internal/`,
   excluding `_test.go` files), together with the named types in the module whose
   method set covers each one.
2. Every `WithX` option and `Config` field whose absence changes what a run can do.
3. Doc comments that hand work to a host or a caller (the last table).

A producer counts as wired only when a non-test call site in `cmd/flynn`, `agent.go` or
`runtime` passes it in. An implementation that exists and is never passed in is a gap.

`internal/portregister` checks this in CI. Every exported interface in the public
packages must be accounted for here, the register may not name a package that no
longer exists, and a `gap` row must say what to build. It does not judge verdicts.

An interface counts as accounted for if it is named anywhere in this document, not only
in a row's first cell. Several interfaces make up one port (the approval stack is
four), and they share a row.
