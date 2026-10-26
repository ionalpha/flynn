# Versioning and compatibility

This is for someone importing `github.com/ionalpha/flynn` as a Go module. If you
run the binary and want to update it, `docs/UPGRADE.md` is the one you want.

## What a version is here

A release is one pushed tag. `vX.Y.Z` triggers the release workflow, which builds
the signed archives, packages and images. The module version is published
separately and automatically: the Go module mirror fetches the tag the first time
anybody asks for it, so `go get github.com/ionalpha/flynn@v0.1.3` works without us
doing anything further.

Pre-releases are tagged `vX.Y.Z-rc.N`. The `go` command prefers release versions,
so a pre-release is only ever selected if you ask for it by name.

## Nothing upgrades under you

Your `go.mod` records an exact version, and Go's minimal version selection
recomputes the build from those requirements rather than from whatever exists
upstream. A new flynn release changes nothing about your build until you run
`go get`. That is worth stating because the rest of this page describes breaks:
none of them can reach you on a day you did not choose.

## The three tiers

`ARCHITECTURE.md` describes them in full. What they mean for you:

| Tier | Packages | What you can expect |
| --- | --- | --- |
| Stable surface | the root `agent` facade, `state`, `observe`, `llm`, `capability`, `fault`, `tools`, `sandbox`, `secret`, `spine`, `resource`, `envelope`, `hlc`, `jobs`, `clock`, `ids`, `provider`, `storage/sqlite`, `memory/memorytest`, `chain` | The embedding contract. Breaks are gated in CI, and any break that ships was argued for in writing. |
| Domain surface | `goal`, `mission`, `reconcile`, `session`, `runtime`, `governed`, `learn`, `budget`, `memory`, `skill`, `extension`, `controlplane`, `orchestration`, `dispatch`, `approval`, and the rest | Importable, and unstable while pre-1.0. Import with that understanding. Breaks are reported in the release notes, not prevented. |
| `internal/` | everything under it | Not importable. No promise at all. |

The gate is `dev/apidiff`, which compares the exported API of the branch against
the last release tag with `golang.org/x/exp/cmd/apidiff`. It runs on every pull
request. Every release's notes carry the same tool's output, so before you
upgrade you can read exactly what moved.

We are pre-1.0, where Go's own rule is that `v0.2.0` need not be compatible with
`v0.1.0`. Gating the stable surface is a stricter promise than the version number
makes, deliberately, because that surface is what a host builds against.

## The ports you implement, and the suites that certify them

If you write a backend for one of the ports below, the promise covers what a
correct backend is, and that is defined by the conformance suite, not by the
interface alone. A signature can survive unchanged while the contract moves.

| Port | Suite you run against your backend |
| --- | --- |
| `resource.Store`, with the optional `KeyLister` and `AnyScopeGetter` | `resource/resourcetest` |
| `spine.Log` | `spine/spinetest` |
| `jobs.Queue`, with the optional `Waker` | `jobs/jobstest` |
| `state.MemoryStore`, and the other `state` ports | `memory/memorytest`, `state/statetest` |
| the Provetrail record format | `chain/conformance` |

A change to a suite that makes an implementation which passed fail is a break of
the port, in the same way as a removed method, even though no signature moved.
That includes adding a case: a new case that a passing backend fails is a break
whether it was meant as a fix or not. So a suite tightens only with the same
written argument a signature break needs, and the pull request says whether any
backend that passed can now fail. A case added for behaviour the port already
documented is argued the same way; being right about the documentation is the
argument, and it is recorded, not assumed. The suites' own exported entry points
(`RunSuite` and the harness types) are in the stable surface and are gated like any
other signature.

`dev/apidiff` cannot judge behaviour, so it does the part a tool can: every run
lists the suite files that changed since the last release, and the release notes
carry the same list under "Conformance suites changed". A reviewer reads that
list and the pull request says whether any backend that passed can now fail.

A port is promised when a suite can say what a correct backend is, and when every
type its exported API names is itself stable. `resource.Store` returns a
`resource.Resource` that embeds `envelope.Envelope` and is stamped by an
`hlc.Clock`, so `envelope` and `hlc` are stable with it (`state` already named
both). Six exposures of an unstable type remain and are listed, with reasons, in
`internal/rigor`: `agent.Config.Agent` is an `*archetypes.Spec`, `agent.Config`
takes `governed` and `mission` types and `Agent.GoalResult` returns a
`governed.Result`, `tools` hands out `mission.Tool` values, `sandbox.WithEgress`
takes a `netguard.Policy`, and `capability` implements the `dispatch` ports. Until
those are settled, a change to `archetypes`, `governed`, `mission`, `netguard` or
`dispatch` can change
what a stable type means while `dev/apidiff` reports it as a domain change. Read
those packages' lines in the release notes if you use the facade, the tools, the
egress policy or the capability grants.

Two ports are deliberately not in the stable surface, for the same reason: no suite
yet states what a correct backend is. `approval` holds the nonce store, which must
keep a spent nonce spent across a restart. `dispatch.Hook` and `dispatch.Admitter`
are the governance gate every action passes through; what an admitter must do with
an error or a panic is not yet pinned by a suite. A host that implements either
should expect the signature to move. `memory` is also outside, but for a different
reason: it is a concrete facade you call, not a port you implement, and the port it
serves is `state.MemoryStore`. None of the three is hidden: all are importable, and
all are listed in the release notes when they change.

## What counts as a break

Direction decides it, so an addition is not automatically safe. Adding a field to
a struct you read is compatible. Adding a method to an interface you implement is
not, because your type stops satisfying it. Adding a slice or a map field to a
struct that used to be comparable breaks anyone using it as a map key. All three
are Go's own rules, and the tool checks them rather than trusting a reviewer to
notice.

Two things the check cannot see, so that a green release is not read as more than
it is. It compares shape, never behaviour: a function that keeps its signature and
changes what it does passes. And it says nothing about formats that cross a
process boundary rather than a link boundary (envelope records, chain records, the
SQLite schema), where the rule is the mirrored one: loosen what you accept, narrow
what you emit. The `chain/conformance` test vectors pin the record format for
external verifiers; nothing else there is gated yet.

## When a bad version ships

A published version cannot be withdrawn. The module mirror keeps what it cached
precisely so that a deletion upstream cannot break someone's build. The remedy is
a `retract` directive published in a **later** version: existing builds keep
working, and nobody upgrades onto the retracted one by accident. If you are on a
version we have retracted, `go list -m -u all` will tell you.

## After v1

At v1 the stable surface stops being breakable within a major version, and the
domain surface is expected to have settled into whatever it is going to be.

A v2 would change the import path itself to
`github.com/ionalpha/flynn/v2`, because Go requires a major version suffix from
v2 onwards. That is a deliberate cost: your build keeps compiling against v1 until
you rewrite the import, so a major version can never arrive by surprise. We would
rather add than break, and the tiering exists to make that possible for longer.

## Go version

`go.mod` declares the minimum Go version, and it is a hard floor: a toolchain
older than that line refuses to build the module. With the default
`GOTOOLCHAIN=auto` your toolchain downloads what it needs and you will not notice.
With `GOTOOLCHAIN=local`, a vendored build, or an air-gapped environment, you need
a toolchain at least that new. Check the `go` line before adopting.
