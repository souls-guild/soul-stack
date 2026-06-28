#!/bin/sh
# postinstall пакета soul-stack-keeper. Готовит хост к clean-install демона
# keeper: системный пользователь soul-stack + рабочие каталоги с правильным
# владельцем. Идемпотентен — вызывается и при первой установке, и при upgrade.
#
# Пользователь общий с soul (User=soul-stack в обоих юнитах — keeper.service
# и soul.service), поэтому getent-guard защищает от двойного создания, если на
# хосте уже стоит soul-stack-soul.
#
# ВАЖНО: bootstrap первого Архонта (keeper init --archon) остаётся РУЧНЫМ —
# postinstall его НЕ делает.
set -e

if ! getent passwd soul-stack >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
fi

# Рабочие каталоги keeper с владельцем soul-stack:
#   /etc/keeper               — конфиг (keeper.yml, keeper.env, tls/), 0750
#   /var/lib/soul-stack-keeper — рантайм-корень (plugins / plugin-src / services /
#                                destiny-кеши; см. keeper main.go defaults), 0700.
# Подкаталоги внутри рантайм-корня keeper создаёт сам через MkdirAll — родителя
# с владельцем soul-stack достаточно.
install -d -m 0750 -o soul-stack -g soul-stack /etc/keeper
install -d -m 0700 -o soul-stack -g soul-stack /var/lib/soul-stack-keeper

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
