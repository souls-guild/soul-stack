# Soul — концепция

`soul`-бинарь — демон-агент на управляемом хосте. Получает от Keeper-а команды «применить такую-то Destiny с такой-то Essence», исполняет шаги модулей, отдаёт события и Soulprint обратно. В SaltStack-словаре это minion; в Soul Stack-словаре — Soul, единичная душа в реестре `souls`.

## Где Soul в общей картине

```
                        Keeper-кластер
                              │
              ┌───────────────┴───────────────┐
              │                               │
   gRPC bidi (mTLS)                       SSH-сессия
              │                               │
              ▼                               ▼
        ┌──────────┐                    ┌──────────┐
        │   soul   │                    │   soul   │
        │ (демон,  │                    │ (oneshot,│
        │  pull)   │                    │   push)  │
        └──────────┘                    └──────────┘
        transport: agent                transport: ssh
        запись в souls                  запись в souls
        + soul_seeds                    без soul_seeds
```

Слева — Keeper (центральный сервер, см. [architecture.md → Топология](../architecture.md#топология)). Справа от Keeper-а — реестр Destiny / Service / Incarnation в Postgres. Soul стоит «под Destiny»: получает атомарный кирпичик, применяет его на одном хосте через [модули](../architecture.md#модель-модулей).

Подробное определение бинарей — [architecture.md → Роли бинарей](../architecture.md#роли-бинарей).

## Два транспорта

Soul доступен через два режима доставки команд от Keeper-а — но это **один и тот же бинарь**, отличаются только условия запуска и идентичность.

| | `transport: agent` (pull) | `transport: ssh` (push) |
|---|---|---|
| Запуск | systemd-демон, держит стрим постоянно | one-shot `soul apply`, поднимается на каждый прогон через SSH-сессию |
| Идентичность | SoulSeed (mTLS-пара), запись в `soul_seeds` | SSH-credentials от провайдера (Vault SSH CA / static / Teleport); `soul_seeds` для push-хоста **не используется** |
| Инициатор связи | Soul → Keeper | Keeper → Soul (SSH) |
| Передача плана прогона | `ApplyRequest` по живому gRPC-стриму, ответ — `TaskEvent`/`RunResult` обратно в стрим | `ApplyRequest` (protojson) в stdin `soul apply`, ответ — NDJSON `TaskEvent` + `RunResult` в stdout |
| Запись в БД | `souls` с `transport: agent` | `souls` с `transport: ssh` — **та же таблица**, разный режим |
| Кеш модулей на хосте | в `/var/lib/soul-stack/{bin,modules}/` (см. [modules.md](modules.md)) | в `/var/lib/soul-stack/{bin,modules}/` (см. [modules.md](modules.md)) |
| Подключение к Keeper | алгоритм `priority + failback` (см. [connection.md](connection.md)) | алгоритм не применяется — Keeper сам ходит на хост |

Push-режим расписан в [keeper/push.md](../keeper/push.md). Push не используется как самостоятельный «agent-less mode» с собственным бинарём; это другой режим запуска того же `soul`-а.

Push-хост может в любой момент мигрировать в pull (поставили демон, оператор сменил `transport`) — запись `souls` остаётся, история не теряется.

## Принципы

- **Никакого исходящего трафика помимо Keeper-а.** Soul по своей инициативе ходит только к своему Keeper-кластеру (и к ресурсам, явно разрешённым внутри Destiny). Никаких телеметрических обращений, никаких автоматических обновлений мимо Keeper-а.
- **Один бинарь — два режима.** Pull-демон и push-oneshot — это `soul` с разным entry-point (`soul` как сервис vs `soul apply` как команда). Состав модулей, исполнение шагов, формат событий — идентичны.
- **Одна запись `souls` на хост, разный `transport`.** Push и pull — это поле в `souls`, а не отдельные сущности. Это даёт единый реестр, единый аудит, единый RBAC и возможность переключения режимов без потери истории.
- **Идентичность отделена от транспорта.** SoulSeed нужен только pull-режиму (там Soul инициирует подключение и должен себя авторизовать). В push идентичность хоста — это его SSH-сторона; см. [keeper/push.md → Аутентификация SSH](../keeper/push.md#аутентификация-ssh--pluggable-provider).
- **Локальный admin-эндпоинт пока не определён.** Нужен ли отдельный сокет/порт на хосте для статуса, force-resync, дампа Soulprint — открытый вопрос (см. [architecture.md → Открытые вопросы](../architecture.md#открытые-вопросы), пункт «Локальный admin-эндпоинт на Soul»).

## См. также

- [identity.md](identity.md) — реестры `souls` / `soul_seeds` / `bootstrap_tokens`, статусы.
- [onboarding.md](onboarding.md) — как Soul становится `connected`.
- [connection.md](connection.md) — `priority + failback` для pull-режима.
- [config.md](config.md) — формат `soul.yml`.
- [modules.md](modules.md) — кеш модулей на хосте.
- [architecture.md → Роли бинарей](../architecture.md#роли-бинарей) — определение `soul` в контексте трёх бинарей.
- [architecture.md → Push-режим](../architecture.md#push-режим-keeperpush) — спецификация push.
- [architecture.md → Модель модулей](../architecture.md#модель-модулей) — то, что Soul исполняет.
