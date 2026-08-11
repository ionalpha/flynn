# Contributing guide for humans and agents

This file is the contract for every contribution to this repository, whether it
comes from a person or an AI agent. Automated triage evaluates pull requests and
issues against it. Read it before opening anything.

## Ground rules

1. **One PR, one topic.** Keep changes focused. Do not bundle unrelated fixes.
2. **Link an issue.** Non-trivial PRs must reference an open issue describing the
   problem. Discuss approach there first for anything large.
3. **No low-quality / unreviewed AI output.** AI assistance is fine; unread,
   untested, or speculative "slop" is not. You are responsible for every line you submit.
4. **It must pass CI.** Build, vet, race tests, lint, and vulnerability checks all green.
5. **Be respectful.** See `CODE_OF_CONDUCT.md`.

## Project shape

- Go module `github.com/ionalpha/flynn`.
- `cmd/flynn` builds the standalone binary; exported packages are importable by a host.
- `state/` defines the persistence/context interfaces (the host boundary). Keep the
  agent host-agnostic; never import a private host from this repository.

## Local development

The `dev/` scripts are the single source of truth: **CI runs these same
scripts**, so a green run locally is a green run in CI. `make` targets forward
to them (`make test` == `./dev/test`); use the scripts directly to pass args.

```sh
./dev/check     # everything CI gates on: build, vet, test, lint, vuln
./dev/build     # go build ./...
./dev/test      # race + coverage; scope with e.g. ./dev/test ./state/...
./dev/lint      # go mod tidy check + golangci-lint (pinned to CI's version)
./dev/fmt       # auto-format (gofumpt + goimports)
./dev/fix       # dev/fmt plus golangci-lint --fix (applies linter autofixes too)
./dev/vuln      # govulncheck
./dev/apidiff   # API compatibility against the last release tag (needs the tags)
./dev/pr        # open a PR against main using the template (needs gh)
```

Run `./dev/check` until it is green before opening a PR.

## Standards

- **Format:** `gofumpt` + `goimports` (local prefix `github.com/ionalpha/flynn`).
- **Lint:** `golangci-lint` must pass (see `.golangci.yml`).
- **Tests:** add tests with behavior changes; prefer table-driven and property-based
  tests. The race detector must stay clean.
- **Duplication:** the third copy of a sequence becomes a gate rather than a review
  comment. When a package is extracted to own something (durable file writes are
  `internal/fsatomic`), the same change adds the lint rule or architecture test that
  fails the pattern it replaced, so the next author meets the rule instead of
  re-deriving the copy.
- **API compatibility:** `./dev/apidiff` compares the exported surface against the
  last release tag and fails on an incompatible change to the stable surface (the
  embedding contract, listed under Stability tiers in `ARCHITECTURE.md`). Passing
  tests do not answer this: they prove the callers in this checkout were updated,
  and a released module has callers outside it. Direction decides the verdict, so
  an addition is not automatically safe: adding a field to a struct is compatible,
  adding a method to an interface a host implements is a break for every host that
  implemented it. A break that is worth making goes in `dev/apidiff-accepted.txt`
  with the reason, verbatim, in the same PR. `docs/VERSIONING.md` is the same
  policy written for the person importing the module.
- **Commits:** Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, ...). Sign off
  with DCO (`git commit -s`). Wrap the message body at 72 columns.
- **Pull request bodies:** one line per paragraph, wrapped by the browser and not by
  you. GitHub renders a newline inside a paragraph as a line break, so a body wrapped
  like a commit message displays as ragged short lines. Lists, tables and fenced code
  are unaffected. CI rejects the wrapped form.
- **Security:** never commit secrets. Report vulnerabilities privately (see `SECURITY.md`).

## Out of scope here

This is the open agent. Host-specific functionality (knowledge graph, fleet learning,
the wider workspace) lives in a separate commercial system and is reached only through
the interfaces in `state/`.
