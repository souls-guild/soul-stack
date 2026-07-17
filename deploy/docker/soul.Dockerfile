# Dockerfile for the `soul` binary (ADR-004) - daemon agent. Multi-stage: builder on
# the full Go toolchain, runtime - distroless static-nonroot (binary only).
#
# Build context - the ROOT of the mono-repo. Build like this:
#   docker build -f deploy/docker/soul.Dockerfile -t soul-stack/soul .
#
# Version is injected via ldflags into main.soulVersion (see Makefile). The form `-X
# main.<var>` is mandatory: entrypoint is package main, and the linker silently ignores
# `-X` with a full import path (the binary would stay 0.0.0-dev -> wrong version in
# Hello/BootstrapRequest -> corrupted audit).

FROM golang:1.26.4 AS builder

WORKDIR /src

COPY go.work go.work.sum ./
COPY proto/go.mod proto/go.sum ./proto/
COPY proto/plugin/go.mod proto/plugin/go.sum ./proto/plugin/
COPY shared/go.mod shared/go.sum ./shared/
COPY sdk/go.mod sdk/go.sum ./sdk/
COPY keeper/go.mod keeper/go.sum ./keeper/
COPY soul/go.mod soul/go.sum ./soul/
COPY soul-lint/go.mod soul-lint/go.sum ./soul-lint/
RUN go mod download

COPY . .

# soulVersion is printed in Hello/BootstrapRequest for audit - injecting the version.
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.soulVersion=${VERSION}" \
        -o /out/soul \
        ./soul/cmd/soul

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/soul /usr/local/bin/soul

# Config is expected at /etc/soul/soul.yml (default path for the soul binary).
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/soul"]
CMD ["run"]
