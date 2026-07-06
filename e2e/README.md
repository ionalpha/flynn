# End-to-end suite

This package drives the real, compiled `flynn` binary as a subprocess and asserts on
observable behavior: exit codes, files produced or withheld, refusal text, and the
emitted verifiable record. It is a black box. Nothing here imports flynn's internal
packages, so it exercises exactly what a user (or an attacker) reaches through the
shipped artifact and its defaults, not the packages behind it.

## How it works

- `TestMain` compiles `./cmd/flynn` once, stamping `internal/version.Version` with the
  same `-ldflags -X` the release uses, and shares that binary across every test.
- Each test runs in an `instance`: an isolated data directory, an isolated workspace
  (the process working directory, where a goal's file artifacts land), and a scrubbed
  environment. Runs are hermetic, so tests neither touch a developer's real flynn state
  nor each other's.
- A scripted, in-process OpenAI-compatible server (`fakeopenai_test.go`) makes runs
  deterministic and offline. The binary is pointed at it with `OPENAI_BASE_URL` (a
  loopback address the provider dials with a plain client), so no run needs a live API
  key or the network. The server records every request, so a test can assert what the
  binary sent (tools advertised, system prompt, injected content), and can script
  tool-call turns, error statuses, and a mid-run block for crash testing.
- Exactly one lane hits a real hosted model (`TestRealModelSmoke`), skipped unless
  `FLYNN_E2E_REAL` is set. It runs a benign happy-path only; every adversarial scenario
  stays on the scripted lane.

Run it:

```sh
go test ./e2e/                 # offline, deterministic
FLYNN_E2E_REAL=1 go test ./e2e/ -run TestRealModelSmoke   # opt-in real model
FLYNN_E2E_ARTIFACTS=/tmp/e2e go test ./e2e/               # dump logs + data dir on failure
```

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
| Credential | at-rest sealed-file encryption via the CLI | not covered (see gaps) |
| Budgets / DoS | a token ceiling halts the run with state intact; a zero ceiling is unlimited | covered |
| Budgets / DoS | fan-out shares one ceiling; runaway circuit breaker | not covered |
| Spine / record | a clean run verifies; a one-byte tamper of the exported record fails verification | covered |
| Spine / record | reorder / drop / insert single-event tamper variants | partial (integrity covered by the byte-level tamper) |
| Replay / determinism | rendering a past run twice is byte-identical | covered |
| Learning loop | skill capture and recall across runs | not covered (runs use `-no-learn`) |
| Local models | catalog browse and filters; fetch refuses an off-catalog source; runtime inventory | covered (offline) |
| Local models | digest-pinned fetch and run of a real model | not covered (network + runtime; opt-in) |
| Providers | a permanent failure (bad key, exhausted quota) fails fast with a typed error, one call, no retry | covered |
| Crash / resume | a hard kill leaves the store intact and the run resumable; a fresh run on the same data dir works; resume replays a converged run | covered |
| Crash / resume | resume continues a run interrupted mid model call | not covered (see gaps: hang) |
| State persistence | two runs across separate invocations both persist and verify independently | covered |
| Honesty | an ungrounded success claim is rejected by the record; a grounded one is accepted | covered |
| Honesty | prompt-injection governance corpus run end to end | not covered |
| Cold start | a fresh install runs with only a key and is safe by default (bind refused, escape denied) | covered |
| Install paths | `go install`, a prebuilt binary, and the container each run a trivial goal | not covered (needs a published release) |

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

2. **At-rest credential encryption is not black-box testable.** `auth set` requires a
   terminal (it refuses a piped secret, which is the correct security default), and there
   is no way to point the vault at an isolated file backend, so writing a key through the
   CLI would hit the machine keychain. Suggested fix: a `FLYNN_VAULT_FILE` (or
   `FLYNN_NO_KEYCHAIN`) switch that forces the passphrase-sealed file backend, useful in
   containers and headless environments as well as here.

3. **Learning loop is not exercised.** Every run uses `-no-learn` for determinism. Skill
   capture, cross-run recall, curation, and reinforcement need their own scenarios with a
   controlled learning store.

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

7. **Install-path smoke is pending a release.** `go install ...@latest`, a prebuilt
   release binary, and the container one-liner can only be smoke-tested once a version is
   tagged and published.

## CI

The suite runs in CI as a dedicated job. It needs no secrets. On failure, set
`FLYNN_E2E_ARTIFACTS` to a directory and the run's stdout, stderr, and a copy of the data
directory are written there for triage; the CI job wires this to an uploaded artifact.
The cross-platform matrix follows the same posture as the unit test job.
