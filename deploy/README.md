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
docker build -t flynn:local --build-arg VERSION=v0.1.0 .
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
| `TELEGRAM_BOT_TOKEN` | Telegram channel. Set it to answer Telegram messages; omit for a monitor-only daemon. |
| `FLYNN_API_TOKEN` | Bearer token for the control-plane API. If unset, Flynn generates one and prints it to the logs at startup. |
| `FLYNN_VAULT_PASSPHRASE` | Unseals the sealed credential vault non-interactively, so configured integrations work without a TTY prompt. Needed once you store an integration credential. Keep it stable: losing it makes stored credentials unrecoverable. |

The default command is `serve --data-dir=/data --api-addr=127.0.0.1:7575`. Add
`--signal-tcp` for a Signal channel, or drop `--api-addr` entirely if you do not want
the control-plane API.

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

For production, pin both base images in the `Dockerfile` by `@sha256` digest.

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

## How this fits

This image is the most direct way to have Flynn host itself: it works today on any
Docker host, Fly.io, Render, or Coolify, with no provider-specific automation. The
hosting provider extensions (Cloudflare, Hetzner, and others) layer on top to make
`flynn deploy` provision and supervise infrastructure, and to run this same image on a
box Flynn provisions itself.
