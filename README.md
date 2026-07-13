<h1 align="center">Flynn</h1>

<p align="center"><strong>A secure, self-improving agent operating system in a single Go binary. Bring your own model, point it at a goal, and grant it real authority, because every action is sandboxed, governed, and sealed into a verifiable, tamper-evident record.</strong></p>

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

Flynn is a lightweight agent runtime and operating system written in Go. It
runs standalone as a single static binary from a laptop to a $5 VPS, works with any
model provider, and stores all of its state locally, so you own it.

Four ideas run through everything it does:

1. **It compounds.** A closed learning loop turns each session into durable
   skills and memory, reinforced by whether the work actually succeeded.
2. **It scales past one task.** A goals-and-missions engine plans, fans out, and
   governs many agent runs toward a single objective.
3. **It owns its cost.** Per-run token and cost budgets with hard ceilings, plus
   native support for local open-weight models, keep continuous operation affordable
   and a runaway spend structurally impossible.
4. **You can trust it with autonomy.** Every action is governed, contained, and sealed
   into a verifiable, tamper-evident record, so giving it real authority is a decision
   you can audit, not a gamble.

## Why Flynn

- **One binary, no runtime.** No Python, no Node, no virtualenv, no
  `node_modules`. `curl | sh` drops a single file. Cross-compiles to Windows,
  macOS, Linux, and ARM, and ships in a container measured in megabytes.
- **Bring your own model.** Provider-agnostic across hosted and local models, with a
  curated open-weight catalog and hardware-fit checks for running fully local. No lock-in.
- **Learns from your work.** Captures skills and memory as you go and reinforces them
  based on real outcomes.
- **Orchestrates, does not just chat.** Turns an instruction into a plan and fans it
  out into concurrent, governed runs under a shared budget.
- **Extends itself.** Writes its own skills, validates them in a sandbox, and puts
  them to work without a redeploy.
- **Useful inside and outside a larger system.** Run it on its own, or import it
  as a Go module and embed it in your own application.

## Install

One line, no toolchain needed. The script downloads a prebuilt binary for your OS and
architecture and verifies its checksum before installing.

```sh
# Linux and macOS
curl -fsSL https://raw.githubusercontent.com/ionalpha/flynn/main/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/ionalpha/flynn/main/install.ps1 | iex
```

Pin a version with `FLYNN_VERSION` (for example `FLYNN_VERSION=v0.1.0`) or change the
install directory with `FLYNN_INSTALL_DIR`.

That is the last time you need the script. From then on the binary maintains itself:

```sh
flynn version list   # what releases exist
flynn upgrade        # install the newest one
```

`flynn upgrade` verifies a release before installing it, with no cosign, no gh, and no
trust placed in the network: it checks the signature against a Sigstore trust root
compiled into the binary, requires the signing identity to be this repository's release
workflow on a version tag, requires the signature to be present in the public Rekor
transparency log, and only then downloads the archive, pinned to the digest the signed
provenance names. It refuses downgrades, will not trample a package-manager install, and
keeps the old binary if the new one does not run. See [docs/UPGRADE.md](docs/UPGRADE.md).

Prefer another method? Each option below installs the same release.

<details>
<summary><b>Docker (GHCR or Docker Hub)</b></summary>

Multi-architecture images (amd64, arm64) are published on each release. Mount a volume
for the durable state (SQLite store, credential vault, learned skills) so it survives
container replacement, and pass a model key:

```sh
docker run -v flynn-data:/data \
  -e ANTHROPIC_API_KEY=your-key \
  ghcr.io/ionalpha/flynn:latest
```

The same image is on Docker Hub as `ionalpha/flynn:latest`. Pin a version by tag
(`:0.1.0`) instead of `latest`. The API binds loopback inside the container on purpose;
add a channel (for example `-e TELEGRAM_BOT_TOKEN=...`) to reach the agent, or see
[`deploy/README.md`](deploy/README.md) for remote access and hardening.

Every published manifest is keyless-signed with cosign and can be verified:

