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
- **Tampering with the run record.** The mission event spine is append-only and ordered,
  so the record of what happened cannot be silently rewritten.

### Repudiation (denying an action took place)

- **An unattributable action.** Every action is recorded on the event spine through the
  one dispatch boundary, with its trust level, so each privileged action is attributable
  and the run is auditable and replayable.

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

### Denial of service (exhausting a resource)

- **A runaway loop or cost blowup.** A per-command wall-clock cap is available at the
  sandbox boundary. Full CPU, memory, and process-count limits with a runaway and cost
  circuit breaker are planned but not yet enforced (see Coverage); until then this class
  is only partially mitigated, and that is stated rather than implied.

### Elevation of privilege (gaining capability not granted)

- **A tool the run was not granted.** Each action is admitted against the run's capability
  grant by name at the dispatch boundary; an action the grant does not permit is refused
  before any side effect, and with no grant bound the agent is unconstrained only in the
  standalone default.
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

## Coverage: enforced today vs planned

Enforced and tested today:

- One dispatch boundary with capability admission and a containment gate; a lint rule
  forbids a bypass through `os/exec`.
- Secure-by-default execution at the kernel-confined tier where the platform provides it
  (read-only host, syscall filter), with per-platform adapters on Linux, macOS, and
  Windows, and a refusal rather than a silent downgrade where it cannot be enforced.
- A red-team containment matrix that proves, per platform on CI, that each tier denies the
  filesystem-write, network, and syscall escapes it claims to.
- Default-deny outbound egress for the agent's own requests, with anti-SSRF and
  metadata-endpoint blocking, plus lint rules against raw dials and unguarded HTTP clients.
- The full model-source trust pipeline: classification, code-executing-format refusal,
  digest verification with pin-on-first-use, runtime version-floor gating, hardened
  file parsing, a forced trusted chat template, loopback-only serving, and explicit
  consent for a risky run with a safe default and a non-interactive refusal.
- Credential isolation: vault-held, redacted, never in a child's environment.
- An append-only, ordered event spine as the record of what happened.

Planned, not yet enforced (a control in this list is not something to rely on today):

- The container, user-space-kernel, and hardware-isolation (microVM) tiers. Until the
  hardware-isolation tier exists, untrusted work (an arbitrary downloaded model, an
  unsigned plugin) is refused rather than run, which is the safe failure, not a silent
  downgrade.
- CPU, memory, and process-count limits with a runaway and cost circuit breaker.
- A per-run outbound allowlist for sandboxed child processes (today the child network is
  either open or denied as a whole, not host-allowlisted).
- A signed, capability-scoped plugin sandbox.
- Runtime anomaly detection and deception tripwires.

## Reporting

Report a suspected vulnerability privately as described in [SECURITY.md](../SECURITY.md).
Please do not open a public issue or pull request for a security report.
