#!/bin/sh
# preremove пакета soul-stack-keeper. Останавливает и снимает с автозапуска
# демон до удаления бинаря/юнита. `|| true` — remove не падает без systemd.
#
# Только при ПОЛНОМ удалении, не при upgrade (см. soul.preremove.sh). Первый
# аргумент: deb даёт remove/purge/..., rpm — число (0 = remove, 1 = upgrade).
set -e

case "$1" in
    remove | purge | 0)
        if [ -d /run/systemd/system ]; then
            systemctl --quiet disable --now keeper.service || true
        fi
        ;;
esac
