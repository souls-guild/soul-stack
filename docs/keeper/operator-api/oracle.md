# Oracle - Vigil / Decree registry endpoints

Domain section [Operator API](../operator-api.md): endpoints `/v1/vigils*` + `/v1/decrees*` - CRUD registries event-driven monitoring Beacons (Vigil = Soul-side check, Decree = rule reactor Portent → match → enqueue scenario, [ADR-030](../../adr/0030-vigil-oracle.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/oracle.md](../mcp-tools/oracle.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 8 routes) - in the root [operator-api.md → Oracle (8)](../operator-api.md). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`VigilCreateRequest` / `VigilView` / `VigilListReply` / `DecreeCreateRequest` / `DecreeView` / `DecreeListReply` - **source of truth in form**). `vigil.*`/`decree.*` - NoSelector. Reactor flow (Portent → match Decree → enqueue scenario) by these permissions is **NOT controlled** - machine Soul-initiated path; security is based on the Decree subject binding ([ADR-030(b)](../../adr/0030-vigil-oracle.md)).

## Subject

`subject` is a nested object shared by Vigil, Decree and the Augur [Rite](augur.md#subject) ([NIM-280](../../adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-280-a-rules-subject-reads-both-levels--targeting-only)). Exactly one of its four dimensions is set; zero or two → `422`. On a response only the populated one is present.

| written as | reaches |
|---|---|
| `{"sid": ["db-01.example.com"]}` | those hosts, by identity |
| `{"incarnation": {"service": "redis", "name": "redis-prod"}}` | every host on that incarnation's roster |
| `{"coven": ["prod"]}` | a host carrying the label, **and** every member of an incarnation carrying it |
| `{"trait": {"key": "tier", "value": "gold"}}` | the same two-level reach, over traits |

Both halves of `incarnation` (`service` + `name`) and of `trait` (`key` + `value`) are required together - half a pair is `422`. An empty array counts as **absent**, so `{"coven": []}` is a subject with zero dimensions and is refused rather than stored as a rule matching everything. The incarnation address is the pair `<service>.<name>`, never the bare name.

**The two label dimensions read both levels.** `coven` / `trait` match the host's own `souls.coven[]` / `souls.traits` **unioned with** the labels of every incarnation it is a member of, resolved at match time. Nothing is written to `souls` - a host still carries only what an operator attached to it, so this is not the label inheritance [NIM-281](../../adr/0008-coven-stable-tags.md#amendment-2026-08-05-nim-281-a-label-is-never-inherited) removed. Consequences worth knowing before writing a rule:

- `{"coven": ["<incarnation-name>"]}` still does **not** scope a rule to that incarnation's hosts. A name is not one of an incarnation's labels; the roster is addressed by `{"incarnation": {...}}`, which reads the membership relation.
- ⚠ Labelling an incarnation widens every existing rule bound to that label to its members, with no rule edited. Symmetrically, unbinding a host silently **unwatches** it - every Vigil that reached it that way stops applying.
- Note the quiet half: a host no Vigil matches never runs the check, raises no Portent, and the Decree side is never consulted. The failure mode is silence, not an error.
- ★ Targeting only: an Archon's RBAC scope (`soul.list on coven=prod`) is resolved from the host's own column and is never widened by an incarnation's labels ([rbac.md](../rbac.md)).

**A Decree carries two incarnation-shaped fields, and they are not the same field.** `subject.incarnation` says WHO may fire the rule; the top-level `incarnation_name` says WHAT the reaction acts on. A Decree may legally name two different ones and then fire for nobody. Independently of the subject, a Decree checks that the sending host is a **member** of its `incarnation_name`, and that check reads the membership relation, never labels — wrong in both directions otherwise: a host merely tagged with an incarnation's name would pass, and a genuine member nobody tagged would fail (fail-closed, no fire and no `oracle.fired` audit). Bind the host to the incarnation; a coven tag is not a substitute.

### `POST /v1/vigils` - create Vigil

Permission: `vigil.create`. MCP-tool: `keeper.oracle.vigil.create`. Read-only by design (observes, does not mutate the host).

**Request `VigilCreateRequest`** (`required: name, subject, interval, check`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab `^[a-z0-9-]{1,63}$`) | yes | Vigil's name. |
| `subject` | `object` | yes | Which hosts run the check - exactly one of `sid` / `incarnation` / `coven` / `trait` ([§ Subject](#subject)). |
| `interval` | `string` (duration) | yes | Check frequency (`30s`). |
| `check` | `string` | yes | Core-beacon address (`core.beacon.file_changed`). |
| `params` | `object` | no | Scan parameters; form depends on `check` (typed schema deferred). |
| `enabled` | `boolean` | no | Whether the check is active. Default `true`. |

**Response `201 VigilView`:** `{name, subject, interval, check, params, enabled, created_by_aid?, created_at, updated_at}`.

Errors: `400` (broken JSON), `409 vigil-already-exists` (`name` busy), `422 validation-failed` (broken `name`/`interval`/`check`, or a subject with zero / two dimensions or half a pair). Audit: `vigil.created`.

### `GET /v1/vigils` - list of Vigils

Permission: `vigil.list`. MCP-tool: `keeper.oracle.vigil.list`. Query `offset`/`limit`. Sort `created_at` DESC, `name` ASC. Response `200 VigilListReply` (`{items, offset, limit, total}`).

### `GET /v1/vigils/{name}` — read Vigil

Permission: `vigil.list` (one permission covers list+get). MCP-tool: `keeper.oracle.vigil.list`. Response `200 VigilView`; `404 not-found` - no entry.

### `DELETE /v1/vigils/{name}` - remove Vigil

Permission: `vigil.delete`. MCP-tool: `keeper.oracle.vigil.delete`. Stops distributing to hosts in `VigilSnapshot`; connected Decrees **DO NOT cascade**. Response `204`; `404 not-found`. Audit: `vigil.deleted`.

### `POST /v1/decrees` - create Decree

Permission: `decree.create`. MCP-tool: `keeper.oracle.decree.create`. Default-deny: the rule is triggered only on its `on_beacon` × subject × `incarnation_name`.

**Request `DecreeCreateRequest`** (`required: name, on_beacon, subject, incarnation_name, action_scenario`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab `^[a-z0-9-]{1,63}$`) | yes | Decree's name. |
| `on_beacon` | `string` (kebab) | yes | The name of the Vigil whose Portent rule responds. |
| `subject` | `object` | yes | Which hosts may **fire** the rule - exactly one of `sid` / `incarnation` / `coven` / `trait` ([§ Subject](#subject)). Not the same field as `incarnation_name`. |
| `incarnation_name` | `string` (`^[a-z0-9][a-z0-9-]{0,62}$`) | yes | Target-incarnation the reaction **acts on** (ServiceRef resolves from it); membership-checked on fire. |
| `action_scenario` | `string` (`^[a-z][a-z0-9_]*$`) | yes | Named scenario (whitelist; raw command rejected). |
| `where` | `string` (CEL) | no | Predicate over `event.data`; compile - checked on create. |
| `action_input` | `object` | no | Script input (vault-ref goes as is). |
| `cooldown` | `string` (duration) | no | Minimum interval between triggers per-(decree, subject). |
| `enabled` | `boolean` | no | Is the rule active? Default `true`. |

**Response `201 DecreeView`:** `{name, on_beacon, where?, subject, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid?, created_at, updated_at}`.

Errors: `400` (broken JSON), `409 decree-already-exists` (`name` busy), `422 validation-failed` (broken `name`/`on_beacon`/`incarnation_name`/`action_scenario`/`where`-CEL/`cooldown`, or a subject with zero / two dimensions or half a pair). Audit: `decree.created`.

### `GET /v1/decrees` - list of Decrees

Permission: `decree.list`. MCP-tool: `keeper.oracle.decree.list`. Query `offset`/`limit`. Sort `created_at` DESC, `name` ASC. Response `200 DecreeListReply`.

### `GET /v1/decrees/{name}` — read Decree

Permission: `decree.list`. MCP-tool: `keeper.oracle.decree.list`. Response `200 DecreeView`; `404 not-found`.

### `DELETE /v1/decrees/{name}` - remove Decree

Permission: `decree.delete`. MCP-tool: `keeper.oracle.decree.delete`. Cascade cleans cooldown-state (`oracle_fires`, `ON DELETE CASCADE`). Response `204`; `404 not-found`. Audit: `decree.deleted`.

> **Secrets in payload.** Audit Vigil/Decree contains `name`/`check`/`interval`/subject (vigil) and `name`/`on_beacon`/`incarnation`/`scenario`/subject (decree); `params` / `where`-CEL / `action_input` **NOT put** (`action_input` may carry vault-ref in transit).
