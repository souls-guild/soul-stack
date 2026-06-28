#!/bin/sh
# postremove пакета soul-stack-soul. После снятия юнита перечитываем systemd,
# чтобы он забыл удалённый soul.service. Пользователь soul-stack и стейт-каталоги
# (/var/lib/soul-stack с SoulSeed) НАМЕРЕННО не трогаем — purge данных оператор
# делает руками.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
