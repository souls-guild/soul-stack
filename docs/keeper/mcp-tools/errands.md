# Errand — MCP-tools pull-ad-hoc exec outside scenario

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.soul.errand.run` / `keeper.errand.*` (ad-hoc exec of a single module on Soul, [ADR-033](../../adr/0033-errand.md)). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth for semantics is [operator-api.md → Errand](../operator-api/errands.md).

### Errand (4)

Pull-ad-hoc exec of a single module on Soul via mTLS EventStream ([ADR-033](../../adr/0033-errand.md)). 1:1 with REST `POST /v1/souls/{sid}/exec` + `GET /v1/errands{,/{errand_id}}` + `DELETE /v1/errands/{errand_id}`. Business logic (validate, INSERT errands-row, send/wait, mask+cap stdout/stderr, async-escalation, cancel-signal, audit) - in `errand.Dispatcher` / `Store`; tool - transport. Available only when the errand stack is raised; else `internal-error` ("errand orchestrator is not configured").

#### `keeper.soul.errand.run`

Running Errand on a specific Soul. Permission: `errand.run`, selector `host=<sid>` ([rbac.md §Errand](../rbac.md)). Endpoint: [`POST /v1/souls/{sid}/exec`](../operator-api/errands.md). Async: **possible** (server-cap 30s; if `async=true` is exceeded from `status=running`, then poll via `keeper.errand.get`).

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (regex SID) | yes | FQDN of target Soul. |
| `module` | `string` | yes | Module address `core.<class>.<state>` or `core.cmd.shell` / `core.exec.run` (whitelist on Soul-side). |
| `input` | `object` | optional | Module Input (form depends on the module). |
| `timeout_seconds` | `integer` (1..300) | optional | Full timeout. Default 30. |
| `dry_run` | `boolean` | optional | `true` → Soul calls `mod.Plan` (read-safe modules only). |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `errand_id` | `string` (ULID) | Launch ID. |
| `sid`, `module` | `string` | Mirror input. |
| `status` | `string` | `running` / `success` / `failed` / `timed_out` / `module_not_allowed`. |
| `async` | `boolean` | `true` → server-cap exceeded, press through `keeper.errand.get`. |
| `exit_code` | `integer` | Exit code of the verb module (NULL for read-safe non-shell). |
| `stdout`, `stderr` | `string` | Masked output (cap 64 KiB). |
| `stdout_truncated`, `stderr_truncated` | `boolean` | Exceeding cap. |
| `duration_ms` | `integer` | Duration of Errand on Soul-side. |
| `error_message` | `string` | Masked reason FAILED/TIMED_OUT/MODULE_NOT_ALLOWED. |
| `output` | `object` | Structural output read-safe modules; for shell/exec is missing. |

Errors: `not-found` (Soul is not connected to the cluster), `validation-failed` (empty sid/module, `timeout_seconds` outside [1, 300]).

#### `keeper.errand.list`

Enumeration of Errands with filtering and pagination. Permission: `errand.list`. Endpoint: [`GET /v1/errands`](../operator-api/errands.md). Async: no. Read-only.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (regex SID) | optional | Filter by target Soul. |
| `status` | `string` (status enum) | optional | Filter by status. |
| `started_after` | `string` (RFC 3339) | optional | Filter by `started_at > value`. |
| `offset` | `integer` (≥0) | optional | Pagination. |
| `limit` | `integer` (1..1000) | optional | Default 50. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `items` | `array<object>` | List of strings (form like `keeper.errand.get`). |
| `offset`, `limit`, `total` | `integer` | Pagination. |

#### `keeper.errand.get`

Errand status by ULID. Permission: `errand.list` (read-permission covers both list and get). Endpoint: [`GET /v1/errands/{errand_id}`](../operator-api/errands.md). Async: no.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `errand_id` | `string` (ULID) | yes | Launch ID. |

**Output:** see fields `keeper.soul.errand.run` (without `async`); `started_by_aid`, `started_at`, `finished_at` are added (for terminals only).

Errors: `not-found` (errand_id does not exist).

#### `keeper.errand.cancel`

Cancels in-flight Errand (slice E5). Permission: `errand.cancel`. Endpoint: [`DELETE /v1/errands/{errand_id}`](../operator-api/errands.md). Async: no. Best-effort: Keeper sends `CancelErrand` to Soul via EventStream, Soul-side `errandrunner` cancels ctx → returns `ErrandResult{CANCELLED}`. The operator views the final status through `keeper.errand.get` (poll).

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `errand_id` | `string` (ULID) | yes | Launch ID. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `errand_id` | `string` | Echo of the entrance. |
| `cancelled` | `boolean` | `true` — cancel signal sent. |

Errors: `not-found` (errand_id does not exist OR Soul is not connected), `errand-not-cancellable` (Errand is already in terminal status - there is nothing to cancel).
