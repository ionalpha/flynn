<h1 align="center">Flynn</h1>

<p align="center"><strong>A secure, self-improving agent operating system in a single Go binary. Bring your own model, point it at a goal, and grant it real authority, because every action is sandboxed, governed, and sealed into a verifiable, tamper-evident record.</strong></p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License: MIT"></a>
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8.svg" alt="Go 1.26+">
  <a href="https://github.com/ionalpha/flynn/actions/workflows/ci.yml"><img src="https://github.com/ionalpha/flynn/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://pkg.go.dev/github.com/ionalpha/flynn"><img src="https://pkg.go.dev/badge/github.com/ionalpha/flynn.svg" alt="Go Reference"></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/ionalpha/flynn"><img src="https://api.securityscorecards.dev/projects/github.com/ionalpha/flynn/badge" alt="OpenSSF Scorecard"></a>
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
install directory with `FLYNN_INSTALL_DIR`. Prebuilt binaries for Windows, macOS, Linux,
and ARM, with a `checksums.txt` you can verify by hand, are attached to every
[release](https://github.com/ionalpha/flynn/releases).

With the Go toolchain (builds from source, needs Go 1.26+):

```sh
go install github.com/ionalpha/flynn/cmd/flynn@latest
```

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

Flynn ships a set of agent archetypes (a balanced generalist, a careful
architect, a fast shipper, a researcher, a critic, and an analyst), each defined
by a set of **capabilities** that map to the concrete tools it is allowed to use,
so an agent only ever has the surface it needs. Define your own archetypes in config.

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
- **Portable.** Every skill is a versioned, attributable resource you can export and
  move between machines.

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
- **Provetrail.** Implements [Provetrail](https://github.com/ionalpha/provetrail), an
  open standard for verifiable execution provenance, and ships a reference verifier and
  the standard's public conformance vectors, so a run's record can be checked by any
  conformant verifier in any language.

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
  durable store alone, and the record follows the open [Provetrail](https://github.com/ionalpha/provetrail)
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

| Command | What it does |
| --- | --- |
| `flynn` | Start an interactive session |
| `flynn goal "<objective>"` | Run a goal to completion |
| `flynn serve` | Run as a service that answers Telegram and Signal messages |
| `flynn auth set <provider>` | Store an API key in the encrypted vault |
| `flynn models` | Browse the model catalog and check which fit your hardware |
| `flynn runs` | List past runs and sessions |
| `flynn resume <run>` | Continue a past run |
| `flynn replay <run>` | Replay a recorded run |
| `flynn spine verify <run>` | Verify a run's signed, tamper-evident record |
| `flynn --version` | Print the version |

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
- **Docker.** A small static-binary image with no language runtime to bundle.
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
- User-facing replay and time-travel: `flynn replay`, fork-from-event, and run diff, plus re-grading captured skills by re-folding the spine.
- A pluggable embeddings port for stronger local semantic recall.

**On the roadmap**

- Standards: an MCP client for external servers, A2A, Zed ACP, and `agentskills.io` import.
- More reach: Discord, Slack, voice, a built-in browser, desktop GUI, and mobile control.
- Proactive operation: monitors, drives, and self-formed goals within an autonomy level.
- The agent authoring and sandbox-testing its own API integrations.
- A cross-machine control plane and Kubernetes pod fan-out.
- OpenTelemetry export to agent-eval tools and Grafana dashboards.
- A Postgres backend and federated, fleet-wide learning.
- Stronger isolation tiers (gVisor, Firecracker/Kata microVM).

## License

[MIT](LICENSE) © Ion Alpha
