# Threat model

This document is the published threat model for Flynn: what it is built to defend
against, where its trust boundaries are, and which defense is responsible for each class
of attack. It is written against what the code actually enforces today. Where a defense
is planned but not yet built, it is marked as such, so a reader can tell an enforced
control from an intended one. Reporting instructions are in [SECURITY.md](../SECURITY.md).

Flynn runs an autonomous agent that takes real actions (shell commands, file edits,
network calls, and running local models) driven by a language model over untrusted input.
The model's output is not trusted, the input it reads is not trusted, and a model file it
runs is not trusted. The design assumption is that any of these can be adversarial, and
the goal is that the host, the user's data, and the user's credentials stay safe anyway.

## Assets

- The host machine and its filesystem outside the working tree.
- The user's credentials (model API keys and any brokered secrets).
- The integrity of the run record (what happened, in what order).
- The user's compute and budget (not burned by a runaway or a hostile model).

## Actors and trust levels

Work is classified by how far it is trusted, which sets the isolation it requires before
it runs (`sandbox.Trust`, enforced at the dispatch boundary):

- **Trusted**: the agent's own built-in tools (structured file operations, the model
  call). Runs at the process-jail floor and above.
- **Semi-trusted**: model-authored content, primarily a shell command the model wrote.
  Not hostile by construction, but not vetted. Requires the kernel-confined tier.
- **Untrusted**: code or data from outside that we cannot vouch for: a downloaded model
  file parsed by a runtime with a history of memory-safety flaws, or an unsigned plugin.
  Requires the hardware-isolation tier (not yet built; see Coverage).
- **External harness**: an installed agent CLI driven as a backend for a run, instead of a
  single model call. Its own code is outside our control and its own sandbox is bypassed, so
  it is treated as untrusted: it runs only inside the kernel-confined tier with its egress
  gated, and its tools are offered to it only through the run's bridge. What it cannot make
  observable, its inner model calls and the context it compacts, is recorded as a declared
  gap rather than trusted.

## Trust boundaries

1. **The dispatch boundary.** Every action the agent takes, a model call, a tool call, a
   shell command, flows through one chokepoint (`dispatch.Dispatcher.Govern`). Admission,
   trust classification, the containment check, event recording, and tracing are applied
   there, once, so no call site can take an action without passing them.
2. **The sandbox boundary.** All command and file execution goes through a
   `sandbox.Sandbox`, which confines work to a working directory and (in stronger tiers)
   applies kernel-enforced filesystem and syscall confinement. A lint rule forbids
   spawning a process through `os/exec` anywhere outside the sandbox package, so a new
   call site cannot bypass the boundary.
3. **The egress boundary.** The agent's own outbound requests go through a default-deny
   network gate that rejects private, loopback, and cloud-metadata destinations, and a
   lint rule forbids raw dials and the unguarded standard HTTP clients outside that gate.
4. **The model-source boundary.** A model is classified, integrity-checked, and gated on
   isolation before it is fetched or run; a model from an unknown source is untrusted by
   default.
5. **The credential boundary.** Credentials live in a vault and are applied at call time;
   the sandbox runs commands with a scrubbed environment, so a secret is never placed in a
   child process's environment, a prompt, or a log.
6. **The inbound boundary.** Every listener Flynn opens is bound through a bind-safe gate
   (`bindguard`) that is loopback-only by default, refuses a wildcard bind (every
   interface) unconditionally, and refuses a non-loopback bind without an explicit
   operator opt-in; a lint rule forbids opening a listener outside that gate. It is the
   inbound mirror of the egress boundary: where egress controls where the agent may
   connect, this controls who may reach what the agent exposes.
7. **The external-agent boundary.** A run can be driven by an external agent CLI in place of
   a native loop. The CLI is untrusted code run as a subprocess, so its own execution surface
   is locked down and the run's tools are offered to it only through a loopback Model Context
   Protocol bridge the run hosts. Every tool call it makes returns through that bridge and is
   admitted, contained, and recorded at the dispatch boundary, the same waist a native loop
   passes, so swapping in an external harness never widens what a run may do or escapes a
   halt. Where the child runs in its own network namespace and cannot reach the host, the
   bridge is reached through an inbound forward that exposes exactly the one loopback service
   the run hosts and nothing else on the host.

## STRIDE analysis

### Spoofing (who or what an input claims to be)

- **A model file impersonating a trusted source.** A reference to a model from an
  unrecognized publisher, a raw URL, or a local file is classified untrusted by default;
  only a vetted, digest-pinned catalog entry is trusted, and a recognized first-party
  publisher is at most semi-trusted (`inference/modelsource`). A matching digest proves
  integrity, not safety, so provenance and isolation, not the digest alone, decide whether
  a model may run.
