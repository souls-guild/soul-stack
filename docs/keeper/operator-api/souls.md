# Soul - host registry endpoints

Domain section [Operator API](../operator-api.md): endpoints `/v1/souls*` (host registry, bootstrap tokens, bulk assignment of Coven labels). Conventions, error-format, pagination, entire mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools.md → Soul](../mcp-tools/souls.md).

## Endpoint sections

### Soul endpoints

Mapping endpoint ↔ MCP-tool ↔ permission (table of 8 routes: create / coven-assign / list + read routes `{sid}` / `{sid}/soulprint` / `{sid}/history` under `soul.list` + issue-token + `{sid}/ssh-target`; note about deferred `soul.get`) - in the root [operator-api.md → Soul (8)](../operator-api.md).

#### `POST /v1/souls/coven` — bulk assignment of Coven tags

Bulk addition (`mode: append`) / removal (`mode: remove`) of **one** Coven tag or **replacement** (`mode: replace`) of an entire set of Coven tags on hosts using a selector. Coven - cold PG tag ([ADR-008](../../adr/0008-coven-stable-tags.md)): pure `UPDATE souls`, no Redis entry. MCP-tool [`keeper.soul.coven-assign`](../mcp-tools/souls.md#keepersoulcoven-assign) - REST parity. CEL predicate - future slice.

**Body (append/remove - one label):**

```json
{
  "mode": "append",
  "label": "prod",
  "selector": { "all": false, "sids": ["a.example.com"], "coven": "staging", "incarnation": "redis", "status": "connected" },
  "dry_run": false
}
```

**Body (replace - set):**

```json
{
  "mode": "replace",
  "labels": ["prod", "dc-eu"],
  "selector": { "incarnation": "redis" }
}
```

- `mode` — `append` | `remove` | `replace`.
- `label` - one kebab-case Coven label for `append`/`remove`. Mandatory for these modes, **prohibited** for `replace`.
- `labels` - array of kebab-case Coven labels for `replace`. Mandatory for `replace`, **prohibited** for `append`/`remove`. Empty array = "uncheck all marks".
- `selector` is a subset of the `soul.*` targeting dictionary:
  - `all` — the entire registry ∩ scope (without host filter);
  - `sids` - dot list of hosts (SID = FQDN);
  - `coven` — hosts with an existing Coven tag;
  - `incarnation` — hosts of this incarnation (incarnation name — root Coven label according to ADR-008; matching via `name = ANY(coven)`);
  - `status` - filter by `souls` status.

Must specify at least one criterion (`all: true` or non-empty `sids`/`coven`/`incarnation`/`status`), otherwise `422`. Combinations are connected by **AND** (`incarnation: redis` + `status: connected` → connected-hosts incarnation `redis`). The free CEL predicate is deliberately not supported (scope check provability).
- `dry_run` — `true` count `matched` without `UPDATE`. Equivalent to the query parameter `?dry_run=true` (combined by OR).

**Answer `200`:** `{mode, label?, labels?, matched, changed, status, dry_run}`, where `label`/`labels` reflect the applied mode form; `matched` - hosts under `selector ∩ scope`, `changed` - actually modified strings (idempotent screening: `append` does not affect the host where the label already exists; `remove` - where there is no label; `replace` - where the set already matches the target set through `coven IS DISTINCT FROM $labels`); `status` - `completed` | `partial`.

**scope behavior:** the action is two-layer authorized (see [rbac.md → Soul](../rbac.md)). An operator with `soul.coven-assign on coven=dev` can only attach `dev`/labels included in its scope and only to hosts with these labels; assigned label outside scope → `422`, host outside scope simply does not fall into `UPDATE`. For `replace`, gate (b) checks EVERY label of the set (any label outside of scope → `422` BEFORE UPDATE; otherwise a statement with scope `dev` could override `prod` through `labels: [dev, prod]`). Bare-permission `soul.coven-assign` or `*` removes both restrictions.

**partial semantics:** massive `UPDATE` goes in chunks with a commit per chunk (keyset iteration over PK) - otherwise one giant transaction would hold the row-lock for `souls` for tens of seconds, blocking the hot heartbeat-flush. If the middle fails, some of the chunks are already committed; the answer is `200` with `status: partial`. This is safe: the operation is idempotent, the operator simply repeats the same request (committed changes are not reapplied).

#### `POST /v1/souls` — register Soul

Permission: `soul.create`. MCP-tool: `keeper.soul.create`.

Creates a registry entry `souls` with status `pending` ([onboarding.md → Onboarding flow](../../soul/onboarding.md)). For `transport: agent`, the first bootstrap token is issued in the same operation (one-time use, default TTL `24h`); The plain token is returned **once**, only `token_hash` is written to the database. For `transport: ssh` bootstrap token is not issued - onboarding comes down to setting up SSH access ([push.md](../push.md)), the `bootstrap_token` field is missing in the response.

**Request `SoulCreateRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | New host SID = FQDN ([identity.md](../../soul/identity.md)). |
| `transport` | `enum` (`agent`/`ssh`) | yes | Host control method ([push.md](../push.md)). It determines whether a bootstrap token is issued. |
| `covens` | `list<string>` | optional | Stable host Coven tags ([ADR-008](../../adr/0008-coven-stable-tags.md)). Default is `[]`. |

```json
{
  "sid": "redis-prod-01.example.com",
  "transport": "agent",
  "covens": ["redis-prod", "dc-eu-west"]
}
```

**Response `201 SoulCreateReply`:**

| Field | Type | Meaning |
|---|---|---|
| `sid` | `string` | SID of the created record. |
| `transport` | `enum` | Mirror from request. |
| `covens` | `list<string>` | Mirror from request. |
| `status` | `enum` | Always `pending` immediately after creation. |
| `registered_at` | `string` (RFC 3339) | Postgres record creation time. |
| `created_by_aid` | `string` | AID of the Archon who made the request (from JWT `sub`). FK on `operators(aid)`. |
| `bootstrap_token` | `string` (optional) | Plain bootstrap token. Present only for `transport: agent`. **One time return** - reissue via `POST /v1/souls/{sid}/issue-token`. |
| `expires_at` | `string` (RFC 3339, optional) | Bootstrap token expiration date. Present along with `bootstrap_token`. |

```json
{
  "sid": "redis-prod-01.example.com",
  "transport": "agent",
  "covens": ["redis-prod", "dc-eu-west"],
  "status": "pending",
  "registered_at": "2026-05-22T15:30:00Z",
  "created_by_aid": "archon-ops-01",
  "bootstrap_token": "bt_3f9a…",
  "expires_at": "2026-05-23T15:30:00Z"
}
```

> **Important for the client:** `bootstrap_token` is given once and is not stored anywhere. Deliver it to the host immediately ([onboarding.md → Delivery methods](../../soul/onboarding.md)); a lost token cannot be restored - only a new one can be issued via `POST /v1/souls/{sid}/issue-token`. The `bootstrap_token` field is masked in all observability channels (masked-keys rule, [§ Secret masking](../operator-api.md)).

**Errors:** `403 forbidden`, `409 soul-already-exists`, `422 validation-failed` (invalid SID/transport).

#### `POST /v1/souls/{sid}/issue-token` — reissue bootstrap token

Permission: `soul.issue-token`. MCP-tool: `keeper.soul.issue-token`. Path-param: `sid` (FQDN, URL-encoded - see [§ Conventions → ID in path](../operator-api.md#conventions)).

Issues a new bootstrap token for the existing Soul with `transport: agent` (operator lost token, scheduled re-issue). The `UNIQUE (sid) WHERE used_at IS NULL` invariant applies - Soul has no more than one active token ([identity.md](../../soul/identity.md)).

- **Without `force`** with an already active (not burned, not expired) token - `409 bootstrap-token-active`: a new token is not issued so as not to produce parallel valid tokens.
- **With `?force=true`** - the current active token is marked used (`used_at = now()`, not `expires_at`: otherwise the line would continue to hold the partial-unique slot `WHERE used_at IS NULL` and the reissue would hit 409), a new one is issued.

For Soul with `transport: ssh` - `422 validation-failed` (ssh host does not have bootstrap phase / SoulSeed).

**Query:**

| Param | Type | Required | Meaning |
|---|---|---|---|
| `force` | `bool` | optional | Default `false`. `true` - expire the active token and issue a new one. With `false` and the presence of an active token → `409 bootstrap-token-active`. |

**Request body:** empty (TTL is taken from the default token, `24h`).

**Response `200 SoulIssueTokenReply`:**

| Field | Type | Meaning |
|---|---|---|
| `sid` | `string` | SID. |
| `bootstrap_token` | `string` | New plain bootstrap token. **Returns once**, masked in logs. |
| `expires_at` | `string` (RFC 3339) | Expiration date for the new token. |

```json
{
  "sid": "redis-prod-01.example.com",
  "bootstrap_token": "bt_a17c…",
  "expires_at": "2026-05-23T16:00:00Z"
}
```

**Errors:** `403 forbidden`, `404 not-found` (SID is not in the registry), `409 bootstrap-token-active` (active token is present, `force` is not specified), `422 validation-failed` (`transport: ssh`).

#### `GET /v1/souls` - Souls list

Permission: `soul.list`. MCP-tool: `keeper.soul.list`.

**Query:** `offset`, `limit` + filters:

| Param | Type | Meaning |
|---|---|---|
| `coven` | `string` | Filter by coven tag (exact-match by any value from `souls.coven[]`). Multiple - repetition of the query parameter. |
| `status` | `enum` | `pending` / `connected` / `disconnected` / `expired`. |
| `transport` | `enum` | `agent` / `ssh` ([push.md](../push.md)). |

**Response `200 SoulListReply`:**

```json
{
  "items": [
    {
      "sid": "redis-prod-01.example.com",
      "transport": "agent",
      "status": "connected",
      "covens": ["redis-prod", "dc-eu-west"],
      "last_seen_at": "2026-05-20T15:29:55Z",
      "last_seen_by_kid": "keeper-eu-west-01",
      "registered_at": "2026-04-12T09:00:00Z"
    }
  ],
  "offset": 0,
  "limit": 50,
  "total": 137
}
```

Item fields (`SoulListEntry`) - registry projection `souls` from Postgres ([storage.md](../storage.md), [`../soul/identity.md`](../../soul/identity.md)).
