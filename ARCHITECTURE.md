# Architecture

This document maps Flynn: the layers packages sit in, the ports a host can replace,
the invariants that must hold, and the stability tiers that say what a host may rely
on. Package doc comments are the reference for a single package (`go doc ./<pkg>`).

Where a rule here is enforced by golangci (`depguard`, `forbidigo`) or a test, the
rule says which.

## Overview

Flynn is a controller-reconciler engine over an event-sourced resource store, the
model Kubernetes uses. State of record is in `resource`: an append-only event log
with resources built from it. A `reconcile.Manager` runs a rate-limited work queue per
resource kind and drives each resource toward its desired state. `goal` is the first
such controller, and `mission` is the executor it runs, advancing a goal as a
tool-using conversation through the `llm` port.

Every model call and tool call passes through `dispatch`, which checks it against a
capability grant, traces it, and brackets it with events on the spine (the event
log). Time, identity and randomness come only from injected ports (`clock`, `ids`),
which makes a run replayable. Persistence and observability are reached only through
interfaces: the open agent runs standalone, and a host can supply its own backends
without the engine importing the host.

## Layers

Packages form an acyclic dependency graph (`go vet` reports no import cycles). A
package may import only packages in a lower layer. A package's layer is one plus the
highest layer it imports.

| Layer | Packages | Role |
|-------|----------|------|
| L0 foundation | `clock`, `fault`, `llm`, `observe` | Pure ports: time source, error taxonomy, the model port, the logging/tracing port. No internal imports. |
| L1 primitives | `spine`, `hlc`, `ids`, `sandbox`, `bus`, `llm/anthropic`, `llm/openai` | Event-log port, hybrid logical clock, seeded id generator, isolation port, in-process event bus, concrete model adapters. |
| L2 core data | `state`, `resource`, `provider` | The host persistence interfaces, the event-sourced resource store, the model-adapter registry. |
| L3 mechanisms | `dispatch`, `reconcile`, `jobs`, `memory`, `skill`, `chain` | The governance gate, the reconcile loop, the leased job queue, durable memory and skill stores, the verifiable-record encoder and signer over spine events. |
| L4 governance + domain | `capability`, `budget`, `internal/spinesink`, `goal`, `learn`, `storage/sqlite` | Capability grants, the per-run spend ceiling, the dispatch-to-spine sink, the goal controller, the learning loop, the SQLite backend. |
| L5 orchestration | `mission`, `runtime` | The conversation executor, the wired-up runtime. |
| L6 composition | `tools`, `session` | The default toolset, the conversational session/stream front door. |
| L7 entry | `cmd/flynn`, root `agent` | The binary and the embedding facade. |

The rule that counts is direction: a primitive never imports a domain package.
`depguard` enforces this for the core bands (see Invariants).

### Import direction and public API are separate controls

- **Which package may import which** is set by the `depguard` layer rules above.
  Folders cannot express import direction, so packages are not nested under an
  `infra/`-style taxonomy. The layering is in the linter.
- **What the module promises importers** is set by `internal/`. Every exported
  package in a public Go module is API, and `internal/` is the one language tool that
  takes a package out of that promise. The code stays readable on GitHub; it just
  cannot be imported.

The top level holds what an embedder should see: the root `agent` facade, the ports a
host implements or consumes, and the product's domain packages. The machinery they are
built from (parsers, guards, stores, probes, adapters) is under `internal/`, one level
flat. Within each band packages stay flat. The only nesting is families of
alternative implementations of one port (`llm/*`, `storage/*`).

A package is top-level if and only if:

- (a) it appears in the exported signatures of a public package. `TestPublicAPINamesNoInternalType`
  in `internal/rigor` audits every non-`internal/` package with `go/types`: an internal
  type in a public signature compiles but is broken API. Known leaks are in an
  allowlist the test lets shrink and never grow.
- (b) it is a port third parties implement (`sandbox` backends, `state` stores, `llm`
  adapters, `driver` loops, `inbox` sources), or
- (c) it is a standard's reference implementation (`chain`, for Provetrail).

Everything else goes under `internal/`.

## Ports

A port is an interface the engine depends on, so the implementation behind it can
change without touching the engine. The main ones:

- **`state.Provider`** is the host persistence interface: a factory of
  capability-scoped stores (`Sessions()`, `Skills()`, `Memory()`). The open agent
  includes an in-memory implementation and a `storage/sqlite` one, and a host can
  supply its own. Domain methods go on the sub-stores, never on `Provider`.
