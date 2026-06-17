# Operator — endpoints управления Архонтами

Доменная секция [Operator API](../operator-api.md): эндпоинты `/v1/operators*` (создание / отзыв / выпуск JWT / чтение Архонтов, [ADR-013](../../adr/0013-bootstrap-archon.md#adr-013-bootstrap-первого-архонта) / [ADR-014](../../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)). Conventions, error-format, pagination, secret-masking, mapping-таблица — в корневом [operator-api.md](../operator-api.md). MCP-сторона — [mcp-tools/operator.md](../mcp-tools/operator.md).

## Endpoint-секции

Mapping endpoint ↔ MCP-tool ↔ permission (таблица 5 роутов + ремарка про отложенную MCP-симметрию read-эндпоинтов) — в корневом [operator-api.md → Operator (5)](../operator-api.md#operator-5--управление-архонтами-adr-013--adr-014).

#### `POST /v1/operators` — создать Архонта

Permission: `operator.create`. MCP-tool: `keeper.operator.create`.

Создаёт запись в реестре `operators` ([storage.md](../storage.md), [ADR-014](../../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)), привязывает к ролям из `keeper.yml → rbac.roles[].operators`, выпускает первый JWT с `auth.jwt.ttl_default` ([config.md → auth](../config.md#auth)).

**Request `OperatorCreateRequest`:**

| Поле | Тип | Required | Смысл |
|---|---|---|---|
| `aid` | `string` (regex `^[a-z0-9][a-z0-9._@-]{1,127}$`) | yes | AID нового Архонта ([naming-rules.md](../../naming-rules.md#идентификаторы)). |
| `display_name` | `string` | yes | Человекочитаемое имя для UI/аудита. |

```json
{
  "aid": "archon-alice",
  "display_name": "Alice Smith"
}
```

**Response `201 OperatorCreateReply`:**

| Поле | Тип | Смысл |
|---|---|---|
| `aid` | `string` | AID созданного Архонта. |
| `display_name` | `string` | Зеркало из request. |
| `created_at` | `string` (RFC 3339) | Время создания записи в Postgres. |
| `created_by_aid` | `string` | AID Архонта, выполнившего запрос (из JWT `sub`). FK на `operators(aid)`. |
| `jwt` | `string` | Выпущенный JWT, TTL = `auth.jwt.ttl_default`. **Возвращается один раз** — повторный выпуск только через `POST /v1/operators/{aid}/issue-token`. |

```json
{
  "aid": "archon-alice",
  "display_name": "Alice Smith",
  "created_at": "2026-05-20T15:30:00Z",
  "created_by_aid": "archon-root",
  "jwt": "eyJhbGc..."
}
```

> JWT в ответе маскируется во всех observability-каналах (правило masked-keys, [§ Secret masking](../operator-api.md#secret-masking-в-логах-и-трейсах)) и **отдаётся один раз** — сохраните токен сразу при получении.

**Errors:** `403 forbidden`, `409 operator-already-exists`, `422 validation-failed` (невалидный AID).

#### `POST /v1/operators/{aid}/revoke` — отозвать Архонта

Permission: `operator.revoke`. MCP-tool: `keeper.operator.revoke`. Path-param: `aid`.

Ставит `revoked_at = now()` в `operators`. Активные JWT продолжают работать до `exp` (нет revocation-blocklist; короткий TTL — естественная защита, [ADR-014](../../adr/0014-operator-identity.md#adr-014-identity-модель-оператора-archon)).

**Request body `OperatorRevokeRequest`:**

| Поле | Тип | Обязательность | Смысл |
|---|---|---|---|
| `reason` | `string` | optional | Свободный текст причины для audit-trail (фиксируется в `payload.reason` audit-event `operator.revoked`, [ADR-022](../../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)). |

**Response `204 No Content`** при успехе. **Errors:** `404 not-found`, `409 would-lock-out-cluster` (нельзя отозвать последнего `*`-Архонта, [§ Self-lockout invariant](../operator-api.md#self-lockout-invariant)).

#### `POST /v1/operators/{aid}/issue-token` — выпустить новый JWT

Permission: `operator.issue-token`. MCP-tool: `keeper.operator.issue-token`. Path-param: `aid`.

Выписывает новый JWT для существующего Архонта (после потери токена, плановой ротации). Старые JWT остаются валидными до `exp`.

**Request body:** пустой (TTL берётся из `auth.jwt.ttl_default`).

**Response `200 IssueTokenReply`:**

| Поле | Тип | Смысл |
|---|---|---|
| `aid` | `string` | AID. |
| `jwt` | `string` | Новый JWT. |
| `expires_at` | `string` (RFC 3339) | Срок истечения. Не путать с JWT-claim `exp` (unix-сек) внутри самого токена — это decoded-форма для удобства клиента. |

#### `GET /v1/operators` — список Архонтов

Permission: `operator.list`. Read-only, без audit.

**Query-параметры:**

| Поле | Тип | Обязательность | Смысл |
|---|---|---|---|
| `auth_method` | `string` (jwt/mtls/combined) | optional | Фильтр по форме credential. |
| `revoked` | `bool` (default `false`) | optional | `false` — только активные (`revoked_at IS NULL`); `true` — включая ревокнутых. |
| `offset` / `limit` | `int` | optional | Стандартная пагинация ([§ Pagination](../operator-api.md#pagination)). |

**Response `200 OperatorListReply`:** `{items: Operator[], offset, limit, total}`. Сортировка `created_at DESC` (новые сверху).

#### `GET /v1/operators/{aid}` — detail Архонта

Permission: `operator.list` (паттерн `soul.list`: одна permission покрывает list+get). Path-param: `aid`. Read-only, без audit.

**Response `200 Operator`:** запись реестра `operators`. **Errors:** `404 not-found` если AID не существует, `422 validation-failed` для битого AID.
