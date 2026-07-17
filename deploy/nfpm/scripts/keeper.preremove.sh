#!/bin/sh
# preremove for the soul-stack-keeper package. Stops and disables the
# daemon before removing the binary/unit. `|| true` -- remove doesn't fail without systemd.
#
# Only on FULL removal, not on upgrade (see soul.preremove.sh). First
# argument: deb gives remove/purge/..., rpm -- a number (0 = remove, 1 = upgrade).
set -e

case "$1" in
    remove | purge | 0)
        if [ -d /run/systemd/system ]; then
            systemctl --quiet disable --now keeper.service || true
        fi
        ;;
esac