- **`llm.Model`** is the model port. `provider` resolves a `provider:model` string to
  a concrete adapter (`llm/anthropic`, `llm/openai`). The planned cost-aware router
  will be an `llm.Model` decorator in front of the registry.
- **`spine.Log`** is the append-only event store: a monotonic `Seq` per stream, and
  events are never changed or deleted. It also keeps stream snapshots, which bound
  the cost of a rebuild as a stream grows. Replay and audit read from it.
- **`sandbox`** runs every command. The local backend exists today; remote backends
  (E2B, Daytona, Modal) are planned as further adapters behind the same port.
- **`observe`** is the logging and tracing port. Nothing else may import `log/slog`
  (enforced); everything logs through `observe.Logger`.
- **`dispatch.Admitter`** admits or refuses every action. Budget enforcement is a
  `dispatch` hook beside it (refuse before, charge after), not a second gate.

### Every port has a producer or a written reason

A port answers whether the implementation can be replaced. It does not answer whether
Flynn can do the work when no host supplies one. Each port therefore carries a
verdict, recorded in [`docs/HOST_BOUNDARY.md`](docs/HOST_BOUNDARY.md):

- **shipped**: Flynn has an implementation and the binary wires it. A host can still
  replace it.
- **justified**: Flynn has no implementation on purpose, the reason is in the doc
  comment, and the absence is visible. A goal that states terms with no auditor stalls
  and names the missing auditor.
- **staged**: Flynn has it, the binary can wire it, and it is off by default. The row
  names the switch and the condition for making it the default.

The test: **if the `flynn` binary plus a temp SQLite file, with no host, cannot
exercise the capability, it is `justified` with a written reason, or it is a gap.**
Deferring to a host is right for a backend the host owns. It is wrong for anything
Flynn can do with the model port, the sandbox, the store and the spine.

The pattern to copy: the rule is in the domain package, the producer is in a sibling
package that may depend on a sandbox and a log, and the binary wires the pair. `goal`
with `evidence` and `progress`; `learn` with its own model distiller;
`memory/consolidate` with `memory/distil`.

The other half is host neutrality: a Flynn contract names only what Flynn owns, and
anything belonging to a host is an opaque typed ref, stored and matched, never
resolved. `internal/hostneutral` enforces this over the public packages. A contract
that names a host's records cannot be exercised without a host.

## The run

A run is the unit the engine governs. One id (a UUIDv7) names its event stream, names
its goal resource, keys its budget, and is the owner a child run points at.

- **Identity.** A run's session, goal and spine stream share one id. A run can be
  replayed and audited by that id, which is stable across restarts and unique across
  a fleet.
- **Ownership.** A resource carries `OwnerReferences`, and one controlling owner drives
  its lifecycle. A garbage collector removes a resource once its owner is gone or
  terminating, so deleting a parent removes the subtree it created. A run with no
  owner is a root.
- **Budget.** A run's spend (tokens and cost) is a durable `Budget` resource keyed by
  the run id. A `dispatch` hook charges every governed action against it and refuses
  the next one once the ceiling is reached. One pool caps a run, or a whole fan-out
  that shares it.
- **Bounded replay.** State is a fold of the event log. A snapshot is a checkpoint at a
  `Seq`: a rebuild starts from the latest snapshot and folds only the events after it.
  Snapshots are a cache; a missing one makes a rebuild slower and nothing else.
- **Attribution.** Every event carries a `Principal`: the identity on whose authority
  it was produced (which agent in a fan-out, which person in a multi-user host).
  `capability.Grant.Narrow` derives a child grant that is a subset of its parent, so
  delegation cannot widen authority.

A single run is the n=1 case of each: no owner, no budget, one principal.

## Invariants

1. **Every action goes through `dispatch`.** Every model call and every tool or command
   execution is admitted against the run's capability grant, traced, and bracketed by
   start and end events on the spine. A call that skips `dispatch` escapes governance,
   audit and replay. *(Enforcement: review. A mechanical architecture test is a
   candidate.)*

2. **Determinism: no wall clock, no ad-hoc randomness.** Time comes from the injected
   `clock.Clock`; identity and randomness from the seeded `ids.Generator`. A replay
   re-seeds and re-reads the same clock and reproduces the run. *(Enforcement:
   `forbidigo` forbids `time.Now`; `depguard` forbids `math/rand`, `math/rand/v2` and
   `crypto/rand` outside `ids`. Indirect nondeterminism, such as map order reaching
   output, is not caught by the linter and is covered by replay-equivalence tests.)*

