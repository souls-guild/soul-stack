# GHCR image for `keeper`, built by GoReleaser (NIM-135). Unlike
# keeper.Dockerfile (multi-stage, compiles from source for local/e2e use), this
# one just COPYs the binary GoReleaser already cross-compiled — the build
# context is GoReleaser's per-image temp dir that holds the `keeper` binary.
# Distroless static-nonroot: binary only, no shell/libc/pkg-manager.
FROM gcr.io/distroless/static:nonroot

COPY keeper /usr/local/bin/keeper

# Config is mounted at /etc/keeper/keeper.yml (keeper's default path); the
# operator supplies it via ConfigMap. run WITHOUT --initialize (auto-bootstrap
# in prod is dangerous — Archon init is a separate one-shot).
ENTRYPOINT ["/usr/local/bin/keeper"]
CMD ["run"]
