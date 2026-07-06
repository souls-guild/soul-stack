# Base-образ для real-soul-container (L3b, ADR-039).
# systemd-PID-1 нужен для тестов core.service.running.
#
# Privileged-run обязателен (см. testcontainers ContainerRequest в L3b-2):
#   Privileged:   true
#   Mounts:       [{type: bind, src: /sys/fs/cgroup, dst: /sys/fs/cgroup}]
#   CgroupnsMode: "host"
#
# soul-binary mount-ится с хоста в /var/lib/soul-stack/bin/ (volume ниже).
# /etc/soul/ca.pem — bind-mount CA-bundle для mTLS-handshake с keeper-ом.

FROM debian:12-slim

# Base deps: systemd + минимальный toolset для core.pkg/core.service-тестов.
# openssl — CLI для AssertRedisTLSCertServed (NIM-54): fingerprint серверного
# cert, который redis отдаёт по TLS (redis install тянет только libssl, не CLI).
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

# Каталоги Soul (см. docs/architecture.md / docs/soul/): bin для binary,
# modules для plugin-кеша, seed для SoulSeed-материала. /etc/soul/ — конфиг
# soul.yml + CA-bundle.
RUN mkdir -p /var/lib/soul-stack/bin /var/lib/soul-stack/modules /var/lib/soul-stack/seed /etc/soul && \
    chown -R root:root /var/lib/soul-stack /etc/soul

# Volume для soul-binary mount (read-only из host).
VOLUME ["/var/lib/soul-stack/bin"]

# Volume для /etc/soul/ — конфиг и CA-bundle.
VOLUME ["/etc/soul"]

# systemd-PID-1. soul-сервис в L3b-2 будет запускаться через container.Exec
# или systemd-unit (решение в L3b-2-slice).
ENTRYPOINT ["/sbin/init"]
