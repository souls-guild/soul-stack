#!/usr/bin/env bash
# Генератор version-aware каталога ВАЛИДНЫХ директив redis.conf (MVP, architect
# 2026-06-26). Результат — COMMITTED-данные в essence (ключ redis_directives), НЕ
# runtime-зависимость: скрипт гоняется автором сервиса вручную при добавлении новой
# поддерживаемой версии Redis, его вывод вклеивается в examples/service/redis/essence/
# _default.yaml. На render-пути сети нет — assert читает уже committed-каталог.
#
# ИСТОЧНИК ИСТИНЫ — src/config.c апстрима Redis (таблица standardConfig через макросы
# createXConfig + специальные директивы, разбираемые в теле loadServerConfigFromString).
# redis.conf-файл как источник ОТВЕРГНУТ: прозаические комментарии (`# Note that ...`)
# неотличимы регуляркой от commented-примеров директив (`# maxmemory <bytes>`) →
# сотни ложных «директив» (English-слова). config.c даёт ИМЕНА директив точно.
#
# Каждая версия → ПЛОСКОЕ множество ИМЁН директив (MVP: только имена, не значения).
# Ключ каталога — серия major.minor (директивы стабильны внутри патч-серии, эволюция
# между минорами). input.version (distro-пин вида 5:7.0.15-1~deb12u7) маппится в ключ
# извлечением X.Y в scenario-assert.
#
# Использование:
#   scripts/gen-redis-catalog.sh                       # серии по умолчанию (ниже)
#   scripts/gen-redis-catalog.sh 7.0.15 8.0.6          # явные теги
# Вывод — YAML-фрагмент redis_directives: на stdout (вклеить в essence/_default.yaml).

set -euo pipefail

# Канонические теги (последний патч серии) поддерживаемых сервисом версий. Сверены
# `git ls-remote --tags https://github.com/redis/redis` (2026-06-28): реальные серии,
# покрываемые version-enum сервиса — 6.2/7.0/7.2/7.4/8.0/8.2/8.4/8.6. Теги ниже — последний
# патч каждой серии на момент сверки (директивы стабильны внутри патч-серии).
DEFAULT_TAGS=(6.2.22 7.0.15 7.2.14 7.4.9 8.0.6 8.2.7 8.4.4 8.6.4)

# Специальные директивы, разбираемые в config.c ВНЕ таблицы standardConfig (в теле
# loadServerConfigFromString через !strcasecmp). Реальные директивы redis.conf, которые
# оператор может передать через redis_settings, но макросом createXConfig не объявлены.
# Сверено по config.c 7.0/8.0/8.2: эти шесть имён стабильны.
SPECIAL_DIRECTIVES=(slaveof include loadmodule rename-command user sentinel)

tags=("$@")
[ ${#tags[@]} -eq 0 ] && tags=("${DEFAULT_TAGS[@]}")

base_url="https://raw.githubusercontent.com/redis/redis"

echo "# СГЕНЕРИРОВАНО scripts/gen-redis-catalog.sh из src/config.c апстрима Redis."
echo "# НЕ редактировать руками — перегенерировать скриптом при добавлении версии."
echo "# Теги-источники: ${tags[*]}"
echo "redis_directives:"

for tag in "${tags[@]}"; do
  series="${tag%.*}"   # 7.0.15 → 7.0
  cfg="$(curl -sf "$base_url/$tag/src/config.c")" || {
    echo "ОШИБКА: не удалось скачать config.c для тега $tag" >&2
    exit 1
  }

  {
    # 1-й аргумент createXConfig — каноническое имя директивы.
    printf '%s\n' "$cfg" | grep -oE 'create[A-Za-z]+Config\("[a-z][a-zA-Z0-9_-]*"' \
      | sed -E 's/.*\("([^"]*)"$/\1/'
    # 2-й аргумент (alias) — необязательный синоним (напр. slaveof для replicaof,
    # *-ziplist-* для *-listpack-*). Берём только непустые (закавыченные).
    printf '%s\n' "$cfg" | grep -oE 'create[A-Za-z]+Config\("[a-z][a-zA-Z0-9_-]*",[[:space:]]*"[a-z][a-zA-Z0-9_-]*"' \
      | sed -E 's/.*,[[:space:]]*"([^"]*)"$/\1/'
    # Специальные директивы вне таблицы.
    printf '%s\n' "${SPECIAL_DIRECTIVES[@]}"
  } | sort -u | {
    echo "  \"$series\":"
    while IFS= read -r name; do
      [ -n "$name" ] && echo "    - $name"
    done
  }
done
