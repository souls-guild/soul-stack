# ADR-053. Tier-ы инфраструктурных зависимостей

> **Статус: канон (active, 2026-06-11).** Классификационный ADR — фиксирует, какие внешние зависимости Keeper-кластера обязательны, какие опциональны и как опциональные обязаны деградировать. Кода по этому ADR не добавляется: решение описывает **уже сложившееся** поведение и закрепляет правило для будущих фич. Решение пользователя: без-Vault режим отвергнут, Vault остаётся обязательным.

**Контекст.** [requirements.md](../requirements.md) подавал Vault в одном ряду со сквозными возможностями «из коробки» (метрики, OTel, MCP, OpenAPI), формулировкой «интеграция с Vault». Это создавало впечатление, что Vault — опциональная интеграция. Фактически Vault **hard-required**: без него Keeper не стартует. Пользователь задал прямой вопрос «а если Vault нет?» — и выяснилось, что канона tier-ов (что обязательно, что опционально, как опциональное деградирует) в документах не существовало. Этот ADR закрывает пробел.

**Решение.** Обязательный инфраструктурный контур Keeper-кластера — **три компонента: PostgreSQL + Redis + Vault**. Все три проверяются на старте и при недоступности валят запуск (**fail-fast**), а не деградируют. Vault — равноправный третий обязательный компонент, не «интеграция».

### Почему Vault hard-required (точки в коде)

