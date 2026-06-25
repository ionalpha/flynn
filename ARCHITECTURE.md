# Architecture

This document is the map of Flynn: the layers packages sit in, the ports
that keep the engine swappable, the invariants that must hold, and the stability
tiers that say what a host may rely on. Package doc comments remain the source of
truth for any single package (run `go doc ./<pkg>`); this file is how the pieces
fit together and why.

It is also a contract. Several rules here are mechanically enforced (golangci
`depguard`/`forbidigo`, tests). Where a rule is enforced, this document says how,
so the doc and the code cannot quietly disagree.

## The shape in one paragraph

Flynn is a controller-reconciler engine over an event-sourced resource
store, the same model Kubernetes uses. State of record lives in `resource` (an
append-only event log with materialized resources). A `reconcile.Manager` runs a
rate-limited work queue per resource kind and drives each resource toward its
desired state; `goal` is the first such controller and `mission` is the executor
it runs, advancing a goal as a tool-using conversation through the `llm` port.
Every model call and tool call passes through one chokepoint, `dispatch`, which
admits it against a capability grant, traces it, and brackets it with events on
the spine. Time, identity, and randomness come only from injected seams
(`clock`, `ids`), so a run is deterministically replayable. Persistence and
observability are reached only through interfaces, so the open agent runs
standalone and a richer host can supply its own backends without the engine
depending on the host.

## Layers

Packages form an acyclic dependency graph (verified: `go vet` reports no import
cycles). A package may only import packages in a lower layer. The layer of a
package is one plus the highest layer it imports.

| Layer | Packages | Role |
|-------|----------|------|
| L0 foundation | `clock`, `fault`, `llm`, `observe` | Pure seams: time source, error taxonomy, the model port, the logging/tracing port. No internal imports. |
| L1 primitives | `spine`, `hlc`, `ids`, `sandbox`, `bus`, `llm/anthropic`, `llm/openai` | Event-log port, hybrid logical clock, seeded id generator, isolation port, in-process event bus, concrete model adapters. |
| L2 core data | `state`, `resource`, `provider` | The host persistence boundary, the event-sourced resource store, the model-adapter registry. |
| L3 mechanisms | `dispatch`, `reconcile`, `jobs`, `memory`, `skill` | The governance waist, the reconcile loop, the leased job queue, durable memory and skill stores. |
| L4 governance + domain | `capability`, `budget`, `spinesink`, `goal`, `learn`, `storage/sqlite` | Capability grants, the per-run spend ceiling, the dispatch-to-spine sink, the goal controller, the learning loop, the SQLite backend. |
| L5 orchestration | `mission`, `runtime` | The conversation executor, the wired-up runtime. |
| L6 composition | `tools`, `session` | The default toolset, the conversational session/stream front door. |
| L7 entry | `cmd/flynn`, root `agent` | The binary and the embedding facade. |

The valuable property is the *direction*, not the count. A primitive must never
reach up into a domain package; that inversion is what turns a clean graph into a
ball of mud. The direction is enforced by `depguard` (see Invariants), so the
table cannot silently rot.

### Why flat, not nested

Packages stay at the top level rather than nested under `infra/`, `agent/`, etc.
In Go a directory *is* a package and its import identity; nesting changes import
paths and (for a public module) breaks importers, but it does not change the
dependency graph. The graph is what matters, and we enforce it directly with the
layer rule above. So the layering lives in `depguard`, not in folders, and the
top level stays scannable. The two adapter families that genuinely cluster
(`llm/*`, `storage/*`) are the only nesting, because they are alternative
implementations of one port.

## Ports (the seams that keep it swappable)

A port is an interface the engine depends on so the thing behind it can change
without touching the engine. The load-bearing ones:

- **`state.Provider`** is the host persistence boundary: a factory of
  capability-scoped stores (`Sessions()`, `Skills()`, `Memory()`), not a god
  object. The open agent ships an in-memory implementation and a `storage/sqlite`
  one; a commercial host supplies its own. Keep this small: domain methods go on
  the sub-stores, never directly on `Provider`.
- **`llm.Model`** is the model port. `provider` resolves a `provider:model`
  string to a concrete adapter (`llm/anthropic`, `llm/openai`). A cost-aware
  router will be a `llm.Model` decorator in front of the registry, so callers
  stay unaware of which model ran.
