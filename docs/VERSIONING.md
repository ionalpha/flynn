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
| Stable surface | the root facade, `state`, `observe`, `llm`, `capability`, `fault`, `tools`, `sandbox`, `secret`, `spine`, `clock`, `ids`, `provider`, `storage/sqlite`, `chain` | The embedding contract. Breaks are gated in CI, and any break that ships was argued for in writing. |
| Domain surface | `goal`, `mission`, `reconcile`, `resource`, `dispatch`, `session`, `runtime`, `learn`, `budget`, `memory`, `skill`, `extension`, `controlplane`, `orchestration`, and the rest | Importable, and unstable while pre-1.0. Import with that understanding. Breaks are reported in the release notes, not prevented. |
| `internal/` | everything under it | Not importable. No promise at all. |

The gate is `dev/apidiff`, which compares the exported API of the branch against
the last release tag with `golang.org/x/exp/cmd/apidiff`. It runs on every pull
request. Every release's notes carry the same tool's output, so before you
upgrade you can read exactly what moved.

We are pre-1.0, where Go's own rule is that `v0.2.0` need not be compatible with
`v0.1.0`. Gating the stable surface is a stricter promise than the version number
makes, deliberately, because that surface is what a host builds against.

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
