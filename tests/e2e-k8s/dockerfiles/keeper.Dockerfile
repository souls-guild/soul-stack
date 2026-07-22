# Keeper image for L3c kind-cluster E2E. Single-stage: reuses the
# `make build-linux` artifact (static keeper-linux-amd64 from `keeper/bin/`),
# COPY the binary into the distroless runtime. PM decision: `gcr.io/distroless/static`
# - minimal attack surface (no shell / no libc).
#
# Build context is the repository root (`docker build -f
# tests/e2e-k8s/dockerfiles/keeper.Dockerfile .`); ENTRYPOINT args and the
# config file come from a ConfigMap, mounted by the K8s Deployment.
#
# The image is DISPOSABLE - built locally, loaded into kind via `kind load
# docker-image`, not published to a registry (see Makefile::docker-build-keeper).

FROM gcr.io/distroless/static:nonroot

# nonroot UID/GID (65532). distroless sets USER=nonroot by default,
# but we duplicate it explicitly - keeper.yml expects to write into
# directories owned by that same UID.
USER nonroot:nonroot

COPY keeper/bin/keeper-linux-amd64 /keeper

ENTRYPOINT ["/keeper"]
CMD ["run", "--config", "/etc/keeper/keeper.yml", "--initialize"]
