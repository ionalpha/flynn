# Contributing

Thanks for your interest in Flynn. Please read [`AGENTS.md`](AGENTS.md) first;
it is the canonical contribution contract (and what automated triage checks against).

## Quick start

1. Open or find an issue describing the change.
2. Fork, branch, and make a focused change.
3. Run `./dev/check` (or `make check`) locally until it is green. It runs exactly
   what CI runs (build, vet, test, lint, vuln) and arms the repo's git hooks
   (`.githooks/`), including a pre-commit secret scan.
4. Open a pull request (`./dev/pr`) that links the issue and follows Conventional Commits.
5. Sign your commits off with DCO: `git commit -s`.
6. Sign the Contributor License Agreement. On your first pull request the CLA bot
   comments with a link to [`CLA.md`](CLA.md); reply with the sign-off phrase it gives.
   This is a one-time step.

## Pinning the commit identity (optional)

If a clone must only ever produce commits under one address, pin it:

```sh
git config hooks.authorEmail you@example.com
```

The pre-commit and pre-push hooks then refuse any commit whose author or committer
is someone else, which is what you want on a machine where the global git identity
is a personal one and an accidental commit under it is expensive to undo once
pushed. Unset (the default), the check does nothing. To cover every clone of a
remote without a per-clone setup step, set it from a conditional include in
`~/.gitconfig` keyed on the remote URL:

```
[includeIf "hasconfig:remote.*.url:**github.com/your-org/**"]
        path = ~/.gitconfig-your-org
```

## What gets merged fast

- Focused, tested, lint-clean changes that reference an issue.
- Bug fixes with a regression test.
- Docs improvements.

## What gets closed

- Unfocused or bundled PRs, unreviewed AI output, or changes with no linked issue.
- Anything that fails CI and is not being actively fixed.

## Contributor License Agreement

All contributions are accepted under the [Contributor License Agreement](CLA.md). You keep
ownership of your work, and in return the project commits to only ever licensing it under
terms approved by the Open Source Initiative: it can never be taken proprietary or made
source-available. The agreement grants the rights the project needs to distribute it and,
if it is ever needed to keep the project open, to move it to a different open-source
license. Signing is a one-time step handled by the CLA bot on your first pull request;
pull requests that are not signed cannot be merged.

## Reporting bugs and requesting features

Use the issue templates. For security problems, do **not** open a public issue; see
[`SECURITY.md`](SECURITY.md).
