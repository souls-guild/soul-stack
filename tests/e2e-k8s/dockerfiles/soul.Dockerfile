# Soul-image для L3c kind-cluster E2E (L3c-3). Privileged systemd-PID-1 Debian-12
# (parity с tests/e2e-live/dockerfiles/debian-12.Dockerfile): нужен реальный
# systemd, чтобы потом тестировать core.service.* через systemctl и core.pkg.*
# через apt-install. Distroless (как у keeper) НЕ годится — у Soul-а должны быть
# shell/systemctl/apt внутри хоста, это инвариант Soul-агента, не attack-surface.
#
# Pod-уровень требует privileged: true + volume /sys/fs/cgroup (hostPath) +
# tmpfs /run — см. manifests/soul/statefulset.yaml.
#
# Контекст сборки — корень репо (`docker build -f tests/e2e-k8s/dockerfiles/
# soul.Dockerfile .`), COPY-ит из `soul/bin/soul-linux-amd64` (артефакт
# `make build-linux`). Образ ОДНОРАЗОВЫЙ — `kind load docker-image soul:e2e-k8s`,
# без push в registry.

FROM debian:12-slim

# systemd + минимальный toolset для core.pkg/core.service-тестов (curl/iproute2
# нужны harness-debug-exec-у). Тот же набор, что в e2e-live/debian-12.Dockerfile.
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

# Каталоги Soul-а (docs/architecture.md / docs/soul/): bin для binary, modules
# для plugin-cache, seed для SoulSeed-материала. /etc/soul/ — конфиг soul.yml +
# CA-bundle + bootstrap-token (volume mount из k8s Secret).
RUN mkdir -p /var/lib/soul-stack/bin /var/lib/soul-stack/modules /var/lib/soul-stack/seed /etc/soul \
    && chown -R root:root /var/lib/soul-stack /etc/soul

# soul-binary COPY в фиксированный путь /usr/local/bin/soul. В L3b binary
# mount-ится с хоста через testcontainers.ContainerFile — у k8s такого механизма
# нет, поэтому baking в image. Артефакт `make build-linux` (статический,
# CGO_ENABLED=0) — без libc-deps, в slim-Debian запускается.
COPY soul/bin/soul-linux-amd64 /usr/local/bin/soul
RUN chmod +x /usr/local/bin/soul

# systemd-unit для soul-агента. НЕ enabled by default — harness вызывает
# `systemctl enable+start soul.service` ПОСЛЕ `soul init` (CSR Bootstrap-flow
# должен пройти и записать SoulSeed в /var/lib/soul-stack/seed до запуска
# `soul run`). См. tests/e2e-k8s/harness/stack.go::DeploySoul.
COPY tests/e2e-k8s/manifests/soul/soul.service /etc/systemd/system/soul.service

ENTRYPOINT ["/sbin/init"]
