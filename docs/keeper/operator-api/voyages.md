# Voyage - endpoints of a unified batch run

Domain section [Operator API](../operator-api.md): endpoints `/v1/voyages*` (batch N incarnations by scenario / N hosts by command, [ADR-043](../../adr/0043-voyage.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/voyages.md](../mcp-tools/voyages.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 5 routes + RBAC-by-kind) - in the root [operator-api.md → Voyage (5)](../operator-api.md).

Unified batch run ([ADR-043](../../adr/0043-voyage.md)): `kind=scenario` — apply named scenario to a set of INCARNATIONS (batch = N incarnations by Legs); `kind=command` — execute a whitelisted module on a set of HOSTS (batch = N hosts, `incarnation.state` is not touched). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`VoyageCreateRequest` / `VoyageCreateReply` / `VoyagePreviewReply`). Below is the normative semantics of behavior on which the contract is based.

#### `POST /v1/voyages` - create Voyage

Permission: **RBAC-by-kind** ([ADR-043 §6](../../adr/0043-voyage.md), security-critical fail-closed guard). Permission is selected by `kind` from the body (`scenario`→`incarnation.run`, `command`→`errand.run`) - middleware-route cannot do this (kind is visible only after the body is decoded), so the check lives inside the handler. MCP-tool: `keeper.voyage.start`.

Async-by-default: **202** + `{voyage_id, kind, scope_size, status, location}` + header `Location: /v1/voyages/{id}`. VoyageWorker selects a string using claim-loop; progress - `GET /v1/voyages/{id}`.

**Target resolution and scope boundaries:**

- **kind=scenario** — bare-check `incarnation.run` (quick failure before resolution), then per-incarnation scope-check over each resolved incarnation (its covens ∪ `{name}`): start on incarnation outside the permission scope = privilege escalation → **403**.
- **kind=command** - target ∩ Purview operator ([ADR-047 §S4](../../adr/0047-purview.md), security-fix). See below.

**Errors:** `400` malformed JSON; `401` `unauthenticated` (no/invalid JWT) or `operator-revoked-token` (AID revoke, command existence-gate, see below); `403` RBAC deny by kind / explicit foreign host (command) / incarnation outside scope (scenario); `404` explicit incarnation does not exist (scenario); `422` invalid `kind` / empty `scenario_name`/`module` by kind / no target / invalid SID/coven/name / `where` > 4 KiB / `on_failure` not from `{abort, continue}` / `batch_size`+`batch_percent` at the same time / `batch_size`+`batch_mode=window` / ranges / empty resolve (`voyage_empty_target`) / scope > `voyage.max_scope` (`voyage_scope_too_large`) / effective batch above `voyage.max_batch_size` (`voyage_batch_size_too_large`); `429` `tempo-exceeded` (rate-limit, see below); `500` orchestrator not configured / DB failure.

> **`input` is not logged.** Audit events `scenario_run.started` / `command_run.invoked` do not carry the body `input` (invariant A of ADR-027).

##### command ∩ Purview - security-fix with behavior change

**Change behavior for scoped roles.** Previously, `kind=command` resolved target **cluster-wide** (bare NoSelector, no intersection with Purview): scoped-Archon with `errand.run on coven=A` could run command-Voyage on `coven=B`. Now the resolved target **intersects with the Purview** operator through the same `soulpurview` resolver that filters `GET /v1/souls` ([ADR-047 §S4](../../adr/0047-purview.md)). Coverage = `target ∩ ResolvePurview(aid, "errand", "run")`. **For `Unrestricted` / cluster-admin (`*`-permission) the behavior does not change** - `Unrestricted` → all Souls, as before. Recorded as **security-fix in release-notes**.

Hybrid semantics - three branches in the target form (user choice 06/09/2026):

