# Errand — endpoints pull-ad-hoc exec outside scenario

Domain section [Operator API](../operator-api.md): endpoints `/v1/souls/{sid}/exec` + `/v1/errands*` (ad-hoc exec of a single module on Soul, [ADR-033](../../adr/0033-errand.md)). Conventions, error-format, pagination, mapping table - in the root [operator-api.md](../operator-api.md). MCP side - [mcp-tools/errands.md](../mcp-tools/errands.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (table of 4 routes) - in the root [operator-api.md → Errand (4)](../operator-api.md).

| Method/Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/souls/{sid}/exec` | `errand.run` (selector `host=<sid>`) | [`keeper.soul.errand.run`](../mcp-tools/errands.md#keepersoulerrandrun) |
| `GET /v1/errands/{errand_id}` | `errand.list` | — (REST polling) |
| `GET /v1/errands` | `errand.list` | [`keeper.errand.list`](../mcp-tools/errands.md#keepererrandlist) |
| `DELETE /v1/errands/{errand_id}` | `errand.cancel` | [`keeper.errand.cancel`](../mcp-tools/errands.md#keepererrandcancel) |

#### `POST /v1/souls/{sid}/exec` - running Errand on the host

Permission: `errand.run` (selector `host=<sid>`, [rbac.md §Errand](../rbac.md)). MCP-tool: `keeper.soul.errand.run`. Path-param: `sid` (FQDN, URL-encoded).

Pull-ad-hoc exec of a single module on a specific Soul via mTLS EventStream. Errand **NOT mutates** `incarnation.state` is a separate registry `errands`. Whitelist of modules - Soul-side defense-in-depth: hard list `core.cmd.shell` / `core.exec.run` or marker interface `ErrandReadSafe` in `sdk/module/`.

**Sync-primary flow (server-cap 30s):** `200` + `ErrandResult` if terminal received before cap; otherwise `202` + `{errand_id}` + `Location: /v1/errands/{errand_id}`, continuation in background to `timeout_seconds` (max 300s) → `ErrandStatus.TIMED_OUT`.

**Request:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `module` | `string` | yes | Module address `core.<class>.<state>` or `core.cmd.shell` / `core.exec.run` (whitelist Soul-side). |
| `input` | `object` | optional | Module Input (form depends on the module). |
| `timeout_seconds` | `int` (1..300) | optional | Full timeout. Default `30`. |
| `dry_run` | `bool` | optional | `true` → Soul calls `mod.Plan` (read-safe modules only). The target must announce the `dry_run` [Soul-capability](../../adr/0076-engine-compat-window.md); otherwise `409` before dispatch — see below. |

**Response (`ErrandResult` / `ErrandStatus`):** `status` ∈ `running` / `success` / `failed` / `timed_out` / `module_not_allowed`; `exit_code` (NULL for read-safe non-shell); `stdout`/`stderr` (masked output, cap 64 KiB) + `*_truncated` flags; `duration_ms`; `error_message` (masked reason FAILED/TIMED_OUT/MODULE_NOT_ALLOWED); `output` (structural output of read-safe modules, not available for shell/exec).

**Errors:** `404 not-found` (Soul is not connected to the cluster), `409 soul-capability-unsupported` (`dry_run` requested and the target did not announce that capability — see below), `422 validation-failed` (empty `module`, `timeout_seconds` outside [1, 300]).

### `dry_run` is gated on the target's capability

`dry_run` promises a pure read: the Soul calls `SoulModule.Plan` instead of `Apply`
([ADR-031(b)/(c)](../../adr/0031-scry-drift.md)). The Soul keeps that promise, not
Keeper — a binary predating the flag reads `ErrandRequest` by the keys it knows,
misses `dry_run`, and runs `Apply`. For the modules Errand reaches that means a
shell command executed for real, inside an operation that advertised a read.

So Keeper checks the target's announced capability set before sending, and refuses
with `409 soul-capability-unsupported` if `dry_run` is not in it
([ADR-0076(i)](../../adr/0076-engine-compat-window.md), the single-host sibling of
the roster gate on the scenario path). Fail-closed on every edge: a missing
announcement, no presence source at all, and a failed check all refuse. `detail`
says which of the two happened, because the fixes differ — upgrade the agent on
that host, or restore Redis. Only `dry_run: true` pays for the check; an ordinary
Errand is dispatched exactly as before.

#### `GET /v1/errands/{errand_id}` / `GET /v1/errands` - reading Errands

Permission: `errand.list` (read-permission covers list+get). MCP-tool: `keeper.errand.list` (list); detail - REST polling.

`GET /v1/errands` - filters `sid` / `status` / `started_after` (RFC 3339) + pagination ([§ Pagination](../operator-api.md#pagination)). `GET /v1/errands/{errand_id}` — string by ULID (form like `POST .../exec` response + `started_by_aid`, `started_at`, `finished_at`).

**Errors:** `404 not-found` (`errand_id` does not exist).

#### `DELETE /v1/errands/{errand_id}` - cancellation of in-flight Errand

Permission: `errand.cancel`. MCP-tool: `keeper.errand.cancel`. Path-param: `errand_id` (ULID).

Cancel-flow (slice E5, best-effort): `DELETE /v1/errands/{errand_id}` sends `CancelErrand` to Soul via the EventStream channel - Soul-side `errandrunner` cancels the ctx of the active Run goroutine, it returns `ErrandResult{status: CANCELLED}` in the same channel, applybus-receiver on Keeper translates the line `errands` to `status=cancelled`. The operator views the final status through `GET /v1/errands/{errand_id}` (poll).

**Response:** `204 No Content` when the signal is sent successfully. **Errors:** `409 errand-not-cancellable` (Errand is already in terminal status), `404 not-found` (`errand_id` is unknown or the target Soul is not connected).
