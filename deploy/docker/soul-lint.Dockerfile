# Dockerfile for the `soul-lint` binary (ADR-004) - offline linter, CLI utility.
# Multi-stage: builder on the full Go toolchain, runtime - distroless static-nonroot.
# Thin image (a single static binary) - exactly what's needed for CLI in CI.
#
# Build context - ROOT of the mono-repo. Build like this:
#   docker build -f deploy/docker/soul-lint.Dockerfile -t soul-stack/soul-lint .
#
# Typical use in CI - mount the repo and lint configs:
#   docker run --rm -v "$PWD:/work" -w /work soul-stack/soul-lint validate-destiny destiny.yml

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

# soul-lint doesn't have a version variable yet - ARG is reserved for future injection.
ARG VERSION=0.0.0-dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags '-s -w' \
        -o /out/soul-lint \
        ./soul-lint/cmd/soul-lint

FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/soul-lint /usr/local/bin/soul-lint

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/soul-lint"]