```sh
cosign verify ghcr.io/ionalpha/flynn:latest \
  --certificate-identity-regexp 'https://github.com/ionalpha/flynn/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

</details>

<details>
<summary><b>Linux packages (deb, rpm, apk)</b></summary>

Native packages for Debian/Ubuntu, Fedora/RHEL, and Alpine are attached to every
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

Needs Go 1.26+:

```sh
go install github.com/ionalpha/flynn/cmd/flynn@latest
```

</details>

<details>
<summary><b>Manual download and verify</b></summary>

Prebuilt binaries for Windows, macOS, Linux, and ARM are attached to every
[release](https://github.com/ionalpha/flynn/releases) alongside a `checksums.txt`. The
checksum file is signed with cosign; verify it before trusting a binary:

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

Store your model API key once. It is encrypted at rest in your OS keychain (or a
passphrase-sealed file where there is no keychain) and revealed only to call the
model, never written to a prompt, a log, or a command's environment:

```sh
flynn auth set openai     # prompts for the key without echoing it
```

Give it a goal and let it work the problem and report back:

```sh
flynn goal "audit the repo for security issues and open a PR with the fixes"
```

Run it as a long-lived service that answers messages from your chat channels:

```sh
flynn serve     # answers Telegram and Signal messages, triaged and driven as goals
```

Building from source: `go build -o flynn ./cmd/flynn`.

## Failure modes designed out

A handful of bugs recur across agent implementations: session and message state
drifting out of sync or going missing, context compaction overwriting earlier work,
a config change quietly disabling a safety check, a misclassified provider error
retrying into a long hang, and crashes that loop on restart. Flynn is built so that
several of these are hard to express in the first place, not because they are caught
after the fact, but because the structure does not contain the boundary they live in.

- **One source of truth for state.** Sessions, messages, skills, and memory are
  projections of a single append-only event log, so there is no second copy to
  drift from and nothing is overwritten in place.
- **No silent loss.** Every change is an ordered, acknowledged, replayable event; a
  failed write is a retryable event rather than a dropped one, and compaction is a
  view over the log, so the original is always recoverable.
- **Deny by default.** Tools are scoped by capability rather than by a blocklist, so
  a config change can remove access but never accidentally grant it.
- **Typed failures.** Errors carry a class set at the adapter boundary, so a
  permanent failure such as a bad key or an unavailable model stops quickly instead
  of retrying into a hang.
- **One static binary.** No language runtime and no native add-ons, which removes
  the install-time and crash-on-startup failure modes that come with them.

This is the project's main bet: the discipline that makes autonomy safe to grant, an
event-sourced, governed, replayable foundation, is the same discipline that keeps the
ordinary failure modes from arising. The foundation comes first, and every capability
below is built as a typed resource on top of it.

## Features

The sections below describe Flynn's capabilities by area. For what runs today
versus what is in progress or planned, see [Status and roadmap](#status-and-roadmap).

### Agents and capabilities

Flynn runs as an **agent**: a system prompt, the model and loop it runs on, and a
set of **capabilities** that map to the concrete tools the agent is allowed to
use, so it only ever has the surface it needs. Agents are versioned resources and
compose (one can extend another), and a delegated sub-agent is granted a subset of
its parent's authority. By default Flynn runs a general-purpose agent.

### Goals, missions, and orchestration

- **Goals and missions.** A *goal* is one objective with a verifiable end-state.
  A *mission* is long-horizon work that owns a tree of sub-goals and outlives any
  single session.
- **A goal tree.** A goal owns its sub-goals, and a mission tracks that tree as it
  fans out and converges, so long-horizon work is structured, not a flat list.
- **Plan and dispatch.** An instruction becomes a plan; the dispatcher fans it out
  into concurrent governed runs, each bounded by the shared budget.
- **A governor.** Every run is bounded by a shared budget pool (tokens and cost),
  an autonomy level, and an approval policy.
- **A mission event spine.** Every decision, tool call, message, approval, and
  checkpoint is an ordered, immutable event that replays for a full audit trail
  and rolls up into live progress.
- **Isolation.** Runs execute in a sandbox, so parallel agents never collide.
- **Declarative and self-healing.** You declare a goal's desired end-state; a
  reconciler drives toward it and converges again after a failure or restart,
  instead of losing the thread mid-task.

### The interactive session

The terminal is not a chat box bolted onto an API. The session is the typed event
spine rendered live, so you watch the agent work with the governance and record
layers in view rather than hidden behind it.

- **The governed stream, on screen.** Governance decisions and record events are
  projected onto the conversation as they happen. A governance overlay (Ctrl+O) shows
  the run's current posture: what was admitted, what was denied, and why.
- **Seal and verify without leaving the shell.** `/seal` seals the current run and
  `/verify` checks it, with a record badge showing the run's verifiable state inline.
- **Replay in place.** `/replay` re-renders a recorded run from its events, and run
  pickers badge each run with its record state.
- **Built for real terminals.** An alternate-screen fallback for hostile emulators,
  cross-emulator key handling (`modifyOtherKeys`), image paste into the composer, and
  `@`-completion ranked by frecency.

### The learning loop

- **Skills from experience.** After complex work, the agent writes reusable skills
  and improves them as it reuses them.
- **Memory.** Durable facts about you and your work, prefetched into context and
  synced after each turn.
- **A curator.** An outcome-driven pass decays and archives skills that stop working,
  so the library stays sharp instead of sprawling. Nothing is ever silently deleted.
- **Reinforced by outcomes.** Skills and memory are strengthened or decayed by real
  signals (tests passing, a task accepted, no correction on the next turn), so the
  agent learns what works, not what it merely tried.
- **Provenance.** Every captured skill or memory is versioned and attributable, so
  you can see which version produced a result, and roll it back.

### Self-extension

The agent treats its own capabilities as data it can author.

- **Integrations are specs, not code.** A new API integration is a catalog entry plus
  a declarative endpoint contract, executed by one generic engine with auth, rate
  limits, and safety built in.
- **It writes its own skills.** When it hits a gap, the agent can author a new skill,
  validate it in a sandbox, and put it to work without a redeploy or a recompile.
- **Extensions run out of process.** An extension is a separate binary that speaks MCP,
  launched confined, so a third-party tool never shares the agent's address space. The
  client is strictly one-directional: an extension answers calls, it cannot drive the
  agent. `flynn extensions dev <name> <binary>` links a locally built one for authoring,
  and `flynn extensions call` runs a single tool confined.
- **Portable.** Every skill is a versioned, attributable resource you can export and
  move between machines.

### Code review

- **A formal verdict, not a comment.** `flynn review <owner>/<repo>#<n>` reviews a pull
  request under the reviewer archetype: one pass over every changed file, then a sweep
  for what the per-file passes missed. Each finding lands on the line it concerns, and
  the verdict links to it.
