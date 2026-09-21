# GHCR image for `soul`, built by GoReleaser (NIM-135). COPYs the binary
# GoReleaser cross-compiled. dockers_v2 stages artifacts per platform
# (<os>/<arch>/soul), so COPY selects via $TARGETPLATFORM.
# See soul.Dockerfile for the source-build variant used by local/e2e.
FROM gcr.io/distroless/static:nonroot

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/soul /usr/local/bin/soul

ENTRYPOINT ["/usr/local/bin/soul"]
CMD ["run"]
