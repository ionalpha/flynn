# Effectiveness scoreboard

The scoreboard runs the same hard exercises through several agents and grades every attempt with the exercise's own verifier, the script under `evals/<skill>/exercises.txt` that runs after the agent stops. What an agent says about its work is never read. The exercises are the ones `flynn skill ab` uses (see [SKILLAB.md](SKILLAB.md)); the board is `flynn skill board`, and `dev/board` runs it.

The machine-readable results are in [scoreboard.json](scoreboard.json): one row per attempt with solved, tokens (input, output, cache read, cache write), USD, wall time, turns and turns to the first file edit.

## Arms

| Arm | What runs |
|---|---|
| `flynn` | Flynn's own loop with the bundled skill pack, set up as `skill ab` sets up a trial: a fresh store, no learning, the operator's shell policy. |
| `flynn-claude` | Flynn driving Claude Code through its external-agent backend. |
| `flynn-codex` | Flynn driving Codex through its external-agent backend. |
| `claude-code` | Bare Claude Code: `claude -p --bare --output-format stream-json`, with a fresh config directory and the API key from Flynn's credential store. `--bare` turns off hooks, plugins, CLAUDE.md discovery, memory and the keychain, so the run sees the exercise and nothing of the developer's setup. |
| `codex` | Bare Codex: `codex exec --json --dangerously-bypass-approvals-and-sandbox`, with a fresh `CODEX_HOME` logged in with the API key from Flynn's credential store. |

An arm whose CLI is not installed, or that has no key or sign-in, is skipped and listed with the reason. It is never counted as a failure.

An attempt that measured nothing about the agent is recorded as **could not evaluate**, never as unsolved: the provider refused to serve it (no credits, a rejected key, a rate limit or a server error), the CLI died before any model response, or the verifier itself could not start. The row keeps the reason and its spend, and is left out of every solve count, spread and token total. An arm with such a row has no USD-per-solve figure.

## First board

Generated 2026-11-28. Cost is priced from reported tokens by `docs/scoreboard-prices.json`. 68 of 72 planned attempts ran. 5 of those could not be evaluated.

| Arm | Model | Ran | Notes |
|---|---|---|---|
| flynn | anthropic:claude-sonnet-5-5 | yes | Flynn's own loop with the bundled skill pack, configured as `skill ab` runs it |
| claude-code | claude-sonnet-5-5 | yes | `claude -p --bare` with stream-json output; turns are its reported num_turns |
| codex | gpt-5.5 | not measured | 5 attempt(s) could not be evaluated: stream disconnected before completion: You have no credits remaining. Add credits to continue using the API at https://platform.openai.com/settings/organization/billing/. |
| flynn-claude | claude-sonnet-5-5 | skipped | claude is not signed in to a Claude subscription for Flynn. Flynn keeps its own Claude Code sign-in, separate from yours, at ~\AppData\Roaming\flynn\claude. Sign in to it once with Claude Code's own login: $env:CLAUDE_CONFIG_DIR = '~\AppData\Roaming\flynn\claude'; claude auth login --claudeai; Remove-Item Env:CLAUDE_CONFIG_DIR |
| flynn-codex | gpt-5.5 | skipped | externagent: ~\AppData\Roaming\npm\codex.cmd is a launcher script but no vendored codex binary for x86_64-pc-windows-msvc was found beside it (looked for vendor\x86_64-pc-windows-msvc\codex\codex.exe) |

### By arm

| Arm | Solved | USD total | USD per solve | Wall s | Turns | Turns to first edit | Tokens in / out / cache read / cache write |
|---|---|---|---|---|---|---|---|
| flynn | 23/24 | 0.9158 | 0.0398 | 23 (13 to 37) | 8 (4 to 11) | 4 (3 to 8) | 996188 / 50730 / 995798 / 123271 |
| claude-code | 16/24 | 0.4583 | 0.0286 | 21 (13 to 46) | 5 (3 to 12) | 3 (2 to 3) | 331213 / 28714 / 330919 / 54982 |
| codex | not measured | 2.1841 | n/a | n/a | n/a | n/a | n/a |

### By exercise