- **`spine.Log`** is the append-only event store: monotonic `Seq` per stream,
  events never mutated or deleted. It also keeps stream snapshots (materialized
  checkpoints) so a rebuild stays bounded as a stream grows. This is the substrate
  for replay and audit.
- **`sandbox`** is the isolation boundary every command execution crosses. Local
  today; remote backends (E2B/Daytona/Modal) are intended as additional adapters
  behind the same port.
- **`observe`** is the logging/tracing port. Nothing else may import `log/slog`
  (enforced); everything logs through `observe.Logger`.
- **`dispatch.Admitter`** is the governance gate every action is admitted through;
  budget enforcement composes alongside it as a `dispatch` hook (charge after,
  refuse before) rather than a second gate.

## The run and its substrate

A run is the unit the engine governs, and one stable id ties its pieces together:
that id (a UUIDv7) names the run's event stream, names its goal resource, keys its
budget, and is the owner a child run points at. Choosing one identity for all of
them is what lets runs compose into trees later without reshaping the core.

- **Identity.** A run's session, its goal, and its spine stream share one id, so a
  run is addressable for replay and audit by a single handle, stable across
  restarts and unique across a fleet.
- **Ownership.** A resource carries `OwnerReferences`; one controller owner drives
  its lifecycle. A garbage collector reaps a resource once its owner is gone or
  terminating, so deleting a parent cascades to the subtree it created. A run with
  no owner is a root: the single-run case.
- **Budget.** A run's spend (tokens and cost) is a durable `Budget` resource keyed
  by the run id. A `dispatch` hook charges every governed action against it and
  refuses one once the ceiling is reached, so one pool caps a whole run, or a whole
  fan-out sharing it, with no per-call wiring.
- **Bounded replay.** State is a fold of the event log, so a rebuild would grow
  without bound as a stream grows. A snapshot is a materialized checkpoint at a
  `Seq`; a rebuild resumes from the latest snapshot and folds only the events after
  it. A snapshot is a derived cache, never a source of truth, so a missing one is
  only slower.
- **Attribution.** Every event carries a `Principal`: the identity on whose
  authority it was produced (which agent in a fan-out, which human in a multi-user
  host), distinct from the coarse actor kind. `capability.Grant.Narrow` derives a
  child grant that is a subset of its parent, so delegation never escalates
  authority.

Each is built so a single run is the n=1 case: an un-owned, un-budgeted,
single-principal run behaves exactly as before, and the multi-everything features
(fan-out, fleet, multi-user, replay-to-regrade) insert without reshaping the core.

## Invariants

These are the rules that make autonomy safe and replay sound. Each is enforced,
not merely encouraged.

1. **Everything routes through the dispatch waist.** Every model call and every
   tool/command execution goes through `dispatch`: admitted against the run's
   capability grant, traced, and bracketed by start/end events on the spine. A
   side channel straight into the sandbox or the model defeats governance, audit,
   and replay at once. *(Enforcement: today upheld by review, and a candidate for
   a mechanical architecture test. The `learn` skill-check verifier was migrated
   onto the waist precisely to remove such a bypass.)*

2. **Determinism: no wall-clock, no ad-hoc randomness.** Time comes from the
   injected `clock.Clock`; identity and randomness from the seeded
   `ids.Generator`. A replay re-seeds identically and re-reads the same clock, so
   it reproduces the original run. *(Enforcement: `forbidigo` forbids `time.Now`,
   `depguard` forbids `math/rand`, `math/rand/v2`, `crypto/rand` outside `ids`.
   Indirect nondeterminism, such as map-iteration order leaking into output, is
   not caught by the linter and is guarded by replay-equivalence testing.)*

3. **Observability only through the port.** No `fmt.Print*`, no direct
   `log/slog` outside `observe`. *(Enforcement: `forbidigo` + `depguard`.)*

4. **Layer direction holds.** A package never imports a higher layer.
   *(Enforcement: `depguard` layer rules.)*

5. **The host boundary is interfaces only.** The engine never imports a concrete
   host. Persistence and observability cross only through `state` and `observe`.

