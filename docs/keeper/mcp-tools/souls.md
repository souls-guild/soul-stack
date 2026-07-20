# Soul — MCP-tools host registry

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.soul.*` (host registration, bootstrap tokens, bulk assignment of Coven tags). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth in semantics is [operator-api.md → Soul](../operator-api/souls.md).

### Soul (5)

`keeper.soul.list` remains the transport of the future M2 registry (in `manifest.go` it is marked stub); the other four are Implemented. The `manifest.go` directory contains five Soul-tools (`create` / `issue-token` / `coven-assign` / `list` / `ssh-target.update`). Read registry routes (`GET /v1/souls/{sid}`, `/soulprint`, `/history`) - **REST-only** (there are no MCP tools, covered by permission `soul.list`).

#### `keeper.soul.create`

Registering a new host in the `souls` registry + issuing the first bootstrap token (for `transport: agent`). Permission: `soul.create`. Endpoint: [`POST /v1/souls`](../operator-api/souls.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | SID of the new host. |
| `transport` | `string` (enum `agent`/`ssh`) | yes | Host control method. |
| `covens` | `array<string>` | optional | Host Coven Tags. Default `[]`. |
| `note` | `string` | optional | Free comment on the post. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `sid` | `string` | SID of the created record. |
| `transport` | `string` (enum) | Mirror input. |
| `covens` | `array<string>` | Mirror input. |
| `status` | `string` (enum) | Always `pending`. |
| `registered_at` | `string` (RFC 3339) | Time of creation. |
| `created_by_aid` | `string` | AID of the Archon who performed the challenge. |
| `bootstrap_token` | `string` (optional) | Plain bootstrap token; present only for `transport: agent`. **Submitted once**, masked in logs. |
| `expires_at` | `string` (RFC 3339, optional) | Token expiration date; along with `bootstrap_token`. Wire key - `expires_at` (synchronous REST `SoulCreateReply` and `openapi.yaml`; legacy name `token_expires_at` is not used). |

#### `keeper.soul.issue-token`

Re-issue of bootstrap token for existing Soul (`transport: agent`). Permission: `soul.issue-token`. Endpoint: [`POST /v1/souls/{sid}/issue-token`](../operator-api/souls.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | SID Soul (echo path-param of the HTTP side). |
| `force` | `boolean` | optional | Default `false`. `true` - expire the active token and issue a new one. If `false` and there is an active token, the tool returns `bootstrap-token-active` error. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `sid` | `string` | SID. |
| `bootstrap_token` | `string` | New plain bootstrap token. **Submitted once**, masked in logs. |
| `expires_at` | `string` (RFC 3339) | Expiration date for the new token. Wire key - `expires_at` (synchronous between REST and `openapi.yaml`). |

For Soul with `transport: ssh` tool returns `validation-failed` error (ssh host does not have bootstrap phase).

#### `keeper.soul.coven-assign`

Bulk assignment of Coven labels: add ONE label (`mode: append`) / remove (`mode: remove`) or REPLACE (`mode: replace`) the entire set of Coven labels on hosts under `selector` ∩ coven-scope operator. Coven - cold PG tag (clean UPDATE `souls`). Permission: `soul.coven-assign`. Endpoint: [`POST /v1/souls/coven`](../operator-api/souls.md). Async: no.

**Scope-intersection (security).** A double check identical to REST is applied: (a) target hosts ⊆ coven-scope statement (predicate `coven && ARRAY[scope]`); (b) assigned label ∈ scope (permission gate `RBAC.Check` with selector `coven=<label>` + service check). For `replace`, gate (b) goes over EVERY set label: a statement with scope `dev` cannot override `prod` through `labels: [dev, prod]` (`forbidden` fails). An operator with `soul.coven-assign on coven=dev` cannot tag/unlabel `prod` (failure `forbidden`) and will not affect hosts outside of `dev`. Bare/`*`-permission removes both restrictions. Without this check, MCP would become a bypass of REST protection (privilege-escalation), so the MCP path performs it on the same service functions as REST.

**Input (XOR `label` ↔ `labels` by mode):**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `mode` | `string` (enum `append`/`remove`/`replace`) | yes | Add (`append`) / remove (`remove`) one label or replace (`replace`) the entire set. |
| `label` | `string` (Coven label) | for append/remove | Assignable/removable label. Forbidden for `replace`. |
| `labels` | `array<string>` (Coven tags) | for replace | Set of labels for `replace` (can be empty = "uncheck all"). Each label must be in the operator's coven-scope. Prohibited for `append`/`remove`. |
| `selector` | `object` | yes | Target hosts (∩ scope); at least one criterion. The combinations are connected by AND. |
| `selector.all` | `boolean` | optional | The entire registry (without host filter). |
| `selector.sids` | `array<string>` | optional | Dot list of SIDs. |
| `selector.coven` | `string` | optional | Hosts that ALREADY have this label. |
| `selector.incarnation` | `string` (incarnation-name) | optional | **Member** hosts of this incarnation (matched via the `incarnation_membership` relation, not a coven — `incarnation.name` is no longer a Coven, [ADR-008 amendment 2026-07-17](../../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). |
| `selector.status` | `string` (enum) | optional | Filter by status `souls`. |
| `dry_run` | `boolean` | optional | Default `false`. `true` - return `matched` without UPDATE. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `mode` | `string` | Mirror input. |
| `label` | `string` | Applied label for `append`/`remove` (mirror input). |
| `labels` | `array<string>` | Applied label set for `replace` (mirror input). |
| `matched` | `integer` | Hosts under `selector` ∩ scope. |
| `changed` | `integer` | Actually modified lines (0 for `dry_run`). |
| `status` | `string` (enum `completed`/`partial`) | `partial` - some chunks are committed, then fail (what is committed is idempotently repeated by the operator, not rolled back). |
| `dry_run` | `boolean` | Mirror input. |

Empty selector (neither `all` nor `sids`/`coven`/`incarnation`/`status`), label/any set label outside coven-scope, or XOR violation (`label`+`labels` together / `label` without `labels` for replace / vice versa) → `validation-failed` / `forbidden` error.

#### `keeper.soul.list`

Souls Enumeration. Permission: `soul.list`. Endpoint: [`GET /v1/souls`](../operator-api/souls.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `coven` | `string` or `array<string>` | optional | Filter by coven tag (exact-match by any value from `souls.coven[]`); multiple - array of values. |
| `status` | `string` (enum) | optional | `pending` / `connected` / `disconnected` / `expired`. |
| `transport` | `string` (enum) | optional | `agent` / `ssh`. |
| `offset` | `integer` | optional | Default `0`. |
| `limit` | `integer` | optional | Default `50`. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `items` | `array<SoulListEntry>` | Items - `{sid, transport, status, covens, last_seen_at, last_seen_by_kid, registered_at}`. |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.soul.ssh-target.update`

