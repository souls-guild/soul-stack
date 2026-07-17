# Prod image for the `keeper` binary (ADR-004). Multi-stage: builder on the
# full Go toolchain, runtime is distroless static-nonroot (binary only, no
# shell/libc/package manager -> minimal attack surface, "security first").
#
# The image is self-contained: it builds from a SINGLE `docker build` without a
# prior `make build-linux` on the host - the builder stage pins the golang
# toolchain, so the build is reproducible in any environment (CI / a third-party
# registry) and does not depend on the state of `keeper/bin/` or the local Go
# version. The operator pushes the finished image to their own registry and
# rolls it out with their own Helm chart (see deploy/README.md).
#
# The build context is the ROOT of the mono-repo (that's where go.work + all 7
# modules live). Build with a version tag from git (this is what
# `make docker-keeper` does):
#   docker build -f deploy/docker/keeper.Dockerfile \
#       --build-arg VERSION="$(git describe --tags --always --dirty)" \
#       -t soul-stack/keeper:<version> .
#
# The version is passed through via ldflags (--build-arg VERSION, defaults to
# 0.0.0-dev) into main.version. The form `-X main.<var>` is mandatory: the
# entrypoint is package main, and the linker silently ignores `-X` with a full
# import path (the binary would stay at 0.0.0-dev -> `keeper version` would
# report the wrong version).
#
# Bootstrap of the first Archon (ADR-013) is a SEPARATE command, NOT part of
# this image by default: `keeper init --archon=<aid>` (one-shot Job/exec),
# then `keeper run`. The CMD here is only `run`, WITHOUT `--initialize`:
# auto-bootstrap in prod is dangerous.

# The Go version is synced with go.mod / go.work (go 1.26.4). When upgrading
# Go, update here and in go.work at the same time.
FROM golang:1.26.4 AS builder

WORKDIR /src

# Module manifests only, first - the layer stays cached as long as deps
# haven't changed. go.work links the local modules, so we copy all go.mod/go.sum
# files across the tree.
COPY go.work go.work.sum ./
COPY proto/go.mod proto/go.sum ./proto/
COPY proto/plugin/go.mod proto/plugin/go.sum ./proto/plugin/
COPY shared/go.mod shared/go.sum ./shared/
COPY sdk/go.mod sdk/go.sum ./sdk/
COPY keeper/go.mod keeper/go.sum ./keeper/
COPY soul/go.mod soul/go.sum ./soul/
COPY soul-lint/go.mod soul-lint/go.sum ./soul-lint/
RUN go mod download

# The rest of the sources.
COPY . .

# Static build without cgo - required for distroless static (no libc).
# The version is injected by the linker into main.version (cmd/keeper/main.go).
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/keeper \
        ./keeper/cmd/keeper

# Runtime: distroless static with an unprivileged user (uid 65532).
FROM gcr.io/distroless/static:nonroot

# OCI labels - the image is published to the operator's registry;
# org.opencontainers.* is the standard channel for provenance/version info
# for scanners and registry UIs.
ARG VERSION=0.0.0-dev
LABEL org.opencontainers.image.title="soul-stack-keeper" \
      org.opencontainers.image.description="Soul Stack Keeper (ADR-004) — central node" \
      org.opencontainers.image.source="https://github.com/souls-guild/soul-stack" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=builder /out/keeper /usr/local/bin/keeper

# The config is expected to be mounted at /etc/keeper/keeper.yml - the default
# path for the keeper binary (defaultConfigPath in cmd/keeper). That's why the
# CMD does not pass --config: the operator mounts the ConfigMap at
# /etc/keeper/keeper.yml, and keeper resolves the remaining secrets (PG-DSN,
# Redis, Vault, JWT signing key) from Vault via the *_ref fields in the config
# (see deploy/README.md -> "Keeper in production").
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/keeper"]
# Prod default: daemon only, WITHOUT --initialize. An empty operators registry
# without --initialize -> keeper run refuses to start (ADR-013) - this is a
# barrier against silent auto-bootstrap. The first Archon is created via a
# separate command `keeper init --archon=<aid>` (one-shot), see the file
# header and deploy/README.md.
CMD ["run"]