| Точка | Что ломается без Vault | Подтверждение |
|---|---|---|
| **vault-клиент на старте** | `setupVault` поднимает клиент через `NewClient`, который завершается `Ping(ctx)`; любая ошибка (addr пуст / auth не прошёл / ping не дошёл) → `errSetupFailed`, процесс не стартует | `keeper/cmd/keeper/daemon.go` (`setupVault`), `keeper/internal/vault/client.go` (`NewClient` → `cl.Ping(ctx)`) |
| **JWT signing-key (auth операторов)** | Подпись/верификация операторских JWT берёт HS256-ключ из `secret/keeper/jwt-signing-key` ([ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). Нет Vault → нет ключа → нет аутентификации Архонтов → управляющая плоскость недоступна | `keeper/internal/jwt/verifier.go`, `keeper/internal/bootstrap/signing_key.go` |
| **souls-PKI (mTLS-идентичность флота)** | Выпуск и ротация SoulSeed — подпись CSR Soul-агента через Vault PKI (`pki/sign/<pki_role>`). Нет Vault → нельзя онбордить новые Souls и ротировать существующие mTLS-пары | `keeper/internal/vault/pki.go`, `keeper/internal/soulseed/soulseed.go`, `keeper/internal/grpc/bootstrap.go` |

Это не «удобная интеграция», а несущие узлы: auth операторов и mTLS-идентичность Souls. Оба завязаны на Vault by design ([security-предпосылка](../requirements.md): «секреты на диск Keeper-кластера не материализуются»).

### Таблица tier-ов

**REQUIRED — fail-fast на старте, без деградации:**

| Компонент | Роль | Поведение без него |
|---|---|---|
| **PostgreSQL** | холодное хранилище состояния ([ADR-005](0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)) | старт падает |
| **Redis** | горячий слой: presence, lease, pub/sub, лидер-выборы ([ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)) | старт падает |
| **Vault** | secret-store + auth (JWT signing-key) + souls-PKI | старт падает (`setupVault` ping) |

**OPTIONAL-with-degradation — отсутствие конфигурируется, фича выключается внятно, Keeper не падает:**

| Возможность | Триггер «не настроено» | Деградация |
|---|---|---|
| **Sigil signing-key** | в реестре нет активного ключа / ref не задан | проверка целостности плагинов выключена, **fail-closed** (неподписанный плагин не допускается — ADR-026) |
| **Augur** (Keeper-side broker) | пустой реестр Augur-источников | **default-deny**, запросы к несконфигурённым источникам отклоняются |
| **Herald `secret_ref`** | у канала не задан `secret_ref` | webhook-доставка идёт **без HMAC-подписи** тела (ADR-052) |
| **push host-CA** | нет push-блока / `ssh_providers` | `keeper.push` — **no-op, push выключен** |
| **metrics basic-auth** | `metrics.basic.enabled: false` | metrics-listener поднимается **без auth** |
| **OTel-экспорт** | endpoint не задан | трейсы/метрики **не экспортируются**, in-process работа не страдает |
| **Kafka audit-sink** | `audit.sink ≠ kafka` (default `pg`) | audit-выгрузка идёт в PG (`audit_log`), Kafka не требуется; при `audit.sink: kafka` недоступность брокера деградирует **fail-closed** (audit compliance-критичен — событие не теряется: durable-fallback/блок write-path, не fail-open) — [ADR-059](0059-audit-sink-pluggable.md) |

**Правило для НОВЫХ фич.**
- Новая **обязательная** инфраструктурная зависимость (четвёртый REQUIRED-компонент) вводится **только через явное решение пользователя** — не «фича притащила зависимость как деталь имплементации».
- Новая **опциональная** возможность обязана деградировать **внятно**: при отсутствии конфигурации/бэкенда фича выключается с понятным логом или ошибкой на границе, но **не валит Keeper**. Выбор fail-open / fail-closed при деградации — осознанный security-trade-off, фиксируется в ADR фичи (ср. Tempo fail-open [ADR-050(b)](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api) против Sigil/Purview fail-closed).

**Обоснование.**
- **Security-предпосылка неприкосновенна.** «Секреты не материализуются на диске Keeper-кластера» ([requirements.md](../requirements.md), модель угроз) держится ровно на том, что secret-store — внешний Vault. Убрать Vault = убрать эту предпосылку.
- **Auth и mTLS-идентичность — не опциональны.** Без JWT signing-key нет операторов, без souls-PKI нет доверенного флота. Это ядро, а не «интеграция».
- **Честная дока — долг.** requirements не должны подавать hard-required Vault как опциональную интеграцию; этот ADR + правка [requirements.md](../requirements.md) устраняют рассинхрон код↔дока.

**Отвергнутые альтернативы.**
- **(а) Без-Vault режим** (auth-ключ из файла/env + встроенный CA вместо Vault PKI). Ломает security-предпосылку: приватник CA оказывается на диске Keeper-а или в PG; в multi-keeper-HA ([ADR-002](0002-transport-grpc-ha.md#adr-002-транспорт-keeper--souls--grpc-bidirectional-stream-поверх-mtls-ha-кластер-keeper)) этот приватник пришлось бы разносить по всем нодам кластера — расширение поверхности атаки на каждую ноду. **Отвергнут пользователем (2026-06-11).**
- **(б) SecretProvider-абстракция** (интерфейс с pluggable-бэкендами: Vault / file / cloud-KMS). Преждевременна — нет требования multi-backend; абстракция ради «вдруг понадобится второй бэкенд» — over-engineering. Вводится только при реальном требовании.

**Operations-нота.** «Обязательный Vault» ≠ «нужен тяжёлый Vault-кластер». Для малой инсталляции достаточно single-binary Vault с file-storage — операционно это сопоставимо с эксплуатацией Redis (один процесс, локальный storage). Рецепт — [`docs/operations/infra.md` → Лёгкий Vault для малых инсталляций](../operations/infra.md#лёгкий-vault-для-малых-инсталляций). **dev-mode Vault для прода непригоден** (теряет данные при рестарте) — это явно отмечено в рецепте.

**Связь с ADR.**
- **[ADR-005](0005-storage-postgres.md#adr-005-хранилище-состояния-keeper--postgres)** / **[ADR-006](0006-cache-redis.md#adr-006-кэш-и-координация--redis)** — два других REQUIRED-компонента.
- **[ADR-014](0014-operator-identity.md#adr-014-identity-модель-оператора-archon)** — JWT signing-key из Vault (одна из hard-required точек).
- **[ADR-026](0026-sigil.md#adr-026-sigil--целостность-плагинов-keeper-signed-digest-индекс)** — Sigil signing-key (OPTIONAL, fail-closed-деградация).
- **[ADR-050](0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)** — пример осознанного fail-open при деградации (контраст с fail-closed).
- **[ADR-052](0052-herald-notifications.md#adr-052-herald--tiding--уведомления-о-событиях-прогонов)** — Herald `secret_ref` (OPTIONAL, доставка без подписи при отсутствии).
- **[ADR-059](0059-audit-sink-pluggable.md)** — Kafka audit-sink (OPTIONAL, **fail-closed**-деградация; default `audit.sink: pg` — обязательный контур цел, Kafka НЕ становится 4-м required).