| Exercise | Arm | Solved | USD per run | Wall s | Turns | Turns to first edit |
|---|---|---|---|---|---|---|
| deletion/missing-rate | flynn | 2/3 | 0.0202 (0.0132 to 0.0319) | 14 (13 to 21) | 4 (4 to 5) | 4 |
| deletion/missing-rate | claude-code | 1/3 | 0.0101 (0.0095 to 0.0142) | 24 (20 to 25) | 5 (5 to 6) | n/a |
| deletion/missing-rate | codex | 0/3 | 0.1583 (0.1267 to 0.1656) | 48 (31 to 52) | 19 (17 to 19) | 15 (13 to 15) |
| external-integration/retry-classified | flynn | 3/3 | 0.0537 (0.0426 to 0.0542) | 31 (29 to 35) | 9 (8 to 9) | 6 (6 to 7) |
| external-integration/retry-classified | claude-code | 3/3 | 0.0218 (0.0213 to 0.0227) | 23 (22 to 24) | 6 (5 to 6) | n/a |
| external-integration/retry-classified | codex | 0/3 | 0.1917 (0.1150 to 0.2237) | 43 (35 to 82) | 18 (15 to 21) | 13 (11 to 14) |
| shared-state/lost-update | flynn | 3/3 | 0.0629 (0.0506 to 0.0817) | 34 (27 to 37) | 10 (8 to 11) | 6 (4 to 8) |
| shared-state/lost-update | claude-code | 0/3 | 0.0148 (0.0131 to 0.0188) | 16 (16 to 20) | 5 (5 to 6) | n/a |
| shared-state/lost-update | codex | 0/3 | 0.1176 (0.1153 to 0.1266) | 34 (28 to 35) | 15 (14 to 18) | 11 |
| software-quality/split-quote | flynn | 3/3 | 0.0378 (0.0255 to 0.0537) | 26 (23 to 28) | 9 (7 to 9) | 5 (3 to 6) |
| software-quality/split-quote | claude-code | 3/3 | 0.0243 (0.0218 to 0.0273) | 17 (17 to 21) | 6 (5 to 7) | 3 |
| software-quality/split-quote | codex | 3/3 | 0.1402 (0.1187 to 0.1476) | 29 (25 to 60) | 15 (15 to 16) | 10 (9 to 11) |
| systematic-debugging/already-fixed | flynn | 3/3 | 0.0177 (0.0142 to 0.0258) | 14 (13 to 17) | 6 | n/a |
| systematic-debugging/already-fixed | claude-code | 3/3 | 0.0141 (0.0136 to 0.0145) | 18 (15 to 35) | 5 | n/a |
| systematic-debugging/already-fixed | codex | 0/3 | 0.1410 (0.1367 to 0.1593) | 57 (55 to 59) | 16 (16 to 20) | 12 (12 to 13) |
| systematic-debugging/wrong-layer | flynn | 3/3 | 0.0417 (0.0337 to 0.0447) | 23 (23 to 23) | 8 (7 to 8) | 4 (3 to 5) |
| systematic-debugging/wrong-layer | claude-code | 3/3 | 0.0218 (0.0200 to 0.0219) | 24 (20 to 25) | 7 (7 to 8) | 3 |
| systematic-debugging/wrong-layer | codex | could not evaluate | n/a | n/a | n/a | n/a |
| test-first/bug-first | flynn | 3/3 | 0.0217 (0.0161 to 0.0218) | 16 (14 to 17) | 5 (4 to 6) | 3 (3 to 4) |
| test-first/bug-first | claude-code | 0/3 | 0.0097 (0.0069 to 0.0120) | 14 (13 to 16) | 4 (3 to 5) | 3 (2 to 3) |
| test-first/bug-first | codex | could not evaluate | n/a | n/a | n/a | n/a |
| untrusted-input/path-join | flynn | 3/3 | 0.0468 (0.0380 to 0.0658) | 31 (25 to 34) | 8 (6 to 8) | 4 (3 to 4) |
| untrusted-input/path-join | claude-code | 3/3 | 0.0430 (0.0161 to 0.0449) | 36 (21 to 46) | 12 (4 to 12) | n/a |

Medians, with the range in brackets when the repeats disagree. Turns to first edit counts only the runs that edited a file. An attempt that could not be evaluated is in no figure but the spend.

## What was run

The first board ran on 2026-11-28 on one Windows 11 machine, with Claude Code 2.1.330 and codex-cli 0.145.0. It is a subset of eight exercises, three repeats each:

`systematic-debugging/already-fixed`, `systematic-debugging/wrong-layer`, `test-first/bug-first`, `untrusted-input/path-join`, `shared-state/lost-update`, `deletion/missing-rate`, `software-quality/split-quote`, `external-integration/retry-classified`.

