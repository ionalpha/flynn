# Testing architecture

Flynn treats testing as a core part of the design, not an afterthought.
The runtime is built from small ports (interfaces) over an injectable clock, seeded
inputs, and an immutable event log, and that architecture is what makes rigorous
testing *cheap*. A failure reproduces from a seed; chaos is just an adapter; a
golden test is a diff of the event spine.

Shared infrastructure lives in [`internal/testkit`](internal/testkit). Write a
generator or a fault plan once, reuse it in every package.

## The tiers

| Tier | Tool | What it buys us |
| --- | --- | --- |
| **Unit** | stdlib `testing` + [`go-cmp`](https://github.com/google/go-cmp) | Plain behavior checks; `cmp.Diff` for readable struct/stream comparisons. |
| **Property-based** | [`rapid`](https://github.com/flyingmutant/rapid) | One property over generated inputs replaces dozens of hand-written cases; failures shrink to a minimal reproducer. Generators for the core types live in `testkit/gen.go`. |
| **Chaos / fault injection** | `testkit` over the ports | `FaultPlan` + `FaultyHandler`/`FaultySink` inject deterministic faults into the dispatch ports, proving the system degrades and recovers cleanly. No framework: the ports *are* the boundary. |
| **Determinism / replay** | `clock.Manual` + `go-cmp` | The same scenario under a manual clock yields byte-identical event streams; behavior changes surface as spine diffs (`testkit.DiffEvents`). |
| **Invariants** | `testkit` assertions | Reusable checks: `RequireLifecycle` (every action is start+end or a single reject), budget-never-exceeded, no-action-without-a-capability (added as the governor lands). |
| **Race** | stdlib `-race` | The concurrent dispatcher/orchestrator runs under the race detector in CI. |
| **Model-based / DST** | `rapid` state machines | Long randomized action sequences against a model, checking invariants after every step (e.g. the spine log), the deterministic-simulation tier, no extra dependency. |
| **Golden / snapshot** | `testkit.Golden` + go-cmp | A whole output (a replay, a rendered spec) compared against a `testdata/*.golden` JSON fixture; `-update` regenerates it, so large outputs need no hand-written expected value. |
| **Fuzzing** | stdlib `go test -fuzz` | Arbitrary-input targets on the error model and the dispatch→spine payload mapping; seed corpora run in CI, deep fuzzing on demand. Expands to parsers/manifests/protocol messages as they land. |

## Conformance vectors

`chain/conformance` is the single source of truth for the [Provetrail](https://provetrail.org)
conformance vectors. The generator builds them deterministically and the goldens are
committed under `chain/conformance/testdata/vectors` (L1 structural) and
`chain/conformance/testdata/crypto` (L2-L4). `TestVectorsConform` and
`TestCryptoVectorsConform` run the reference verifier over every generated vector, so
the generator and the verifier are proven to agree by construction; `TestGoldenVectorsMatch`
and `TestCryptoGoldenMatch` byte-compare the committed fixtures and their `manifest.json`
against what the generator produces, so any drift is caught in review.

An intentional vector change flows in one direction only:

1. Change the spec text (the [provetrail](https://github.com/ionalpha/provetrail) repo:
   `SPEC.md`, `CONFORMANCE.md`, the predicate docs).
2. Change the generator here, then regenerate the goldens and manifests:
   `go test ./chain/conformance -update`.
3. Copy the regenerated `testdata/vectors` and `testdata/crypto` into the provetrail
   repo's `vectors/structural` and `vectors/crypto`.
4. Update the clients.

Provetrail's `vectors/` are downstream copies of flynn's `testdata`, and provetrail CI
(`scripts/check_vector_drift.py`) byte-compares the two on a schedule. A vector edited in
the provetrail repo first, or here without `-update`, is therefore a drift bug, not a new
truth: the fixture on disk must always be exactly what this generator emits.

## The numbers on the README badges

Every number on a README badge is measured, never typed. After a green suite on
`main`, CI runs [`dev/badges`](dev/badges), which reads statement coverage out of the
profile that run just produced, counts the `Test`, `Fuzz` and `Benchmark` functions
the tree declares, and counts the packages carrying a property test by the same rule
[`internal/rigor`](internal/rigor) enforces as a gate. The result is published as
shields.io endpoint documents on the orphan `badges` branch, which is the only thing
the badges read. A badge therefore cannot outlive the code it describes: it moves
when the suite moves, or not at all.

The property-test badge is the rigor gate's burn-down, on the front page. The gate
holds every new package to the floor and lets the list of packages that predate it
only shrink, so the fraction only goes one way, and it is visible without opening a
file.

Subtests and table cases are deliberately not counted. That number depends on what a
particular run expanded, so it would move without the tree moving. The badge counts
what is declared, which is what a reader can go and open.

Reproduce it locally with `./dev/test && ./dev/badges`.

## Deferred (planned, not yet wired)

- **Deterministic concurrency:** `testing/synctest` (a fake-clock "bubble" with
  deterministic goroutine scheduling), now GA in the standard library, is pending
  adoption. It will replace sleep-based concurrency tests.
- **Benchmarks:** stdlib `testing.B` + `benchstat` for dispatch and spine overhead.
- **Mutation testing:** a CI job to verify the suite actually catches injected bugs.

(We evaluated `gosim` for full goroutine/disk/network simulation; it is unmaintained, so
we take the idea, model-based DST on our own primitives, not the dependency.)

## Dependencies

Test-only, pure-Go, permissively licensed, and actively maintained:

- `pgregory.net/rapid`: property-based testing (MIT).
- `github.com/google/go-cmp`: value/stream comparison (BSD-3).

Neither ships in the `flynn` binary; nothing in the binary's import graph reaches them.