- **A swapped model file after vetting.** Weights are verified against a pinned SHA-256
  before use; a source without a pinned digest is pinned on first use and a later mismatch
  is refused, so a registry that serves a different file than was vetted is rejected.

### Tampering (unauthorized modification)

- **Writing outside the working tree.** The kernel-confined tier makes the host read-only
  and grants write access only to the working directory, so a command, including a
  model-authored shell command, cannot modify host files outside the grant. Proven per
  platform by the containment matrix.
- **A poisoned chat template rewriting the prompt contract.** A model file can embed its
  own chat template. Flynn parses the file with its own hardened reader, never the
  runtime's parser, and forces a known, trusted template at run time, so a template
  embedded in hostile weights cannot rewrite the prompt contract.
- **Tampering with the run record.** The mission event spine is append-only and ordered.
  On its own that resists silent rewriting only as far as the operator of the store is
  trusted. A run is therefore also sealed into a signed, tamper-evident record: its
  events are committed to an append-only RFC 6962 Merkle log and the head is signed as a
  COSE_Sign1 checkpoint with the instance's Ed25519 key. Any alteration, a changed,
  reordered, dropped, or inserted event, no longer reproduces the signed root, so an
  independent party can detect tampering with `flynn spine verify` without trusting the
  host. The verifier rehashes the exact bytes the record carries rather than
  re-serializing, so verification holds across languages.

- **Substituting the binary through the upgrade path.** A self-updating program is a
  remote code execution path with a friendly name, so `flynn upgrade` trusts nothing it
  downloads. The release listing is used only to enumerate candidates. What is installed
  is decided by a SLSA provenance statement signed at build time by GitHub's OIDC
  identity for flynn's release workflow, verified in-process against a Sigstore
  certificate authority compiled into the binary, with the certificate checked as of the
  moment Rekor recorded the signature (a Fulcio certificate lives ten minutes) and the
  identity pinned to this repository's release workflow on a version tag, by numeric
  repository and owner id as well as by URL. The signature must also be present in the
  public transparency log: the inclusion proof has to reconstruct a root the log
  operator signed, and the logged entry has to commit to this exact envelope, so a
  forged release cannot be issued quietly to one victim. Only then is the archive
  downloaded, pinned to the digest the signed provenance names and verified as it
  streams. TLS is not the trust anchor at any point, so a hostile network, a
  mis-issuing certificate authority, or a compromised mirror does not move the
  attacker closer. There is no long-lived release signing key to steal. See
  [UPGRADE.md](UPGRADE.md).
- **Rollback and freeze through the upgrade path.** Both attacks use genuinely signed
  releases, so no signature check can catch them. flynn remembers the highest version it
  has ever verified and the newest it has ever been offered: it refuses to install
  anything below the former without an explicit `--allow-downgrade`, and it reports a
  listing that withdraws a release it has already shown, which is what a machine being
  held on a vulnerable version looks like from the inside.
- **Subverting the install itself.** The new binary is staged in the target's own
  directory and renamed into place, so the swap is atomic and no partially written
  binary is ever executable. The running executable's path is resolved through its
  symlinks before anything is written, so a link planted between the check and the write
  cannot redirect it. Nothing that has not verified is ever executed, and the new binary
  must run and report its own version before it is kept. An install owned by a package
  manager is refused rather than trampled.

### Repudiation (denying an action took place)

- **An unattributable action.** Every action is recorded on the event spine through the
  one dispatch boundary, with its trust level, so each privileged action is attributable
  and the run is auditable and replayable.
- **Denying the record itself.** The sealed run record is signed by the instance's key,
  and the signature binds the whole event sequence through the Merkle root, so the
  producer cannot later disown a run it sealed or claim a different sequence of events.
  The signing key is identified by a self-certifying id carried in the record, so a
  verifier checks the signature against the key the record names.

### Information disclosure (leaking data)

- **Credential exfiltration through a command's environment.** The sandbox never inherits
  the agent's environment; a command sees only a minimal, credential-free baseline plus
  variables granted to it by name, so a model-run command cannot read the agent's keys
  from its own environment.
- **Credentials in prompts or logs.** Secrets are held as a redacting type and resolved
  from the vault at call time, so they are not formatted into prompts, logs, or error
  output.
- **Server-side request forgery and exfiltration over the network.** The agent's outbound
  HTTP goes through a default-deny egress gate that refuses loopback, private, link-local,
  and cloud-metadata addresses and is re-checked on every redirect hop, closing the SSRF
  and metadata-endpoint class. A local model server is bound to the loopback interface
  only, so it is never exposed off the machine.
