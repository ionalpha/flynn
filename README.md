<h1 align="center">Flynn</h1>

<p align="center"><strong>A self-improving agent operating system in a single Go binary. Bring your own model and give it a goal. Every action runs sandboxed, inside the budget and approvals you set, and is sealed into a tamper-evident record anyone can verify.</strong></p>

<p align="center">
  <a href="https://scorecard.dev/viewer/?uri=github.com/ionalpha/flynn"><img src="https://api.securityscorecards.dev/projects/github.com/ionalpha/flynn/badge" alt="OpenSSF Scorecard"></a>
</p>

<p align="center">
  <a href="https://github.com/ionalpha/flynn/actions/workflows/ci.yml"><img src="https://github.com/ionalpha/flynn/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/ionalpha/flynn/actions/workflows/codeql.yml"><img src="https://github.com/ionalpha/flynn/actions/workflows/codeql.yml/badge.svg" alt="CodeQL"></a>
  <a href="TESTING.md"><img src="https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/ionalpha/flynn/badges/tests.json" alt="Tests"></a>
  <a href="TESTING.md"><img src="https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/ionalpha/flynn/badges/coverage.json" alt="Coverage"></a>
  <a href="TESTING.md"><img src="https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/ionalpha/flynn/badges/fuzz.json" alt="Fuzz targets"></a>
  <a href="TESTING.md"><img src="https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/ionalpha/flynn/badges/property-tests.json" alt="Packages with a property test"></a>
</p>

<p align="center">
  <a href="https://github.com/ionalpha/flynn/releases/latest"><img src="https://img.shields.io/github/v/release/ionalpha/flynn?sort=semver&color=blue" alt="Latest release"></a>
  <a href="#install"><img src="https://img.shields.io/badge/releases-cosign%20signed-0a7bbb.svg" alt="Releases are cosign signed"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-green.svg" alt="License: Apache 2.0"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/ionalpha/flynn?color=00ADD8&logo=go" alt="Go version"></a>
  <a href="https://pkg.go.dev/github.com/ionalpha/flynn"><img src="https://pkg.go.dev/badge/github.com/ionalpha/flynn.svg" alt="Go Reference"></a>
  <a href="https://x.com/ionalpha_"><img src="https://img.shields.io/badge/Follow-@ionalpha__-1DA1F2.svg" alt="Follow on X"></a>
</p>

---

Flynn is an agent runtime written in Go. It runs as one static binary on anything
from a laptop to a $5 VPS, works with any model provider, hosted or local, and keeps
its state in one SQLite file on your machine. You can also import it as a Go module
and embed it in your own application.

## Install

The install script downloads the prebuilt binary for your OS and architecture and
checks its checksum before installing it.

```sh
# Linux and macOS
curl -fsSL https://raw.githubusercontent.com/ionalpha/flynn/main/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/ionalpha/flynn/main/install.ps1 | iex
```

Pin a version with `FLYNN_VERSION` (for example `FLYNN_VERSION=v0.2.0-rc.1`) or change
the install directory with `FLYNN_INSTALL_DIR`.

