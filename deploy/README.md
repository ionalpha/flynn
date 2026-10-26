# Self-hosting Flynn

Flynn ships as a single static binary, so it hosts itself anywhere a container runs
with a persistent volume: a VPS with Docker, Fly.io, Render, Coolify, or any Kubernetes
cluster. This directory holds a hardened `Dockerfile` and ready-to-edit configs.

## What the image is

A multi-stage build (`Dockerfile` at the repository root) compiles `cmd/flynn` into a
fully static binary (the SQLite store driver is pure Go, so there is no cgo and no libc
dependency) and copies it onto `gcr.io/distroless/static-debian12:nonroot`. The result
is a small image with no shell and no package manager, running as a non-root user
(uid `65532`), carrying only the binary plus CA certificates and time-zone data. There
is no attack surface beyond the binary itself.

```sh
# from the repository root
docker build -t flynn:local --build-arg VERSION=v0.2.0 .
```

## The data volume

All durable state lives under one directory, passed with `--data-dir`: the SQLite
store, the credential vault's sealed file, and accumulated learning. Mount a volume at
`/data` so it survives container replacement.

```sh
docker run -d --name flynn \
  -v flynn-data:/data \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e TELEGRAM_BOT_TOKEN=123:abc \
  -e FLYNN_TELEGRAM_ALLOW=123456789 \
  flynn:local
```

A fresh named volume inherits the non-root ownership baked into the image, so it is
writable without any `chown`. For a bind mount, make the host directory owned by uid
`65532` first.

## Running it

### Docker Compose

`compose.yaml` builds the image, mounts a named volume, loads secrets from `.env`, and
applies the hardening below. Copy the env template, fill it in, and start it:

```sh
cd deploy
cp .env.example .env   # then edit .env
docker compose up -d
docker compose logs -f flynn
```

### Fly.io

`fly.toml` runs Flynn as one always-on machine bound to one persistent volume at
`/data`, which is what 24/7 durable self-hosting needs. The header of `fly.toml` has the
exact create-app, create-volume, set-secrets, and deploy steps; run them from the
repository root.

Two things are specific to Fly. First, it runs the container as root: Fly mounts the
volume root-owned and the distroless image has no shell to adjust ownership at boot, so
`fly.toml` builds the image with `APP_USER=root`. This is safe on Fly because each app
is its own isolated Firecracker microVM, which is the isolation boundary; every other
host here keeps the non-root default. Second, the durable state is a single-writer
SQLite store, so the app must run exactly one machine: do not `fly scale count` above 1
and do not add a second region. Two machines would each get their own volume and split
the store.

### Any other container host

Render, Coolify, Kubernetes, and similar all take the same image: give it a persistent
volume at `/data` and the same environment variables. Chat channels are outbound, so no
inbound port is required for the agent to answer messages.

## Configuration

| Variable | Purpose |
| --- | --- |
| `ANTHROPIC_API_KEY` | Model provider key. Required once a channel is configured so the agent can run goals. |
| `TELEGRAM_BOT_TOKEN` | Telegram channel. Set it to answer Telegram messages; omit for a monitor-only daemon. Requires `FLYNN_TELEGRAM_ALLOW`. |
| `FLYNN_TELEGRAM_ALLOW` | Comma separated Telegram user ids who may start a run. Numeric ids only (message `@userinfobot` to find yours): a username can be renamed or reassigned. Required with `TELEGRAM_BOT_TOKEN`; `flynn serve` refuses to start without it. |
| `FLYNN_SIGNAL_ALLOW` | Comma separated Signal numbers (`+15551234567`) who may start a run. Required with `--signal-tcp`. |
| `FLYNN_API_TOKEN` | Bearer token for the control-plane API. If unset, Flynn generates one and prints it to the logs at startup. |
| `FLYNN_VAULT_PASSPHRASE` | Unseals the sealed credential vault non-interactively, so configured integrations work without a TTY prompt. Needed once you store an integration credential. Keep it stable: losing it makes stored credentials unrecoverable. |
| `FLYNN_VAULT_FILE` | Set to `1` to store credentials only in the passphrase-sealed file and never touch the OS keychain. Recommended in a container or on a headless host (no keychain service), and for anyone who does not want a key in the OS keychain. Pair it with `FLYNN_VAULT_PASSPHRASE`. |

