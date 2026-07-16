# Incarnation - endpoints of the life cycle of runtime instances

Domain section [Operator API](../operator-api.md): endpoints `/v1/incarnations*` (creating / running scripts / reading / unlock / upgrade / drift / destroy, [ADR-009](../../adr/0009-scenario-dsl.md)) + global read-view of runs `/v1/runs*` (page "All Runs"; runs belong to incarnations - handler and permission from the incarnation domain). Conventions, error-format, pagination, secret-masking (including masking `state`/`spec` in GET responses), mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/incarnations.md](../mcp-tools/incarnations.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 17 routes) - in the root [operator-api.md → Incarnation (17)](../operator-api.md); global `/v1/runs*` - [operator-api.md → Runs (2)](../operator-api.md).

#### `POST /v1/incarnations` — create instance

Permission: `incarnation.create`. MCP-tool: `keeper.incarnation.create`.

Runs the selected bootstrap script for the specified service; creates an entry `incarnation` in Postgres ([architecture.md → Incarnation](../../architecture.md)). The starting script is specified by the `create_scenario` field (a mechanism for several create scripts - any script with top-level `create: true` is valid, see § Selecting a starting script and bare-incarnation below). Asynchronous operation (for bare-incarnation - synchronous, without running).

**Request `IncarnationCreateRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` | `string` (kebab-case) | yes | The name of the new instance, also known as the root Coven label ([ADR-008](../../adr/0008-coven-stable-tags.md)). |
| `service` | `string` | yes | Service name from `keeper.yml → services[].name` ([config.md → services](../config.md#services--default_destiny_source--default_module_source)). |
| `covens` | `list<string>` | optional | Declared environment tags incarnation ([ADR-008](../../adr/0008-coven-stable-tags.md) amendment a). The format of each tag is `^[a-z][a-z0-9]*(-[a-z0-9]+)*$` (same as Soul tags). Default is `[]`. Carry RBAC coven-scope incarnation operations (see below). |
| `traits` | `object` | optional | Operator-set key-value incarnation trait marks ([ADR-060](../../adr/0060-traits.md) R1 slice a): key → value `scalar` OR `list of scalars` (`{"owner": "alice", "owners": ["alice", "bob"]}`). Placed in `incarnation.traits` (source of truth) and materialized into `souls.traits` member hosts. Nested object/array-within-array → `422`. Default is `{}` (no labels). Day-2 replacement - `PUT /v1/incarnations/{name}/traits`. |
| `create_scenario` | `string` | conditional | The name of the starting (bootstrap) script - a script with top-level `create: true` in `scenario/<name>/main.yml` (a mechanism for several create scripts; the name `create` is NOT privileged, only the key `create: true` gives validity). Format `^[a-z][a-z0-9_]*$`. **Required, if the service offers ≥1 create script**: empty field → `422 validation-failed` with text listing valid scripts. Value outside the create set (operational script, e.g. `add_user`, or non-existent name) → `422 validation-failed`. **For a service without create scripts, the field is ignored** - a bare incarnation is created (see below). Saved in `incarnation.created_scenario`; `rerun-last` uses it on the create path (when the last one to fall was the start script). |
| `input` | `object` | optional | Input for the selected startup script, is validated against the `scenario/<create_scenario>/input:` service schema (NOT necessarily `create`). For bare-incarnation it is not validated (there is no run). Default is `{}`. |

```json
{
  "name": "redis-prod",
  "service": "redis",
  "covens": ["prod", "dc-eu-west"],
  "input": {
    "spawn": {
      "provider": "aws-prod",
      "profile": "redis-medium-eu",
      "count": 3
    }
  }
}
```

**RBAC coven-scope (ADR-008 amendment a).** `covens` specifies env tags by which RBAC limits incarnation operations. Effective scope incarnation = `covens ∪ {name}` (name is the root Coven label). The role `incarnation.* on coven=prod` gets access to incarnations with `prod` in declared `covens` (or named `prod`); role `incarnation.* on service=redis` - to all incarnations of service `redis` regardless of tags. On **create** scope is checked by `service` + declared `covens ∪ {name}` from the body: an operator with scope `coven=prod` cannot create an incarnation with `covens=["dev"]` (will receive `403 forbidden`) - this is protection from privilege-escalation through a tag outside its scope. Details - [rbac.md → Selector grammar](../rbac.md).

**Response `202 Accepted`:**

```json
{
  "apply_id": "01HABCDEFGHJKMNPQRSTVWXYZ",
  "incarnation": "redis-prod"
}
```

`apply_id` — launch ULID (present in OTel traces, audit log, `state_history.apply_id` after a successful commit). Status poll - `GET /v1/incarnations/redis-prod` (`status`/`status_details`) and `GET /v1/incarnations/redis-prod/history`.

**Errors:** `403 forbidden`, `409 incarnation-already-exists`, `422 service-not-registered`, `422 validation-failed`.

**Manifest `lifecycle.auto_create` ([architecture.md → Service](../../architecture.md)).** If `manifest.lifecycle.auto_create: false`, `POST /v1/incarnations` creates an entry in `ready` **without** running the start script - `apply_id` is not in the response, the operator runs the selected script manually from Run-forms. By default (`true`, backcompat), the startup script runs immediately. Resolved from a snapshot of the deployed service-ref at the time of the request. This is **not** a bare incarnation: the bootstrap script is selected (`created_scenario` non-empty), the run is just postponed.

##### Selecting a starting script and bare-incarnation

Starting set of service = **exactly** scripts with top-level `create: true` in `scenario/<name>/main.yml` (auto-discover, [service/manifest.md → Starting script](../../service/manifest.md)). The name `create` is NOT privileged - it is included in the set only if `scenario/create/main.yml` itself carries `create: true`. Three branches depend on the value of `create_scenario` and the composition of this set:

- **The service offers ≥1 create-script + `create_scenario` non-empty and in the set** → the selected script is launched, `input` is validated against ITS `input:`-schema, `created_scenario` = selected name. Async run (`202` + `apply_id`).
- **Service offers ≥1 create-script + `create_scenario` empty** → `422 validation-failed` (`create_scenario_required`): selection is required because `input` is validated against the schema of a SPECIFIC script, and Keeper does not guess which one. `detail` lists valid scripts.
- **Service without a single create script + `create_scenario` empty** → **bare incarnation**: record is created in `ready` **synchronously, without running**, `apply_id` is not in the response, `created_scenario` = `null`. Ready for day-2 operations via `POST /v1/incarnations/{name}/scenarios/{scenario}`. Non-empty `create_scenario` for such a service → `422 validation-failed` (name not in the set).

A `create_scenario` value that is not included in the starter set (an operational script like `add_user` or a non-existent name) is always → `422 validation-failed`, the incarnation is not created (failure at the model stage). A name that is invalid in format (`^[a-z][a-z0-9_]*$`, path-traversal guard) is repelled with the same `422` until the set is resolved.

Example (redis carries three create scripts - `create` / `create_from_souls` / `migrate_cluster`): to raise a cluster from scratch, the operator passes `"create_scenario": "create"`; to upload data from an external cluster when creating - `"create_scenario": "migrate_cluster"`.

#### `POST /v1/incarnations/{name}/rerun-last` - restart the last crashed script from `error_locked`

Permission: `incarnation.rerun-last`. MCP-tool: `keeper.incarnation.rerun-last`. Path-param: `name`. OperationID: `rerunLastIncarnation`.

Atomically removes the `error_locked` block and **with the same action** restarts **the last fallen script** incarnation ([architecture.md → Atomicity and `error_locked`](../../architecture.md)) - this can be like a bootstrap script (`create`/..., if the creation failed), as well as any day-2 operation (`add_user`, `restart`, ...). The name of the failed script reads under `FOR UPDATE` (create-path is `incarnation.created_scenario`, day-2-path is the script of the last failed run). Under one `FOR UPDATE`: `error_locked → applying` bypassing `ready` (race-free), `state` is NOT touched (last known-good is saved, the snapshot of the transition is written to `state_history` with the general `apply_id`). Difference from `unlock`: `unlock` only clears the block (the operator decides what to do next), and `rerun-last` clears the block and restarts the fallen script with one confirmed action. Asynchronous operation - `202` + `apply_id`, status polling via `GET /v1/incarnations/{name}`.

**Recovering the input of a crashed run.** The script is restarted with the SAME input values ​​that the crashed run had (and not with defaults) - otherwise rerun with required fields (for example, redis cluster: `version`/`shards`) would have failed on input validations or applied defaults. Source input:

- **create-path** - `incarnation.spec.input` (what the operator declared when creating);
- **day-2-path** - recipe for the failed run `apply_runs.recipe.input` (read from `apply_id` of the last snapshot of the same `FOR UPDATE`; vault-refs are stored in strings, secrets are not revealed).

Works **only from status `error_locked`**. Two failure cases - **different problem-type** (both `409 Conflict`, machine-readable difference for UI/SDK):

- **status not `error_locked`** (nothing to restart - no run in error) → `409 incarnation-locked`;
- **input of the failed run is not available** (fail-closed), `recipe IS NULL` for one of three reasons: the run fell **before dispatch** (render_failed / no_hosts / pre-flight - terminal line `apply_runs` was written without a recipe); recipe **cleaned up with retention** Reaper (`purge_apply_runs`); **legacy-run** without saved recipe → `409 rerun-input-unavailable` ([§ Error types](../operator-api.md)). The transaction is NOT committed; the operator removes the block with the usual `unlock` and runs the desired script manually with an explicit input.

The same `apply_id` goes both in the `state_history`-snapshot of the unlock transition and in the restarted run - the snapshot correlates with the run.

**Request `IncarnationRerunLastRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `reason` | `string` (1..500 characters) | yes | Free text for audit-trail (written in payload audit-event `incarnation.rerun_last`). Confirmation of awareness of the action - UI requires confirm. |

```json
{ "reason": "fixed network ACL — retry failed scenario on redis-prod" }
```

**Response `202 Accepted`:** `{"apply_id": "<ULID>", "incarnation": "redis-prod", "scenario": "add_user"}` - `scenario` echoes the name of the restarted (crashed) script.

**Errors:** `403 forbidden` (no `incarnation.rerun-last`), `404 not-found` (incarnation does not exist), `409 incarnation-locked` (status not `error_locked`), `409 rerun-input-unavailable` (input of the failed day-2 run is not available - the run fell to dispatch and the recipe was not written / the recipe was cleared retention / legacy run without a prescription; see above), `422 validation-failed` (empty `reason` / `reason` longer than 500 characters / invalid path-`name` / incarnation service is not registered in the Service registry), `500 internal-error` (runner not configured / transaction / run launch).

**RBAC:** scope is the same as `incarnation.run` / `incarnation.unlock` - `coven=`/`service=`/`incarnation=` (landing on path-`name`: declared `covens ∪ {name}` + `service` from the incarnation line).

**Audit:** `incarnation.rerun_last` (`source: api` / `mcp`, `correlation_id=apply_id`, payload `{name, reason, scenario, previous_status, apply_id}`) - written by the handler after a successful unlock transition (`previous_status` is known only after it), does NOT reuse `incarnation.unlocked`.

#### `POST /v1/incarnations/{name}/scenarios/{scenario}` — run a custom script

Permission: `incarnation.run`. MCP-tool: `keeper.incarnation.run`. Path-params: `name`, `scenario`.

Runs scenario `<scenario>` against an existing incarnation. Asynchronous operation, response `202` + `apply_id`. The long path was chosen deliberately - RESTful (scenario as a sub-resource incarnation).

**The existence of a script is an async contract.** Keeper synchronously checks only the grammar of the name (`scenario.ScenarioNamePattern`), not its existence: scripts live in the service git repo (`scenario/<name>/main.yml`) and are resolved only after git-load inside the run, not in the registry. So the **unknown-but-grammatically-valid** script name gives `202 Accepted`, and the run then goes to `error_locked` from `scenario_load_failed` to `status_details`. This is a conscious async contract, consistent with `POST /v1/incarnations` (Create): the operator learns the result through `GET /v1/incarnations/{name}` (`status: applying` → `ready` or `error_locked`), and not from the synchronous `404`/`422`. Synchronous `422 validation-failed` returns only on a name that fails `ScenarioNamePattern` (path-traversal guard).

**Request:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `input` | `object` | optional | Input scenario, validated against `scenario/<scenario>/input:`. Default is `{}`. |

> **Batch / invocation-time chunking - to `/v1/voyages`.** Former invocation-time fields `target` / `wave` / `concurrency` (Tide, [ADR-040](../../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)) **removed in Wave 5**. This endpoint is only a single-incarnation scenario-run (without a batch). Batch N incarnations - `POST /v1/voyages` with `kind=scenario` + `batch_size` / `concurrency` ([ADR-043](../../adr/0043-voyage.md), see [operator-api/voyages.md](voyages.md)).

```json
// single-incarnation scenario-run
{
  "input": { "username": "alice", "role": "readonly" }
}
```

**Response `202 Accepted`:**
- Classic single-run (without `wave`): `{"apply_id": "<ULID>", "incarnation": "redis-prod", "scenario": "add-user"}`.
- Batch (several incarnations) - separate endpoint `POST /v1/voyages` (`kind=scenario`): per-incarnation `apply_id` are linked to Voyage via `voyage_targets.apply_id` (back-link lives in the orchestrator table, not in `apply_runs`). Progress - `GET /v1/voyages/{voyage_id}` ([ADR-043](../../adr/0043-voyage.md)).

**Errors:** `403 forbidden`, `404 not-found` (incarnation does not exist), `409 incarnation-locked`, `409 migration-failed`, `422 validation-failed` (script name did not pass `ScenarioNamePattern`). A non-existent-but-valid script is a **not** error for this endpoint: `202` → `error_locked` (see async contract above).

#### `GET /v1/incarnations/{name}` - read spec + state + status

Permission: `incarnation.get`. MCP-tool: `keeper.incarnation.get`. Path-param: `name`.

**Response `200 IncarnationGetReply`:**

| Field | Type | Meaning |
|---|---|---|
| `name` | `string` | Name instance. |
| `service` | `string` | Service name. |
| `service_version` | `string` (git-ref) | Pin version of the service. |
| `state_schema_version` | `int` | state_schema version ([ADR-019](../../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)). |
| `covens` | `list<string>` | Declared environment tags ([ADR-008](../../adr/0008-coven-stable-tags.md) amendment a). Source RBAC coven-scope (`covens ∪ {name}`). Always an array (empty if there are no tags). |
| `created_scenario` | `string` (optional) | The name of the starting (bootstrap) script that created the incarnation (the mechanism of several create scripts). `rerun-last` uses it on the create path (when the last one to fall was the start script). For **bare incarnation** (created without bootstrap script) - `null`/omitted (field with `omitempty`). |
| `spec` | `object` | jsonb is what the operator declared ([architecture.md → Incarnation](../../architecture.md)). Sensitive values ​​are masked (`***MASKED***`, see [§ Masking state/spec in GET responses](../operator-api.md)). |
| `state` | `object` | jsonb - current structured configuration. Sensitive values ​​are masked (see ibid.). |
| `status` | `enum` | `provisioning` / `ready` / `applying` / `error_locked` / `migration_failed` / `drift` / `destroying`. |
| `status_details` | `object` (nullable) | Error details if `status` is locking. |
| `created_by_aid` | `string` | FK on `operators(aid)`. |
| `created_at`, `updated_at` | `string` (RFC 3339) | Audit. |
| `last_drift_check_at` | `string` (RFC 3339, optional) | [ADR-031](../../architecture.md) Slice C: completion time of the last dry_run run `converge` - background (Reaper rule `scry_background`) or on-demand (`POST /v1/incarnations/{name}/check-drift`, Slice B). Absent if incarnation has never been scanned. |
| `last_drift_summary` | `object` (optional) | [ADR-031](../../architecture.md) Slice C: counts-aggregate of the latest DriftReport. Keys: `hosts_drifted`, `hosts_clean`, `hosts_unsupported`, `hosts_failed`, `total_hosts`, `scanned_at` (RFC 3339). Counts-only - the full DriftReport is not stored in the database (Slice B returns it directly to the check-drift response). Absent if incarnation has never been scanned. |

#### `GET /v1/incarnations` — list of instances

Permission: `incarnation.list`. MCP-tool: `keeper.incarnation.list`.

**Query:** `offset`, `limit` ([§ Pagination](../operator-api.md#pagination)) + optional filters:

| Param | Type | Meaning |
|---|---|---|
| `service` | `string` | Filter by service name. |
| `status` | `enum` (see above) | Filter by status. |

**Response `200`:** `{items: [IncarnationGetReply], offset, limit, total}` (elements are the same form as in `GET /v1/incarnations/{name}`).

#### `GET /v1/incarnations/{name}/history` — state changelog

Permission: `incarnation.history`. MCP-tool: `keeper.incarnation.history`. Path-param: `name`. Query:

| Parameter | Type | Required | Meaning |
|---|---|---|---|
| `offset` | `int` | no | Pagination offset (≥ 0, default 0). |
| `limit` | `int` | no | Pagination limit (1..200, default 50). |
| `apply_id` | `string` (ULID) | no | Optional filter by `state_history.apply_id`. Validates as Crockford-base32 ULID (26 characters). Non-existent, but syntactically valid `apply_id` for an existing incarnation → `200` + `items=[]`, not `404` (the absence of a row for the filter is a normal outcome, for example, the run has not yet been completed with a commit or has not resulted in state-changes). Invalid format - `400 malformed-request`. |

**Response `200`:** `{items: [StateHistoryEntry], offset, limit, total}`, where the element is the record `state_history` ([architecture.md → state_history](../../architecture.md)):

| Field | Type | Meaning |
|---|---|---|
| `history_id` | `string` (UUID) | PK. |
| `scenario` | `string` | The name of the script that caused the change (`"migration"` for migration steps). |
| `state_before` | `object` | jsonb state before. Sensitive values ​​are masked (`***MASKED***`, see [§ Masking state/spec in GET responses](../operator-api.md)). |
| `state_after` | `object` | jsonb state after. Sensitive values ​​are masked (see ibid.). |
| `changed_by_aid` | `string` | FK on `operators(aid)`. |
| `apply_id` | `string` (ULID) | Launch ULID. |
| `created_at` | `string` (RFC 3339) | When. |

#### `GET /v1/incarnations/{name}/runs` - list of incarnation runs

Permission: `incarnation.history` (reuse read-tier: whoever sees the history of the incarnation also sees its runs; separate permission is not entered). **REST-only - MCP-tool - but not.** Path-param: `name`. OperationID: `listIncarnationRuns`.

Read-view of runs (convolution of `apply_runs` by `apply_id`), under the UI "execution status / current job". Run (apply_run) - **NOT Voyage**: a single run of the script has its own read-view (closes UI bug `apply_id`→`/voyages/` 404). Difference from `GET …/history`: history is a log of **state changes** (`state_history`, the entry appears after a successful commit), runs is a log of **the runs themselves** (including running `applying` and failed ones that did not have a state commit).

**Data boundary.** `apply_runs` stores the status on the **host line** (planned...orphaned), not per-task progress (`TaskEvent` is aggregated on Soul, [ADR-012](../../adr/0012-keeper-soul-grpc.md)). The only per-task detail is the address of the failed task on the failed line (see detail endpoint below).

**Query:** `offset` (≥0, default 0), `limit` (1..1000, default 50) — [§ Pagination](../operator-api.md#pagination); out-of-range → `400`.

**Response `200`:** `{items: [RunSummaryEntry], offset, limit, total}`, newest from above (`MIN(started_at) DESC`):

| Field | Type | Meaning |
|---|---|---|
| `apply_id` | `string` (ULID) | Run ID. |
| `scenario` | `string` | Run script name. |
| `status` | `enum` | Aggregate status of the ENTIRE run - convolution of host lines: `applying` (at least one line is not terminal), `failed` (all are terminal, there is `failed`/`orphaned` - priority is given to `cancelled`), `cancelled` (all are terminal, there is `cancelled`, none `failed`/`orphaned`), `success` (`success`/`no_match` only). |
| `started_at` | `string` (RFC 3339) | `MIN(started_at)` by host lines. |
| `finished_at` | `string` (RFC 3339, optional) | `MAX(finished_at)`, only when ALL host lines have finished; otherwise the key is omitted (run still `applying`). |
| `started_by_aid` | `string` (AID, optional) | Initiator; the key is omitted if the initiator is removed. |

**RBAC:** gate — existence-`RequireAction(incarnation, history)`; per-`{name}` scope - in-handler inScope-predicate (same as History): incarnation outside Purview-scope or non-existent → single `404 not-found`.

**Errors:** `400 malformed-request` (out-of-range `offset`/`limit`), `404 not-found`, `422 validation-failed` (invalid path-`name`).

#### `GET /v1/incarnations/{name}/runs/{apply_id}` - run details (per-host)

Permission: `incarnation.history`. **REST-only - there is no MCP-tool.** Path-params: `name`, `apply_id` (ULID; non-ULID → `400 malformed-request`). OperationID: `getIncarnationRun`.

Slice of one run by hosts: header (`apply_id`/`scenario`/`status`/`started_at`/`finished_at`/`started_by_aid` - list form above) + `hosts[]`. There are N lines per host (according to Passage staged-render) - the UI sees the address of the failed per-passage task.

**`hosts[]` — `RunHostStatusEntry`:**

| Field | Type | Meaning |
|---|---|---|
| `sid` | `string` (FQDN) | Host. |
| `status` | `enum` | Host-level status line: `planned`/`claimed`/`running`/`dispatched`/`success`/`failed`/`cancelled`/`orphaned`/`no_match`. |
| `passage` | `int` | Passage line number. |
| `failed_task_idx` | `int` (optional) | LOCAL index of the failed task in `ApplyRequest` of its Passage; only on a crashed host (otherwise the key is omitted). |
| `failed_plan_index` | `int` (optional) | GLOBAL end-to-end `plan_index` of the same task across the entire scenario plan (correlation key with the plan); only on the crashed host. |
| `error_summary` | `string` (optional) | Operator-facing reason (`task <idx> <module>: <message>`, secret-masked on write-path); only on the crashed host. |
| `attempt` | `int` | Line attempt number. |
| `cancel_requested` | `bool` | Whether cancellation has been requested. |

**Errors:** `400 malformed-request` (non-ULID `apply_id`), `404 not-found` (incarnation outside scope/does not exist; `apply_id` does not exist **or belongs to another incarnation** - store layer filters `WHERE apply_id AND incarnation_name`, cross-incarnation reading of runs is excluded), `422 validation-failed` (invalid path-`name`).

#### `GET /v1/incarnations/{name}/runs/{apply_id}/tasks` - run tasks (plan + per-host)

Permission: `incarnation.history` (same read-tier as RunDetail - **NOT** `audit.read`). **REST-only - MCP-tool, but not.** Path-params: `name`, `apply_id` (ULID; non-ULID → `400 malformed-request`). OperationID: `getIncarnationRunTasks`.

**Per-task** run slice (unlike the detail endpoint above - it gives the host lines `apply_runs`): task plan (`apply_run_plan`) + the result of each task on each host from the audit log (`task.executed`), join by `plan_index`. Under the UI tab is the "run progress": which script was used, from which tasks, what changed. `hosts[]` carries only hosts with a result in audit (pending is not included - the front will finish it off).

**Response `200 RunTasksReply`:** `{tasks: [RunTaskEntry]}` (empty plan → `[]`), order - `plan_index`:

| Field | Type | Meaning |
|---|---|---|
| `plan_index` | `int` | End-to-end task index in the scenario plan (correlation key with `failed_plan_index` detail endpoint). |
| `passage` | `int` | Number Passage staged-render. |
| `name` | `string` | Task name. |
| `module` | `string` | Task module (`core.pkg.installed`, ...). |
| `no_log` | `bool` | `true` → task is marked `no_log:`; `params` and per-host `output`/`error.message` are suppressed - not sent at all. |
| `params` | `object` (optional) | Rendered operator input parameters of the task, **secret-masked** (secret note below). The key is omitted for `no_log` tasks and tasks without params. |
| `hosts[]` | `RunTaskHostEntry` | Per-host total: `sid` (FQDN or synthetic `keeper` for step `on: keeper`), `status` (`TASK_STATUS_*`), `output` (register data, optional), `error` (`{code, module, message?}` - only on the failed host; `message` suppressed for `no_log`). |

**RBAC:** existence-`RequireAction(incarnation, history)` + in-handler inScope predicate (parity RunDetail); incarnation is out of scope/does not exist **or** `apply_id` belongs to another incarnation → single `404 not-found`.

**Errors:** `400 malformed-request` (non-ULID `apply_id`), `404 not-found`, `422 validation-failed` (invalid path-`name`).

> **★ Secret hygiene `params`.** `/tasks` shows **rendered** `params` tasks to operators with `incarnation.history`. The values are masked by the seal-aware mechanism on the write-path (before writing to `apply_run_plan`, `audit.MaskSecretsSealed`; the same layer as `state`/`spec` - [§ Masking state/spec in GET responses](../operator-api.md)) - OR three layers ([templating.md §7.4](../../templating.md)): sealed-provenance (cell whose raw `${…}` read the secret-input of the active scheme / `vault(...)`), vault-ref-marker and regex-last-resort by sensitive-key name (`token`/`secret`/`password`/…); tasks `no_log: true` `params` are not shown at all.
>
> **Limitation.** A secret entered as a **plaintext constant directly into `params`** under an innocent key name (without `vault(...)` / `${…}` / secret-input), masking **will not catch** - there is no sealed-provenance (there was no expression reading the source secret), and the innocent name is not matchit regex-last-resort. Don't hardcode secrets into `params` - use `vault(...)`, secret-input or `no_log: true`.

#### `GET /v1/runs` - global list of runs

Permission: `incarnation.history` (reuse read-tier per-incarnation runs). **REST-only - MCP-tool, but not.** OperationID: `listRuns`. "All Runs" UI page.

The same convolution of `apply_runs` by `apply_id`, but **through all incarnations**: element - form `RunSummaryEntry` (see `GET …/runs` above) + field `incarnation` (`string`, incarnation-owner of the run - the global list is unreadable without it). Sort by `started_at DESC, apply_id DESC` (newest on top).

**Query:**

| Param | Type | Meaning |
|---|---|---|
| `status` | `enum` (`applying`/`success`/`failed`/`cancelled`) | Optional filter by aggregate run status. It is applied at the SQL level (otherwise `total`/`offset` would be separated from the post-filter). Invalid value → `422 validation-failed`. |
| `incarnation` | `string` | Optional filter by the name of the owner's incarnation. Invalid name → `422 validation-failed`. |
| `offset` | `int` (≥0, default 0) | [§ Pagination](../operator-api.md#pagination). |
| `limit` | `int` (default 50) | **Cap = 100** (not general 1000: global convolution is more expensive than flat list). Out-of-range / >100 → `400 malformed-request`. |

**Response `200`:** `{items: [GlobalRunEntry], offset, limit, total}`; `total` - the total number of runs under the same filters and scope.

**RBAC (Purview, [ADR-047](../../adr/0047-purview.md)):** gate - existence-`RequireAction(incarnation, history)` on chi-group `/v1/runs`; narrowing of visibility - in-handler with the same scope-resolution as `GET /v1/incarnations` (action=`history`): Purview-scope goes into SQL as a subquery on table `incarnation` and AND-intersects with user filters. **Fail-closed:** no claims / scope does not resolve / empty Purview → empty list (`200`, NOT `403` and NOT all Souls' runs).

**Errors:** `400 malformed-request` (pagination/limit>100), `403 forbidden` (no `incarnation.history`), `422 validation-failed` (invalid `status`/`incarnation` filter), `500 internal-error`.

#### `GET /v1/runs/stats` - summary run counters

Permission: `incarnation.history`. **REST-only - MCP-tool, but not.** OperationID: `getRunsStats`. There are no parameters.

Run counters by aggregate status within the boundaries of the Purview-scope operator (the same fail-closed resolve as `GET /v1/runs`: empty scope → zero aggregate, `200`).

**Response `200 RunsStatsReply`** - two baskets of the same shape:

| Field | Type | Meaning |
|---|---|---|
| `all` | `object` | For all the time. |
| `last_24h` | `object` | Runs **started** in the last 24 hours (window by `started_at` run - same axis as list order). |

Cart form: `{total, applying, success, failed, cancelled}` (`int`; `total` = amount; zero counters enabled - enum closed; `failed` enabled orphaned hosts).

**Errors:** `403 forbidden`, `500 internal-error`.

#### ~~`GET /v1/incarnations/{name}/tides` - list of Tide runs~~ - superseded-by `GET /v1/voyages` ([ADR-043](../../adr/0043-voyage.md), endpoint `/v1/tides` and table `tides` removed in Wave 5; section below is historical entry)

Permission: `incarnation.history` (parity GET `/history` - read about the runtime state of incarnation runs; a separate `tide.read` perm is not entered until the operator requests it, [ADR-040 § RBAC reuse](../../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)). MCP-tool: `keeper.tide.list` (ADR-040 W-4). Path-param: `name`.

**Query:** `offset`, `limit` ([§ Pagination](../operator-api.md#pagination)) + optional status filter:

| Param | Type | Meaning |
|---|---|---|
| `status` | `enum` (`pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`) | Filter by Tide status. |

**Response `200`:** `{items: [Tide], offset, limit, total}`. Sort by `started_at DESC` (fresh first). The form of one element is the same as in `GET /v1/tides/{tide_id}`.

#### ~~`GET /v1/tides/{tide_id}` - snapshot of Tide-run~~ - superseded-by `GET /v1/voyages/{voyage_id}` ([ADR-043](../../adr/0043-voyage.md), removed in Wave 5; section below is historical record)

Permission: `incarnation.history` (parity GET `/history`, see above). MCP-tool: `keeper.tide.get` (ADR-040 W-4). Path-param: `tide_id` (ULID).

UI does client-side polling for progress (every 2–5 s) until the native SSE endpoint appears (delayed, see ADR-040 open Q "Tide-progress SSE for UI").

**Response `200 Tide`:**

| Field | Type | Meaning |
|---|---|---|
| `tide_id` | `string` (ULID) | PK Tide. |
| `incarnation_name` | `string` | Which incarnation is it running on? |
| `scenario_name` | `string` | Which scenario is divided into Surge waves. |
| `status` | `enum` | `pending`/`running`/`succeeded`/`failed`/`partial_failed`/`cancelled`. |
| `total_surges` | `int` | Planned number of Surge waves (`ceil(scope_size / surge_size)`). |
| `current_surge_index` | `int` | 1-based number of the current Surge (0 = nothing running / Tide pending). |
| `surge_size` | `int` | Souls per Surge (echo `wave.size`). |
| `scope_size` | `int` | Size snapshot SID[] (`target_resolved_souls`). |
| `on_surge_failure` | `enum` | `abort`/`continue` (echo `wave.on_failure`). |
| `target_coven_override` | `array<string>` (optional) | Echo invocation-time `target.coven`-override. |
| `target_where_override` | `string` (optional) | Echo invocation-time `target.where`-override. |
| `concurrency_override` | `int` (optional) | Echo REPLACE-override scenario `serial:`. |
| `current_apply_id` | `string` (ULID, optional) | ULID head of the apply_run of the current Surge (NULL for pending). |
| `attempt` | `int` | How many times Tide was picked up by TideWorker (increment with reclaim Reaper rule `reclaim_tides`). |
| `started_by_aid` | `string` | FK on `operators(aid)`. |
| `started_at` | `string` (RFC 3339) | When Tide is inserted (POST-handler). |
| `finished_at` | `string` (RFC 3339, optional) | Finalization time (NULL for pending/running). |
| `summary` | `object` (optional) | `{surges: [TideSurgeRecord]}` — per-Surge result after the finalization of Tide. |

`TideSurgeRecord` fields: `surge_index` (int) / `apply_id` (ULID) / `terminal` (`success`/`failed`/`cancelled`/`orphaned`/`no_match`) / `started_at`, `finished_at` (RFC 3339) / `failed_souls` (int, omitempty) / `state_commit_error` (string, omitempty - per-Surge state-commit error, [ADR-009 §7](../../architecture.md), [ADR-040 amendment](../../adr/0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)).

**Errors:** `400 malformed-request` (invalid ULID in path), `403`, `404` (`tide_id` does not exist), `500`.

#### `POST /v1/incarnations/{name}/unlock` — remove `error_locked`

Permission: `incarnation.unlock`. MCP-tool: `keeper.incarnation.unlock`. Path-param: `name`.

Clears the `error_locked` status after manually analyzing the consequences of a partial failure. The operator takes responsibility for ensuring that the hosts are in a consistent state.

**Request `IncarnationUnlockRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `reason` | `string` (1..500 characters) | yes | Free text for audit-trail. Written in `state_history.metadata.unlock_reason`. |

```json
{ "reason": "manual cleanup verified — orphan keys removed on redis-prod-02" }
```

**Response `200`:**

| Field | Type | Meaning |
|---|---|---|
| `name` | `string` | Name instance. |
| `previous_status` | `enum` | `error_locked` (for confirmation). |
| `status` | `enum` | Typically `ready`. |
| `unlocked_by_aid` | `string` | AID that performed unlock. |
| `unlocked_at` | `string` (RFC 3339) | Time. |

**Errors:** `404 not-found`, `409` if the status is not `error_locked` (`detail` indicates the current status), `422 validation-failed` if `reason` is empty.

#### `POST /v1/incarnations/{name}/upgrade` - translation to new state_schema_version

Permission: `incarnation.upgrade`. MCP-tool: `keeper.incarnation.upgrade`. Path-param: `name`.

Starts state migration by [ADR-019](../../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl) + switches `service_version`. One PG transaction ([migrations.md](../../migrations.md)).

With [ADR-0068](../../adr/0068-service-upgrade-v2.md) upgrade - two-phase: if the target version has an upgrade script (`upgrade/<slug>/` with `from:` ⊇ current pin, mode `found`) - after the migration, host orchestration of the transition is automatically started (`status: applying` → `ready`); otherwise (`legacy`) - the same behavior (pin change + state migration + `drift`, the operator finishes with the usual apply). The paired READ endpoint `GET /v1/incarnations/{name}/upgrade-paths` ("where and how can I update": cheap - registry tags + `is_current`; `?to=` - `direction`/`mode`/`reachable`) is described in a separate section below.

**Request:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `to_version` | `string` (git-ref service) | yes | Target version of the service. |

**Response `202 Accepted`:** `{"apply_id": "<ULID>", "run_apply_id": "<ULID>"}` - two ULIDs = two-phase ([ADR-0068 §5](../../adr/0068-service-upgrade-v2.md)):

- `apply_id` (M) — state migration ULID, always present.
- `run_apply_id` (R) — ULID of the Runner for the upgrade script; **only in the found branch** (there is an upgrade script for the transition → autorun). In the legacy branch (no script → `drift`), the field is omitted (`omitempty`). Poll run - `GET .../runs/{run_apply_id}`.

Incarnation status poll - `GET /v1/incarnations/{name}` (`status: applying` → `ready` or `migration_failed`).

**Errors:** `404 not-found`, `409 incarnation-locked`, `409 migration-failed`, `422 validation-failed` (target version not registered).

#### `GET /v1/incarnations/{name}/upgrade-paths` - upgrade paths

Permission: `incarnation.upgrade` (read-edge). Path-param: `name`. Query-param: `to` (optional, git-ref targets). **READ, without audit.** Design - [ADR-0068 §6](../../adr/0068-service-upgrade-v2.md); enum dictionary - [naming-rules.md → Upgrade v2](../../naming-rules.md).

Two mutually exclusive blocks (`paths` without `?to=` / `target` with `?to=`) + common `current_version` and `current_state_schema_version` (current pin and incarnation scheme):

- **Without `?to=` - cheap**: `paths[]` - service registry tags (`ref` / `type` / `commit` / `is_current`). `is_current` - match the tag with the current pin. Direction (forward/downgrade) **not calculated** - prohibition of semver parsing of tag names ([ADR-007](../../adr/0007-versioning-git-ref.md)).
- **With `?to=<ref>` - on-demand analysis of one target**: object `target` (below).

**`target` (`?to=` only):**

| Field | Type | Meaning |
|---|---|---|
| `to` / `resolved_commit` / `target_state_schema_version` | `string` / `string` / `int` | Requested snapshot ref, sha1, state_schema of target. |
| `direction` | `enum` | `no-op` \| `downgrade` \| `forward` \| `same-schema` (ref-bump without schema change). |
| `mode` | `enum` | `found` \| `legacy` - only for `forward`/`same-schema` (if downgrade/no-op is omitted). |
| `slug` | `string` | slug upgrade script at `found` (omitted otherwise). |
| `downgrade` | `bool` | The target is below in the diagram (the chain is not loaded, forward-only). |
| `reachable` | `bool` | The goal is achievable with an upgrade. `false` only if the migration chain is broken. |
| `unreachable_reason` | `string` | Human-readable reason for unreachability (at `reachable: false`), e.g. `migration chain to <to> is broken: <details>`. Omitted if reachable. |
| `state_migrations[]` | `array` | Applicable chain `{from, to, path}` ([ADR-019](../../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)); empty if downgrade/broken chain. |

**Errors:** `404 not-found` (no incarnation / out of scope). **Broken migration chain is NOT an error**: `200` with `reachable: false` + `unreachable_reason` (preview gives the unreachable target as data). `502` — ls-remote tags / load snapshot target; `500` - other migration chain failure.

#### `POST /v1/incarnations/{name}/check-drift` — Scry drift check

Permission: `incarnation.check-drift`. MCP-tool: `keeper.incarnation.check-drift`. Path-param: `name`. **Sync operation** (not async, unlike `run`/`upgrade`/`destroy`): handler blocks before building `DriftReport` and returns it with a 200 response.

Implements the on-demand pilot [ADR-031](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile). Keeper parses `scenario/converge/main.yml` from the current git snapshot of the service, renders the plan as for a regular apply, but sends `ApplyRequest{dry_run:true}` to all hosts via work-queue (Acolyte). Soul calls `mod.Plan` (pure-read) instead of `mod.Apply`, returns native `changed` for each task. Keeper collects per-host aggregates and generates `DriftReport`. The information status `drift` is set to post-check if there is hosts_drifted/hosts_failed > 0 (NOT blocking, [ADR-031(d)](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)).

**Input-resolve convention.** converge-script declares `input:` schema; for each parameter the value is taken:
1. from `input.<name>` body of the request if the operator passed override;
2. else from `incarnation.state.<name>` ("by name" convention);
3. else from `default:` schema;
4. otherwise `required: true` without source → `422 validation-failed` (drift-input-missing).

**Request:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `input` | `object` | no | Override converge parameters. The names/types match the `input:` schema in the `scenario/converge/main.yml` service. |

**Response `200 OK`:** `DriftReport` (see [openapi.yaml → DriftReport](../openapi.yaml)):

```json
{
  "checked_at": "2026-05-26T10:15:30Z",
  "incarnation": "redis-prod",
  "scenario_ref": "converge",
  "hosts": [
    {
      "sid": "host-a.example.com",
      "status": "drifted",
      "tasks": [
        {"idx": 0, "module": "core.pkg.installed", "action": "Install redis", "changed": false},
        {"idx": 1, "module": "core.file.present", "action": "redis.conf", "changed": true}
      ]
    }
  ],
  "summary": {"hosts_drifted": 1, "hosts_clean": 0, "hosts_unsupported": 0, "hosts_failed": 0}
}
```

**Per-host `status`:**
- `clean` — all host tasks returned `changed=false`;
- `drifted` - at least one task `changed=true`;
- `unsupported` — at least one community module without `PlanReadSafe`-capability (default-deny, [ADR-031(f)](../../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile));
- `failed` is a real Plan error (different from `unsupported` by code in `TaskError`).

**Errors:** `404 not-found`, `422 validation-failed` (converge is missing in the current service-snapshot - "drift-checker is not available for this service", informational; or drift-input does not resolve), `500` (drift-checker is not configured - the only inline mode is acolytes=0).

**RBAC:** scope is the same as `incarnation.run` - `coven=`/`service=`/`incarnation=` (env-RBAC, OR-Check by `IncarnationCovenContexts`).

**Audit:** `incarnation.drift_checked` is written by the handler after the report is compiled, `correlation_id=apply_id`, payload `{name, scenario, apply_id, drift_summary}`.

#### `DELETE /v1/incarnations/{name}` — delete instance

Permission: `incarnation.destroy`. MCP-tool: `keeper.incarnation.destroy`. Path-param: `name`.

Demolishes instance. Operator-facing flag `allow_destroy` is mapped to internal `force` (unification force↔allow_destroy): `false` - regular destroy via teardown script `destroy` service (with tombstone period for cloud VMs, [cloud.md → Security destroy](../cloud.md)); `true` - demolition without teardown (DELETE lines directly, escape-hatch for instance without external resources, warning in audit). Asynchronous operation.

**Query:**

| Param | Type | Required | Meaning |
|---|---|---|---|
| `allow_destroy` | `bool` | yes | Mandatory confirmation flag (absent or non-boolean → `400 malformed-request`). `false` - destroy via teardown script `destroy`; if there is no script in the service snapshot `destroy` → `422 validation-failed` (there is nothing to perform teardown with, pass `true`). `true` - demolition without teardown (force). Mapped to internal `force` (status `destroying`, [`status_details.force`]). Symmetry with MCP-tool [`keeper.incarnation.destroy`](../mcp-tools/incarnations.md#keeperincarnationdestroy). |

**Response `202 Accepted`:** `{"apply_id": "<ULID>"}`. **Errors:** `400 malformed-request` (`allow_destroy` is missing/not a boolean), `404 not-found`, `409 incarnation-locked` (status does not allow destroy - `applying` / `destroying`), `422 validation-failed` (`allow_destroy=false` and no script `destroy`).

**Manifest `lifecycle.auto_destroy` ([architecture.md → Service](../../architecture.md)).** If `manifest.lifecycle.auto_destroy: false`, deletion is **always** direct (DELETE without teardown), priority over `allow_destroy` - even `allow_destroy=false` does not run a teardown script and does not run into `422` "no script `destroy`." By default (`true`, backcompat), deletion follows the usual `allow_destroy` logic. Resolved from a snapshot of the deployed service-ref.

#### `PATCH /v1/incarnations/{name}/hosts` — edit declared `spec.hosts[]`

Permission: `incarnation.update-hosts`. Path-param: `name`. **REST-only - no MCP-tool** (`manifest.go` does not contain `keeper.incarnation.hosts.update`; UI Hosts editing goes directly to REST). **Sync operation** (not async): edit declared `spec.hosts[]` is not a run, the response returns an updated incarnation, without `apply_id`.

Edits the declared list of incarnation hosts (`spec.hosts[]`, [ADR-008](../../adr/0008-coven-stable-tags.md)). `spec.hosts` — declared-input of the next run (source of truth for bootstrap-`create` and topology-resolution `soulprint.hosts[].role`), **not** state-transition: `state_history`-row is not written. Atomicity - one PG transaction (`SELECT FOR UPDATE` → status guard → batch validation of SID in the registry `souls` → `UPDATE spec`).

**Three mode semantics** above the current `spec.hosts[]`:
- `replace` - complete replacement of the list with the passed set. Empty `hosts: []` is valid - deliberate clearing of declared-spec (`422` is deliberately **not** issued for an empty set).
- `append` - insert-or-update by SID: new hosts are added, if the SID `role` matches the existing entry, it is overwritten. Empty `hosts: []` - no-op.
- `remove` — delete records with passed SIDs; `role` in payload with `remove` is ignored (only `sid` is important). Empty `hosts: []` - no-op.

**Request `IncarnationUpdateHostsRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `mode` | `enum` (`replace`/`append`/`remove`) | yes | Type of operation on `spec.hosts[]`. Unknown value → `422 validation-failed`. |
| `hosts` | `list<IncarnationSpecHost>` | yes | A set for applying the mode operation. Can be empty (see mode semantics above). |

`IncarnationSpecHost` (item):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (FQDN) | yes | Host SID; must exist in the registry `souls` (otherwise `422`). |
| `role` | `string` (kebab-case, 1..63) | optional | Declared role. Format `^[a-z][a-z0-9]*(-[a-z0-9]+)*$` or missing/empty ([ADR-008](../../adr/0008-coven-stable-tags.md) allows null for hosts outside declared-spec). Operator-asserted string, the list is not predefined (`master`/`replica` - frequent, but not exhaustive). |

```json
{
  "mode": "append",
  "hosts": [
    { "sid": "redis-prod-04.example.com", "role": "replica" },
    { "sid": "redis-prod-05.example.com" }
  ]
}
```

**Response `200 OK`:** full `IncarnationGetReply` (same form as `GET /v1/incarnations/{name}`) with `spec.hosts[]` edit already applied. `state`/`spec` are masked according to the general rule ([§ Masking state/spec in GET responses](../operator-api.md)).

**Errors:** `400 malformed-request` (broken JSON / unknown body field - decoder in strict mode `DisallowUnknownFields`), `403 forbidden`, `404 not-found` (incarnation does not exist), `409 incarnation-locked` (status `destroying` / `destroy_failed` - spec edit when demolition is meaningless; other statuses, including `applying`, are valid), `422 validation-failed` (invalid path-`name` / invalid `sid` / invalid `role` / unknown `mode` / SIDs are not in the registry `souls`), `500 internal-error`.

**RBAC:** scope selector `incScope` (env-RBAC, parity `run`/`upgrade`/`destroy` - `coven=`/`service=`/`incarnation=` by path-`name`: declared `covens ∪ {name}` + `service`). Permission `incarnation.update-hosts` narrowed from the previous `incarnation.update` (PM-decision 2026-06-02); backcompat-alias `incarnation.update` is canonicalized to `incarnation.update-hosts` on the RBAC snapshot load.

**Audit:** `incarnation.hosts_updated` (`source: api` / `mcp`, `archon = JWT.sub`, payload `{name, mode, old_hosts, new_hosts}`) - written by the handler **after** commit (payload contains old/new snapshot, available only after `UpdateHosts`); does not go through generic audit-middleware.

#### `PUT /v1/incarnations/{name}/traits` — replace incarnation trait marks

Permission: `incarnation.traits-set`. MCP-tool: `keeper.incarnation.traits-set`. Path-param: `name`. **Sync operation** (not async): editing operator-set labels is not a run, the response returns an updated incarnation, without `apply_id`.

Integrity **replaces** operator-set trait incarnation tags (`incarnation.traits` jsonb - source of truth, [ADR-060](../../adr/0060-traits.md) R1 slice a). Trait - organizational label of the owner/product/namespace of the entire instance (`owner=alice`, `product=aboba`, `namespace=dba-ns`), **separate axis next to the flat Coven** ([ADR-008](../../adr/0008-coven-stable-tags.md)): Coven - membership/targeting/RBAC, Trait - key-value attributes. Atomicity - one PG transaction (`SELECT FOR UPDATE` → `UPDATE traits`); There is no status gate (it is safe to change labels at any status). After a commit, the sync-hook **materializes** a new set into `souls.traits` of all member hosts of the incarnation (host = SID whose incarnation name is ∈ `souls.coven[]`). Best-effort projection: its failure does not crash the request (`incarnation.traits` has already been written, it will converge at the next bind/sync).

Replaces the per-soul write path `POST /v1/souls/traits` (deprecated, see [Soul → bulk traits](souls.md)). Per-soul write still works, but **is erased by the projection** `incarnation.traits` at the next sync.

**Request `IncarnationSetTraitsRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `traits` | `object` | optional | Full set of trait marks: key → value `scalar` (`string`/`number`/`boolean`) OR `list of scalars` (`["alice", "bob"]`). Key - `^[a-z][a-z0-9]*([_-][a-z0-9]+)*$` (kebab/snake-case, `_` allowed - NIM-67). **Replace semantics** - the passed set replaces the current one entirely; empty `{}` / omitted field = **clear** all labels. Nested object/array-within-array → `422`. |

```json
{
  "traits": {
    "owner": "alice",
    "owners": ["alice", "bob"],
    "namespace": "dba-ns"
  }
}
```

**Response `200 OK`:** full `IncarnationGetReply` (same form as `GET /v1/incarnations/{name}`) with replacement `traits` already applied. `state`/`spec` are masked according to the general rule ([§ Masking state/spec in GET responses](../operator-api.md)).

**Errors:** `400 malformed-request` (broken JSON / unknown body field), `403 forbidden`, `404 not-found` (incarnation does not exist), `422 validation-failed` (invalid path-`name` / invalid key / nested trait value), `500 internal-error`.

**RBAC:** scope selector is the same as `incarnation.update-hosts` (env-RBAC, `coven=`/`service=`/`incarnation=` by path-`name`: declared `covens ∪ {name}` + `service`). trait-**key** NOT a scope-dimension - there is no gate for keys.

**Audit:** `incarnation.traits_changed` (`source: api` / `mcp`, `archon = JWT.sub`, payload `{name, old_keys, new_keys}`) - written by the handler **after** the commit. Payload carries only sorted lists of trait-**KEYS** before and after; the trait-**VALUES** themselves are NOT included in audit (secret-hygiene: trait-value can carry host infrastructure data - symmetrically `soul.traits-changed`).
