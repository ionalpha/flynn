# syntax=docker/dockerfile:1
#
# Flynn container image: a single static binary on a minimal, non-root base, so the
# agent can host itself on any container platform with a persistent volume.
#
# The store driver is pure Go (modernc.org/sqlite, no cgo), so the binary builds fully
# static with CGO_ENABLED=0 and runs on a distroless base that has no libc, no shell,
# and no package manager to attack. Build:
#
#   docker build -t flynn:local --build-arg VERSION=v0.1.0 .
#
# Pin both base images by @sha256 digest for production builds; the tags below track a
# minor line so the image stays reproducible within it.

# ---- build stage --------------------------------------------------------------
FROM golang:1.25.11-bookworm AS build
WORKDIR /src

# Download modules first so the layer caches across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is stamped into the binary; default matches the in-tree development value.
ARG VERSION=0.0.0-dev

# A fully static build: CGO disabled (the SQLite driver is pure Go), -trimpath to keep
# absolute build paths out of the binary, and -buildvcs=false so the build does not
# depend on the .git directory being in the context. -s -w drop the symbol table and
# DWARF to shrink the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -buildvcs=false \
      -ldflags="-s -w -X github.com/ionalpha/flynn/internal/version.Version=${VERSION}" \
      -o /out/flynn ./cmd/flynn

# Prepare the data directory with the runtime user's ownership, so a fresh named volume
# mounted over it is writable by the non-root user (Docker seeds an empty volume from
# the image path's ownership).
RUN install -d -o 65532 -g 65532 /data

# ---- runtime stage ------------------------------------------------------------
# distroless static: ~2 MB, no shell or package manager, runs as a non-root user
# (uid 65532), and ships ca-certificates and tzdata so outbound HTTPS to model
# providers and integrations works out of the box.
FROM gcr.io/distroless/static-debian12:nonroot

# APP_USER selects the runtime user. The default `nonroot` (uid 65532) is the hardened
# posture for shared-kernel hosts (Docker, Kubernetes): a Docker named volume inherits
# the image path's ownership, so /data is writable without a chown. Some hosts instead
# mount the data volume root-owned and offer no shell to fix it at boot (Fly.io, where
# every app is its own isolated Firecracker microVM). Build those with
# `--build-arg APP_USER=root` so the process can write the volume; the microVM is the
# isolation boundary there, not the in-VM user.
ARG APP_USER=nonroot

COPY --from=build /out/flynn /usr/local/bin/flynn
COPY --from=build --chown=65532:65532 /data /data

USER ${APP_USER}
WORKDIR /data

# Durable state lives here: the SQLite store, the credential vault's sealed file, and
# accumulated learning. Mount a volume so it survives container replacement.
VOLUME ["/data"]

# The control-plane API binds loopback by default. Flynn refuses to bind a wildcard
# address (every interface) on purpose, so the API is reachable inside the container
# (via `docker exec` or a tunnel) and is never silently exposed. Auth is on by default:
# a bearer token is generated and logged when FLYNN_API_TOKEN is unset. To have the
# agent answer messages, add a channel (TELEGRAM_BOT_TOKEN) and a model key
# (ANTHROPIC_API_KEY); see deploy/README.md for remote access and hardening.
ENTRYPOINT ["flynn"]
CMD ["serve", "--data-dir=/data", "--api-addr=127.0.0.1:7575"]