The default command is `serve --data-dir=/data --api-addr=127.0.0.1:7575`. Add
`--signal-tcp` (with `--signal-allow` or `FLYNN_SIGNAL_ALLOW`) for a Signal channel, or
drop `--api-addr` entirely if you do not want the control-plane API.

A chat message becomes a goal that runs with the agent's tools, so a channel answers only
the senders you list and refuses to start with none. A message from anyone else is dropped
before it is stored, and the sender's id (not the message) is recorded on the
`serve.intake` stream. Each served goal runs under the same flags as a terminal run
(`--require-approval`, `--allow`, `--max-tokens`, `--max-cost`); nothing in a chat can
answer an approval prompt, so an action you list for approval is refused. With no budget
flag, each goal is capped at 1,000,000 tokens.

## Security model

Flynn is secured by default; the container configuration keeps it that way.

- **The control-plane API never binds a wildcard.** Flynn refuses to listen on
  `0.0.0.0` or `::` (every interface) outright, and refuses any non-loopback address
  unless you pass `--api-expose`. The default binds `127.0.0.1`, so the API is reachable
  inside the container's network namespace and is never accidentally published. This is
  why the compose and Fly configs open no inbound port.
- **API auth is always on.** A supplied `FLYNN_API_TOKEN` authenticates the operator;
  when none is supplied, Flynn generates one and logs it once rather than serving
  openly. There is no unauthenticated mode.
