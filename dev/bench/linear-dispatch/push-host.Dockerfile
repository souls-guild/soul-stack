# Push host of the linear-dispatch benchmark: the dev soul image plus sshd. No soul
# daemon runs here — Keeper reaches it over SSH (`transport: ssh`), delivers the soul
# binary and execs `soul apply` per message. bench.sh copies the host key, its
# certificate (signed with no principals, so the cert holds for whatever address
# Keeper dials) and the client's authorized key in at start.
ARG BASE=soul-stack-dev-soul:debian12
FROM ${BASE}
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        openssh-server \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /etc/ssh/ssh_host_* \
    && mkdir -p /root/.ssh && chmod 700 /root/.ssh \
    && printf '%s\n' \
        'HostKey /etc/ssh/bench_host_key' \
        'HostCertificate /etc/ssh/bench_host_key-cert.pub' \
        'PermitRootLogin prohibit-password' \
        'PasswordAuthentication no' \
        'LogLevel VERBOSE' \
        > /etc/ssh/sshd_config.d/bench.conf \
    && systemctl enable ssh
