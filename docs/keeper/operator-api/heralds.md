# Herald - endpoints of the registry of notification delivery channels

Domain section [Operator API](../operator-api.md): endpoints `/v1/heralds*` - CRUD registry `heralds` (channels for delivering notifications about run events, [ADR-052](../../adr/0052-herald-notifications.md), S4). Herald answers the question "where to send" (Tiding - "what to respond to", [tidings.md](tidings.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/heralds.md](../mcp-tools/heralds.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 5 routes) - in the root [operator-api.md → Herald (5)](../operator-api.md). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`HeraldCreateRequest` / `HeraldUpdateRequest` / `Herald` / `HeraldListReply` - **source of truth by form**). `herald.*` - NoSelector (cluster-level control, like `push-provider.*` / `omen.*`). Routes are mounted only when the registry is configured (`router.go`: `if heraldH != nil`); when disabled - catch-all → `404`.

`type` - closed-enum (`webhook` in MVP; `slack`/`email` - additive post-MVP). `config` - per-type JSONB: for webhook `{ url, optional headers }` plus SSRF-opt-out flags (below). After committing each mutation, `herald.Service` invalidates the snapshot of the in-process dispatcher cache and cross-keeper via Redis pub/sub `herald:invalidate`.

### SSRF circuit (enabled by default)

Webhook = outgoing HTTP call to operator-specified URL → SSRF vector. The circuit is armed by default (pattern `core.url`, safe default + auditable opt-out):

- **https-only by default** - `http://`-URL only explicit `config.http_allowed=true` (with warning).
- **deny private IPs by default** - dial in loopback / RFC1918 / link-local / metadata-endpoint is blocked by the actually resolved IP; `config.allow_private=true` is removed.
- **redirect prohibition** + **timeout** for delivery (general keeper-side `shared/netguard`).

Broken/forbidden URL or conflicting config → `422 validation-failed` on create/update.

### `secret_ref` and signature `X-SoulStack-Signature`

`secret_ref` (nullable) - **vault-ref only** (`vault:<mount>/<path>`) on signing-token; the secret itself is NOT stored in the record (not cleartext, it is masked in errors). When `secret_ref` is specified, the webhook delivery signs the request body with the header:

```
X-SoulStack-Signature: sha256=<hex>
```

where `<hex>` is `HMAC-SHA256(body, signing-token)` in hex encoding. The receiver validates this way: it takes the raw request body, considers `HMAC-SHA256` to be the same general signing token, and compares the hex result with the part after `sha256=` (constant-time comparison). If `secret_ref: null` is not signed, the header is not set.

Payload notifications do NOT carry resolved-secrets (`input`/vault-resolved values ​​are not included; secret-masking passes, invariant A [ADR-027](../../adr/0027-apply-work-queue.md)).

### `POST /v1/heralds` - create Herald

Permission: `herald.create`. MCP-tool: `keeper.herald.create`.

**Request `HeraldCreateRequest`** (`required: id, type, config`): `{id (^[a-z0-9-]{1,63}$), type (enum: webhook), config (object), secret_ref? (vault-ref|null), enabled? (bool, omitted → true)}`.

**Response `201 Herald`:** `{id, type, config, secret_ref, enabled, created_at, updated_at, created_by_aid}`.

Errors: `400` (broken JSON / unknown strict-probe field), `409` (`name` busy), `422 validation-failed` (broken `name`/`type`/`config`/`secret_ref` or SSRF circuit violation). Audit: `herald.created`.

### `GET /v1/heralds` — list of Herald channels

Permission: `herald.list`. MCP-tool: `keeper.herald.list`. Query `offset`/`limit`. Sort `updated_at` DESC, `name` ASC. Response `200 HeraldListReply` (`{items, offset, limit, total}`).

### `GET /v1/heralds/{id}` - read one channel

Permission: `herald.read`. MCP-tool: `keeper.herald.read`. Response `200 Herald`; `404 not-found` - no entry.

### `PUT /v1/heralds/{id}` — replace channel (replace semantics)

Permission: `herald.update`. MCP-tool: `keeper.herald.update`. **Replace** - body completely replaces mutable fields (`type`/`config`/`secret_ref`/`enabled`); `name` (PK) immutable. Like Push-Provider - `PUT` (complete replacement), not `PATCH`. The SSRF invariant is the same as that of create.

**Request `HeraldUpdateRequest`** (`required: type, config`): `{type (enum: webhook), config (object), secret_ref? (vault-ref|null), enabled? (bool)}`.

**Response `200 Herald`.** Errors: `400`, `404 not-found`, `422 validation-failed`. Audit: `herald.updated`.

### `PUT /v1/heralds/{id}/label` — set the display caption

Permission: `herald.label-set`. MCP-tool: `keeper.herald.label-set`. OperationID: `setHeraldLabel`. The caption participates in **nothing derived** — no Vault path, no RBAC scope, no snapshot directory, no CEL root ([ADR-0085](../../adr/0085-entity-id-and-label.md)) — which is what makes *"I changed the label and nothing moved"* a guarantee rather than a hope. `name` addresses the row and does not change; there is no rename operation anywhere.

Deliberately narrower than `PUT /v1/heralds/{id}` above, which REPLACES the channel: granting a caption edit through that one would have granted a rewrite of `secret_ref`. And the caption is **not** the `<entity>` segment of `secret/herald/<entity>/<field>` — `name` is — which matters more here than anywhere else: that path is one hop from the registry row with nothing in between to notice, and `secretwrite` replaces rather than merges, so a caption there would orphan the channel's signing secret in silence.

**Request `LabelSetRequest`:** `{label? (string|null)}` — free text with capitals, spaces and punctuation; no `pattern`, no `maxLength`. `null`, an omitted field or an empty body `{}` **clears** the caption, after which consumers show `name` again; surrounding whitespace is trimmed and an all-whitespace value stores NULL.

**Response `200 Herald`** — the channel as it now reads. Errors: `400`, `403`, `404 not-found`, `422` (invalid path-`name`). Audit: `herald.label_changed`, payload `{id, old_label, new_label}` (`label` explicitly `null` when cleared).

### `DELETE /v1/heralds/{id}` — delete channel

Permission: `herald.delete`. MCP-tool: `keeper.herald.delete`. Cascadingly demolishes associated Tiding subscriptions (`tidings.herald ON DELETE CASCADE`). Response `204`; `404 not-found`. Audit: `herald.deleted`.

## Delivery terminals (not CRUD)

Outcomes of webhook delivery worker writes in audit as `herald.delivered` (success) / `herald.failed` (failure after exhaustion of retry); This is the `keeper_internal` area, not CRUD routes. Statuses of in-flight attempts - hot data in Redis (invariant hot→Redis, [ADR-006](../../adr/0006-cache-redis.md)), synchronously in PG for each attempt. Delivery semantics are at-least-once (a rare double is acceptable).

## See also

- [tidings.md](tidings.md) - paired register of subscription rules (`event_types` → Herald).
- [mcp-tools/heralds.md](../mcp-tools/heralds.md) - MCP side (`keeper.herald.*`).
- [ADR-052](../../adr/0052-herald-notifications.md) - Herald/Tiding design (tap over audit-writer, at-least-once webhook-delivery, SSRF invariants).
- [rbac.md](../rbac.md) — permissions directory `herald.*`.