1. **Explicit foreign host in `sids[]`** (the operator listed specific SIDs, some outside Purview) → **403** (anti-escalation, parity with scenario path). Explicitly specifying a foreign host is an attempt at escalation, not a broad filter; silent cuts here would be a disguise.
2. **Wide target** (`coven=…` / `where:`-predicate, late-binding) → **truncates** to `target ∩ Purview` without failure (like list-visibility: the operator gets what's inside its bound).
3. **Empty intersection** (after pruning there are no hosts left) → **422 `voyage_empty_target`** (valid request, nothing to execute - distinguishable from 403 escalation).

**Existence-gate** - single, through `ResolvePurview("errand","run")`: "whether the operator holds the right in at least some scope" is checked by the same resolve that gives the scope boundary - without a separate nil-context bare-check (single `Check(nil)` falsely denies a scoped role with a non-empty scope). Empty Purview (`Scope.Empty`: no measurements and no `Unrestricted`) → failure BEFORE resolving the target; the reason is classified by the enforcer: **revoked-token → `operator-revoked-token` (401)**, no-perm → **403** (`operator lacks required permission errand.run`).

> **AND semantics `coven` for command.** `coven=[A,B]` means a host that is included in **ALL** listed covens (`souls.coven @> [A,B]`) - intentionally, in contrast to the scenario path (where coven is one filter env tag). This is an AND-merge security invariant ([ADR-043 §5](../../adr/0043-voyage.md)): invocation narrows the scope, does not expand.

> **soulprint/state-measurements Purview** in command∩Purview still under-display (fail-closed: with incomplete measurement support, the resolver would rather cut down its host, accessible ONLY by soulprint, than show someone else's); coven/regex/host are working fully (S3b-2b is postponed).

##### Tempo rate-limit

`POST /v1/voyages` — resolver-heavy write endpoint under [Tempo](../config.md#tempo) per-AID rate-limiter ([ADR-050](../../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)): bucket `voyage_create` (default `10 rps`, burst `20`). Excess → **429 `tempo-exceeded`** + `Retry-After`. If Redis is unavailable, the limit is fail-OPEN (passthrough). GET/list/cancel are not limited (cheap).

#### `POST /v1/voyages/preview` — dry-resolve scope without creating Voyage

Permission: **RBAC-by-kind** (same as Create). REST-only (no MCP-tool). Purpose - **predisplay of the number of batches in the UI** for late-binding target (`coven` / `require_alive`, where the number of hosts is resolved by Keeper): runs the same resolve and the same gates as Create, but **does not write** to `voyages`/`voyage_targets` and **does not expand the SID list** - gives only numbers. For a snapshot target (explicit `incarnations[]`/`sids[]`), the client counts the number of batches itself - the endpoint is not required.

**Request** - the same body as `POST /v1/voyages` (`VoyageCreateRequest`). Taken into account (affect resolution/arithmetic): `target`, `kind`, `batch`/`batch_size`/`batch_percent`/`batch_mode`, `concurrency`, `max_failures`, `require_alive`. Ignored (not read in reply): `dry_run`, `schedule_at`, `inter_batch_interval_ms`, `inter_unit_interval_ms`, `on_failure`, `input`.

**Response `200 VoyagePreviewReply`:**

| Field | Type | Meaning |
|---|---|---|
| `kind` | `string` | `scenario` / `command` (echo). |
| `scope_size` | `int` | Number of resolved units (incarnations/hosts). The SID list is NOT expanded. |
| `total_batches` | `int` | Number of Legs (barrier) or `1` (window). 0 is not possible - the empty scope is cut off 422 before the reply is built. |
| `batch_mode` | `string` | `barrier` / `window`. ALWAYS present - explains the semantics of the remaining fields. |
| `effective_batch_size` | `int?` | Resolved Leg (barrier) size. **Omitted** for `batch_mode=window` (window width = `concurrency`, not Leg - UI reads `concurrency`) or if the entire scope is one Leg (batch is not specified). |

**Consistency with Create** - preview fails EXACTLY in the same place: the same general decode/validation/resolve path. `403`/`404`/`422` come under the same conditions as Create (scenario - per-incarnation scope-check; command - hybrid-semantics 403/cut/422; scope > `voyage.max_scope` → 422). Rate-limited via **own** Tempo-bucket `voyage_preview` ([ADR-050 amendment](../../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)) - **softer** than create (default `30 rps`, burst `60` vs `10`/`20`), because preview has a read-like effect (without persist/audit), but the resolver is heavy in cost. Excess → **429 `tempo-exceeded`** + `Retry-After`.

> **Note (mapping↔router).** In [`router.go`](../../../keeper/internal/api/router.go) Voyage has a sixth read route - `GET /v1/voyages/{id}/targets` (All-runs drill, ADR-043 S5), not reduced to the root mapping table `### Voyage (5)`. There is no MCP symmetry (REST-only). Mixing - separate doc-PR (see docs-writer's report on drift).
