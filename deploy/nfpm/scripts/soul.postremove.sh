#!/bin/sh
# postremove for the soul-stack-soul package. After the unit is removed, reload
# systemd so it forgets the deleted soul.service. The soul-stack user and state
# directories (/var/lib/soul-stack with SoulSeed) are INTENTIONALLY left alone --
# purging data is a manual operator action.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
