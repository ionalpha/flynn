# Testing architecture

Flynn treats testing as a first-class part of the design, not an afterthought.
The runtime is built from small ports (interfaces) over an injectable clock, seeded
inputs, and an immutable event log — and that architecture is what makes rigorous
testing *cheap*. A failure reproduces from a seed; chaos is just an adapter; a
golden test is a diff of the event spine.

Shared infrastructure lives in [`internal/testkit`](internal/testkit). Write a
generator or a fault plan once, reuse it in every package.

## The tiers

| Tier | Tool | What it buys us |
| --- | --- | --- |
| **Unit** | stdlib `testing` + [`go-cmp`](https://github.com/google/go-cmp) | Plain behavior checks; `cmp.Diff` for readable struct/stream comparisons. |
| **Property-based** | [`rapid`](https://github.com/flyingmutant/rapid) | One property over generated inputs replaces dozens of hand-written cases; failures shrink to a minimal reproducer. Generators for the core types live in `testkit/gen.go`. |
| **Chaos / fault injection** | `testkit` over the ports | `FaultPlan` + `FaultyHandler`/`FaultySink` inject deterministic faults into the dispatch ports, proving the system degrades and recovers cleanly. No framework — the ports *are* the seam. |
| **Determinism / replay** | `clock.Manual` + `go-cmp` | The same scenario under a manual clock yields byte-identical event streams; behavior changes surface as spine diffs (`testkit.DiffEvents`). |
| **Invariants** | `testkit` assertions | Reusable checks: `RequireLifecycle` (every action is start+end or a single reject), budget-never-exceeded, no-action-without-a-capability (added as the governor lands). |
| **Race** | stdlib `-race` | The concurrent dispatcher/orchestrator runs under the race detector in CI. |
| **Model-based / DST** | `rapid` state machines | Long randomized action sequences against a model, checking invariants after every step (e.g. the spine log) — the deterministic-simulation tier, no extra dependency. |
| **Golden / snapshot** | `testkit.Golden` + go-cmp | A whole output (a replay, a rendered spec) compared against a `testdata/*.golden` JSON fixture; `-update` regenerates it, so large outputs need no hand-written expected value. |
| **Fuzzing** | stdlib `go test -fuzz` | Arbitrary-input targets on the error model and the dispatch→spine payload mapping; seed corpora run in CI, deep fuzzing on demand. Expands to parsers/manifests/protocol messages as they land. |

## Deferred (planned, not yet wired)

- **Deterministic concurrency** — `testing/synctest` (a fake-clock "bubble" with
  deterministic goroutine scheduling) lands when the module moves to Go 1.25, where it
  is GA. It will replace sleep-based concurrency tests.
- **Benchmarks** — stdlib `testing.B` + `benchstat` for dispatch and spine overhead.
- **Mutation testing** — a CI job to verify the suite actually catches injected bugs.

(We evaluated `gosim` for full goroutine/disk/network simulation; it is unmaintained, so
we take the idea — model-based DST on our own primitives — not the dependency.)

## Dependencies

Test-only, pure-Go, permissively licensed, and actively maintained:

- `pgregory.net/rapid` — property-based testing (MIT).
- `github.com/google/go-cmp` — value/stream comparison (BSD-3).

Neither ships in the `flynn` binary — nothing in the binary's import graph reaches them.
