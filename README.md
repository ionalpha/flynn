<h1 align="center">Flynn</h1>

<p align="center"><strong>An open-source agent operating system in a single Go binary. Bring your own model, orchestrate many agents toward a goal, give it autonomy you can actually trust, and watch it get better the more you use it.</strong></p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="License: MIT"></a>
  <img src="https://img.shields.io/badge/Go-1.24+-00ADD8.svg" alt="Go 1.24+">
  <a href="https://x.com/ionalpha_"><img src="https://img.shields.io/badge/Follow-@ionalpha__-1DA1F2.svg" alt="Follow on X"></a>
</p>

---

Flynn is a lightweight agent runtime and operating system written in Go. It
runs standalone as a single static binary on anything from a $5 VPS to a
Kubernetes cluster, works with any model provider, and can optionally connect to
an [Ion Alpha](https://x.com/ionalpha_) instance for a shared knowledge graph and
fleet-wide learning.

Four ideas run through everything it does:

1. **It compounds.** A closed learning loop turns each session into durable
   skills and memory, reinforced by whether the work actually succeeded.
2. **It scales past one task.** A goals-and-missions engine plans, fans out, and
   governs many agent runs toward a single objective.
3. **It owns its cost.** A cost-aware router and on-demand tool loading keep token
   usage low, so running it continuously is affordable.
4. **You can trust it with autonomy.** Every action is governed, reversible,
   auditable, and replayable, so giving it real authority is a decision, not a gamble.

## Why Flynn

- **One binary, no runtime.** No Python, no Node, no virtualenv, no
  `node_modules`. `curl | sh` drops a single file. Cross-compiles to Windows,
  macOS, Linux, and ARM, and ships in a container measured in megabytes.
- **Bring your own model.** Provider-agnostic across hosted and local models. A
  cost-aware router sends each step to the cheapest model that can do it, no lock-in.
- **Learns from your work.** Captures skills and memory as you go, curates them in
  the background, and reinforces them based on outcomes.
- **Orchestrates, does not just chat.** Turns an instruction into a goal graph and
  runs it in parallel under a budget.
- **Extends itself.** Writes its own skills and integrations, tests them in a
  sandbox, and puts them to work, without a redeploy.
- **Acts on its own initiative.** Watches your signals and pursues your goals
  ahead of you, within limits you set.
- **Useful inside and outside a larger system.** Run it on its own, or import it
  as a Go module and embed it in your own application.

## Features

### Agents and capabilities

Flynn ships a set of agent archetypes (a balanced generalist, a careful
architect, a fast shipper, a researcher, a critic, and an analyst), each defined
by a set of **capabilities** that map to the concrete tools it is allowed to use,
so an agent only ever has the surface it needs. Define your own archetypes in config.

### Goals, missions, and orchestration

- **Goals and missions.** A *goal* is one objective with a verifiable end-state.
  A *mission* is long-horizon work that owns a graph of sub-goals and outlives any
  single session.
- **A typed goal graph.** Goals relate through `decomposes_into`, `depends_on`,
  and `blocks` edges, so a mission is a dependency graph, not a flat list.
- **Plan and dispatch.** An instruction becomes a plan; the dispatcher fans it out
  into governed runs, sequentially, in parallel, or in parallel within each
  dependency level.
- **A governor.** Every run is bounded by a shared budget pool (tokens and cost),
  an autonomy level, and an approval policy.
- **A mission event spine.** Every decision, tool call, message, approval, and
  checkpoint is an ordered, immutable event that replays for a full audit trail
  and rolls up into live progress.
- **Isolation.** Runs can execute in their own git worktree or sandbox, so
  parallel agents never collide.
- **Declarative and self-healing.** You declare a goal's desired end-state; a
  reconciler drives toward it and converges again after a failure or restart,
  instead of losing the thread mid-task.

### The learning loop

- **Skills from experience.** After complex work, the agent writes reusable skills
  and improves them as it reuses them.
- **Memory.** Durable facts about you and your work, prefetched into context and
  synced after each turn.
- **A curator.** A background pass consolidates, archives, and pins skills so the
  library stays sharp instead of sprawling. Nothing is ever silently deleted.
- **Reinforced by outcomes.** Skills and memory are strengthened or decayed by real
  signals (tests passing, a task accepted, no correction on the next turn), so the
  agent learns what works, not what it merely tried.
- **Provenance.** Every captured skill or memory is versioned and attributable, so
  you can see which version produced a result, and roll it back.

### Self-extension

The agent treats its own capabilities as data it can author.

- **Integrations are specs, not code.** A new API integration is a catalog entry
  plus an OpenAPI document, executed by one generic engine with auth, rate limits,
  and safety built in.
- **It writes its own tools.** When it hits a gap, the agent can author a new skill
  or plugin manifest, validate it in a sandbox, and put it to work without a
  redeploy or a recompile.
- **Open standard.** Skills follow the `agentskills.io` format, and importers
  migrate existing skills and config from other agents.

### Computer use and reach

- **Runs real tasks on a real computer.** Terminal, filesystem, and a built-in
  browser with CDP and self-healing selectors, plus desktop GUI control, mobile
  (ADB), and voice.
- **Lives where you do.** Talk to it from the terminal or from Telegram, Discord,
  Slack, and Signal through a single gateway, with voice memos transcribed
  automatically.
- **Scheduled automations.** Built-in cron runs reports, backups, and audits
  unattended, delivered to any connected channel.

### Proactive and ambient

Most agents wait to be prompted. Flynn can take initiative.

- **Watches your signals.** Monitors, data sources, and events feed it context
  continuously.
- **Forms its own goals.** When something matters, it proposes or pursues a goal on
  your behalf, within its autonomy level, and surfaces the result.
- **Driven, not idle.** A drives model gives it a sense of what is worth doing next
  instead of sitting still until spoken to.

### Agent-native economy

- **A wallet and budgets.** The governor enforces hard spend ceilings per goal and
  per mission, in tokens and in real money.
- **Pays for what it uses.** Tools, compute, and data, with full per-run accounting.
- **A skill marketplace.** Agents publish, discover, and acquire skills and
  integrations, so capability spreads without code.

### Tools and standards

- **MCP.** Connect any Model Context Protocol server, and expose the agent's own
  tools to other clients.
- **A2A.** Speak the Agent-to-Agent protocol for cross-agent coordination, governed
  alongside MCP by the Linux Foundation's Agentic AI Foundation.
- **Editor integration.** Run as a Zed Agent Client Protocol (ACP) server inside
  editors.

### Optional: connect to Ion Alpha

On its own, Flynn stores state locally in SQLite. Point it at an
[Ion Alpha](https://x.com/ionalpha_) instance and it gains a richer substrate
without any change to how you use it:

- A **typed knowledge graph** as its memory, able to connect facts and surface
  contradictions, instead of flat recall.
- A **fleet brain**: many agents sharing one permissioned, compounding pool of
  skills and knowledge, so every agent learns from every other agent's verified
  experience.
- Team workspaces, cross-project context, and full audit and backup.

The boundary is clean: the agent depends only on interfaces, and the host
implements them. The agent always builds and runs standalone.

## Trust and safety

Flynn is built to be handed real authority over untrusted input and real tools.

- **Capability-scoped tools.** An agent only ever has the tools its capabilities grant.
- **Sandboxed runs.** Runs execute in an isolated git worktree or a sandbox backend
  (E2B, Daytona, or Modal); plugins run read-only by default.
- **Governed autonomy.** Budgets, autonomy levels, and approval policies mean risky
  actions pause for a human instead of proceeding silently.
- **Reversible by default.** Actions are recorded so they can be undone, and
  destructive steps can be rehearsed in a dry run before they execute.
- **Adversary review.** A reviewer red-teams a plan for unsafe actions and prompt
  injection before it runs.
- **Untrusted channels.** Inbound messages from unknown senders are gated by
  pairing and allowlists, not processed blindly.
- **Secrets stay out of context.** Credentials live in a vault and are applied at
  call time, never placed in prompts or logs.

## Reproducible by design

Because the mission event spine is ordered and immutable, a run is not a black box.

- **Deterministic replay.** Re-run any mission from its recorded events.
- **Fork from any point.** Branch a new run from any event to explore an alternative.
- **Diff and time-travel.** Compare two runs event by event, and step backward to
  see exactly where a decision was made.

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

## Install

```sh
# With the Go toolchain
go install github.com/ionalpha/flynn/cmd/flynn@latest

# Or download a prebuilt binary for Windows, macOS, Linux, or ARM from Releases
```

## Quick start

```sh
flynn --model anthropic:claude-opus-4-8     # start an interactive session
flynn --version
```

Give it a goal and let it plan, fan out, and report back:

```sh
flynn goal "audit the repo for security issues and open a PR with the fixes"
```

Talk to it from your chat apps:

```sh
flynn gateway     # starts the Telegram, Discord, Slack, and Signal gateway
```

Building from source: `go build -o flynn ./cmd/flynn`.

## Command reference

| Command | What it does |
| --- | --- |
| `flynn` | Start an interactive session |
| `flynn goal "<objective>"` | Plan and run a goal to completion |
| `flynn watch` | Run proactively against your monitors and signals |
| `flynn replay <mission>` | Replay or time-travel-debug a past mission |
| `flynn gateway` | Start the messaging gateway |
| `flynn model` | Choose the provider and model |
| `flynn skills` | List and manage learned skills |
| `flynn --version` | Print the version |

## Use it as a library

Flynn is a Go module, so a host application can embed it directly (no
submodule, no FFI):

```go
import agent "github.com/ionalpha/flynn"

a := agent.New(agent.Config{Model: "anthropic:claude-opus-4-8"})
_ = a.Run(ctx)
```

## Run it anywhere

- **Locally** as a single binary.
- **Docker.** A small static-binary image with no language runtime to bundle.
- **Kubernetes.** Because runs are isolated and governed, a mission can fan its
  worker runs out as pods, scale them independently, and tear them down when the
  goal is met. The tiny image and fast cold start make per-run pods practical.
- **Serverless or a $5 VPS.** Hibernates when idle and wakes on demand, so a
  continuously available agent costs almost nothing between sessions.

## Observability

Flynn emits OpenTelemetry traces and metrics. The mission event spine maps
directly onto spans and structured events, and every run reports tokens, cost,
latency, and outcome.

- **Traces** export over OTLP and OpenInference to agent-eval tools such as
  Langfuse and Arize Phoenix, for step-level tracing and evaluation.
- **Metrics** export to [VictoriaMetrics](https://victoriametrics.com) or any
  Prometheus-compatible backend for long-term, high-cardinality cost and performance.
- **Dashboards** in Grafana for spend, throughput, success rate, and skill reuse.

## Integrations

| Area | Works with |
| --- | --- |
| Models | Any OpenAI-compatible or native endpoint, hosted or local; routed cost-aware |
| Messaging | Telegram, Discord, Slack, Signal, and the terminal |
| Computer use | Terminal, filesystem, browser (CDP), desktop GUI, mobile (ADB), voice |
| Tools | Any MCP server; A2A peers; Zed ACP for editor integration |
| Skills | `agentskills.io` format, with importers from other agents |
| Payments | Per-goal budgets and agent-native payment rails |
| Storage | SQLite (local), Postgres, or an Ion Alpha instance |
| Observability | OpenTelemetry, OpenInference, Langfuse, Arize Phoenix, VictoriaMetrics, Grafana |
| Runtime | Local, Docker, Kubernetes, serverless; sandboxed runs via E2B, Daytona, or Modal |
| Source control | Git worktrees for isolated, parallel runs |

## Architecture

```
cmd/flynn/          standalone binary entry point
agent.go          core runtime (Config, Agent, Run)
state/            persistence and context interfaces (the host boundary)
orchestration/    goals, missions, dispatcher, governor, event spine
skills/           skill capture, curation, and reinforcement
router/           cost-aware model routing
integrations/     data-driven integration and plugin engine
gateway/          messaging channels and computer use
economy/          wallet, budgets, and the skill marketplace
replay/           deterministic record and replay
mcp/              tool protocol server and client
internal/         build and runtime internals
```

The agent depends only on the interfaces in `state/`. Local implementations ship
in this repository; a host such as an Ion Alpha instance can supply a richer one
backed by a knowledge graph and fleet-wide learning, without this repository ever
depending on the host.

## Own your agent

Your skills, memory, and the model of how you work belong to you. Export them as a
portable artifact and move them between machines, and run the agent fully local
with a local model and no external calls when you need sovereignty.

## Configuration

Configuration lives in a single file plus environment variables for secrets. Set
your model and provider, choose which tools and channels are enabled, and set
budgets and autonomy defaults. See the documentation for the full reference.

## Contributing

Issues and pull requests are welcome. See the open issues for the current roadmap
and good first tasks.

## Status

Flynn is actively being extracted from a much larger system. Follow
[@ionalpha_](https://x.com/ionalpha_) for progress.

## License

[MIT](LICENSE) © Ion Alpha
