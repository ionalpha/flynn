# End-to-end suite

This package drives the real, compiled `flynn` binary as a subprocess and asserts on
observable behavior: exit codes, files produced or withheld, refusal text, and the
emitted verifiable record. It is a black box. Nothing here imports flynn's internal
packages: it exercises what a user (or an attacker) reaches through the released
binary and its defaults.

## How it works

- `TestMain` compiles `./cmd/flynn` once, stamping `internal/version.Version` with the
  same `-ldflags -X` the release uses, and shares that binary across every test.
- Each test runs in an `instance`: an isolated data directory, an isolated workspace
  (the process working directory, where a goal writes its files), and a scrubbed
  environment. Runs are hermetic, so tests neither touch a developer's real flynn state
  nor each other's.
- A scripted, in-process OpenAI-compatible server (`fakeopenai_test.go`) makes runs
  deterministic and offline. The binary is pointed at it with `OPENAI_BASE_URL` (a
  loopback address the provider dials with a plain client), so no run needs a live API
  key or the network. The server records every request, so a test can assert what the
  binary sent (tools advertised, system prompt, injected content), and can script
  tool-call turns, error statuses, and a mid-run block for crash testing.
- Two lanes reach a real model, both skipped unless `FLYNN_E2E_REAL=1`: the plain-reply
  smoke test (`TestRealModelSmoke`) and the live-model lane described below. Every
  adversarial scenario stays on the scripted lane.

Run it:

```sh
go test ./e2e/                 # offline, deterministic
FLYNN_E2E_REAL=1 go test ./e2e/ -run TestRealModelSmoke   # opt-in real model
FLYNN_E2E_REAL=1 go test ./e2e/ -run TestLive -v          # the live-model lane
FLYNN_E2E_ARTIFACTS=/tmp/e2e go test ./e2e/               # dump logs + data dir on failure

# against a prebuilt binary (an absolute path) instead of a fresh compile
FLYNN_E2E_BIN=$PWD/dist/flynn_linux_amd64_v1/flynn FLYNN_E2E_VERSION=0.2.0 go test ./e2e/
```

CI runs the suite twice: once over its own compile on every OS (`E2E`), and once over
the binaries the release snapshot built (`E2E on release artifact`), the same bytes a
release publishes: stripped, stamped, and cross-compiled by goreleaser.

## The live-model lane

`TestLive*` (in `live_*_test.go`) runs real tasks against a real model through the binary
under test. Point `FLYNN_E2E_BIN` (an absolute path) and `FLYNN_E2E_VERSION` at a
published release binary to test the bytes that ship.

A model's wording varies from run to run, so no assertion reads it. A scenario asserts on
the sealed run record (the tools called, the actions refused, whether the run converged;
`spine verify` must report the record intact and every action admitted) and on what the
host sees without the model's help: a file on disk, a connection a listener did or did not
receive. Every goal runs under a cost and a token cap, with a bound on each invocation.

| Variable | Meaning |
| --- | --- |
| `FLYNN_E2E_REAL=1` | switches the lane on; without it every `TestLive*` skips and says so |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `DEEPSEEK_API_KEY` | a hosted provider is covered when its key is present; only the key of the provider under test reaches the binary |
| `FLYNN_E2E_REAL_MODEL` | the `provider:model` the scenarios share; by default the first provider with a key, in the order above |
| `FLYNN_E2E_REAL_MODEL_<NAME>` | replaces a hosted provider's default model (for example `FLYNN_E2E_REAL_MODEL_OPENAI`), since providers retire model names |
| `FLYNN_E2E_LOCAL_MODEL` | `provider:model` for one local model (`ollama:`, `llamacpp:` or `lmstudio:`); the runtime is yours to start |
| `OLLAMA_BASE_URL`, `LLAMACPP_BASE_URL`, `LMSTUDIO_BASE_URL` | where that runtime listens, passed through to the binary |
| `FLYNN_E2E_REAL_MAX_COST`, `FLYNN_E2E_REAL_MAX_TOKENS` | replace the per-goal caps (0.50 and 400000) |
| `FLYNN_E2E_REAL_TIMEOUT` | bound on one invocation, a Go duration (default 8m); raise it for a slow local model |

```sh
ANTHROPIC_API_KEY=... FLYNN_E2E_REAL=1 go test ./e2e/ -run TestLive -v

# a local model through a running llama-server
FLYNN_E2E_REAL=1 FLYNN_E2E_REAL_MODEL=llamacpp:my-model FLYNN_E2E_LOCAL_MODEL=llamacpp:my-model \
  LLAMACPP_BASE_URL=http://127.0.0.1:8080/v1 FLYNN_E2E_REAL_TIMEOUT=15m go test ./e2e/ -run TestLive -v
```

