#!/bin/sh
# postinstall пакета soul-stack-soul. Готовит хост к clean-install демона soul:
# системный пользователь soul-stack + стейт-каталоги с правильным владельцем.
# Идемпотентен — вызывается и при первой установке, и при upgrade.
#
# ВАЖНО: онбординг (soul init --token + CA на диск) остаётся РУЧНЫМ — токен
# одноразовый, postinstall его НЕ делает. Здесь только база для запуска демона.
set -e

# Системный пользователь демона. useradd — из пакета passwd (Depends), есть
# в Debian и Ubuntu base. --system: без срока действия пароля, UID из system-range.
if ! getent passwd soul-stack >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
fi

# Стейт-каталоги с владельцем soul-stack. Без этого `soul init`/`soul run` под
# soul-stack падают на MkdirAll (root-owned родитель).
#   /etc/soul                   — конфиг (soul.yml, soul.env), 0750
#   /var/lib/soul-stack         — корень стейта, 0700
#   /var/lib/soul-stack/seed    — SoulSeed cert+key (приватный материал), 0700
#   /var/lib/soul-stack/modules — кеш custom-модулей по SHA-256, 0755
install -d -m 0750 -o soul-stack -g soul-stack /etc/soul
install -d -m 0700 -o soul-stack -g soul-stack /var/lib/soul-stack
install -d -m 0700 -o soul-stack -g soul-stack /var/lib/soul-stack/seed
install -d -m 0755 -o soul-stack -g soul-stack /var/lib/soul-stack/modules

# Подхватываем новый/обновлённый юнит. Только под systemd (контейнер/chroot — no-op).
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
