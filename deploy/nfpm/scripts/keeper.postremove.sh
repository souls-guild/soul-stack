#!/bin/sh
# postremove for the soul-stack-keeper package. After removing the unit, reload systemd.
# The soul-stack user (may be shared with soul) and working directories
# (/var/lib/soul-stack-keeper, /etc/keeper) are INTENTIONALLY left alone - purge is
# done by the operator by hand.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