3. **Observability only through the port.** No `fmt.Print*`, no `log/slog` outside
   `observe`. *(Enforcement: `forbidigo` and `depguard`.)*

4. **Layer direction holds.** The graph is acyclic, and the core bands (foundation,
   primitives, core data) never import the engine and domain bands above them.
   *(Enforcement: the `depguard` `layer-direction` rule covers core to engine. Edges
   between adjacent engine layers are convention.)*

5. **The host boundary is interfaces only, and it runs one way.** The engine never
   imports a concrete host; persistence and observability cross only through `state`
   and `observe`. A Flynn contract names only what Flynn owns and can verify on its
   own. A host's record is carried as an opaque typed ref: `state.Anchor` is a
   `{Kind, ID}` pair that is stored, indexed and matched, and never resolved or checked
   for referential integrity, so a ref to a deleted record matches nothing. Naming a
   host's record type in a Flynn contract would make Flynn's correctness depend on a
   record only the host can produce. The test for an acceptance criterion: if Flynn
   plus a temp SQLite file with no host cannot exercise it, it belongs to the host.
   *(Enforcement: `internal/hostneutral` fails the build when an exported name or
   serialized property in the public packages names a host record type, or when a
   ref-shaped type closes its vocabulary with a named `Kind`. The shared memory vectors
   in `memory/memorytest` require every backend to store and match an anchor that
   resolves to nothing.)*

6. **Budgets bound spend.** A run's token and cost pool is charged in `dispatch` after
   each action and checked before the next. Once the ceiling is reached the action is
   refused with a `BudgetExceeded` fault. A run with no budget is unlimited, the
   default. *(Enforcement: the budget hook in `dispatch`, property-tested, including
   concurrent charges against a shared pool.)*

7. **Process execution goes through the sandbox.** Every command and subprocess runs
   through the `sandbox` port, never `os/exec` directly. Two exceptions read no
   untrusted input: the hardware probe, which has to read the real host and runs a
   fixed command, and the terminal's editor handoff, which opens the user's own editor
   at their request. *(Enforcement: `depguard` forbids importing `os/exec` outside
   `sandbox`, `hardware` and `internal/tui/term`.)*

## Event evolution

Events are read back by newer code than wrote them (replay-to-regrade, crash resume,
audit, fleet sync). `spine.Event` carries identity fields for that:
`OriginInstanceID` (which instance produced it), `CausationID` (causal replay),
`Principal` (on whose authority), and a `SchemaVersion` discriminator.

The payload is `map[string]any`. Adding a key is safe. Renaming, removing or retyping
one needs a new `SchemaVersion` and an upcaster per (type, version) that migrates the
old payload to the current shape at read time. Upcasting must be deterministic
(invariant 2).

Snapshots keep replay bounded. A `spine.Log` stores checkpoints of a stream, and a
rebuild starts from the latest one and folds only the events after it. Events are the
source of truth; a snapshot can always be rebuilt by folding from an earlier point.

## Verifiable run records