6. **Budgets bound spend.** A run's token and cost pool is charged at the dispatch
   waist after each action and checked before the next; an action is refused with a
   `BudgetExceeded` fault once the ceiling is reached. A run with no budget bound is
   unlimited (the zero-config default). *(Enforcement: the budget hook on the waist;
   property-tested, including concurrent charges against a shared pool.)*

## Event evolution

Events are the durable truth and are read back by *newer* code than wrote them
(replay-to-regrade, crash-resume, audit, future fleet sync). `spine.Event` carries
forward-compatible identity fields chosen so those features never force a refactor:
`OriginInstanceID` (which instance produced it), `CausationID` (causal replay),
`Principal` (on whose authority), and a `SchemaVersion` discriminator.

The payload is `map[string]any`, additively forward-compatible: new keys are safe,
but renaming, removing, or retyping a key is not. That is what `SchemaVersion`
exists to make safe: an upcaster per (type, version) migrates an old payload to the
current shape at read time so new code recognises an old event instead of
misreading it. Upcasting must be deterministic (invariant 2).

Snapshots keep replay bounded as the log grows. A `spine.Log` stores materialized
checkpoints of a stream, and a rebuild resumes from the latest one and folds only
the events after it. The events stay the immutable source of truth; a snapshot is a
cache that can always be rebuilt by folding from an earlier point.

## Stability tiers

Flynn is a public Go module, so every exported package is, in principle, a
promise. Until v1.0 the promise is deliberately scoped:

- **Stable surface** (the embedding contract): the root `agent` facade and the
  ports a host implements or consumes, `state`, `observe`, `llm`, `capability`,
  `fault`, `tools`. Keep these small and guarded; breaking them is a major-version
  event.
- **Engine** (`goal`, `mission`, `reconcile`, `resource`, `dispatch`, `session`,
  `runtime`, `learn`, ...): importable and visible, but **unstable while pre-1.0**.
  These churn as the orchestration graph and router land. Power users may import
  them with that understanding.

Pre-1.0 Go semver already permits breaking changes, so the engine stays
refactorable while the top level stays visible. The decision to physically move
the engine under `internal/` is deferred to the v1.0 cut, when this tier list
says exactly what must lock.

## Concurrency and lifecycle

The runtime is a set of long-lived loops started with a `context.Context` and
stopped by cancelling it: the `reconcile.Manager` resync loop, the `goal.Worker`
poll loop, and the `bus` subscribers. The work queue (`reconcile.Queue`, a
rate-limited, backoff-aware queue in the client-go mould) is drained and then
`ShutDown()`. Goroutine ownership is explicit: whoever starts a loop owns its
lifetime and ties it to the passed context. New long-lived goroutines must follow
the same rule, so shutdown stays clean and races stay absent (the race detector
is part of CI).

## Map of the territory

- **`cmd/flynn`** the binary (`flynn goal "..."` drives a goal to a result with the
  durable store and learning loop); **root `agent`** the embedding facade a host
  imports, where `Goal(ctx, objective)` assembles the runtime and sandboxed toolset
  and runs one objective to its answer. The interactive `flynn` session is not
  wired yet.
- **`session`** conversational front door and event stream; **`runtime`** wires
  the controller, worker, store, and bus together.
- **`goal`** the controller + worker; **`mission`** the conversation executor;
  **`reconcile`** the generic loop they run on.
- **`resource`** event-sourced state of record; **`spine`** the raw event log;
  **`storage/sqlite`** the durable backend; **`state`** the host boundary.
- **`dispatch`** the governance waist; **`capability`** grants; **`budget`** the
  per-run spend ceiling; **`spinesink`** routes dispatched actions onto the spine.
- **`learn`** the closed learning loop (capture, verify, reinforce, regrade);
  **`memory`** and **`skill`** its durable stores.
- **`llm`** + `llm/anthropic`/`llm/openai` + **`provider`** the model port and
  adapters; **`tools`** the default agentic toolset; **`sandbox`** isolation.
- **`clock`**, **`ids`**, **`hlc`** the determinism seams; **`fault`** the error
  taxonomy; **`observe`** logging/tracing; **`bus`** the in-process event bus;
  **`jobs`** the leased job queue.
