# Base image for the real-soul-container (L3b, ADR-039).
# systemd-PID-1 is needed for core.service.running tests.
#
# Privileged-run is mandatory (see testcontainers ContainerRequest in L3b-2):
#   Privileged:   true
#   Mounts:       [{type: bind, src: /sys/fs/cgroup, dst: /sys/fs/cgroup}]
#   CgroupnsMode: "host"
#
# soul-binary is mounted from the host into /var/lib/soul-stack/bin/ (volume below).
# /etc/soul/ca.pem - bind-mount CA bundle for the mTLS handshake with the keeper.

FROM debian:12-slim

# Base deps: systemd + a minimal toolset for core.pkg/core.service tests.
# openssl - CLI for AssertRedisTLSCertServed (NIM-54): fingerprint of the server
# cert that redis presents over TLS (the redis install pulls in only libssl, not the CLI).
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        systemd systemd-sysv \
        ca-certificates \
        curl \
        gnupg \
        procps \
        iproute2 \
        dbus \
        openssl \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /lib/systemd/system/multi-user.target.wants/* \
            /etc/systemd/system/*.wants/* \
            /lib/systemd/system/local-fs.target.wants/* \
            /lib/systemd/system/sockets.target.wants/*udev* \
            /lib/systemd/system/sockets.target.wants/*initctl* \
            /lib/systemd/system/basic.target.wants/* \
            /lib/systemd/system/anaconda.target.wants/*

# Soul directories (see docs/architecture.md / docs/soul/): bin for the binary,
# modules for the plugin cache, seed for SoulSeed material. /etc/soul/ - config
# soul.yml + CA bundle.
RUN mkdir -p /var/lib/soul-stack/bin /var/lib/soul-stack/modules /var/lib/soul-stack/seed /etc/soul && \
    chown -R root:root /var/lib/soul-stack /etc/soul

# Volume for the soul-binary mount (read-only from host).
VOLUME ["/var/lib/soul-stack/bin"]

# Volume for /etc/soul/ - config and CA bundle.
VOLUME ["/etc/soul"]

# systemd-PID-1. The soul service in L3b-2 will be started via container.Exec
# or a systemd-unit (decision in the L3b-2 slice).
ENTRYPOINT ["/sbin/init"]