The spine makes a run replayable, and on its own you have to trust the operator not to
rewrite it. The `chain` package turns a run into a signed, tamper-evident record that
anyone can verify, in any language, without trusting the producer. The format is
[Provetrail](https://github.com/ionalpha/provetrail), an open standard for verifiable
execution provenance, and `chain` is its reference implementation. It uses existing
standards throughout:

1. **Canonical encoding.** Each event is encoded as deterministic CBOR (RFC 8949 Core
   Deterministic Encoding): the same event always gives the same bytes, whatever the
   map order or language.
2. **Merkle log.** Each event's bytes are a leaf in an append-only Merkle log using the
   RFC 6962 tree hash (domain-separated SHA-256, also used by RFC 9162). The log gives
   inclusion proofs (an event is under a signed root) and consistency proofs (the log
   only grew between two signed roots).
3. **Signed checkpoint.** The Merkle head is signed as a COSE_Sign1 (RFC 9052)
   checkpoint with an Ed25519 key. The key id is the instance's self-certifying
   principal id, so a verifier recovers the public key from the record.
4. **Sealed record.** When a run converges it is sealed into a portable record (the
   signed checkpoint plus every event's canonical bytes) and stored on the run's own
   stream. The record is stored as captured. Re-deriving it from stored events would
   change the bytes, because the store serializes payloads as JSON.

A verifier rehashes the exact bytes the record carries and never re-serializes, so
implementations in different languages cannot disagree on encoding.
`flynn spine verify <run>` is the reference verifier. `chain/conformance` holds the
public test vectors (structural, signature and transparency tiers) that a second
implementation can be checked against.

## Stability tiers

Every exported package in a public Go module is a promise. The layout sets three
tiers:

- **Stable** (the embedding contract): the root `agent` facade and the ports a host
  implements or consumes: `state`, `observe`, `llm`, `capability`, `fault`, `tools`,
  `sandbox`, `secret`, `spine`, `clock`, `ids`, `provider`, `storage/sqlite`, and
  `chain` (external verifiers import it). Breaking one is a major-version change.
- **Domain** (`goal`, `mission`, `reconcile`, `resource`, `dispatch`, `session`,
  `runtime`, `learn`, `budget`, `memory`, `skill`, `extension`, `controlplane`,
  `orchestration`, ...): importable, and **unstable until 1.0**.
- **Mechanisms** (`internal/...`): not importable, no promise.

`dev/apidiff` (a CI job, also runnable locally) compares the exported API of the
newest release tag with the branch using `golang.org/x/exp/cmd/apidiff`, and gives
each tier its own verdict: an incompatible change to the stable tier fails, one to the
domain tier is reported, and `internal/` is skipped. A deliberate break goes in
`dev/apidiff-accepted.txt` with its reason. Direction decides the verdict: adding a
field to a struct a host reads is compatible; adding a method to a port a host
implements is not.

[docs/VERSIONING.md](docs/VERSIONING.md) explains this for importers, including how a
bad version is retracted and what a v2 would cost.

`dev/apidiff` compares shape, not behaviour. It also does not cover formats that cross
a process boundary: envelope records, chain records, the SQLite schema. Those follow
the same rule (accept more, emit less) and are covered only by the `chain/conformance`
test vectors.

## Concurrency and lifecycle

The runtime is a set of long-lived loops started with a `context.Context` and stopped
by cancelling it: the `reconcile.Manager` resync loop, the `goal.Worker` poll loop,
and the `bus` subscribers. The work queue (`reconcile.Queue`, rate-limited with
backoff, modelled on client-go) is drained and then `ShutDown()`. Whoever starts a
loop owns its lifetime and ties it to the context it was given. New long-lived
goroutines follow the same rule. The race detector runs in CI.

## Package map

- **`cmd/flynn`** the binary. `flynn` with no arguments starts an interactive session,
  `flynn goal "..."` drives one objective to a result, and `flynn inspect`, `runs` and
  `resume` replay, list and continue past runs. **Root `agent`** is the embedding
  facade: `Goal(ctx, objective)` assembles the runtime and sandboxed toolset and runs
  one objective.
- **`session`** conversational front door and event stream; **`runtime`** wires the
  controller, worker, store and bus together.
- **`goal`** the controller and worker; **`mission`** the conversation executor;
  **`reconcile`** the loop they run on.
- **`resource`** event-sourced state of record; **`spine`** the event log;
  **`storage/sqlite`** the durable backend; **`state`** the host persistence
  interfaces.
- **`dispatch`** the governance gate; **`capability`** grants; **`budget`** the per-run
  spend ceiling; **`internal/spinesink`** writes dispatched actions to the spine.
- **`chain`** verifiable records: canonical CBOR, the RFC 6962 Merkle log, COSE
  signing, sealed run records, and the verifier behind `flynn spine verify`.
- **`learn`** the learning loop (capture, verify, reinforce, regrade); **`memory`** and
  **`skill`** its stores.
- **`llm`**, `llm/anthropic`, `llm/openai` and **`provider`** the model port and
  adapters; **`tools`** the default toolset; **`sandbox`** isolation.
- **`clock`**, **`ids`**, **`hlc`** determinism; **`fault`** error taxonomy;
  **`observe`** logging and tracing; **`bus`** in-process event bus; **`jobs`** leased
  job queue.
- **`internal/...`** mechanisms: model handling (`inference`, `gguf`, `gbnf`,
  `modelformat`, `modeltrust`, `huggingface`, `hardware`, `acquire`), network and
  credential guards (`bindguard`, `vault`, `credential`), the extension engine
  (`integrations`, `catalog`, `flow`, `ops`, `service`, `playbook`, `dependency`,
  `fetch`, `source`), and the rest. Not importable; see Stability tiers.
