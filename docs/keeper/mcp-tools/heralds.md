# Herald - MCP-tools registry of notification delivery channels

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.herald.*` (registry CRUD `heralds`, [ADR-052](../../adr/0052-herald-notifications.md), S4). Transport, auth, tool declaration format, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics, bodies, error codes is [operator-api/heralds.md](../operator-api/heralds.md).

### Herald (6)

6 tools 1:1 `keeper.herald.<verb>` ↔ permission `herald.<verb>` ↔ REST `POST/GET/PUT/DELETE /v1/heralds*` (selector - NoSelector). Business logic (validation `config`/`secret_ref` + SSRF circuit https-only + deny private IPs + dispatcher cache invalidation via Redis `herald:invalidate`) lives in `herald.Service`; tool - transport. Tools are available only when the registry is connected (`HeraldSvc`); when disabled, the call returns `internal-error` ("herald registry is not configured").

#### `keeper.herald.create`

Creates a Herald notification delivery channel (webhook in MVP: `config.url` + opt. `headers`). The SSRF circuit (https-only + deny private IPs) is enabled by default, disabled `config.http_allowed` / `config.allow_private`. `secret_ref` (opt.) - vault-ref to signing-token (webhook signature `X-SoulStack-Signature: sha256=<hex>`, HMAC-SHA256). Permission: `herald.create`. Endpoint: [`POST /v1/heralds`](../operator-api/heralds.md). Async: no.

**Input** (`required: name, type, config`): `{name (^[a-z0-9-]{1,63}$), type (enum: webhook), config (object), secret_ref? (vault-ref|null), enabled? (bool, omitted → true)}`.

**Output:** `Herald` - `{name, type, config, secret_ref, enabled, created_at, updated_at, created_by_aid}`. Errors: `herald-already-exists` (`name` busy), `validation-failed` (broken `name`/`type`/`config`/`secret_ref` or SSRF circuit violation).

#### `keeper.herald.update`

Replaces the mutable fields of the Herald channel (replace semantics; `name` is the key, does not change). The SSRF invariant is the same as that of create. Permission: `herald.update`. Endpoint: [`PUT /v1/heralds/{name}`](../operator-api/heralds.md). Async: no.

**Input** (`required: name, type, config`): `{name, type (enum: webhook), config (complete new config - replace), secret_ref? (|null), enabled?}`. **Output:** `Herald`. Errors: `not-found` (no entry).

#### `keeper.herald.label-set`

Replaces the channel's **display caption** ([ADR-0085](../../adr/0085-entity-id-and-label.md)). The caption is free text - capitals and spaces allowed, nothing validates its form; `null` (or an omitted `label`) clears it and consumers fall back to showing `name`. `name` addresses the row and is NOT changed. Deliberately narrower than `keeper.herald.update`, which REPLACES the channel: a caption edit through that one would also rewrite `secret_ref`. The caption participates in nothing derived - in particular it is **not** the `<entity>` segment of `secret/herald/<entity>/<field>`, which is `name`, and that path is one hop from the registry row with nothing in between to notice, so a caption there would orphan the channel's signing secret in silence. Permission: `herald.label-set`. Endpoint: [`PUT /v1/heralds/{name}/label`](../operator-api/heralds.md). Async: no.

**Input** (`required: name`): `{name (^[a-z0-9-]{1,63}$), label? (string|null)}`. **Output:** `Herald` - the row as it now reads. Errors: `not-found`.

#### `keeper.herald.delete`

Deletes the Herald channel; cascade demolishes related Tiding subscriptions (`ON DELETE CASCADE`). Permission: `herald.delete`. Endpoint: [`DELETE /v1/heralds/{name}`](../operator-api/heralds.md). Async: no.

**Input:** `{name}`. **Output:** empty object (REST equivalent - 204). Errors: `not-found`.

#### `keeper.herald.list`

Enumeration of Herald channels (sort `updated_at` DESC, `name` ASC). Permission: `herald.list`. Endpoint: [`GET /v1/heralds`](../operator-api/heralds.md). Async: no.

**Input:** `{offset?, limit?}`. **Output:** `{items: array<Herald>, offset, limit, total}`.

#### `keeper.herald.read`

Reads one Herald channel by name. Permission: `herald.read` (separated from `list` - parallel to `operator.read`↔`operator.list`). Endpoint: [`GET /v1/heralds/{name}`](../operator-api/heralds.md). Async: no.

**Input:** `{name}`. **Output:** `Herald`. Errors: `not-found`.