- **An agent service silently reachable off-host.** A listener Flynn opens (the
  control-plane API, a local model server, a future served endpoint) is bound through the
  inbound gate (`bindguard`), which binds the loopback interface by default, refuses a
  wildcard bind that would expose every interface, and refuses any non-loopback bind unless
  the operator explicitly opts in. The recommended way to reach a service from off the
  machine is a tunnel to its loopback bind, so the safe shape is the default and exposure
  is always a deliberate, auditable choice rather than an accident.
- **An exposure that lingers after it is no longer needed.** Every listener opened through
  the inbound path is recorded in an exposure registry: logged on open and on close, and
  enumerable, so nothing is exposed silently. An exposure given a time-to-live is torn down
  automatically when its lease ends rather than living until the process dies, so an
  ephemeral opening (a preview server, a temporary tunnel) is bounded by construction.
- **An unauthenticated service reachable by anyone.** Authentication is on by default, by
  construction. The control-plane API resolves every request to a scoped principal through
  an authenticator; a server built without one fails closed (it denies every request)
  rather than serving openly, so an unauthenticated API is not representable by omission.
  When no operator credential is supplied, a strong bearer token is generated and shown
  once rather than the API running open, so the secured path is the zero-config default and
  there is no reason to fall back to an unauthenticated one. A service the agent itself
  stands up as a child process (one whose internals Flynn cannot add auth to) is contained
  at the network boundary instead: bound loopback by default and, where the sandbox
  enforces it, run with its network confined, so an unauthenticated service it brings up is
  not reachable off-host regardless.

### Denial of service (exhausting a resource)

- **A runaway loop or cost blowup.** A run carries a total spend and token ceiling
  (`--max-cost`, `--max-tokens`) enforced at the dispatch boundary: each action is checked
  against the run's remaining budget and refused once the ceiling is reached, and a
  fan-out's children draw on the one shared ceiling so a delegated tree cannot spend past
  it. A runaway circuit breaker halts a run whose step count climbs without converging, on
  both the single-conversation and fan-out paths. A per-command wall-clock cap is available
  at the sandbox boundary. Memory and process-count caps on a command the agent runs
  (`--max-memory`, `--max-processes`) are enforced where the platform supports it, through
  the confined command's job object on Windows today; the Linux and macOS floors record the
  limits but do not yet impose them (a per-run cgroup needs cgroup-v2 delegation that is not
  guaranteed for an unprivileged agent), so a command needing a hard memory or process cap
  there runs under the container or hardware-isolation tier. That per-platform gap is the
  remaining piece and is stated rather than implied (see Coverage).

### Elevation of privilege (gaining capability not granted)

- **A tool the run was not granted.** Each action is admitted against the run's capability
  grant by name at the dispatch boundary; an action the grant does not permit is refused
  before any side effect, and with no grant bound the agent is unconstrained only in the
  standalone default.
- **A delegated sub-task gaining authority its parent did not hold.** A run can fan out into
  child runs, and a child must never exceed the authority of the run that spawned it.
  Delegation is itself a capability-gated action, admitted against the parent's grant at the
  dispatch boundary, so a run whose grant omits it cannot spawn at all. The mechanism requires
  a child's grant to be the parent's grant narrowed to the actions requested for it, a strict
  subset, so authority only shrinks down the tree; the children draw on the parent's single
  budget, so a fan-out cannot spend past the run's ceiling; and each child is a resource owned
  by the parent, reaped when the parent is cancelled or removed.
- **Running model-authored or untrusted code on a host that cannot contain it.** Each work
  kind carries a trust level, and the containment gate refuses work whose trust needs
  stronger isolation than the host provides, rather than silently running it at a weaker
  tier. Semi-trusted work needs the kernel-confined tier; untrusted work needs the
  hardware-isolation tier and is refused until one is available.
- **Escaping the kernel-confined tier through a dangerous syscall.** The syscall filter
  denies the calls a working command has no honest need for and that would let it escalate
  or escape; the containment matrix proves a forbidden syscall is denied under the filter.
- **A kernel exploit in an untrusted model's runtime reaching the host.** A downloaded
  model is parsed by a runtime with a history of remote-code-execution flaws, so its worst
  case is arbitrary code execution that escapes a shared kernel. The hardware-isolation
  tier runs such work in a guest with its own kernel on hardware virtualization, so a
  kernel exploit inside cannot reach the host kernel. The tier holds two boundaries: the
  guest's own kernel boundary, and the monitor-to-host boundary, where the
  virtual-machine monitor is itself run jailed and least-privilege (dropped privileges, a
  syscall filter, its own resource limits, a unique uid per guest, and no network device
  while egress is denied), so a guest escape still does not become a host compromise. The
  guest runs with egress denied, resources capped, no credentials, and weights mounted
  read-only; the tier refuses to start a guest whose posture is weaker than that.