A scenario whose prerequisite is missing (no key, no `curl`, no model named) skips and
says which. A hosted provider without a key is a skipped subtest named for it, so a run
reports the providers it did not cover. The tool-driven scenarios need a model that calls
tools; one that answers in prose fails them with a message saying it never called a tool,
since a pass there would prove nothing.

| Scenario | What it asserts |
| --- | --- |
| edit a file and run its tests | the model fixes a bug in a scratch module and runs `go test` in the sandbox; the host's `go test` then passes and the tests were not edited |
| network and credentials | by default a listener is not reached and a planted credential is not read, and the record's posture says so; `-allow-network` opens the network only and `-allow-credential-reads` opens the credential only |
| approval | with `-require-approval write` and nobody to ask, the write is refused in the record and no file appears |
| kill and steer | a second process steers and then kills a live goal; the steer is accepted, the goal exits with the operator's reason, leaves later work undone and its record verifies. That the steer reached the model is not asserted: it is recorded only when a run claims completion, which a killed run never does |
| resume | after a hard kill the work done is kept, `resume` finishes the objective and the record verifies |
| spend cap | a token ceiling stops a long task with a budget reason and a non-zero exit |
| memory and skills | a second run is pushed the memory and reads the skill the first left |
| `serve` | a channel message is answered, and the API refuses a missing or wrong token and lists the run |
| MCP | the full server writes and reads; the read-only server denies a write and a shell command |
| hosted providers | each provider with a key carries a tool call through its wire format |
| local model | the model `FLYNN_E2E_LOCAL_MODEL` names answers and its record verifies; it must also carry a tool call as a hosted provider does, and a model that makes none skips the test with that stated |

On Windows the default sandbox records that it denies the network but does not enforce
it. The network scenario asserts what the record claims there, and when the model's
request reaches the listener anyway it skips with the words `KNOWN FAILURE`, so the
output carries the gap instead of a silent pass. If the request is not reached, it fails
so the marker gets removed.

The record decoder the lane relies on is itself checked in the default run
(`TestRecordDecoderReadsToolCallsRefusalsAndPosture`), against a scripted model, so a
change to the record's shape fails there and not as a live scenario that sees nothing.

## Coverage

Grouped by trust boundary. "Covered" means at least one test asserts the behavior end to
end against the built binary.

| Boundary | Scenario | Status |
| --- | --- | --- |
| Core loop / CLI | goal converges, produces an artifact, captures a replayable record | covered |
| Core loop / CLI | command surface exits with documented codes (`runs`, `models`, `ps`, `auth`, `spine`, `serve`, `mcp`, `deploy`, `inspect`, `--version`) | covered |
| Core loop / CLI | `--version` reports exactly the linked stamp | covered |
| Dispatch / capability | a sandbox-confined tool refuses an out-of-jail action | covered |
| Dispatch / capability | delegation grant is a strict subset of the parent; config can remove but never silently grant a capability | not covered (needs fan-out run-creation) |
| Sandbox containment | path jail refuses traversal and absolute reads; a write cannot escape the workspace | covered |
| Sandbox containment | kernel-confined per-OS tiers; refuse-rather-than-downgrade | partial (denial observed; per-tier matrix not asserted) |
| Egress | outbound to private, link-local, and cloud-metadata addresses is refused at the dial | covered |
| Inbound / bind | wildcard and non-loopback binds refused; an operator token is generated when none is given | covered |
| Credential | `auth set` refuses a piped secret; the key never appears in output; a child process env is scrubbed of it | covered |
| Credential | the default shell runs a command on every OS; `~/.ssh` is hidden from it on Linux and macOS, and on Windows the record and a notice say it was readable, and `--require-credential-hiding` refuses | covered |
| Credential | at-rest sealed-file encryption via the CLI | not covered (see gaps) |
| Budgets / DoS | a token ceiling halts the run with state intact; a zero ceiling is unlimited | covered |
| Budgets / DoS | fan-out shares one ceiling; runaway circuit breaker | not covered |
| Spine / record | a clean run verifies; a one-byte tamper of the exported record fails verification | covered |
| Spine / record | reorder / drop / insert single-event tamper variants | partial (integrity covered by the byte-level tamper) |
| Replay / determinism | rendering a past run twice is byte-identical | covered |
| Learning loop | the bundled pack is in a fresh install's store; a run reads a bundled skill through `skill_read` and the read and the win are credited to it | covered |
| Learning loop | a converged run is distilled; the skill passes its check and is kept, and the next run is handed the skill and the memory | covered |
| Learning loop | an operator-written memory (`/remember`) is pushed at the next run's wake and counted by `memory usage` | not covered (interactive only; see gaps) |
| Run terms | `goal --goal-spec` with a term whose check fails stops the run naming the term; one whose check passes lets it finish | covered |
| Approval | `--require-approval` with nobody to ask refuses the action, takes nothing, and tells the model | covered |
| Allowances | an action marked `--irreversible` and not declared stops the run with the ask; declared with `--allow`, it is taken | covered |
| Kill | `flynn kill` from a second invocation stops a running goal before its next action, and the goal exits with the operator's reason | covered |
| Local models | catalog browse and filters; fetch refuses an off-catalog source; runtime inventory | covered (offline) |
| Local models | digest-pinned fetch and run of a real model | not covered (network + runtime; opt-in) |
| Providers | a permanent failure (bad key, exhausted quota) fails fast with a typed error, one call, no retry | covered |
| Providers | the same fake server under `openai:`, `llamacpp:`, `ollama:` and `lmstudio:` gets the same tools, and no request reports `capability_denied` | covered |
| Providers | a server named in `providers.json` is selected as `--model <name>:<model>` and the request reaches it; an unnamed one is refused | covered |
| Crash / resume | a hard kill leaves the store intact and the run resumable; a fresh run on the same data dir works; resume replays a converged run | covered |
| Crash / resume | resume continues a run interrupted mid model call | not covered (see gaps: hang) |
| State persistence | two runs across separate invocations both persist and verify independently | covered |
| Honesty | an ungrounded success claim is rejected by the record; a grounded one is accepted | covered |
| Honesty | prompt-injection governance corpus run end to end | not covered |
| Cold start | a fresh install runs with only a key and is safe by default (bind refused, escape denied) | covered |
| Install paths | the release's own binaries on each OS, and its container image reporting its version | covered (CI release snapshot) |
| Install paths | the previous release upgrades itself to a new one with `flynn upgrade` | covered after publishing (`dev/upgrade-check`, run by the release workflow) |