- **`flynn` and `claude-code`:** all 24 attempts each were evaluated.
- **`codex`:** not measured. The OpenAI account ran out of credits during the run: 15 attempts were evaluated (3 solved), 5 were refused by the provider, and 4 never ran. The evaluated ones cover only part of the subset, so they are not an arm total to compare.
- **`flynn-claude`:** skipped. Flynn's own Claude Code sign-in was not set up.
- **`flynn-codex`:** skipped. Codex is installed, but Flynn does not find the binary in the layout npm installs on Windows.
- **Attempts made again:** the first pass stopped after 62 attempts when Windows failed to start processes (status `0xC0000142`). For `flynn` and `claude-code`, `test-first/bug-first` repeat 3 was run again (one had a verifier that never started, the other a CLI that never started), and `untrusted-input/path-join` repeats 1 to 3, which had not run, were run with `--resume`. The seven rows that could not be evaluated were marked as such by hand in the first pass's JSON, because the board that wrote them predated that field.
- **Spend:** 3.82 USD of a 25 USD cap, priced from reported tokens: 3.56 USD in the rows here (`flynn` 0.92, `claude-code` 0.46, `codex` 2.18), 0.24 USD for one three-arm trial run beforehand, and 0.02 USD for the one attempt that was replaced.

## Reading it fairly

- **Home ground.** The exercises are the hard exercises written for Flynn's bundled skills, and the `flynn` arm runs with those skills while the bare CLIs run without them. The board says how each agent does on what the pack teaches. It does not rank them as general coding agents.
- **Small sample.** Three repeats per exercise per arm is enough to see spread and too few to rank close arms. A difference of one solve in an exercise is noise.
- **Different models.** `flynn` and `claude-code` both ran Claude Sonnet 5.5; `codex` ran GPT-5.5. The `codex` row compares an agent and a model together.
- **Turns are counted differently.** `flynn` counts model calls that returned. `claude-code` reports `num_turns`. Codex reports no model calls, so its turns are the actions and messages it completed (commands, file changes, replies). Compare turns within an arm, and across arms only loosely.
- **Turns to first edit** counts only the runs that edited a file. An exercise whose correct answer is to change nothing (`systematic-debugging/already-fixed`) has no edit in a good run. Only edit tools count: a file changed by a shell command is not seen. Bare Claude Code often edits that way, so its `n/a` and its figures in this column are not comparable with the other arms.
- **Cost** is priced by Flynn from the tokens each agent reported, using [scoreboard-prices.json](scoreboard-prices.json), so all arms are priced by the same table. For `claude-code` the board also keeps the cost Claude Code reported (`reportedUsd`) as a cross-check; on this board the two totals agree to within rounding. The rates were read from the vendors' pricing pages on 2026-11-28 and change, so reprice an old board before comparing it with a new one.
- **Tokens.** `flynn` read about three times the input tokens of `claude-code`, nearly all of them from the cache, and cost more per solve (0.040 against 0.029 USD).
- **Wall time** was measured on a shared Windows development machine with other builds running. The arms take turns on each exercise and repeat, so drift reaches all of them, but absolute seconds will differ elsewhere.
- **Windows.** The `flynn` arm runs commands under Flynn's Windows confinement, which on this platform cannot hide paths or deny the network; the bare CLIs run commands with no sandbox at all. Some exercises in the full set have Python fixtures that use `tempfile`, which the Windows sandbox cannot open, so the first board's subset leaves them out.

## Rerunning it

You need Go, Python 3 on `PATH` as `python3` (the verifiers call it), and for each bare arm its CLI on `PATH`. Store the keys in Flynn's credential store once:

```sh
go run ./cmd/flynn auth set anthropic   # flynn and claude-code arms
go run ./cmd/flynn auth set openai      # codex arm
```

Then, from the repository root:

```sh
./dev/board --cheap     # 3 exercises, 1 repeat, capped at 3 USD: checks every arm runs
./dev/board --subset    # the 8 exercises above, 3 repeats, capped at 25 USD; rewrites docs/scoreboard.json
./dev/board             # every exercise, 3 repeats, capped at 25 USD
```

Arguments after the mode go to `flynn skill board` and override the defaults, for example `./dev/board --subset --arms flynn,claude-code --repeats 1`. `--keep <dir>` saves each attempt's report, transcript, verifier output and, for the bare CLIs, the raw JSON stream. The board's tables print at the end of the run and, with `--markdown <file>`, are written there; paste them into this page when the board is rerun. `--max-cost` is checked before each attempt, so a board can pass it by at most one attempt, and an arm the price table cannot price stops a capped board after its first attempt rather than spend an unknown amount.

The JSON is rewritten after every attempt, so a board that is killed keeps the attempts it finished, marked as in progress. To finish it, or to retry attempts that could not be evaluated, add `--resume`: it keeps the rows already in `--out`, runs only the missing and unevaluated ones for the arms named, and counts `--max-cost` against this invocation alone.

```sh
./dev/board --subset --arms flynn,claude-code --resume --max-cost 5
```
