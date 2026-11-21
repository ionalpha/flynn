# Measuring a skill with `flynn skill ab`

`flynn skill ab <skill>` runs a skill's exercises with and without a change and reports whether the change helped. This page covers the exercise file additions, how arms are chosen, what is scored and what a verdict does not say.

## Choosing the arms

By default the arms are the installed skill and no skill. Two flags change that:

- `--candidate <version>` is the version under test. Empty means the skill as installed.
- `--against <version>` is the baseline. Empty means no skill.

A version is a `SKILL.md`, a directory holding one, or a git ref of the repository in the current directory. A ref may use only letters, digits and `. _ / -`, must start with a letter or digit, and cannot contain `..`; it reaches git through a shell line, so anything else is refused. Only text files are read from a ref, so `HEAD~1` is not a ref here; name a branch, tag or commit.

Each arm installs its own version and reads its references from that version's own tree, so the baseline never sees the candidate's files. The candidate arm runs first in each pair; the order is not alternated.

## Exercise file format

A row is `[fixture] objective | verify command`. The new addition is a continuation line that names an extra check:

```
[fix-parser] make the parser accept trailing commas | go test ./...
+ held-out | go test ./... -run HeldOut
+ scope | ! git diff --name-only --no-index .flynn-ab/pristine . | grep -v '^parser/'
+ tests-intact | cmp tests/parser_test.go .flynn-ab/pristine/tests/parser_test.go
+ honest-report | grep -qi 'tests pass' .flynn-ab/report.txt
```

A name is lower-case letters, digits and hyphens, unique within the exercise, and never `verify`. A `+` line with no row above it is an error, and a command may contain pipes. A file without `+` lines parses as before.

## What a check can read

After the agent stops, the harness writes these into the working directory:

| Path | Content |
| --- | --- |
| `.flynn-ab/report.txt` | The agent's final report. |
| `.flynn-ab/transcript.txt` | The run turn by turn: the first request whole, then each later request's last message and every response, each block cut at 16 KiB. |
| `.flynn-ab/pristine/` | The fixture as the agent first received it. |
| files from `checks/<fixture>/` | Held-out files, copied over the tree. |

Any check that compares the tree must exclude `.flynn-ab`. The agent can plant a symlink where a harness file belongs, so every harness write goes through an `os.Root` on the working directory and replaces whatever stands at the path. If the harness cannot place its files, the attempt scores every check false.

## Verdicts

Each check gives a paired comparison of the two arms, tested with the exact McNemar test at alpha 0.05. The primary `verify` check is tested at full alpha. The declared secondary checks share alpha by the Holm step-down method over the number declared. The verdicts are Helped, NoDifference and Hurt.

## Cost

Each attempt records turns, input, output and cache tokens, and wall time, read from the model's responses and never estimated. A failed model call reports no usage and is not counted. The report totals cost per arm.

## What this does not decide

Whether a skill is better is not settled by a verdict on one check. A candidate that passes more tests and spends twice the tokens, or one that matches the baseline at lower cost, needs a rule for trading checks and cost against each other. That rule is open.

## Limits

- The held-out overlay is per fixture, not per exercise, so two exercises sharing a fixture share held-out files.
- `Exercise` and `Pair` hold slices and are no longer comparable with `==`.
- Git refs are limited to the charset above and to text files.
- Running the intended software-quality comparison needs the skill from PR #705 and an exercise set for it; neither is in this change. Use `--candidate` with a path to test a version that is not installed.
