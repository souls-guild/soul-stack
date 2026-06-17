# Soul — индекс

Документация по `soul`-бинарю — демону-агенту на управляемом хосте. Один и тот же `soul` работает в pull (как демон) и в push (как одноразовая команда от Keeper); отсюда и единая сборка документов.

## С чего начать

| Документ | О чём |
|---|---|
| [concept.md](concept.md) | Что такое `soul`-бинарь, его место в общей картине, два транспорта (`agent` / `ssh`), принципы (никакого исходящего трафика помимо Keeper-а, один бинарь на оба режима, одна запись `souls` в БД). |
| [identity.md](identity.md) | Идентичность Soul: SID = FQDN, SoulSeed (mTLS), реестры `souls` / `soul_seeds` / `bootstrap_tokens`, статусы и переходы, ротация SoulSeed, revoke, переименование хоста. |
| [onboarding.md](onboarding.md) | Жизненный цикл bootstrap-токена (выписка → доставка → CSR → сжигание), детали со стороны Keeper-а и Soul-а, рекомендации оператору, способы доставки токена на хост, защиты. |
| [connection.md](connection.md) | Подключение к Keeper-кластеру: алгоритм `priority + failback`, YAML-конфиг блока `keeper:`, параметры (`retry`, `failback.interval`, `failback.spray`), гарантии. |
| [config.md](config.md) | Формат `soul.yml`: `sid`, `paths`, `keeper:`, `soulprint:`, `cleanup:`, `logging:`, `metrics:`, `otel:`, `tls`. Раскладка конфига на стороне Soul-хоста. |
| [modules.md](modules.md) | Кеш core- и custom-модулей на хосте: раскладка `/var/lib/soul-stack/{bin,modules}/`, схема имени `soul-mod-<имя>-<sha>`, поведение в pull и push, локальный cleanup. |
| [soulprint.md](soulprint.md) | Soulprint typed-схема MVP ([ADR-018](../adr/0018-soulprint-typed.md#adr-018-soulprint-typed-схема-mvp)): поля `SoulprintFacts` (os/kernel/cpu/memory/network/hostname/sid), таблица маппинга `family→pkg_mgr/init_system`, канонические CEL-аксессоры `soulprint.self.<path>`, граница Soulprint↔`souls`-registry (covens — Keeper-side проекция), `collected_at` vs `received_at`. |

## Связанные документы

- [`docs/architecture.md`](../architecture.md) — слои выше и ниже Soul:
  - [Жизненный цикл Soul и реестр душ](../architecture.md#жизненный-цикл-soul-и-реестр-душ) — реестры и статусы.
  - [Подключение Soul: priority и failback](../architecture.md#подключение-soul-priority-и-failback) — спецификация алгоритма.
  - [Push-режим (`keeper.push`)](../architecture.md#push-режим-keeperpush) — второй транспорт.
  - [Модель модулей](../architecture.md#модель-модулей) — что Soul исполняет.
  - [Доставка SoulSeed-токена на хост](../architecture.md#доставка-soulseed-токена-на-хост) — способы доставки токена.
- [`docs/naming-rules.md`](../naming-rules.md) — словарь имён (SID, KID, SoulSeed, Coven, Reaper).
- [`examples/soul/soul.yml`](../../examples/soul/soul.yml) — рабочий пример конфига Soul-хоста.