- **Secrets stay in the environment and the vault.** Pass provider keys and tokens as
  environment variables (Docker secrets, Fly secrets, or your platform's secret store),
  never baked into the image. Integration credentials are sealed in the vault under
  `/data`, separate from the resources that reference them.
- **The runtime is minimal and unprivileged.** Non-root user, no shell, read-only root
  filesystem (state is on the volume), no privilege escalation, and all Linux
  capabilities dropped (see `compose.yaml`).

Both base images in the `Dockerfile` are pinned by `@sha256` digest, so a build is
reproducible and the base cannot change under its tag. Update the digest together with
the tag when moving to a newer base.

## Reaching the control plane remotely

Because the API binds loopback, reach it deliberately rather than by opening a port:

- **Inspect state directly.** `flynn get` reads the local store, so the simplest check
  needs no API at all: `docker compose exec flynn flynn get goals` (or `fly ssh console
  -C "flynn get goals"`).
- **Forward the loopback API.** On Fly, `fly proxy 7575:7575` forwards the in-container
  loopback bind to your machine; then call `http://127.0.0.1:7575` with
  `Authorization: Bearer <token>`.
- **Run a reverse-proxy sidecar.** For a long-lived remote endpoint, run a TLS-
  terminating proxy in the same network namespace (`network_mode: "service:flynn"`) that
  forwards to `127.0.0.1:7575`. Flynn keeps its loopback bind; the proxy owns TLS and the
  public interface.
- **Use a private overlay.** A WireGuard or Tailscale network, or your platform's
  private networking, lets a remote client reach the container without any public bind.

## Running under systemd

`systemd/flynn-runner.service` runs `flynn serve` on a Linux host with the sandbox
working. It is a hardened service unit: no capabilities, `NoNewPrivileges=true`, a
read-only system, a private `/tmp`, and a syscall filter. A hardened unit that sets
`RestrictNamespaces=true` or filters `mount` stops the sandbox from creating the
namespaces it confines each command in, and the agent then refuses to run commands. The
runner unit adds exactly two things to that baseline, each commented in the file with the
reason:

- `RestrictNamespaces=user net mnt`, the three namespace types the sandbox creates.
- `SystemCallFilter=mount` after the `@privileged` exclusion, and
  `SystemCallErrorNumber=EPERM`, so the launcher can make the mounts private inside its
  own namespace and a refused call fails with a readable error instead of killing the
  process.

It grants no capability and sets no ambient capability. `deploy/runner_unit_test.go`
diffs the unit against `testdata/hardened-baseline.service` and fails if anything other
than those namespace and syscall lines differs.

### Checking a host

```sh
flynn sandbox check          # or --json for a machine-readable result
```

The command starts a real confined no-op child, so the result reflects what the kernel,
the unit and any security module do to the sandbox on this host, not what their settings
say. Run it as the service user, ideally from the unit itself (`systemd-run` with the
unit's properties, or `ExecStartPre=`). It exits non-zero on anything but a clean pass,
and an outcome it cannot classify is a refusal, never a pass.

| Cause | What it means | Fix |
|---|---|---|
| `namespace-restricted` | the unit's `RestrictNamespaces=` excludes a type the sandbox needs | set `RestrictNamespaces=user net mnt` and restart |
| `user-namespaces-disabled` | `kernel.unprivileged_userns_clone=0` or `user.max_user_namespaces=0` | raise the limit, or use the container or microVM tier |
| `apparmor-userns-restricted` | Ubuntu 24.04 and later restrict user namespaces per program | install `apparmor/flynn` (below) |
| `seccomp-conflict` | a syscall filter blocks a call the launcher needs | allow `mount` after the `@privileged` exclusion and set `SystemCallErrorNumber=EPERM` |
| `unsupported-platform` | the host is not Linux, where the check has nothing to start | use the platform's own confinement tier |
| `unknown` | the failure matched no known cause | read the detail line; use the container or microVM tier |

When the host refuses, the agent refuses to launch commands and says why. It does not run
them unconfined, and no flag, environment variable or configuration turns the check off.
The alternative it points to is the container tier or the microVM tier, which do not
need unprivileged namespaces from the host process.

`RestrictNamespaces=` is enforced only on x86, x86-64, ppc64, s390 and some MIPS
kernels. On other architectures (arm64 included) the unit's line has no effect, which is
why the check runs the real child rather than reading the unit, and why it reports the
architecture and a warning.

### Ubuntu 24.04 and later

These releases restrict unprivileged user namespaces with AppArmor, which blocks the
sandbox inside a hardened unit. Install the per-binary profile rather than setting
`kernel.apparmor_restrict_unprivileged_userns=0`, which lifts the restriction for every
program on the host:

```sh
install -m 0644 deploy/apparmor/flynn /etc/apparmor.d/flynn
apparmor_parser -r /etc/apparmor.d/flynn
```

The profile names `/usr/local/bin/flynn`, grants the `userns` permission and runs
unconfined otherwise, so it changes nothing else about what the binary can do. Edit the
path if the binary lives elsewhere. AppArmor's restriction has known bypasses through
other binaries that hold the permission, so treat it as a narrowing and not a boundary.

### Mounts with locked flags

Under `NoNewPrivileges=true` systemd mounts `/` and the private `/tmp` with `nosuid`.
The kernel locks such flags once a mount is copied into a user namespace, and a remount
that leaves one out fails with `EPERM`. The launcher therefore carries a mount's existing
`nosuid`, `nodev`, `noexec` and access-time flags through the read-only and writable
remounts it performs. Keeping a flag only removes capability, so it widens nothing.

### Best effort is an opt-in

A Local built with `WithDefaultConfinement` fails closed. A host that cannot confine
gets a refusal that carries the classified cause, never a command run unconfined. An
embedder that accepts running at the process-jail floor says so in code with
`WithBestEffortConfinement(notice)`. Each launch that falls back is counted
(`ConfinementFallbacks`) and passed to the notice, which defaults to standard error, so
the operator sees it. Nothing in the environment, a flag or a file selects it, and it is
never offered for a Local that denies the network, governs egress, hides paths with
`WithUnreadablePaths` or holds a read grant the floor cannot honour.

### Recording readiness

`flynn sandbox check --ready-file PATH` writes the result as JSON, with the CPU
architecture and the tier the probe measured, for a supervisor or a load balancer to
read. It refuses a path the sandbox can write (the work directory, a granted directory,
`/tmp`, `/var/tmp`, `/dev/shm`) with an error, so a confined command cannot forge a pass. Point it
into the unit's `RuntimeDirectory=` (`/run/flynn`, mode 0700), which no sandbox view
leaves writable. A failed check leaves the runner not ready; a later launch failure
clears any cached pass and runs the check again.

### Hiding the runner's own files

`flynn run`, `flynn serve` and the other commands that launch the agent's shell hide, by
default, the Flynn data directory (the run database, the instance signing key, the stored
provider credentials), the runner's runtime directory (`/run/flynn` and `RUNTIME_DIRECTORY`),
and the credential locations of the account the service runs as: `~/.ssh`, `~/.gnupg`,
`~/.aws`, `~/.azure`, `~/.config/gcloud`, `~/.config/gh`, `~/.docker`, `~/.kube`, `~/.netrc`,
`~/.npmrc`, `~/.pypirc`, `~/.git-credentials`, the Claude and Codex login tokens, the
keyring and password-store directories, the paths named by `SSH_AUTH_SOCK`, `GNUPGHOME`,
`KUBECONFIG` and the cloud SDK variables, and the session bus, keyring and agent sockets in
the user's runtime directory. Inside the sandbox a hidden directory is empty and a hidden
file reads as empty, while the rest of the host stays readable. The mechanism is
`WithUnreadablePaths`: it needs `WithReadOnlyFS` and Linux, and a Local that cannot apply it
refuses with `ErrReadDeny` and does not run the command. A path reachable under a second
name (another bind mount of the same directory) is not hidden under that name.

The same default gives a command no network: it runs in a network namespace with no
interfaces, so it cannot send what it reads anywhere, and the loopback services of the host
(the control-plane API among them) are out of reach too. Each run records its posture as a
`sandbox.posture` event on its own stream: the tier, whether the network was `denied` or
`open`, whether the credentials were `hidden` or `readable`, and the hidden paths.

Two flags relax the default, and the event records each:

- `--allow-network` gives the commands the host's network. Git over https or ssh, package
  installs and anything else a command fetches need it. It is all or nothing: the commands
  can then reach every address the host can.
- `--allow-credential-reads` leaves the data directory and the credential locations
  readable. It is also the only way to run a shell on a platform that cannot hide a path
  (macOS and Windows today), where the default makes every launch refuse with `ErrReadDeny`.
  Git over ssh with a key in `~/.ssh` needs it as well as `--allow-network`, and the commands
  then see every credential in that list, so prefer a deploy key kept outside it.

### What the sandbox does not hide

A command's user namespace maps to the service's own uid, so to the filesystem it is the
same user as the runner, and it shares the runner's process namespace. Several consequences
follow, none of which the unit's directory modes change:

- Files the service account owns that are outside the default hide set (anything but the
  data directory, `/run/flynn` and the credential locations above) are readable to a
  command, and so is everything under `--allow-credential-reads`.
- Another process's command line is readable through `/proc/<pid>/cmdline`, which has no
  ptrace check, so a token passed as an argument to a process of the same account is
  exposed. Only a PID namespace with its own procfs would hide it, and the sandbox has none.
  Pass secrets through the environment or a file, not argv.
- A command at the process-jail floor (the `WithBestEffortConfinement` opt-in) can read
  `/proc/<runner pid>/environ`, which holds the `EnvironmentFile=` secrets.
- A kernel-confined command cannot read that file: it sits in a child user namespace
  without CAP_SYS_PTRACE over the runner. `TestConfinedCommandCannotReadRunnerEnviron`
  checks this against a real kernel, with the floor read as its control.

Marking the runner non-dumpable (`prctl(PR_SET_DUMPABLE, 0)`) would close the floor case
and is not done: the kernel then makes `/proc/<child pid>/uid_map` root-owned, the runner
can no longer map the namespace of a command it confines, and every confined launch
fails the readiness check. Keep secrets out of a runner that runs the floor tier, or do
not run that tier.

### What the tests prove

`deploy/negative_control_linux_test.go` runs transient units with `systemd-run`: the
shipped directives must pass the check, and `RestrictNamespaces=yes` or a tightened
`SystemCallFilter` must fail it with a named cause. Set `FLYNN_BIN` to a built `flynn`
to run them against the real binary. They skip without systemd, root or an x86 kernel,
because `RestrictNamespaces=` is inert elsewhere.

## How this fits

This image is the most direct way to have Flynn host itself: it works today on any
Docker host, Fly.io, Render, or Coolify, with no provider-specific automation. The
hosting provider extensions (Cloudflare, Hetzner, and others) layer on top to make
`flynn deploy` provision and supervise infrastructure, and to run this same image on a
box Flynn provisions itself.
