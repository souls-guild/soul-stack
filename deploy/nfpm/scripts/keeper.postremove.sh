#!/bin/sh
# postremove пакета soul-stack-keeper. После снятия юнита перечитываем systemd.
# Пользователь soul-stack (может быть общим с soul) и рабочие каталоги
# (/var/lib/soul-stack-keeper, /etc/keeper) НАМЕРЕННО не трогаем — purge оператор
# делает руками.
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