- **An external agent CLI taking an action off the governed path.** A run driven by an
  external agent CLI runs that CLI as an untrusted subprocess with its native execution
  surface denied. A CLI with its own read-only sandbox flag has it set and native approvals
  denied; a CLI without one has its effector tools denied under a permission mode that
  neither prompts nor auto-approves, so in the fresh per-episode configuration the run gives
  it, a tool the run did not allow is denied by default, including one a future build might
  add. Either way the only way the CLI can affect the workspace is a bridged tool admitted at
  the dispatch boundary, and the kernel-confined tier contains what it attempts regardless of
  the CLI's cooperation. What the harness does that the run cannot observe, its inner model
  calls and its direct channel to its own provider, is recorded as a declared gap carrying a
  provenance tier, so an external-agent run never claims the integrity of a native one.
- **An external harness reaching another local service through the bridge.** The bridge the
  harness is pointed at is the run's own, on the host loopback. A child confined to its own
  network namespace cannot reach the host loopback at all; it reaches the bridge through an
  inbound forward that pipes exactly one host address, so forwarding the bridge in does not
  hand the child the rest of the host's local services, proven by the sandbox forward tests.

## Coverage: enforced today vs planned

Enforced and tested today:

- One dispatch boundary with capability admission and a containment gate; a lint rule
  forbids a bypass through `os/exec`.
- Secure-by-default execution at the kernel-confined tier where the platform provides it
  (read-only host, syscall filter), with per-platform adapters on Linux, macOS, and
  Windows, and a refusal rather than a silent downgrade where it cannot be enforced.
- A red-team containment matrix that proves, per platform on CI, that each tier denies the
  filesystem-write, network, and syscall escapes it claims to.
- A CI-gated governance benchmark that measures the dispatch waist's attack-success rate
  against a versioned prompt-injection and jailbreak corpus while holding a benign-request
  pass floor, so the decision-layer guardrail is measured on every change rather than
  asserted. It is the decision-layer analog of the containment matrix: the matrix proves
  isolation, this measures how much the capability gate reduces a hostile instruction's
  success over an ungoverned baseline.
