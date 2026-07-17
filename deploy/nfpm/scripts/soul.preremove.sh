#!/bin/sh
# preremove script for the soul-stack-soul package. Stops the daemon and disables
# its autostart before the binary/unit is removed. `|| true` - remove must not
# fail if systemd is absent (container) or the unit is already disabled.
#
# Only on FULL removal, not on upgrade: otherwise a package upgrade would disable
# the unit's autostart. First argument: deb gives remove/purge/upgrade/..., rpm -
# a number (0 = final remove, 1 = upgrade). We stop on remove/purge/0.
set -e

case "$1" in
    remove | purge | 0)
        if [ -d /run/systemd/system ]; then
            systemctl --quiet disable --now soul.service || true
        fi
        ;;
esac