Updates per-host SSH push-flow details (`souls.ssh_target` jsonb: `ssh_port`/`ssh_user`/`soul_path`, [ADR-032](../../adr/0032-push-orchestrator.md) amendment 2026-05-26, S7-1). Source-of-truth for `PGFallbackTargetResolver`; `keeper.yml::push.targets[]` - legacy fallback under the `push.allow_legacy_push_targets` flag. Permission: `soul.ssh-target-update`; selector `host=<sid>`. Endpoint: [`PUT /v1/souls/{sid}/ssh-target`](../operator-api/souls.md). Async: no.

3-segment MCP-tool `keeper.soul.ssh-target.update` ↔ 2-segment permission `soul.ssh-target-update` (permission grammar is exactly `<resource>.<action>`; parallel `keeper.sigil.key.introduce` ↔ `sigil.key-introduce`).

**Input** (`required: sid, ssh_port, ssh_user, soul_path`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (FQDN, echo path-param) | yes | Host SID. |
| `ssh_port` | `integer` (1..65535) | yes | SSH port. |
| `ssh_user` | `string` (≥1) | yes | SSH user. |
| `soul_path` | `string` (abs. Unix path `^/.+`) | yes | Path to the `soul` binary on the host. |

**Output:** `{sid, ssh_target: {ssh_port, ssh_user, soul_path}}`. Errors: `not-found` (SID missing from registry `souls`).