- A per-run spend and token ceiling enforced at the dispatch boundary (an action is refused
  once the run's budget is reached, and a fan-out shares one ceiling), a runaway circuit
  breaker on both the single-conversation and fan-out paths, and best-effort memory and
  process-count caps on a command the agent runs where the platform supports them (a Windows
  job object today).
- Default-deny outbound egress for the agent's own requests, with anti-SSRF and
  metadata-endpoint blocking, plus lint rules against raw dials and unguarded HTTP clients.
- An external-agent backend that drives an installed agent CLI as an untrusted subprocess:
  its native tool surface denied, its tool calls routed through a loopback MCP bridge and
  governed at the dispatch boundary, its egress gated to the provider, the bridge reached
  from a confined child through an inbound forward that opens exactly that one host address,
  and the parts it cannot make observable recorded as declared provenance gaps rather than
  trusted. A live episode refuses on a platform whose governed-egress leg is not present
  rather than running with the child's egress open.
- Bind-safe inbound listeners: every listener is opened through a loopback-by-default gate
  that refuses a wildcard bind and a non-loopback bind without an explicit opt-in, plus a
  lint rule against opening a listener outside that gate.
- Auth on by default for the control-plane API: requests resolve to a scoped principal, a
  server built without an authenticator fails closed, and a missing operator credential is
  generated rather than dropped to open access.
- An exposure registry that records every opened listener (logged and enumerable) and
  tears down a leased exposure when its TTL expires, so nothing stays exposed silently.
- The full model-source trust pipeline: classification, code-executing-format refusal,
  digest verification with pin-on-first-use, runtime version-floor gating, hardened
  file parsing, a forced trusted chat template, loopback-only serving, and explicit
  consent for a risky run with a safe default and a non-interactive refusal.
- Provenance-classified durable memory with a write-path refusal gate: every item carries
  the sources it came from, trust is derived from the weakest of them, an untrusted-origin
  write carrying a hidden-instruction payload is refused before it is stored, and a fact
  produced in a run that read untrusted input is marked tainted at the write even when it
  credits only the agent itself. Taint and untrusted provenance both bar an item from the
  wake digest, the one path that reaches a reader without anyone asking; they do not bar it
  from recall, where the reader sees its provenance and decides.
- Credential isolation: vault-held, redacted, never in a child's environment.
- An append-only, ordered event spine as the record of what happened, and, on a run's
  convergence, a signed, tamper-evident sealed record of it (RFC 6962 Merkle log under a
  COSE/Ed25519 signed checkpoint) that a standalone verifier (`flynn spine verify`)
  checks without trusting the host, backed by a public conformance vector suite for the
  record format.

Planned, not yet enforced (a control in this list is not something to rely on today):

- The container, user-space-kernel, and hardware-isolation (microVM) tiers. Until the
  hardware-isolation tier exists, untrusted work (an arbitrary downloaded model, an
  unsigned plugin) is refused rather than run, which is the safe failure, not a silent
  downgrade.
- Host CPU, memory, and process-count caps at the always-present tier on Linux and macOS.
  Windows enforces memory and process caps through a job object today and the spend, token,
  and runaway breakers are cross-platform; the Linux and macOS floors record the memory and
  process limits but do not yet impose them, pending cgroup-v2 delegation (a hard cap there
  runs under the container or hardware-isolation tier).
- A per-run outbound allowlist for sandboxed child processes (today the child network is
  either open or denied as a whole, not host-allowlisted).
- A signed, capability-scoped plugin sandbox.
- Runtime anomaly detection and deception tripwires.
- The run-creation wiring behind fan-out: spawning a child run is gated as a capability today,
  while the code that materializes each child run under the narrowed grant and the parent's
  shared budget is being landed, so the narrowing and budget guarantees above are a contract of
  the delegation interface rather than a property to rely on in a deployed run yet.
- A defense that reduces prompt-injection and jailbreak success beyond what capability
  admission already gives. The Tampering section covers a hostile model file rewriting the
  prompt contract; this is the runtime case where untrusted input the agent reads (a web
  page, a file, a tool result) carries instructions that hijack the model's next action.
  The blast radius is already bounded: a hijacked action is still admitted against the run's
  capability grant, contained at the sandbox boundary, and recorded, and the governance
  benchmark above measures how often an injected instruction changes the agent's action.
  Actively lowering that rate (input-provenance separation, instruction-versus-data
  isolation) is not yet built.
- The promotion flow that lets the agent's own untainted notes into the wake digest. Their
  classification says "eligible once a trusted reviewer promotes it", and until the reviewed
  promotion record exists the digest can only carry the operator's own untainted memories.
  Two residuals go with it: a host that never marks its ingest path leaves taint to what
  provenance alone implies, and a promotion performed by an LLM review is itself attack
  surface, which is why an untrusted-origin or tainted item is denied outright rather than
  made promotable.
- Provenance-tagged learned skills. Durable memory carries provenance, trust classification
  and taint (above); a skill is loaded as procedure the agent follows and is not yet
  classified the same way, so a skill written from untrusted content is read back with the
  trust of a first-party one.
- Confused-deputy hardening for ambient authority. When the agent acts on input from a
  channel it holds standing credentials for (a mailbox, a connected integration), an
  attacker who can place input on that channel can borrow the agent's authority without
  holding it. Capability admission bounds what any single action may do, but the binding
  that would tie a requested action back to the principal actually allowed to ask for it is
  not yet enforced.
- A post-quantum migration path for the tamper-evident record. The sealed run record's
  integrity rests on Ed25519 signatures over an RFC 6962 Merkle log. That is sound against a
  classical adversary; a future cryptographically-relevant quantum computer would break the
  signature, though not the hash-based log structure. Signature agility (a post-quantum or
  hybrid signature over the same checkpoint) is named here as the intended migration rather
  than shipped.
- Supply-chain integrity for what the agent installs and for Flynn's own build. When the
  agent runs a package install (`npm`, `pip`, and the like) it pulls third-party code whose
  provenance Flynn does not yet verify; the egress and sandbox boundaries contain what that
  code can then do but do not vet the artifact itself. Provenance verification for
  agent-installed dependencies, and build-provenance attestation for Flynn's own release
  artifacts, are planned.
- Scrubbing secrets out of tool output before it returns to the model. The Information
  disclosure section covers a credential in a child's environment, in a prompt, and in a
  log; it does not cover a secret that a tool result itself carries back into the model's
  context (a file read, a command's stdout, an API response), from where a later action
  could exfiltrate it. A scan-and-redact pass on the tool-output path, the inbound mirror of
  the outbound egress gate, is planned.

## Reporting

Report a suspected vulnerability privately as described in [SECURITY.md](../SECURITY.md).
Please do not open a public issue or pull request for a security report.
