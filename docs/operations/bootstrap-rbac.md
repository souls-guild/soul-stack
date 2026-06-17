# Bootstrap первого Архонта и RBAC

Процедура первой инициализации кластера, выпуск дополнительных Архонтов через API/MCP, базовые RBAC-операции. Подробности дизайна — [ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта), [ADR-014](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon), [ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres); каталог permissions, формат строк и встроенные роли — [`docs/keeper/rbac.md`](../keeper/rbac.md).

## `keeper init` — первый Архонт

При первой инициализации реестр `operators` в Postgres пуст; без специального механизма все API/MCP вернут 403 (`default_policy: deny`, [ADR-013](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)). Bootstrap — administrative subcommand самого `keeper`-бинаря:

```sh
keeper init \
  --archon=archon-alice \
  --config=/etc/keeper/keeper.yml \
  --credential-out=/etc/keeper/archon-alice.jwt
```

Что происходит:

1. Берётся **PG advisory lock** на bootstrap-lock-id ([ADR-013(e)](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)). Race между несколькими `keeper init` решается на уровне БД.
2. Проверяется, что `operators` пуст. Если не пуст — отказ с сообщением `cluster already initialized; archon <aid> exists since <ts>`, exit != 0.
3. Создаётся запись `operators(aid=archon-alice, created_by_aid=NULL, bootstrap_initial=true)` ([ADR-014(a)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). Инвариант — ровно одна запись с `created_by_aid IS NULL` (partial unique index).
4. Привязывается роль `cluster-admin` (seed-роль из миграции, `permissions: ["*"]`) — строка `rbac_role_operators(cluster-admin, archon-alice)` ([ADR-028](../adr/0028-rbac-storage.md#adr-028-rbac-storage--postgres)).
5. Выпускается JWT с TTL = `auth.jwt.ttl_bootstrap` (default 30 дней; настраивается в `keeper.yml`).
6. JWT пишется в `--credential-out` с **`mode 0400`**, owner — пользователь, запустивший `keeper init`.
7. Audit: `operator.created` (`source=keeper_internal`, `archon_aid=NULL`, `payload={bootstrap_initial: true, ...}`).

### Restart-семантика

После catastrophic wipe Postgres (truncate `operators`) Keeper **не делает re-bootstrap автоматически** — это защита от случайного выпуска admin-token-а в логах ([ADR-013(d)](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)):

| Условие | Поведение |
|---|---|
| `operators` пуст + **нет** `--initialize` (или env `KEEPER_INITIALIZE=true`) | Keeper отказывается стартовать: `operators registry is empty; run 'keeper init --archon=<aid>' before starting the cluster`. Exit != 0. |
| `operators` пуст + `--initialize` | Стартует в read-only-режиме: listeners поднимаются, но все API/MCP-вызовы возвращают `503 cluster awaiting first archon`, пока `keeper init` не отработает. |
| `operators` непуст | Стартует штатно (read-only-проверка). |

В HA-кластере `keeper init` запускается **один раз** на одном инстансе. Остальные одновременные `keeper init` ждут advisory lock, видят непустой реестр и отказываются с указанием уже созданного Архонта.

### Хранение bootstrap JWT

Файл `--credential-out` — **исходный материал для первой настройки**, не «долговременное хранилище токена»:

- `mode 0400`, owner — оператор-человек (не `soul-stack`). Прячется в password manager / Vault оператора немедленно после bootstrap.
- TTL 30 дней — окно, чтобы успеть настроить дальнейшее администрирование (выпустить токены для CI, machine-identity, дополнительных людей). После использования первого Архонта для создания второго — изначальный JWT можно отозвать (см. [§ Ревокация](#ревокация-архонта)) или дать истечь.
- В git, в /etc/keeper/, в systemd-credential-store — **не класть** долго. Это admin-credential с правами `*`.

## Второй+ Архонт через Operator API

После bootstrap единственный путь создания Архонтов — Operator API (`POST /v1/operators`) или MCP-tool `keeper.operator.create` с permission `operator.create` ([`docs/keeper/rbac.md` → каталог permissions](../keeper/rbac.md#каталог-permissions)).

### Создание Архонта

```sh
curl -X POST https://keeper.internal:8080/v1/operators \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" \
  -H "Content-Type: application/json" \
  -d '{"aid": "archon-bob", "display_name": "Bob"}'
```

Ответ — JSON с полями созданного Архонта. Сам JWT для нового Архонта возвращает **отдельный endpoint** `operator.issue-token` (permission `operator.issue-token`):

```sh
curl -X POST https://keeper.internal:8080/v1/operators/archon-bob/issue-token \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" \
  -H "Content-Type: application/json" \
  -d '{"ttl": "24h"}'
```

Ответ: `{"jwt": "eyJ…", "exp": "2026-05-26T15:30:00Z"}`. JWT — единственный момент, когда оператор-исполнитель его видит; Keeper не хранит выпущенные токены (только signing-key и реестр Архонтов).

### Назначение роли

После создания Архонт сам по себе **не имеет ни одного permission** (default-deny). Привязка к роли — отдельная операция (permission `role.grant-operator`):

```sh
curl -X POST https://keeper.internal:8080/v1/roles/db-operator/operators \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" \
  -H "Content-Type: application/json" \
  -d '{"aid": "archon-bob"}'
```

Если роли `db-operator` ещё нет — её создаёт `role.create`:

```sh
curl -X POST https://keeper.internal:8080/v1/roles \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "db-operator",
    "permissions": [
      "incarnation.* on service=redis-cluster,vault-cluster",
      "soul.list"
    ]
  }'
```

Грамматика permission-строки — [`docs/keeper/rbac.md` → Формат permissions](../keeper/rbac.md#формат-permissions). Селектор `on <key>=<values>` поддерживает ключи `service` / `coven` / `incarnation` / `host`.

## RBAC: scope по coven / service / incarnation

Permission-строка с селектором — единственный механизм узкого scope-а ([rbac.md → Формат permissions](../keeper/rbac.md#формат-permissions)). Примеры из реальной инсталляции:

| Задача | Permission |
|---|---|
| Полный admin кластера | `*` |
| Только чтение всех Souls | `soul.list` |
| Apply на конкретный coven | `incarnation.run on coven=prod-eu-west` |
| Apply на конкретный сервис в любом coven | `incarnation.* on service=redis-cluster` |
| Создание / отзыв Архонтов | `operator.create`, `operator.revoke`, `operator.issue-token` |
| Управление ролями | `role.create`, `role.delete`, `role.update`, `role.grant-operator`, `role.revoke-operator` |
| Чтение audit-log (когда `GET /v1/audit` появится) | `audit.read` |
| Push на хосты | `push.apply on coven=<coven>` |
| Service-registry CRUD | `service.create`, `service.list`, `service.update`, `service.delete` |
| Drift-check ([ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) | `incarnation.check-drift on service=<svc>` |

`cluster-admin` — встроенная роль с `*`, удалить её через `role.delete` нельзя (`builtin=true` в `rbac_roles`).

### Защита от self-lockout

Инвариант ([ADR-013(c)](../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта)): **нельзя удалить последнего оператора с активным `*`-permission** (через `cluster-admin` или через явную роль с `permissions: ["*"]`). Попытка через API — `409 Conflict` с `would lock out the cluster`.

То же для `revoked_at`: попытка ревокации последнего `*`-Архонта — 409. Реальное «отозвать всех админов» = «сначала создать нового, потом отзывать старых».

## Ревокация Архонта

```sh
curl -X POST https://keeper.internal:8080/v1/operators/archon-old/revoke \
  -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)"
```

Что происходит:

- Устанавливается `operators.revoked_at = NOW()`. Архонт остаётся в реестре для аудита, новые JWT для него больше не выпускаются.
- **Активные JWT отозванного Архонта продолжают работать до своего `exp`** ([ADR-014(d)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). Короткий TTL (`ttl_default: 24h`) — естественная защита.
- Принудительный отзыв «всех живых JWT» — отдельная задача post-MVP (требует JWT-blocklist / session-store, не часть MVP).

### Аварийный отзыв всех JWT — ротация signing-key

Если живой JWT компрометирован и ждать `exp` нельзя — единственный надёжный способ в MVP: **ротация JWT signing-key** ([`docs/keeper/prod-setup.md` → Ротация signing-key](../keeper/prod-setup.md#jwt-signing-key-прод)). Сразу инвалидирует **все** живые JWT (подпись не сойдётся) — потребуется заново выпустить токены всем Архонтам.

```sh
vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"
systemctl reload keeper  # hot-reload перечитает ключ
```

После этого — пере-issue JWT через `operator.issue-token` для всех активных Архонтов (старые Bearer'ы → 401).

## Аудит RBAC-операций

Все RBAC-операции пишутся в `audit_log` ([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)) с конкретным `event_type`:

| Operation | `event_type` |
|---|---|
| Создание Архонта | `operator.created` |
| Ревокация Архонта | `operator.revoked` |
| Выпуск JWT для Архонта | `operator.token_issued` |
| Создание роли | `role.created` |
| Удаление роли | `role.deleted` |
| Привязка Архонта к роли | `role.operator_granted` |
| Отвязка | `role.operator_revoked` |

`source` — `api` или `mcp` (зависит от транспорта вызова), `archon_aid` — кто инициировал (бутстрапный `operator.created` — `NULL`, `payload={bootstrap_initial: true}`). Retention — `purge_audit_old` (default 365 дней). Просмотр audit-log через `GET /v1/audit` — отдельная задача (см. [ADR-022(j)](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)); пока — прямой SQL-запрос в PG.

## Сценарии операционного администрирования

### Передача доступа от увольняющегося Архонта

1. Текущий Архонт (или другой `cluster-admin`) создаёт нового: `POST /v1/operators` + `role.grant-operator`.
2. Выпускает JWT новому: `POST /v1/operators/archon-new/issue-token`.
3. **Через пересечение TTL** старый Архонт ревокается: `POST /v1/operators/archon-old/revoke`.
4. Живые JWT старого работают до `exp` (`ttl_default: 24h`) — оператор использует прежний токен до его истечения, потом перестаёт. Если нужно отозвать **немедленно** — ротация signing-key (см. выше).

### Machine-identity (CI / scripts)

MVP — JWT с длинным TTL (отдельный Архонт `archon-ci-deployer`, узкая роль с нужным набором permissions). Хранится в CI secret-store как Bearer-токен. Пере-выпуск по расписанию (короче `ttl_default` — например, 7 дней) — через `operator.issue-token` от admin-Архонта или скриптом, использующим прежний JWT до его `exp`.

mTLS-cert-форма Архонта для machine-identity — post-MVP ([ADR-014(b)](../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), расширение через `auth_method` enum без breaking changes.

### Сброс к «единственному админу» (catastrophic recovery)

Случай: потеря всех живых JWT, забыли credentials, оператор-человек ушёл. **Без доступа к Keeper-хосту** — никак (`keeper init` требует физического доступа).

С физическим доступом:

1. **НЕ truncate `operators`** — это сломает FK-цепочки (`created_by_aid`, `state_history.changed_by_aid`, аудит).
2. Создать **временного admin-а** через SQL прямо в PG:
   ```sql
   INSERT INTO operators (aid, display_name, auth_method, created_at, created_by_aid)
   VALUES ('archon-recovery', 'recovery', 'jwt', NOW(), NULL);
   INSERT INTO rbac_role_operators (role_name, aid, granted_at, granted_by_aid)
   VALUES ('cluster-admin', 'archon-recovery', NOW(), NULL);
   ```
3. Выпустить JWT для `archon-recovery` админ-утилитой (см. [open question в `disaster-recovery.md`](disaster-recovery.md#open-questions-runbook) — нужен ли отдельный subcommand `keeper issue-token`; на момент написания — задача post-MVP, делается через ротацию signing-key + повторный bootstrap-like процесс при необходимости).

Это **аварийная процедура**, требует audit-trail и доступа к PG. В штатной операционной модели — иметь минимум 2 `cluster-admin`-Архонтов и хранить их JWT в раздельных password manager-ах.

## См. также

- [`docs/keeper/rbac.md`](../keeper/rbac.md) — полный каталог permissions, грамматика, встроенные роли.
- [`docs/keeper/operator-api.md`](../keeper/operator-api.md) — REST-endpoint-ы, JWT-claims, RFC 7807 errors.
- [`docs/keeper/mcp-tools.md`](../keeper/mcp-tools.md) — MCP-tools (1:1 с REST-endpoint-ами).
- [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md) — JWT signing-key, ротация.
- [`docs/architecture.md` → ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention) — audit-pipeline.