The script installs the newest release, v0.1.3. This README describes `main`, which is
the v0.2.0 release candidate: governed goals, the bundled skill pack and the new memory
are in v0.2.0-rc.1 and not in v0.1.3. To try the candidate, install with
`FLYNN_VERSION=v0.2.0-rc.1`, or run `flynn upgrade --pre` from v0.1.3. Read the
[v0.2.0 notes](docs/releases/v0.2.0.md#upgrading-from-v013) before upgrading: commands
the agent runs now have no network unless you pass `--allow-network`.

**Testing a release candidate?** The lines above install the latest final release,
because GitHub's "latest" never includes a release candidate. To get the candidate, set
`FLYNN_PRERELEASE`; it takes the newest release of any kind:

```sh
# Linux and macOS
curl -fsSL https://raw.githubusercontent.com/ionalpha/flynn/main/install.sh | FLYNN_PRERELEASE=1 sh
```

```powershell
# Windows (PowerShell)
$env:FLYNN_PRERELEASE = '1'; irm https://raw.githubusercontent.com/ionalpha/flynn/main/install.ps1 | iex
```

Already installed? `flynn upgrade --pre` moves to the newest candidate. Either way,
`flynn version` tells you which release you ended up on.

After that, the binary updates itself:

```sh
flynn version list   # list the releases
flynn upgrade        # install the newest one
```

`flynn upgrade` needs no cosign or gh. It checks the release signature against a
Sigstore trust root compiled into the binary, requires the signer to be this
repository's release workflow on a version tag, and requires the signature to be in
the public Rekor transparency log. Only then does it download the archive, pinned to
the digest the signed provenance names. It refuses downgrades, leaves package-manager
installs alone, and keeps the old binary if the new one fails to start. See
[docs/UPGRADE.md](docs/UPGRADE.md).

Every method below installs the same release.

<details>
<summary><b>Docker (GHCR or Docker Hub)</b></summary>

Images for amd64 and arm64 are published with each release. Mount a volume for the
durable state (SQLite store, credential vault, learned skills) and pass a model key:

```sh
base=https://github.com/ionalpha/flynn/releases/latest/download
curl -fsSLO "$base/flynn-seccomp.json"
curl -fsSLO "$base/flynn-apparmor-container"
sudo apparmor_parser -r flynn-apparmor-container   # AppArmor hosts (Ubuntu, Debian)
docker run -v flynn-data:/data \
  --security-opt seccomp=flynn-seccomp.json \
  --security-opt apparmor=flynn-container \
  -e ANTHROPIC_API_KEY=your-key \
  ghcr.io/ionalpha/flynn:latest
```

Both profiles are Docker's defaults with one change each, so the agent's command
sandbox can start: the seccomp profile allows `clone`, `unshare` and `mount`, and the
AppArmor profile allows the sandbox's own mounts. Without them the agent refuses to run
commands. On a host without AppArmor (Docker Desktop, Fedora), skip the AppArmor lines.
See [`deploy/README.md`](deploy/README.md#the-sandbox-in-a-container).

The same image is on Docker Hub as `ionalpha/flynn:latest`. Pin a version by tag
(`:0.2.0`) instead of `latest`. The API listens on loopback inside the container. To
reach the agent, add a channel (for example `-e TELEGRAM_BOT_TOKEN=... -e FLYNN_TELEGRAM_ALLOW=<your user id>`), or see
[`deploy/README.md`](deploy/README.md) for remote access and hardening.

Until v0.2.0 ships, `latest` (the image tag and the release download) is v0.1.3, which
has no shell and no profiles. To run the release candidate, set
`base=https://github.com/ionalpha/flynn/releases/download/v0.2.0-rc.1` and use the image
`ghcr.io/ionalpha/flynn:0.2.0-rc.1`.

Every published manifest is keyless-signed with cosign:

```sh
cosign verify ghcr.io/ionalpha/flynn:latest \
  --certificate-identity-regexp 'https://github.com/ionalpha/flynn/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

</details>

<details>
<summary><b>Linux packages (deb, rpm, apk)</b></summary>

Packages for Debian/Ubuntu, Fedora/RHEL and Alpine are attached to every
[release](https://github.com/ionalpha/flynn/releases). Download the one for your
distribution and architecture, then:

```sh
sudo dpkg -i flynn_*_linux_amd64.deb     # Debian, Ubuntu
sudo rpm -i  flynn_*_linux_amd64.rpm     # Fedora, RHEL
sudo apk add --allow-untrusted flynn_*_linux_amd64.apk   # Alpine
```

</details>

<details>
<summary><b>Go toolchain (build from source)</b></summary>

Needs Go 1.26 or later:

```sh
go install github.com/ionalpha/flynn/cmd/flynn@latest
```

</details>

<details>
<summary><b>Manual download and verify</b></summary>

Binaries for Windows, macOS, Linux and ARM are attached to every
[release](https://github.com/ionalpha/flynn/releases) with a `checksums.txt`. The
checksum file is signed with cosign. Verify it before trusting a binary:

```sh
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity-regexp 'https://github.com/ionalpha/flynn/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum -c checksums.txt --ignore-missing
```

</details>

## Quick start

```sh
flynn --model anthropic:claude-opus-4-8     # start an interactive session
flynn --version
```

Store your model API key once. It is encrypted at rest in your OS keychain, or in a
passphrase-sealed file where there is no keychain, and is used only to call the
model. It never appears in a prompt, a log or a command's environment.

```sh
flynn auth set openai     # prompts for the key without echoing it
```

Give it a goal:

```sh
flynn goal "audit the repo for security issues and open a PR with the fixes"
```

Run it as a service that answers your chat channels:

```sh
flynn serve     # answers Telegram and Signal messages, each driven as a goal
```

Flynn is configured with flags (`flynn --help`) and environment variables. The model
you pick with `flynn models use` or `/model` is remembered.

### Local model servers

`llamacpp:`, `ollama:` and `lmstudio:` run a goal against a server on your machine. They
need no API key and listen on localhost by default (llama.cpp on 8080, Ollama on 11434,
LM Studio on 1234).

```sh
flynn goal --model ollama:gpt-oss:20b "fix the failing test"
flynn models        # lists what a running Ollama or LM Studio already holds
```

The model gets the same tools whichever name reaches the server: the tool grant comes
from the run's toolset, never from the provider name, and a test holds it so. What differs
is only how the tools are sent. llama.cpp gets them as a decode-time grammar; Ollama and
LM Studio ignore that field, so they get the standard `tools` field.

`flynn models` asks a running Ollama (`/api/tags`, `/api/show`) and LM Studio (`/v1/models`)
which models they hold and lists them as local and unverified: the size and tool support
are the server's own report, not a catalog entry.

Move a provider with its base-URL variable: `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`,
`DEEPSEEK_BASE_URL`, `GEMINI_BASE_URL`, `LLAMACPP_BASE_URL`, `OLLAMA_BASE_URL`,
`LMSTUDIO_BASE_URL`. A key is never sent over plaintext http to a host other than
localhost. `LLAMACPP_VISION`, `OLLAMA_VISION` and `LMSTUDIO_VISION` turn image input on
for a local server that serves a vision model.

To name a server once and reuse it, put a `providers.json` in the data directory:

```json
{
  "allow_hosts": ["gpu-box"],
  "providers": {
    "box": {"kind": "ollama", "base_url": "http://gpu-box:11434/v1"},
    "gw": {"kind": "openai", "base_url": "https://gw.example.com/v1", "key": "GW_API_KEY"}
  }
}
```

Then `--model box:gpt-oss:20b` or `--model gw:some-model`. `kind` is `openai`, `llamacpp`,
`ollama` or `lmstudio`. `key` names a secret in the vault or environment, found the way a
built-in provider's key is. A server with no key is refused unless its host is localhost or
listed in `allow_hosts`, because it would receive the conversation unauthenticated. A name
that is already a built-in provider is refused.

## Features

<details>
<summary><b>Goals:</b> an objective becomes a ledger of items, each with a check that runs and is recorded</summary>

- `flynn goal "<objective>"` plans before it builds. Each ledger item carries a check,
  and `--require-proof` holds the run to those checks.
- `flynn goal --goal-spec terms.json` states what must stay true while the run works,
  each term with the command that checks it. Terms are checked after every step, and a
  breach stops the run and names the term.
- With `--fanout`, a goal hands sub-goals to concurrent child agents under one shared
  budget and folds their results into one sealed record. A goal's spec can also carry a
  unit graph (Go API only, for now), run as child goals in dependency order.
- A reconciler drives each goal toward its declared end state and picks it up again
  after a failure or restart.

</details>

<details>
<summary><b>Limits and approvals:</b> budgets, approvals and allowances stop a run before it overreaches</summary>

- `--max-cost` and `--max-tokens` set per-run ceilings. Both default to 0, which means
  unlimited. Once set, a run that reaches one stops.
- `--require-approval <action>` pauses that action for a person. An action marked
  `--irreversible` runs only where `--allow` declares it.
- A rate brake limits how fast a run can act.
- `flynn kill <run>` stops a running goal from another terminal. `flynn steer <run> "..."`
  redirects it, and the run cannot report success until it says what it did about the
  redirect.
- A run whose steps stop changing anything is stopped.

</details>

<details>
<summary><b>Sandbox and network:</b> commands run kernel-confined, and outbound traffic is denied by default</summary>

- Commands run in a kernel-confined sandbox (read-only host, syscall filter) on Linux,
  macOS and Windows. A red-team containment matrix runs in CI. Where a tier is not
  available, Flynn refuses the work instead of running it with less isolation.
- Container (Docker, Podman) and hardware tiers sit above it. The hardware tier boots a
  guest kernel through a microVM runtime the host provides (`FLYNN_MICROVM_RUNTIME`).
  Without one, untrusted work is refused.
- Outbound requests go through a deny-by-default egress gate that blocks private,
  loopback and cloud-metadata addresses.
- Listeners bind loopback by default and refuse a wildcard bind. Every non-loopback
  opening needs explicit opt-in and is logged with an expiring lease.
- Lint rules fail the build on any code that dials or listens without going through
  these gates.
- Tools are granted by capability. A config change can remove a tool and cannot add one.
- Credentials stay in the vault and are applied at call time.

The [threat model](docs/THREAT_MODEL.md) lists the trust boundaries and which defense
covers each class of attack, and marks what is planned. To report a vulnerability, see
the [security policy](SECURITY.md).

</details>

<details>
<summary><b>Verifiable records:</b> each run is sealed into a signed record that a third party can check</summary>

- Every model call, tool call, message, approval and checkpoint is an event in one
  ordered, append-only log. Sessions, messages, skills and memory are built from that
  log, and nothing is overwritten in place.
- Every model and tool call passes through one dispatcher, which checks it against the
  agent's capability grant, traces it and records it.
- A run's events are committed to a Merkle log under a COSE-signed checkpoint.
  `flynn spine verify <run>` reports the record tier by tier: integrity, governance and
  ground truth.
- The record uses the open [Provetrail](https://provetrail.org) format. Flynn is its
  reference implementation and includes the reference verifier and the public
  [conformance vectors](https://github.com/ionalpha/provetrail), so any conformant
  verifier can check a Flynn run without trusting the host that produced it.
- `flynn inspect <run>` replays a run from its events. The [`demo/`](demo/) directory
  walks through a record that verifies and records that are rejected.

</details>

<details>
<summary><b>Learning:</b> skills and memory are drawn from finished runs and kept or decayed by outcome</summary>

- After a run converges, the agent writes reusable skills from it. A skill's own check
  runs in the sandbox before the skill is kept.
- Thirteen engineering skills are built into the binary (systematic debugging,
  test-first, untrusted input, dependencies and more). A run sees each skill's name and
  description and reads one with `skill_read`. A skill is graded on the runs that read
  it. `flynn skill ab <skill>` measures whether it helps.
- A memory has a subject, and a new fact on a subject supersedes the old one. A run
  starts with a digest of what you wrote down or reviewed, and recalls what matches its
  objective.
- Recall ranks by words. Set `FLYNN_EMBED_MODEL` to a `provider:model` spec
  (`openai:text-embedding-3-small`, or `llamacpp:<model>` against a local server) to
  rank by meaning as well. If the endpoint fails, recall falls back to words.
- Memories the agent writes for itself wait for your review: `flynn memory ls --pending`
  lists them with their source, and `flynn memory promote <id>` adds one to every later
  run's starting digest. A memory from a run that used the shell or spawned a child
  cannot be promoted, since the shell may have fetched what it says. It is still
  recalled.
- Skills and memory are strengthened or decayed by outcomes (tests passing, a task
  accepted, no correction on the next turn). Skills that stop working are archived.
  Nothing is deleted without you.
- Every skill and memory is versioned and attributed, so you can see which version
  produced a result and roll it back. Skills use the open Agent Skills (`SKILL.md`)
  format.

</details>

<details>
<summary><b>Models:</b> any provider, local open-weight models, or another coding agent as the backend</summary>

- Anthropic and OpenAI adapters, and any OpenAI-compatible endpoint, behind a
  `provider:model` registry.
- Local models: a curated open-weight catalog, hardware-fit checks, one-command fetch
  and run, a model pool, and grammar-constrained decoding so a local model cannot emit a
  malformed tool call. `flynn models probe <id>` measures a model's tool-use reliability.
- `--model claude` or `--model codex` lets another coding agent drive the run. Its own
  tools are denied, and it can call only the tools Flynn bridges to it, so every action
  is still checked and recorded. This works on Linux, macOS and WSL2. Native Windows
  refuses it, since Flynn cannot yet gate the child's network there.

```sh
flynn --model claude                            # interactive session through the CLI
flynn --model codex:gpt-5-codex goal "fix the flaky test"
flynn review owner/repo#123 --model claude
```

In a session the CLI holds the conversation, and Flynn keeps the sealed record.
`/model claude:<other-model>` changes the model from the next turn. Switching to a
different harness mid-run is refused: start a new session. Runs driven by an external
agent do not feed Flynn's skills and memory, `/compact` does not apply, and
`flynn serve` and `flynn watch` do not accept an external backend.

</details>

<details>
<summary><b>Code review:</b> <code>flynn review</code> posts findings on the lines they concern and a formal verdict</summary>

- `flynn review <owner>/<repo>#<n>` reviews every changed file, then sweeps for what the
  per-file passes missed. Each finding is posted on its line, and the verdict links to
  it.
- A finding still standing is rechecked on the next push instead of repeated, and its
  conversation resolves once the problem is gone.
- The reviewer can comment and request changes. Approving needs `--approve --as`.
- The command exits non-zero when it requests changes, for use in a pipeline.

</details>

<details>
<summary><b>Channels and extensions:</b> terminal, Telegram, Signal, file markers, MCP</summary>

- `flynn serve` answers Telegram and Signal messages, each triaged and driven as a goal.
- `flynn watch` picks up `ai!` and `ai?` markers in your files and runs each as a
  governed goal.
- `flynn mcp serve` exposes the agent's tools to any MCP client over stdio.
- An extension is a separate binary that speaks MCP and runs confined. It answers calls
  and cannot drive the agent. `flynn extensions dev <name> <binary>` links one for
  local development.
- A new API integration is a catalog entry and a declarative endpoint contract, run by
  one engine that handles auth and rate limits.

</details>

<details>
<summary><b>Interactive session:</b> the terminal shows governance decisions and record state as the agent works</summary>

- Governance decisions and record events appear in the conversation as they happen.
  Ctrl+O opens an overlay showing what was admitted, what was denied, and why.
- `/seal` seals the current run and `/verify` checks it. A badge shows the record state.
- `/replay` re-renders a recorded run from its events.
- Cross-emulator key handling (`modifyOtherKeys`), an alternate-screen fallback, image
  paste, and `@`-completion ranked by frecency.

</details>

<details>
<summary><b>Testing:</b> property, chaos, replay and fuzz tests, and an end-to-end suite on the release binaries</summary>

- Property-based tests check the planner, governor and budget logic against invariants
  over generated inputs.
- Chaos tests inject faults into tools, providers and the network, and kill and resume
  runs.
- Golden missions replay in CI, so a behavior change shows up as a diff.
- Tool inputs, manifests and protocol messages are fuzzed.
- Every pull request builds the release in snapshot form and runs the end-to-end suite
  against those binaries on Linux, macOS and Windows.
- The concurrent orchestrator runs under the race detector.

See [TESTING.md](TESTING.md).

</details>

<details>
<summary><b>Observability:</b> tokens, cost, latency and outcome per run, and sealed profile bundles</summary>

- Every run reports tokens, cost, latency and outcome. The event log maps onto spans and
  structured events through an OpenTelemetry-style port. The default build has a no-op
  exporter.
- `--profile <dir>` writes a sealed bundle (CPU, heap, goroutines and a sampled
  timeline under a hashed manifest), with CPU samples attributed to the action that
  caused them.

</details>

## Command reference

Run `flynn` with no arguments for an interactive session, or `flynn --help` for every
flag.

<details>
<summary><b>Running work</b></summary>

| Command | What it does |
| --- | --- |
| `flynn` | Start an interactive session |
| `flynn goal "<objective>"` | Drive a goal to completion in the current directory |
| `flynn goal --goal-spec <file>` | The same, held to the terms in the file; `flynn help` shows its shape |
| `flynn resume <run>` | Continue a parked or interrupted run |
| `flynn steer <run> "..."` | Redirect a running goal |
| `flynn kill <run> ["reason"]` | Stop a running goal at its next model or tool call |
| `flynn watch` | Run each `ai!` / `ai?` marker in the working tree as a governed turn |
| `flynn review <pr>` | Review a pull request and submit a verdict |
| `flynn playbook` | List the playbooks, or `playbook run <name>` to run one |
| `flynn serve` | Answer chat messages (Telegram, Signal) and/or expose the read-only monitor API |
| `flynn mcp serve` | Expose the toolset to an MCP client over stdio, every call governed and recorded |

</details>

<details>
<summary><b>Inspecting what happened</b></summary>

| Command | What it does |
| --- | --- |
| `flynn runs` | List past runs (id, phase, objective) |
| `flynn status [<run>]` | Show the live overview, or one run's phase and progress |
| `flynn ps` | List instances with their heartbeat-aware state |
| `flynn get <kind>` | List resources of a kind (instances, agents, runs, ...) |
| `flynn describe <kind> <id>` | Show one resource's fields and recent change history |
| `flynn diff <kind> <a> <b>` | Show the fields that differ between two resources |
| `flynn inspect <run>` | Replay a past run's recorded events (alias: `replay`) |
| `flynn spine verify <run>` | Report a run's record tier by tier: integrity, governance, ground truth |
| `flynn spine export <run>` | Write a sealed run's record to a file for third-party verification |

</details>

<details>
<summary><b>Capabilities and models</b></summary>

| Command | What it does |
| --- | --- |
| `flynn auth set <provider>` | Store an API key in the encrypted vault |
| `flynn integrations` | List the integrations, show one, or call an operation |
| `flynn extensions` | Link a local extension, list, call one confined, or unlink |
| `flynn deps` | List the external tools an integration declares; `deps install <name>` fetches a pinned one |
| `flynn deploy <extension>` | Deploy through a hosting extension and track the result as a managed service |
| `flynn services` | List the managed services; `services rm <name>` removes one |
| `flynn models` | Browse the model catalog (filter with `--local`, `--fit`, `--vram`, ...) |
| `flynn models install [rt]` | Fetch and verify a pinned local runtime (default: llama.cpp) |
| `flynn models run <id>` | Provision, serve and query a local model |
| `flynn models probe <id>` | Measure a local model's tool-use reliability and record its profile |
| `flynn models check` | Report installed local runtimes and known parser advisories |

</details>

<details>
<summary><b>Skills and memory</b></summary>

| Command | What it does |
| --- | --- |
| `flynn skill ls` | List the skills a run can be offered: the built-in pack and what this install learned |
| `flynn skill show <skill>` | Print one skill: its purpose, how runs have used it, and its body |
| `flynn skill ab <skill>` | Run a skill's exercises with and without it, paired, to measure whether it helps |
| `flynn memory ls [--pending]` | List what this install remembers: ids, sources, and whether each is in the starting digest |
| `flynn memory promote <id>` | Add one of the agent's own memories to every later run's starting digest (`revoke <id>` removes it) |
| `flynn memory forget <id>` | Delete a memory, including one pinned with `/remember` |
| `flynn memory usage` | Show which memories were offered to runs and which they used |
| `flynn memory consolidate` | Merge each subject's accumulated episodes into one lesson |
| `flynn regrade` | Re-grade learned skills against the working directory |

</details>

<details>
<summary><b>Maintenance</b></summary>

| Command | What it does |
| --- | --- |
| `flynn db reset` | Move an unusable database aside (backed up) so the next run creates a new one |
| `flynn notices` | Show the signed security advisories and release notices for this build |
| `flynn version list` | List the releases |
| `flynn upgrade` | Replace this binary with a newer, signature-verified release |
| `flynn --version` | Print the version |

</details>

Key flags: `--model`, `--goal-spec <file>`, `--require-proof`,
`--require-approval <action>`, `--irreversible <action>`, `--allow <action>`,
`--fanout`, `--verify "<cmd>"`, `--max-cost`, `--max-tokens`, `--max-memory`,
`--max-processes`, `--allow-network`, `--allow-credential-reads`,
`--require-credential-hiding`, `--no-learn`, `--no-bundled-skills`, `--data-dir`,
`--profile <dir>`. A command the agent runs has no network and, on Linux and macOS,
cannot read `~/.ssh`, cloud and CLI login tokens or the data directory unless the
matching `--allow-` flag is set. Windows cannot hide those paths: they stay readable,
Flynn says so when a run starts, and `--require-credential-hiding` makes it refuse to
run commands instead.
Run flags go before the objective: `flynn goal --require-approval shell "deploy it"`.

## Use it as a library

Import Flynn as a Go module (no submodule, no FFI):

```go
import agent "github.com/ionalpha/flynn"

a := agent.New(agent.Config{Model: "anthropic:claude-opus-4-8"})
result, err := a.Goal(ctx, "audit the repo for TODOs and summarize them")
```

The embedding contract is checked against compatibility breaks in CI. The domain
packages are importable and unstable until 1.0, and `internal/` promises nothing. Each
release's notes include the exported-API diff against the previous release. See
[docs/VERSIONING.md](docs/VERSIONING.md).

Flynn depends only on the interfaces in `state/` (persistence) and `observe/`
(observability). This repository includes local implementations and no-op defaults.
A host can supply its own, such as a graph-backed store or a multi-tenant backend; see
[docs/HOST_BOUNDARY.md](docs/HOST_BOUNDARY.md).

## Architecture

```
cmd/flynn/          standalone binary entry point
agent.go            embedding facade (Config, Agent, Goal)
state/              persistence interfaces a host implements
observe/            logging and tracing port (slog + tracer, no-op default)
dispatch/           the one path every action takes: governance, tracing, events
capability/         capability grants, checked by dispatch
budget/             per-run token and cost ceiling
spine/              the ordered event log (source of truth, replay)
resource/           resources built from the log
reconcile/          the level-triggered controller loop
goal/               the goal controller and worker
mission/            the conversation executor that advances a goal
learn/              the learning loop (capture, verify, reinforce)
skill/, memory/     durable skill and memory stores
llm/, provider/     the model port and concrete adapters
tools/              the default toolset
sandbox/            isolation for command execution
clock/, ids/, hlc/  determinism: time source, sortable ids, write ordering
fault/              typed, classified errors
runtime/            wires controller, worker, store and bus together
session/            conversational front door and event stream
storage/sqlite/     the SQLite backend
internal/           private machinery: model plumbing, guards, the
                    integration and extension engine, and the rest
```

[ARCHITECTURE.md](ARCHITECTURE.md) has the layer map, the ports a host implements,
and the invariants the engine enforces.

## Ion Alpha

Flynn needs no host. [Ion Alpha](https://ionalpha.io) is the first product to embed it:
every agent in Ion Alpha is a Flynn run, with Ion Alpha's own tools and screens on top.
Through the `state/` and `observe/` interfaces it adds a skill and knowledge pool
shared across people and projects, a typed knowledge graph as memory, team
workspaces, SSO, and audit and backup.

## Status and roadmap

The features above work today on `main` and in v0.2.0-rc.1; see [Install](#install)
for what v0.1.3 lacks. Follow [@ionalpha_](https://x.com/ionalpha_) for
progress.

**In progress**

- Missions that outlive a single session, and declaring a goal's unit graph from the CLI.
- A cost-aware model router in front of the registry.
- Remote sandbox backends (E2B, Daytona, Modal) behind the same isolation port.
- Fork-from-event and run diff on top of replay.
- An embedding model Flynn provisions and serves itself, so ranking by meaning needs no
  account or endpoint.

**Planned**

- Standards: A2A, Zed ACP, `agentskills.io` import, and Flynn as an MCP client.
- More channels: Discord, Slack, voice, a built-in browser, desktop GUI and mobile.
- Proactive operation: monitors, drives and self-formed goals within an autonomy level.
- The agent writing and sandbox-testing its own API integrations.
- A cross-machine control plane and Kubernetes pod fan-out.
- OTLP and OpenInference export (Langfuse, Arize Phoenix), Prometheus metrics and
  Grafana dashboards.
- A Postgres backend and fleet-wide learning.
- A configuration file, and export of skills and memory.
- Dry runs for destructive steps, and undoing a recorded action.
- A gVisor sandbox tier.

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) and
[AGENTS.md](AGENTS.md) first.

## License

[Apache License 2.0](LICENSE) © Ion Alpha. See [NOTICE](NOTICE).

The license grants no rights in the Ion Alpha or Flynn names, logos, or trademarks.
