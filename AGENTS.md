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

```sh
make build      # build the flynn binary
make test       # go test -race ./...
make lint       # golangci-lint
make fmt        # gofumpt + goimports
make vuln       # govulncheck
make ci         # everything CI runs, locally
```

## Standards

- **Format:** `gofumpt` + `goimports` (local prefix `github.com/ionalpha/flynn`).
- **Lint:** `golangci-lint` must pass (see `.golangci.yml`).
- **Tests:** add tests with behavior changes; prefer table-driven and property-based
  tests. The race detector must stay clean.
- **Commits:** Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, ...). Sign off
  with DCO (`git commit -s`).
- **Security:** never commit secrets. Report vulnerabilities privately (see `SECURITY.md`).

## Out of scope here

This is the open agent. Host-specific functionality (knowledge graph, fleet learning,
the wider workspace) lives in a separate commercial system and is reached only through
the interfaces in `state/`.
