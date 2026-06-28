#!/bin/sh
# preremove пакета soul-stack-soul. Останавливает и снимает с автозапуска демон
# до удаления бинаря/юнита. `|| true` — remove не должен падать, если systemd
# отсутствует (контейнер) или юнит уже снят.
#
# Только при ПОЛНОМ удалении, не при upgrade: иначе апгрейд пакета отключил бы
# автозапуск юнита. Первый аргумент: deb даёт remove/purge/upgrade/..., rpm —
# число (0 = последний remove, 1 = upgrade). Останавливаем на remove/purge/0.
set -e

case "$1" in
    remove | purge | 0)
        if [ -d /run/systemd/system ]; then
            systemctl --quiet disable --now soul.service || true
        fi
        ;;
esac