- **Findings that persist across pushes.** A standing finding is handed back to the
  reviewer on the next run so it is rechecked rather than repeated, and a conversation
  resolves once the finding it raised is gone.
- **Authority is bounded.** Approval is gated behind an explicit `--approve --as`; by
  default the reviewer can request changes and comment but never approve. The command
  exits non-zero when it requests changes, so it drops straight into a pipeline.

### External agent backends

Flynn can drive another coding agent as the model behind a run (`--model claude` or
`--model codex`) while keeping its own governance around it. The external harness is
locked to Flynn's bridge: its native tool surface is denied, and only the tools Flynn
bridges to it are callable, so every action still passes the dispatch waist and lands
in the run's record as attested events. On platforms without a governed-egress leg the
command refuses and says so rather than running the child unconfined.

Either backend drives a one-shot goal, a pull-request review, or an interactive
session:

```bash
flynn --model claude                            # chat, turn by turn, through the CLI
flynn --model codex:gpt-5-codex goal "fix the flaky test"
flynn review owner/repo#123 --model claude
```

In a session the conversation belongs to the CLI: each turn continues the conversation
the harness itself holds, so it answers with the context of the turns before it. The
run is still one durable, sealed record on Flynn's side. `/model claude:<other-model>`
retargets the model the CLI drives from the next turn on, without disturbing that
conversation. Switching to a different harness mid-run is refused: a record declares
the one harness that drove it, so the swap belongs in a new session.

A session driven by an external agent does not learn back into Flynn's skills and
memory, and `/compact` does not apply to it: the harness holds the conversation and
manages its own context. `flynn serve` and `flynn watch` do not take an external
backend, because a server's independent requests have no single conversation to
continue.

### Channels and computer use

- **Real tools on a real machine.** A sandboxed, path-confined toolset for the
  terminal and filesystem: run commands, read, edit, glob, and grep, each admitted
  at the dispatch waist against a capability grant.
