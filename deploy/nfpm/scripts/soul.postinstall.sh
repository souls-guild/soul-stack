#!/bin/sh
# postinstall for the soul-stack-soul package. Prepares the host for a clean install of the soul daemon:
# system user soul-stack + state directories with the correct owner.
# Idempotent - runs on both first install and upgrade.
#
# IMPORTANT: onboarding (soul init --token + CA to disk) remains MANUAL - the token
# is one-time, postinstall does NOT do it. Only the base for running the daemon is here.
set -e

# System user for the daemon. useradd comes from the passwd package (Depends), present
# in Debian and Ubuntu base. --system: no password expiry, UID from the system range.
if ! getent passwd soul-stack >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
fi

# State directories owned by soul-stack. Without this, `soul init`/`soul run` running as
# soul-stack fail on MkdirAll (root-owned parent).
#   /etc/soul                   - config (soul.yml, soul.env), 0750
#   /var/lib/soul-stack         - state root, 0700
#   /var/lib/soul-stack/seed    - SoulSeed cert+key (private material), 0700
#   /var/lib/soul-stack/modules - custom-module cache keyed by SHA-256, 0755
install -d -m 0750 -o soul-stack -g soul-stack /etc/soul
install -d -m 0700 -o soul-stack -g soul-stack /var/lib/soul-stack
install -d -m 0700 -o soul-stack -g soul-stack /var/lib/soul-stack/seed
install -d -m 0755 -o soul-stack -g soul-stack /var/lib/soul-stack/modules

# Pick up the new/updated unit. Only under systemd (container/chroot - no-op).
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
