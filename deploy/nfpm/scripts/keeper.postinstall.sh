#!/bin/sh
# postinstall for the soul-stack-keeper package. Prepares the host for a clean install of the
# keeper daemon: soul-stack system user + working directories with the correct
# owner. Idempotent - runs on both first install and upgrade.
#
# The user is shared with soul (User=soul-stack in both units - keeper.service
# and soul.service), so the getent-guard protects against double creation if
# soul-stack-soul is already installed on the host.
#
# IMPORTANT: bootstrap of the first Archon (keeper init --archon) remains MANUAL -
# postinstall does NOT do it.
set -e

if ! getent passwd soul-stack >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
fi

# Keeper working directories owned by soul-stack:
#   /etc/keeper               - config (keeper.yml, keeper.env, tls/), 0750
#   /var/lib/soul-stack-keeper - runtime root (plugins / plugin-src / services /
#                                destiny caches; see keeper main.go defaults), 0700.
# Subdirectories inside the keeper runtime root are created by keeper itself via
# MkdirAll - the parent owned by soul-stack is enough.
install -d -m 0750 -o soul-stack -g soul-stack /etc/keeper
install -d -m 0700 -o soul-stack -g soul-stack /var/lib/soul-stack-keeper

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
