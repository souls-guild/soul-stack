# ADR-064. Secret write-path — приём plaintext-секрета от оператора и запись в Vault keeper-side

> **Статус: accepted, реализация pending.** Дизайн architect-а, решение пользователя (2026-07-01). Фиксация docs-first ДО кода. ADR **обобщает уже существующий** keeper-side write-path в Vault (`sigil.Introduce` / cert `issueMaterial` / `core.vault.kv-present`) на новый случай: приём plaintext-секрета **от оператора** (не генерируемого системой) через API/UI. **Amends [ADR-052](0052-herald-notifications.md) (Herald `secret_ref` → dual-mode) / [ADR-017](0017-keeper-side-core.md) (Provider `credentials_ref` → dual-mode).**

**Контекст.** Сейчас у оператора **единственный** путь дать Keeper-у секрет — vault-ref: оператор сам кладёт секрет в Vault и передаёт в API только путь `vault:<path>` (Herald `secret_ref`, [ADR-052](0052-herald-notifications.md); Provider `credentials_ref`, [ADR-017](0017-keeper-side-core.md); Augur `auth_ref`, [ADR-025](0025-augur.md)). Это соответствует security-предпосылке «секрет не покидает Vault» ([requirements.md](../requirements.md)) — по проводу идёт только путь. Но для оператора это трение: чтобы завести telegram-бота или cloud-учётку, он обязан сначала сходить в Vault руками, положить туда токен по какому-то пути, и только потом заполнить форму. Для «friendly» онбординга нужен второй путь: оператор вводит секрет **plaintext-ом** в UI/API → Keeper **сам** пишет его в Vault по детерминированному пути → в Postgres хранится только внутренний ref.

**Ключевое: write-path не новая механика.** Keeper уже пишет секреты в Vault в трёх местах, все переиспользуют `vault.Client.WriteKV` и хелпер построения ref:

- **`sigil.Introduce`** ([keeper/internal/sigil/keyservice.go](../../keeper/internal/sigil/keyservice.go)) — Keeper генерит ed25519-пару, пишет приватник в Vault KV `secret/keeper/sigil-keys/<key_id>`, в PG кладёт `vault_ref` ([ADR-026(d)](0026-sigil.md)).
- **cert `issueMaterial`** ([keeper/internal/reaper/rotate_certs.go](../../keeper/internal/reaper/rotate_certs.go)) — `SignCSR` → `WriteKV` → в PG `warrant.ref`.
- **`core.vault.kv-present`** ([keeper/internal/coremod/vault/kvpresent.go](../../keeper/internal/coremod/vault/kvpresent.go)) — generate-if-absent (redis/mongo passwords, система генерит значение).

Отсутствует **один** кусок: приём plaintext-значения **от оператора** (не генерируемого) + запись по auto-path. ADR добавляет прикладной слой поверх существующей инфраструктуры, **не новый инфра-код**.

## Решение

Ввести dual-mode приёма секрета в Herald- и Provider-CRUD (Operator API + MCP):

- **Оператор передаёт `secret`** (plaintext) **XOR** `secret_ref` (vault-путь, текущее поведение). Два поля, взаимоисключающие. В UI — radio-переключатель «значение / путь».
- При `secret` (plaintext) Keeper:
  1. строит **детерминированный путь** `secret/<domain>/<entity>/<field>` (`domain` = `herald`|`provider`, `entity` = имя записи, `field` = логическое имя секрета — `secret`/`credentials`);
  2. пишет plaintext в Vault KV этим keeper-side Vault-клиентом (`vault.Client.WriteKV` — тот же, что sigil/cert);
  3. кладёт в Postgres **внутренний ref** формата `vault:<path>#<field>` (как sigil/warrant, `vaultRefForPath`-хелпер уже есть);
  4. plaintext **нигде не персистится** — только в Vault; в PG/логах/audit его нет.
- При `secret_ref` (vault-путь) — поведение как сейчас: путь пишется в PG как есть, Keeper в Vault **не пишет** (оператор уже положил).
- **`update` перезаписывает** секрет по тому же детерминированному пути (idempotent-write, не создаёт новую версию-путь).

Резолв секрета на месте потребления (Herald webhook-подпись / Provider→CloudDriver creds-flow) не меняется — он и так читает по ref из PG.

### Scope MVP

- **Herald** — webhook signing-secret (`secret_ref`) + channel-token (telegram/slack bot-token). Оператор знает значение.
- **Provider** — cloud credentials (`credentials_ref`). Оператор знает значение.

За рамками MVP — см. «Отложенное».

## Security trade-off (осознанное послабление, решение пользователя)

Плей `secret` (plaintext) **ломает** инвариант «секрет не покидает Vault» ([requirements.md](../requirements.md), «безопасность на первом месте»): plaintext идёт оператор → Keeper по проводу. Vault-ref специально устроен так, чтобы этого не было. Послабление принято **ради UX** и **приемлемо только при обязательных митигациях** (все — блокеры реализации):

