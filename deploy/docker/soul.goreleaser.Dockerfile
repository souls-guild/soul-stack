# GHCR image for `soul`, built by GoReleaser (NIM-135). COPYs the binary
# GoReleaser cross-compiled (build context = GoReleaser's per-image temp dir).
# See soul.Dockerfile for the source-build variant used by local/e2e.
FROM gcr.io/distroless/static:nonroot

COPY soul /usr/local/bin/soul

ENTRYPOINT ["/usr/local/bin/soul"]
CMD ["run"]
