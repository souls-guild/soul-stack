# Soul image for L3c kind-cluster E2E (L3c-3). Privileged systemd-PID-1 Debian 12
# (parity with tests/e2e-live/dockerfiles/debian-12.Dockerfile): needs a real
# systemd so we can later test core.service.* via systemctl and core.pkg.*
# via apt-install. Distroless (like keeper's) does NOT work here - a Soul must have
# shell/systemctl/apt inside the host, that's a Soul agent invariant, not attack surface.
#
# Pod level requires privileged: true + volume /sys/fs/cgroup (hostPath) +
# tmpfs /run - see manifests/soul/statefulset.yaml.
#
# Build context is the repo root (`docker build -f tests/e2e-k8s/dockerfiles/
# soul.Dockerfile .`), COPYs from `soul/bin/soul-linux-amd64` (artifact of
# `make build-linux`). The image is ONE-SHOT - `kind load docker-image soul:e2e-k8s`,
# no push to a registry.

FROM debian:12-slim

# systemd + a minimal toolset for core.pkg/core.service tests (curl/iproute2
# are needed by the harness debug-exec). Same set as in e2e-live/debian-12.Dockerfile.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        systemd systemd-sysv \
        ca-certificates \
        curl \
        gnupg \
        procps \
        iproute2 \
        dbus \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /lib/systemd/system/multi-user.target.wants/* \
            /etc/systemd/system/*.wants/* \
            /lib/systemd/system/local-fs.target.wants/* \
            /lib/systemd/system/sockets.target.wants/*udev* \
            /lib/systemd/system/sockets.target.wants/*initctl* \
            /lib/systemd/system/basic.target.wants/* \
            /lib/systemd/system/anaconda.target.wants/*

# Soul directories (docs/architecture.md / docs/soul/): bin for the binary, modules
# for the plugin cache, seed for SoulSeed material. /etc/soul/ - the soul.yml config +
# CA bundle + bootstrap token (volume mount from a k8s Secret).
RUN mkdir -p /var/lib/soul-stack/bin /var/lib/soul-stack/modules /var/lib/soul-stack/seed /etc/soul \
    && chown -R root:root /var/lib/soul-stack /etc/soul

# soul binary COPY to the fixed path /usr/local/bin/soul. In L3b the binary is
# mounted from the host via testcontainers.ContainerFile - k8s has no such
# mechanism, so it's baked into the image instead. Artifact of `make build-linux` (static,
# CGO_ENABLED=0) - no libc deps, runs fine in slim Debian.
COPY soul/bin/soul-linux-amd64 /usr/local/bin/soul
RUN chmod +x /usr/local/bin/soul

# systemd unit for the soul agent. NOT enabled by default - the harness calls
# `systemctl enable+start soul.service` AFTER `soul init` (the CSR bootstrap flow
# must complete and write the SoulSeed to /var/lib/soul-stack/seed before
# `soul run` starts). See tests/e2e-k8s/harness/stack.go::DeploySoul.
COPY tests/e2e-k8s/manifests/soul/soul.service /etc/systemd/system/soul.service

ENTRYPOINT ["/sbin/init"]