- **Lives where you do.** Run it from the terminal, or as a service (`flynn serve`)
  that answers Telegram and Signal messages, each triaged and driven as a goal.

Wider reach (Discord, Slack, voice, a built-in browser, desktop GUI, and mobile
control) is on the [roadmap](#status-and-roadmap).

### Ambient triggers

- **React to markers.** A `flynn watch` mode picks up inbound `ai!` / `ai?` markers
  in your files and turns them into governed goals, so work can start without a
  prompt at the terminal.

Autonomy that forms its own goals from monitored signals is on the
[roadmap](#status-and-roadmap).

### Cost control

- **Hard budgets.** The governor enforces spend ceilings per goal and per mission,
  in tokens and in real money, with full per-run accounting. A run cannot exceed its
  budget; it stops.

### Tools and standards

- **MCP server.** Expose the agent's own tools to any Model Context Protocol client
  (`flynn mcp serve`). Consuming external MCP servers as a client is on the roadmap.
- **Provetrail.** Emits every run as a portable, third-party-verifiable record in the
  open [Provetrail](https://provetrail.org) format. See
  [Verifiable provenance](#verifiable-provenance-provetrail).

### Verifiable provenance (Provetrail)

Flynn is the reference implementation of [Provetrail](https://provetrail.org), an open
standard for verifiable execution provenance: a portable, third-party-verifiable record
of what an agent did, in what order, and under what authority. Flynn ships the reference
verifier and the standard's public [conformance vectors](https://github.com/ionalpha/provetrail),
so any run's record can be checked by any conformant verifier, in any language, without
trusting the host that produced it.

## Trust and safety

Flynn is built to be handed real authority over untrusted input and real tools.

- **Capability-scoped tools.** An agent only ever has the tools its capabilities grant.
- **Sandboxed runs.** Commands execute in a kernel-confined sandbox (read-only host,
  syscall filter) with per-platform adapters, proven by a red-team containment matrix
  in CI; plugins run read-only by default. Stronger container and microVM tiers, and
  remote sandbox backends (E2B, Daytona, Modal) behind the same port, are on the roadmap.
- **Contained network.** The agent's outbound requests go through a default-deny egress
  gate that blocks private, loopback, and cloud-metadata destinations; inbound listeners
  bind loopback-only by default and refuse a wildcard bind. Both are enforced by lint
  rules, so no code can dial or listen around them.
- **Governed autonomy.** Budgets, autonomy levels, and approval policies mean risky
  actions pause for a human instead of proceeding silently.
- **Reversible by default.** Actions are recorded so they can be undone, and
  destructive steps can be rehearsed in a dry run before they execute.
- **Secrets stay out of context.** Credentials live in a vault and are applied at
  call time, never placed in prompts or logs.
- **Verifiable execution.** Each run is sealed into a signed, tamper-evident record:
  every event is committed to an append-only Merkle log under a signed checkpoint, so
  an independent party can confirm what the agent did, and that the record was not
  altered, without trusting the host. `flynn spine verify <run>` checks a run from the
  durable store alone, and the record follows the open [Provetrail](https://provetrail.org)
  format so any conformant verifier can check it. The [`demo/`](demo/) directory is a
  runnable walkthrough: a signed but ungoverned or unproven record is rejected, a real
  one verifies.

The [threat model](docs/THREAT_MODEL.md) sets out the trust boundaries and which defense
covers each class of attack, marking what is enforced today versus planned. To report a
vulnerability, see the [security policy](SECURITY.md).

## Reproducible by design

Because the mission event spine is ordered and immutable, a run is not a black box.
Fork-from-event and run-diff time-travel are on the [roadmap](#status-and-roadmap).

- **Deterministic replay.** Re-run any mission from its recorded events.
- **Verifiable, not just replayable.** A run is sealed into a signed record a
  standalone verifier checks (`flynn spine verify`), so replay rests on tamper-evidence
  a third party can confirm, not on trusting the operator.

## Declarative core

Everything Flynn is (every agent, skill, tool, integration, policy, route,
and goal) is a typed, versioned, schema-checked resource, not hard-coded
behavior. Engines reconcile those resources toward their declared state, which is
what makes the agent self-authoring, shareable across a fleet, replayable, and
safe to change: a new capability is a spec, not a release.

## Engineering and reliability

Most agent projects test the happy path and ship. Flynn is built with the
methods used for systems people depend on.

- **Property-based testing.** The planner, governor, and budget logic are checked
  against invariants over generated inputs, not just hand-picked cases.
- **Chaos engineering.** Faults are injected into tools, providers, and the network,
  and runs are killed and resumed, to prove the agent degrades and recovers cleanly.
- **Deterministic replay harness.** Golden missions replay in CI so behavior changes
  are caught as diffs.
- **Fuzzing.** Tool inputs, manifests, and protocol messages are fuzzed for safety.
- **Simulation and dry-run.** High-impact actions can be rehearsed before they touch
  anything real.
- **Enforced invariants.** Budgets are never exceeded, no action runs without a
  capability, and the concurrent orchestrator is checked under the race detector.

## Command reference

Run `flynn` with no arguments for an interactive session, or `flynn --help` for the
full flag list.

**Running work**

| Command | What it does |
| --- | --- |
| `flynn` | Start an interactive session |
| `flynn goal "<objective>"` | Drive a goal to completion in the current directory |
| `flynn resume <run>` | Continue a parked or interrupted run |
| `flynn watch` | Watch the working tree for `ai!` / `ai?` markers and run each as a governed turn |
| `flynn review <pr>` | Review a pull request and submit a formal verdict |
| `flynn playbook` | List the playbooks, or `playbook run <name>` to run one |
| `flynn serve` | Run as a service: answer chat messages (Telegram, Signal) and/or expose the read-only monitor API |
| `flynn mcp serve` | Expose the toolset to an MCP client over stdio, every call governed and recorded |

**Inspecting what happened**

| Command | What it does |
| --- | --- |
| `flynn runs` | List past runs (id, phase, objective) |
| `flynn status [<run>]` | Show the live overview, or one run's phase and progress |
| `flynn ps` | List instances with their live, heartbeat-aware state |
| `flynn get <kind>` | List resources of a kind (instances, agents, runs, ...) |
| `flynn describe <kind> <id>` | Show one resource's fields and recent change history |
| `flynn diff <kind> <a> <b>` | Show the fields that differ between two resources |
| `flynn inspect <run>` | Replay a past run's recorded events (alias: `replay`) |
| `flynn spine verify <run>` | Report a run's record tier by tier: integrity, governance, ground truth |
| `flynn spine export <run>` | Write a sealed run's portable record to a file for third-party verification |

**Capabilities and models**

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
| `flynn models run <id>` | Provision, serve, and query a local model |
| `flynn models probe <id>` | Measure a local model's agentic reliability and record its profile |
| `flynn models check` | Report installed local runtimes and any known parser advisories |

**Maintenance**

| Command | What it does |
| --- | --- |
| `flynn regrade` | Re-grade learned skills against the working directory |
| `flynn db reset` | Move an unusable database aside (backed up) so the next run recreates it |
| `flynn --version` | Print the version |

Key flags: `--model`, `--fanout`, `--verify "<cmd>"`, `--max-cost`, `--max-tokens`,
`--max-memory`, `--max-processes`, `--no-learn`, `--data-dir`, `--profile <dir>`.

## Use it as a library

Flynn is a Go module, so a host application can embed it directly (no
submodule, no FFI):

```go
import agent "github.com/ionalpha/flynn"

a := agent.New(agent.Config{Model: "anthropic:claude-opus-4-8"})
result, err := a.Goal(ctx, "audit the repo for TODOs and summarize them")
```

## Run it anywhere

- **Locally** as a single binary.
- **Docker.** A small static-binary image with no language runtime to bundle,
  published multi-arch and signed as `ghcr.io/ionalpha/flynn` (also on Docker Hub as
  `ionalpha/flynn`). See [Install](#install).
- **A $5 VPS.** The tiny image and fast cold start make a continuously available
  agent cheap to run.

Kubernetes pod fan-out and serverless hibernation are on the
[roadmap](#status-and-roadmap): because runs are isolated and governed, a mission can
fan its worker runs out as pods once the control plane lands.

## Observability

Flynn exposes an OpenTelemetry-style observability port: the mission event spine maps
directly onto spans and structured events, and every run reports tokens, cost, latency,
and outcome. The default build ships a no-op implementation. Concrete OTLP and
OpenInference export to agent-eval tools (Langfuse, Arize Phoenix), a VictoriaMetrics or
Prometheus metrics backend, and Grafana dashboards are on the
[roadmap](#status-and-roadmap).

## Integrations

| Area | Works with |
| --- | --- |
| Models | Any OpenAI-compatible or native endpoint, hosted or local; Anthropic and OpenAI adapters |
| Local models | Curated open-weight catalog, hardware-fit checks, fetch-and-run, grammar-constrained decoding |
| Messaging | Telegram, Signal, and the terminal |
| Computer use | Terminal and filesystem, sandboxed and path-confined |
| Tools | MCP server; Provetrail reference verifier |
| Budgets | Per-goal and per-mission token and cost ceilings, with per-run accounting |
| Storage | SQLite (local), or a host that implements the `state` interfaces |
| Runtime | Local binary or Docker; kernel-confined command sandbox (per-platform) |

Planned integrations (Discord, Slack, browser, desktop, mobile, voice, A2A, Zed ACP,
external MCP servers, OpenTelemetry export, Postgres, remote sandbox backends) are
tracked in [Status and roadmap](#status-and-roadmap).

## Architecture

```
cmd/flynn/          standalone binary entry point
agent.go            embedding facade (Config, Agent, Goal)
state/              persistence interfaces (the host boundary)
observe/            logging and tracing port (slog + tracer, no-op default)
dispatch/           the action chokepoint: governance, tracing, events
capability/         capability grants, admitted at the dispatch waist
budget/             per-run token and cost ceiling
spine/              the canonical ordered event log (source of truth, replay)
resource/           event-sourced resources materialized from the log
reconcile/          the level-triggered controller loop
goal/               the goal controller and worker
mission/            the conversation executor that advances a goal
learn/              the closed learning loop (capture, verify, reinforce)
skill/, memory/     durable skill and memory stores
llm/, provider/     the model port and concrete adapters
tools/              the default agentic toolset
sandbox/            the isolation boundary for command execution
clock/, ids/, hlc/  determinism: time source, sortable ids, write ordering
fault/              typed, classified error model
runtime/            wires controller, worker, store, and bus together
session/            conversational front door and event stream
storage/sqlite/     the durable SQLite backend
internal/           the mechanism band: model plumbing, guards, the
                    integration/extension engine, and other non-API machinery
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full layer map, the ports a host
implements, and the invariants the engine enforces.

The agent depends only on the interfaces in `state/` (persistence) and
`observe/` (observability). Local implementations and no-op defaults ship in this
repository; a host such as an Ion Alpha instance can supply a richer one (for
example a graph-backed store or a hosted, multi-tenant backend), without this
repository ever depending on the host.

## Own your agent

Your skills, memory, and the model of how you work belong to you. Export them as a
portable artifact and move them between machines, and run the agent fully local
with a local model and no external calls when you need sovereignty.

## Configuration

Configuration lives in a single file plus environment variables for secrets. Set
your model and provider, choose which tools and channels are enabled, and set
budgets and autonomy defaults. See the documentation for the full reference.

## Optional: connect to Ion Alpha

Flynn runs standalone and stores its state locally in SQLite, and it is gaining
graph-backed memory and federated, fleet-wide learning in its own right (see the
roadmap). [Ion Alpha](https://x.com/ionalpha_) is the optional managed host that
delivers those at team and organization scale, turnkey, with no change to how you
use the agent:

- A hosted, multi-tenant foundation: one permissioned, compounding pool of skills
  and knowledge shared across people and projects, so every agent can build on
  every other agent's verified experience.
- A typed knowledge graph as memory, able to connect facts and surface
  contradictions, instead of flat recall.
- Team workspaces, cross-project context, SSO, and full audit and backup.

The boundary is clean: the agent depends only on interfaces, the host implements
them, and the agent always builds and runs standalone.

## Contributing

Issues and pull requests are welcome. See the open issues for the current roadmap
and good first tasks.

## Status and roadmap

Flynn is being extracted from a much larger system and moves fast. The
foundations are in place and a real agent loop runs today; the breadth described
above is filling in on top of that foundation. Follow
[@ionalpha_](https://x.com/ionalpha_) for progress.

**Running today**

- A single static Go binary, cross-compiled for Windows, macOS, Linux, and ARM.
- An event-sourced spine with materialized resources and a self-healing reconcile loop.
- The dispatch waist: every model and tool call admitted against a capability grant, traced, and bracketed by spine events.
- Per-run token and cost budgets with hard ceilings.
- Deterministic replay, with golden missions guarding behavior in CI.
- Signed, tamper-evident run records: each run's events are committed to an append-only Merkle log under a COSE signature, checkable by a standalone verifier (`flynn spine verify`), with a public conformance vector suite for the record format.
- An interactive TUI that renders the typed session spine live: in-session `/seal` and `/verify` with a record badge, a governance overlay (Ctrl+O), `/replay`, and cross-emulator input handling with an alternate-screen fallback.
- A real agent loop (`flynn goal "..."`) with sandboxed, path-confined terminal, filesystem, edit, glob, and grep tools.
- Provider-agnostic models: Anthropic and OpenAI adapters behind a `provider:model` registry.
- Local models end to end: a curated open-weight catalog, hardware-fit checks, one-command fetch and run, a model pool, and grammar-constrained decoding so a local model cannot emit a malformed tool call.
- The learning loop: skills and memory captured from work, reinforced by outcomes, with skills that stop working decayed and archived.
- An MCP server (`flynn mcp serve`) that exposes the agent's own tools to any MCP client.
- Out-of-process MCP extensions: a third-party tool runs as a separate confined binary, answering calls over a strictly one-directional client, with `flynn extensions dev` for authoring one locally.
- Pull-request review (`flynn review`): per-file passes plus a sweep for what they missed, findings anchored to their lines and rechecked across pushes, with approval gated behind an explicit flag.
- External agent backends (`--model claude`, `--model codex`): another coding agent drives the turn with its native tool surface denied, callable only through Flynn's bridged tools, recorded as attested events.
- Replay and inspection surfaces: `flynn inspect`/`replay`, `flynn diff` between two resources, `flynn describe` with change history, and `flynn regrade` to re-grade learned skills.
- Sealed runtime profile bundles (`--profile`): cpu, heap, goroutines, and a sampled timeline under a hashed manifest, with CPU samples attributed to the action that caused them.
- Credentials sealed in an OS keychain or a passphrase vault, kept out of prompts and logs.
- A kernel-confined execution sandbox (read-only host, syscall filter) with per-platform adapters on Linux, macOS, and Windows, proven by a red-team containment matrix in CI, refusing rather than silently downgrading where a tier is unavailable.
- Default-deny outbound egress that blocks SSRF to private, loopback, and cloud-metadata addresses, lint-enforced so nothing dials around it.
- Bind-safe inbound listeners (loopback by default, wildcard binds refused, non-loopback gated on explicit opt-in) with an exposure registry that logs and lease-expires every opening, lint-enforced.
- Inbound over the terminal, Telegram, and Signal.
- SQLite-durable state, and importable as a Go module.
- A published threat model, an OpenSSF Scorecard, and property, chaos, and fuzz test tiers.

**In progress**

- Multi-agent goal-graph orchestration: fan-out across a dependency graph and missions that outlive a single session.
- A cost-aware model router in front of the registry.
- Remote sandbox backends (E2B, Daytona, Modal) behind the same isolation port.
- Time-travel on top of replay: fork-from-event and run diff.
- A pluggable embeddings port for stronger local semantic recall.

**On the roadmap**

- Standards: A2A, Zed ACP, and `agentskills.io` import.
- More reach: Discord, Slack, voice, a built-in browser, desktop GUI, and mobile control.
- Proactive operation: monitors, drives, and self-formed goals within an autonomy level.
- The agent authoring and sandbox-testing its own API integrations.
- A cross-machine control plane and Kubernetes pod fan-out.
- OpenTelemetry export to agent-eval tools and Grafana dashboards.
- A Postgres backend and federated, fleet-wide learning.
- Stronger isolation tiers (gVisor, Firecracker/Kata microVM).

## License

[Apache License 2.0](LICENSE) © Ion Alpha. See [NOTICE](NOTICE).

The license grants no rights in the Ion Alpha or Flynn names, logos, or trademarks.
