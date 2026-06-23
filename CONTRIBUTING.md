# Contributing

Thanks for your interest in Flynn. Please read [`AGENTS.md`](AGENTS.md) first;
it is the canonical contribution contract (and what automated triage checks against).

## Quick start

1. Open or find an issue describing the change.
2. Fork, branch, and make a focused change.
3. Run `./dev/check` (or `make check`) locally until it is green — it runs exactly
   what CI runs (build, vet, test, lint, vuln).
4. Open a pull request (`./dev/pr`) that links the issue and follows Conventional Commits.
5. Sign your commits off with DCO: `git commit -s`.

## What gets merged fast

- Focused, tested, lint-clean changes that reference an issue.
- Bug fixes with a regression test.
- Docs improvements.

## What gets closed

- Unfocused or bundled PRs, unreviewed AI output, or changes with no linked issue.
- Anything that fails CI and is not being actively fixed.

## Reporting bugs and requesting features

Use the issue templates. For security problems, do **not** open a public issue; see
[`SECURITY.md`](SECURITY.md).
