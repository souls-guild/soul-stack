# Voyage - endpoints of a unified batch run

Domain section [Operator API](../operator-api.md): endpoints `/v1/voyages*` (batch N incarnations by scenario / N hosts by command, [ADR-043](../../adr/0043-voyage.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/voyages.md](../mcp-tools/voyages.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 5 routes + RBAC-by-kind) - in the root [operator-api.md → Voyage (5)](../operator-api.md).

Unified batch run ([ADR-043](../../adr/0043-voyage.md)): `kind=scenario` — apply named scenario to a set of INCARNATIONS (batch = N incarnations by Legs); `kind=command` — execute a whitelisted module on a set of HOSTS (batch = N hosts, `incarnation.state` is not touched). The full request/response scheme is [`openapi.yaml`](../openapi.yaml) (`VoyageCreateRequest` / `VoyageCreateReply` / `VoyagePreviewReply`). Below is the normative semantics of behavior on which the contract is based.

#### `POST /v1/voyages` - create Voyage

Permission: **RBAC-by-kind** ([ADR-043 §6](../../adr/0043-voyage.md), security-critical fail-closed guard). Permission is selected by `kind` from the body (`scenario`→`incarnation.run`, `command`→`errand.run`) - middleware-route cannot do this (kind is visible only after the body is decoded), so the check lives inside the handler. MCP-tool: `keeper.voyage.start`.

Async-by-default: **202** + `{voyage_id, kind, scope_size, status, location}` + header `Location: /v1/voyages/{id}`. VoyageWorker selects a string using claim-loop; progress - `GET /v1/voyages/{id}`.

**Target resolution and scope boundaries:**

- **kind=scenario** — bare-check `incarnation.run` (quick failure before resolution), then per-incarnation scope-check over each resolved incarnation (its covens ∪ `{name}`): start on incarnation outside the permission scope = privilege escalation → **403**. A resolved incarnation that is `error_locked` refuses the whole Voyage with **409** — see below.
- **kind=command** - target ∩ Purview operator ([ADR-047 §S4](../../adr/0047-purview.md), security-fix). See below.

**Errors:** `400` malformed JSON or (`kind=command`) `dry_run` on a verb-shell module — see below; `401` `unauthenticated` (no/invalid JWT) or `operator-revoked-token` (AID revoke, command existence-gate, see below); `403` RBAC deny by kind / explicit foreign host (command) / incarnation outside scope (scenario); `404` explicit incarnation does not exist (scenario); `409` `incarnation-locked` — the resolved target holds an `error_locked` incarnation (scenario, see below); `422` invalid `kind` / empty `scenario_name`/`module` by kind / no target / invalid SID/coven/name / `where` > 4 KiB / `on_failure` not from `{abort, continue}` / `batch_size`+`batch_percent` at the same time / `batch_size`+`batch_mode=window` / ranges / empty resolve (`voyage_empty_target`) / scope > `voyage.max_scope` (`voyage_scope_too_large`) / effective batch above `voyage.max_batch_size` (`voyage_batch_size_too_large`); `429` `tempo-exceeded` (rate-limit, see below); `500` orchestrator not configured / DB failure.

> **`input` is not logged.** Audit events `scenario_run.started` / `command_run.invoked` do not carry the body `input` (invariant A of ADR-027).

##### `kind=scenario`: an `error_locked` incarnation in the target

A scenario Voyage whose resolved target holds an `error_locked` incarnation is refused
with **`409 incarnation-locked`** before the Voyage exists — the answer the
single-incarnation route `POST /v1/incarnations/{id}/scenarios/{scenario}` gives for the
same incarnation. The `detail` names the locked incarnations (up to ten, then a count)
with the run that locked each, and the two ways out:

```
incarnation demo is error_locked by run 01M4X5…: unlock it (POST /v1/incarnations/demo/unlock)
or rerun its last run (POST /v1/incarnations/demo/rerun-last) first
```

Leave the incarnation out of the target, or resolve it first. The refusal comes after
the per-incarnation scope check (`403`) and the `max_scope` cap (`422
voyage_scope_too_large`), so a forbidden or oversized target is answered as such.

Only `error_locked` is refused here. An incarnation that is `applying`, or in another
status a run cannot start from (`migration_failed`, `destroying`, `destroy_failed`), is
accepted, and its target fails with the reason on its row (below). The check is advisory
— an incarnation locked after the `202` fails its target the same way — and a
[Cadence](cadences.md) firing does not pass through it at all: a scheduled run on a
locked incarnation is recorded per target, not refused.

##### command ∩ Purview - security-fix with behavior change

**Change behavior for scoped roles.** Previously, `kind=command` resolved target **cluster-wide** (bare NoSelector, no intersection with Purview): scoped-Archon with `errand.run on coven=A` could run command-Voyage on `coven=B`. Now the resolved target **intersects with the Purview** operator through the same `soulpurview` resolver that filters `GET /v1/souls` ([ADR-047 §S4](../../adr/0047-purview.md)). Coverage = `target ∩ ResolvePurview(aid, "errand", "run")`. **For `Unrestricted` / cluster-admin (`*`-permission) the behavior does not change** - `Unrestricted` → all Souls, as before. Recorded as **security-fix in release-notes**.

Hybrid semantics - three branches in the target form (user choice 06/09/2026):

1. **Explicit foreign host in `sids[]`** (the operator listed specific SIDs, some outside Purview) → **403** (anti-escalation, parity with scenario path). Explicitly specifying a foreign host is an attempt at escalation, not a broad filter; silent cuts here would be a disguise.
2. **Wide target** (`coven=…` / `where:`-predicate, late-binding) → **truncates** to `target ∩ Purview` without failure (like list-visibility: the operator gets what's inside its bound).
3. **Empty intersection** (after pruning there are no hosts left) → **422 `voyage_empty_target`** (valid request, nothing to execute - distinguishable from 403 escalation).

**Existence-gate** - single, through `ResolvePurview("errand","run")`: "whether the operator holds the right in at least some scope" is checked by the same resolve that gives the scope boundary - without a separate nil-context bare-check (single `Check(nil)` falsely denies a scoped role with a non-empty scope). Empty Purview (`Scope.Empty`: no measurements and no `Unrestricted`) → failure BEFORE resolving the target; the reason is classified by the enforcer: **revoked-token → `operator-revoked-token` (401)**, no-perm → **403** (`operator lacks required permission errand.run`).

> **AND semantics `coven` for command.** `coven=[A,B]` means a host that is included in **ALL** listed covens (`souls.coven @> [A,B]`) - intentionally, in contrast to the scenario path (where coven is one filter env tag). This is an AND-merge security invariant ([ADR-043 §5](../../adr/0043-voyage.md)): invocation narrows the scope, does not expand.

> **soulprint/state-measurements Purview** in command∩Purview still under-display (fail-closed: with incomplete measurement support, the resolver would rather cut down its host, accessible ONLY by soulprint, than show someone else's); coven/regex/host are working fully (S3b-2b is postponed).

##### `dry_run` for `kind=command`

`dry_run: true` makes every per-host [Errand](errands.md) ask the module for a `Plan`
instead of applying it, with exactly the semantics of the single-SID path — the same
flag on the same request, one per resolved host. Two consequences follow from that,
both per-target:

- a host whose Soul did not announce the `dry_run` capability is refused **before** its
  Errand is created, instead of applying for real ([errands.md](errands.md) →
  capability gate);
- a module the runner does not admit on the `Plan` path runs and comes back
  `failed`, and the Errand carries `error_message: errand_dry_run_unsupported`.

The run continues or stops on those according to `max_failures` / `on_failure`, like
any other per-host failure.

**Reading the outcome per host.** Both cases above show `failed`, and `error_code` on
the target tells them apart ([Target error codes](#target-error-codes)). The first —
refused ahead of dispatch — has no `errand_id`, because no `errands` row was written;
its code (`soul_capability_unsupported` / `soul_capability_unverifiable`) is the whole
reason. The second has an `errand_id`: follow it to
[`GET /v1/errands/{errand_id}`](errands.md) for `error_message`. Same shape for any
other pre-dispatch refusal, `soul_not_connected` included.

**Refused at creation:** `dry_run` together with a verb-shell module
(`core.cmd.shell` / `core.exec.run`) → **`400`**, from the same validator the
single-SID path uses ([errands.md](errands.md)). The check runs inside scope
resolution, so it lands before a scope is resolved, before a `voyages` row exists and
before the `202` — the alternative is a run that reports accepted and then produces
one identical, unavoidable failure per host.

> **Fixed 2026-08-08 ([NIM-559](../../adr/0043-voyage.md)):** the flag was accepted,
> stored and echoed back, but was **not** put on the per-host Errand request — every
> target ran for real while the run described itself as a preview. If you have
> `kind=command` runs recorded with `dry_run: true` from before this release, the
> hosts were modified; the API answer and the UI for those runs say otherwise.

##### Tempo rate-limit

`POST /v1/voyages` — resolver-heavy write endpoint under [Tempo](../config.md#tempo) per-AID rate-limiter ([ADR-050](../../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)): bucket `voyage_create` (default `10 rps`, burst `20`). Excess → **429 `tempo-exceeded`** + `Retry-After`. If Redis is unavailable, the limit is fail-OPEN (passthrough). GET/list/cancel are not limited (cheap).

#### `POST /v1/voyages/preview` — dry-resolve scope without creating Voyage

Permission: **RBAC-by-kind** (same as Create). REST-only (no MCP-tool). Purpose - **predisplay of the number of batches in the UI** for late-binding target (`coven` / `require_alive`, where the number of hosts is resolved by Keeper): runs the same resolve and the same gates as Create, but **does not write** to `voyages`/`voyage_targets` and **does not expand the SID list** - gives only numbers. For a snapshot target (explicit `incarnations[]`/`sids[]`), the client counts the number of batches itself - the endpoint is not required.

**Request** - the same body as `POST /v1/voyages` (`VoyageCreateRequest`). Taken into account (affect resolution/arithmetic): `target`, `kind`, `batch`/`batch_size`/`batch_percent`/`batch_mode`, `concurrency`, `max_failures`, `require_alive`. Not reflected in the reply: `schedule_at`, `inter_batch_interval_ms`, `inter_unit_interval_ms`, `on_failure`, `input`. `dry_run` changes no number here either, but it **is** read: preview shares Create's admission, so `dry_run` on a verb-shell module gets the same `400` (a preview that answered `200` for a run Create would refuse is worse than useless).

**Response `200 VoyagePreviewReply`:**

| Field | Type | Meaning |
|---|---|---|
| `kind` | `string` | `scenario` / `command` (echo). |
| `scope_size` | `int` | Number of resolved units (incarnations/hosts). The SID list is NOT expanded. |
| `total_batches` | `int` | Number of Legs (barrier) or `1` (window). 0 is not possible - the empty scope is cut off 422 before the reply is built. |
| `batch_mode` | `string` | `barrier` / `window`. ALWAYS present - explains the semantics of the remaining fields. |
| `effective_batch_size` | `int?` | Resolved Leg (barrier) size. **Omitted** for `batch_mode=window` (window width = `concurrency`, not Leg - UI reads `concurrency`) or if the entire scope is one Leg (batch is not specified). |

**Consistency with Create** - preview fails EXACTLY in the same place: the same general decode/validation/resolve path. `403`/`404`/`409`/`422` come under the same conditions as Create (scenario - per-incarnation scope-check; command - hybrid-semantics 403/cut/422; scope > `voyage.max_scope` → 422). Rate-limited via **own** Tempo-bucket `voyage_preview` ([ADR-050 amendment](../../adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api)) - **softer** than create (default `30 rps`, burst `60` vs `10`/`20`), because preview has a read-like effect (without persist/audit), but the resolver is heavy in cost. Excess → **429 `tempo-exceeded`** + `Retry-After`.

> **Note (mapping↔router).** In [`router.go`](../../../keeper/internal/api/router.go) Voyage has a sixth read route - `GET /v1/voyages/{id}/targets` (All-runs drill, ADR-043 S5), not reduced to the root mapping table `### Voyage (5)`. There is no MCP symmetry (REST-only). Mixing - separate doc-PR (see docs-writer's report on drift).

#### `GET /v1/voyages/{id}/targets` — the units of a run

One row per target: `target_kind` / `target_id`, `batch_index`, `status`, the back-link
to its child run (`apply_id` for `kind=scenario`, `errand_id` for `kind=command`),
`finished_at`, and — for a target that did not succeed — `error_code` and
`error_message`. Both are omitted on a target that succeeded, and on a scenario target
whose run exists and failed: the run holds that reason, follow `apply_id`.

`error_message` is a sentence Keeper composed — for `incarnation_error_locked` it names
the run that locked the incarnation. It is never a raw error: a dispatch error can quote
request input. Codes from the Errand path carry no message; the code is the reason.
A target the run itself stopped — an abort, a lost lease, a shutdown — gets no code.
Which status such a host is left in depends on where the stop caught it, and some of
those rows are never closed (NIM-1001).

##### Target error codes

| Code | Kind | Meaning |
|---|---|---|
| `incarnation_error_locked` | scenario | The incarnation was `error_locked` when its run was to start — locked after the Voyage was accepted, or reached by a Cadence. `error_message` names the run that locked it, unless the lock landed in the instant between Keeper's check and the run's start. Unlock it or rerun its last run, then run again. |
| `incarnation_applying` | scenario | Another run was applying the incarnation; `error_message` names it on the same terms. |
| `incarnation_not_runnable` | scenario | The incarnation's status permits no run (`migration_failed`, `destroying`, `destroy_failed`); `error_message` names the status on the same terms. |
| `incarnation_not_found` | scenario | The incarnation was deleted after the Voyage was accepted. |
| `service_not_registered` | scenario | The incarnation's service is not in the service registry. |
| `orphan_lock_release_failed` | scenario | Keeper could not check for, or release, an applying lock an earlier attempt of this Voyage may have left on the incarnation; the run was not started. |
| `await_failed` | scenario | Keeper stopped tracking the run before it ended; the run itself holds its outcome (`apply_id`). |
| `soul_not_connected` | command | The host's Soul was not connected; no Errand was created. |
| `errand_dry_run_unsupported` | command | `dry_run` on a module that cannot be planned; no Errand was created. |
| `soul_capability_unsupported` | command | The host's Soul did not announce the `dry_run` capability — upgrade that agent. |
| `soul_capability_unverifiable` | command | Keeper could not confirm the capability (no presence source); refused rather than risk applying. |
| `invalid_request` | command | The per-host request was malformed (empty SID or module, timeout out of range). |
| `errand_failed` | command | The Errand ran and failed; follow `errand_id`. |
| `module_not_allowed` | command | The Soul refused the module (allow-list); follow `errand_id`. |
| `timed_out` | command | The Errand timed out; follow `errand_id`. |
| `cancelled` | command | The Errand itself was cancelled. A host the run itself stopped carries no code. |
| `await_timeout` | command | Keeper stopped waiting for the Errand before it ended; follow `errand_id` for its outcome. |
| `async_escalation` | command | The Errand escalated to async and this Keeper has no source to await it from. |
| `unknown_status` | command | The Errand ended in a status Keeper does not know. |
| `input_decode_error` | either | The Voyage `input` is not a JSON object. |
| `ownership_check_failed` | either | Keeper could not confirm it still owns the Voyage (a transient database error), so it did not start this unit. |
| `spawn_error` | either | Any other refusal before the unit started; the detail is in the Keeper log only. |
