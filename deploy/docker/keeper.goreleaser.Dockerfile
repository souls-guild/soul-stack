# GHCR image for `keeper`, built by GoReleaser (NIM-135). Unlike
# keeper.Dockerfile (multi-stage, compiles from source for local/e2e use), this
# one just COPYs the binary GoReleaser already cross-compiled. dockers_v2 stages
# artifacts per platform (<os>/<arch>/keeper), so COPY selects via $TARGETPLATFORM.
# Distroless static-nonroot: binary only, no shell/libc/pkg-manager.
FROM gcr.io/distroless/static:nonroot

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/keeper /usr/local/bin/keeper

# Config is mounted at /etc/keeper/keeper.yml (keeper's default path); the
# operator supplies it via ConfigMap. run WITHOUT --initialize (auto-bootstrap
# in prod is dangerous — Archon init is a separate one-shot).
ENTRYPOINT ["/usr/local/bin/keeper"]
CMD ["run"]