- **(a) TLS обязателен** на транспорте, несущем plaintext (Operator API / MCP). Без TLS приём `secret` не выполняется.
- **(b) Строгий masking во всех sink-ах** (логи / audit / OTel / UI / отчёты) + **guard-тесты на leak**. Поле называется `secret` — попадает под `shared/audit.sensitiveKeyRe` (substring-match `secret|token|password|…`, [mask.go](../../shared/audit/mask.go)) → авто-маска по имени ключа. **★ huma-request-body по умолчанию НЕ проходит через `MaskSecrets`** — требуется явный аудит всех точек логирования тела запроса Herald/Provider + guard-тест, что plaintext не утекает ни в один канал (регресс-тест на каждый sink, [feedback: guard-тесты на инварианты](../../CLAUDE.md)).
- **(c) plaintext нигде не персистится** — только Vault. В Postgres — только ref; в crash-dump/memory plaintext живёт лишь на время обработки запроса.
- **(d) RBAC / vault-policy** — Keeper пишет секрет **своей** Vault-policy (write-префиксы `secret/herald/*`, `secret/provider/*`) от имени оператора; операторский RBAC — переиспользуемые `herald.create` / `provider.create` (см. ниже). Vault-policy Keeper-а расширяется write-грантом на детерминированные префиксы.

Конфликт требований (security-предпосылка ↔ UX) — прерогатива пользователя; послабление **подтверждено 2026-07-01**.

## Совместимость: dual-mode (оба, не замена)

vault-ref **остаётся** первичным для advanced/GitOps-сценариев:

- **essence/GitOps** — plaintext в git класть нельзя, ref-only обязателен для декларативных пайплайнов.
- **advanced security** — оператор может держать полный контроль над Vault-путями и версионированием.
- **backward-compat** — сотни существующих тестов и конфигов на `secret_ref`/`credentials_ref` не ломаются.

plaintext-write — **friendly слой поверх**, не замена. Форма — `secret` XOR `secret_ref`.

## RBAC и имя

- **RBAC — без нового permission.** Приём `secret` идёт тем же write-эндпоинтом, что и запись `secret_ref` — переиспользуются `herald.create` / `provider.create` ([rbac.md](../keeper/rbac.md)). Отдельный `secret.write` **отвергнут**: право «завести Herald/Provider» уже включает «задать его секрет», гранулярность на способ передачи секрета не нужна.
- **Имя — без нового паттерна словаря.** Механика не вводит новой сущности Soul Stack — это **DevOps-поле `secret`** в API (правило [«мелкое = DevOps-термины»](../naming-rules.md)). Тематические имена-паттерны (`Consign`/`Entrust` — «вверить Keeper-у на хранение») **отвергнуты**: обобщение существующего write-path не тянет на именованную сущность, лишнее имя удорожает словарь.

## Отвергнутые альтернативы

- **Имя-паттерн `Consign` / `Entrust`.** Тематическая сущность «вверить секрет Keeper-у». Отвергнута — write-path уже существует трижды безымянно, приём plaintext-а его лишь обобщает; новое имя в [naming-rules.md](../naming-rules.md) — over-naming.
- **`oneof`-форма контракта** (`secret` и `secret_ref` как один oneof-union). Отвергнута в пользу двух явных взаимоисключающих полей + серверная валидация XOR: `oneof` в OpenAPI/huma даёт хуже UX формы и слабее типизацию клиента, чем плоские опциональные поля с проверкой «ровно одно задано».
- **Новый permission `secret.write`.** Отвергнут — `herald.create`/`provider.create` уже покрывают право задать секрет записи; отдельная гранулярность на способ передачи избыточна (см. RBAC выше).
- **ULID-immutable path** (`secret/<domain>/<entity>/<ulid>`, каждый write — новый неизменяемый путь). Отвергнут для Herald/Provider — там ровно один актуальный секрет на запись, `update` должен перезаписывать по стабильному пути. Детерминированный `secret/<domain>/<entity>/<field>` проще (нет orphan-путей от прошлых версий, нет GC-долга). ULID-immutable уместен там, где нужна история версий секрета — не в MVP-scope.

## Отложенное (post-MVP, без breaking changes)

- **Оператор-TLS-PEM** (когда оператор приносит свой сертификат/ключ, не PKI-issued). Секрет тот же класс — оператор знает значение — но ref живёт в **essence** (git), поэтому write-path для него тянет миграцию `essence → PG` (отдельное крупное решение). **Вне MVP**; вводится отдельным ADR.
- **ULID-immutable путь** — как опция для секретов с историей версий (не Herald/Provider), при реальном запросе.
- **Прочие точки приёма секрета** (Augur `auth_ref` и т.п.) — обобщаются тем же паттерном аддитивно, при запросе.

## Impact (для реализации, вне этого ADR)

Operator API Herald/Provider CRUD + OpenAPI (drift-regen) + companion UI (`types.gen` + контрол «значение/путь», `gen:api`) + доменный herald/provider CRUD (+plaintext→`WriteKV`) + `shared/audit` masking (guard leak-тесты) + RBAC (переиспользование) + audit-event + `vault-policy.hcl` (write-префиксы) + MCP. Масштаб Herald+Provider — средний (~10–15 core+UI-точек). **Реализация НЕ входит в этот ADR** (фиксация решения документом).

## Связь с ADR

- **[ADR-052](0052-herald-notifications.md)** — Herald `secret_ref`; этот ADR добавляет dual-mode приёма (`secret` XOR `secret_ref`).
- **[ADR-017](0017-keeper-side-core.md)** — Provider `credentials_ref` (cloud creds-flow); dual-mode приёма.
- **[ADR-026](0026-sigil.md)** — образец keeper-side write-path в Vault (`sigil.Introduce` → `WriteKV` → PG-ref).
- **[ADR-053](0053-dependency-tiers.md)** — Vault hard-required (write-path опирается на него); дополнительное послабление security-предпосылки фиксируется здесь как осознанный trade-off.
- **[ADR-014](0014-operator-identity.md)** — паттерн keeper-side секрета в Vault KV.
