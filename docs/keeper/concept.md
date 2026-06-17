# Keeper — концепция

Keeper — центральный сервер Soul Stack (аналог `salt-master`). Управляющий узел, к которому подключаются Souls и через который оператор управляет парком.

## Роль

- **Реестр Souls.** Хранит и обслуживает таблицы `souls`, `soul_seeds`, `bootstrap_tokens` в Postgres ([storage.md](storage.md)). Принимает CSR при онбординге, выпускает SoulSeed-сертификаты через Vault PKI, отслеживает heartbeat и статусы.
- **Destiny-каталог.** Тянет git-репозитории Destiny / Service / Module по `ref:`-ам из конфига ([config.md](config.md)), валидирует и рендерит шаги перед раздачей. См. [architecture.md → Артефакты Soul Stack](../architecture.md#артефакты-soul-stack-что-в-git-что-в-бд).
- **Раздача прогонов.** В pull-режиме шлёт команды Souls по живому gRPC bidi-стриму поверх mTLS ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). В push-режиме сам ходит по SSH через модуль `keeper.push` ([push.md](push.md)).
- **Агрегация Soulprint.** Соулс шлют принты (факты о хосте) — Keeper складывает в Postgres, отдаёт через API с RBAC-фильтром.
- **Интеграция с Vault.** Полный клиент: Essence-секреты, PKI для выпуска SoulSeed, SSH-CA для `keeper.push`, credentials cloud-driver-ов ([config.md](config.md) → блок `vault:`).

## HA-кластер, stateless

Keeper — **горизонтально масштабируемый stateless-кластер** ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)). Несколько инстансов `keeper` с разными KID стоят за общими Postgres и Redis. Любой инстанс может обслужить любой запрос — источник правды лежит снаружи бинаря:

- **Postgres** — холодное хранилище: реестры, Destiny-каталог, incarnation, state_history, журналы ([ADR-005](../adr/0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)).
- **Redis** — горячий слой и координация: heartbeat-кэш Souls, lease на SID, pub/sub между Keeper-инстансами, лидерский lease для Reaper ([ADR-006](../adr/0006-cache-redis.md#adr-006-кэш-и-координация--redis)).

Подробности раскладки данных — в [storage.md](storage.md). Соул-сторонний алгоритм выбора Keeper-а из списка endpoint-ов — в [../soul/connection.md](../soul/connection.md).

## KID

Каждый инстанс Keeper-кластера имеет **KID** (Keeper ID) — стабильный человекочитаемый идентификатор. Используется для:

- **Lease на SID** в Redis: `SET sid:lock <kid> NX EX <ttl>` — какой Keeper сейчас держит активный gRPC-стрим к данному Soul.
- **Колонка `last_seen_by_kid`** в `souls` — какой Keeper последним видел этого Soul.
- **Аудит-логи и метрики** — для разделения событий по инстансам.

KID задаётся в конфиге одной строкой (см. [config.md](config.md) → `kid:`).

## Интерфейсы оператора

Первичный путь — **OpenAPI и MCP** ([ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper)). Всё, что делает оператор (заведение Souls, выписка bootstrap-токенов, создание incarnation, push-прогон, управление Provider/Profile, чтение Soulprint), — через OpenAPI или MCP-tool. RBAC применяется к обоим единообразно ([rbac.md](rbac.md)).

CLI допустим как **тонкая обёртка** поверх API, не как первичный путь. Будет ли он отдельным бинарём, подкомандой `keeper` в клиентском режиме или только сторонними тулзами — [open Q №2](../architecture.md#открытые-вопросы).

Внутренний транспорт Keeper ↔ Souls — gRPC bidi на отдельном listener-е, наружу через mTLS.

## Что входит в `keeper`-бинарь

По [ADR-004](../adr/0004-binaries.md#adr-004-раскладка-бинарей--keeper-soul-soul-lint-push-режим--модуль-внутри-keeper) Soul Stack поставляет три отдельных бинаря (`keeper`, `soul`, `soul-lint`). В составе `keeper`-а:

- **gRPC-сервер для Souls** — приём bidi-стримов, mTLS, выпуск SoulSeed.
- **OpenAPI + MCP фасад** — gRPC-Gateway/connect-go поверх того же ядра, MCP-сервер.
- **Модуль `keeper.push`** — SSH-доставка Destiny на хосты без Soul-агента. Не отдельный бинарь, см. [push.md](push.md).
- **Модуль `keeper.cloud`** — cloud-операции (Provider/Profile, CloudDriver-плагины), см. [cloud.md](cloud.md).
- **Reaper / Жнец** — фоновая чистка БД, лидер через Redis-lease, см. [reaper.md](reaper.md).
- **Plugin-host для CloudDriver и SshProvider** — sub-process по gRPC-stdio, см. [plugins.md](plugins.md).
- **Интеграции из коробки** — Vault, OTel, Prometheus-метрики, RBAC, ротация логов, hot-reload конфига ([requirements.md](../requirements.md)).

В `keeper`-бинарь **не входит**: серверный код агентов, реализация core-модулей Destiny (это `soul`), офлайн-линтер (это `soul-lint`).

## См. также

- [storage.md](storage.md) — где живут реестры и кэш.
- [push.md](push.md) — модуль `keeper.push`.
- [reaper.md](reaper.md) — фоновая чистка БД.
- [plugins.md](plugins.md) — CloudDriver и SshProvider.
- [cloud.md](cloud.md) — `keeper.cloud`.
- [rbac.md](rbac.md) — RBAC.
- [config.md](config.md) — формат `keeper.yml`.
- [`../soul/`](../soul/README.md) — `soul`-бинарь (соседний компонент).
- [architecture.md → Роли бинарей](../architecture.md#роли-бинарей) — короткая сводка по всем трём бинарям.
- [architecture.md → Топология](../architecture.md#топология) — место Keeper-а в общей картине.