## Known gaps

These are behaviors the threat model or docs describe that this suite does not yet
exercise, with the reason and, where relevant, a suggested fix.

1. **Resume of a mid-call interruption hangs.** When a run's last turn was interrupted
   before its model call completed, `resume` does not re-issue the call; it neither
   continues nor fails safe. Model resolution is identical to a fresh goal (both go
   through `provider.ResolveWith` with the same credential source), so the cause is in
   the resume/replay continuation, not configuration. The suite asserts the guarantees
   that hold (no corruption, resumable phase, artifact preserved, a converged run
   replays) and leaves this case unasserted so it cannot hang. It should be fixed in the
   run/session layer and then covered here.

2. **At-rest credential encryption is not black-box testable.** `FLYNN_VAULT_FILE=1`
   forces the passphrase-sealed file backend and the harness sets it, so the suite never
   touches the machine keychain. But `auth set` requires a terminal (it refuses a piped
   secret, which is the correct security default), so a key cannot be written through
   the CLI and the sealed file inspected without a pseudo-terminal in the harness.

3. **An operator-written memory needs a terminal.** `/remember` is reachable only from
   the interactive session, which needs a TTY. A distilled memory waits for a reviewed
   promotion, which `flynn memory promote` does; the live lane promotes one that way and
   checks that the next run is pushed it, but only against a model that distils a memory.
   The in-process `TestStandaloneASessionWakesWithADigestAndCountsThePush` covers the push
   deterministically.

4. **Fan-out is not exercised.** Delegation grant-subset enforcement, a shared budget
   ceiling across a delegated tree, and the runaway circuit breaker on the fan-out path
   need fan-out run-creation and are not yet asserted.

5. **Kernel-confined containment tiers are asserted only by denial, not per-OS.** The
   per-platform read-only-host / working-dir-only-write / forbidden-syscall matrix and
   the refuse-rather-than-downgrade behavior on a tier that cannot enforce what the work
   needs are not directly checked.

6. **Prompt-injection blast-radius corpus is not run end to end.** The honesty tests
   cover ground-truth grounding; running the governance benchmark corpus through the
   binary is future work.

7. **The one-line installers are not smoke-tested.** The release's own binaries, its
   container image and the upgrade from the previous release are covered (see the table).
   `install.sh`, `install.ps1` and `go install ...@latest` are not run against a published
   tag.

## CI

The suite runs in CI as a dedicated job. It needs no secrets. On failure, set
`FLYNN_E2E_ARTIFACTS` to a directory and the run's stdout, stderr, and a copy of the data
directory are written there for triage; the CI job wires this to an uploaded artifact.
The cross-platform matrix follows the same posture as the unit test job.
