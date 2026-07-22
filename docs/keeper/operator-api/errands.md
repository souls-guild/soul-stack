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
| `dry_run` | `bool` | optional | `true` → Soul calls `mod.Plan` (read-safe modules only). |

**Response (`ErrandResult` / `ErrandStatus`):** `status` ∈ `running` / `success` / `failed` / `timed_out` / `module_not_allowed`; `exit_code` (NULL for read-safe non-shell); `stdout`/`stderr` (masked output, cap 64 KiB) + `*_truncated` flags; `duration_ms`; `error_message` (masked reason FAILED/TIMED_OUT/MODULE_NOT_ALLOWED); `output` (structural output of read-safe modules, not available for shell/exec).

**Errors:** `404 not-found` (Soul is not connected to the cluster), `422 validation-failed` (empty `module`, `timeout_seconds` outside [1, 300]).

#### `GET /v1/errands/{errand_id}` / `GET /v1/errands` - reading Errands

Permission: `errand.list` (read-permission covers list+get). MCP-tool: `keeper.errand.list` (list); detail - REST polling.

`GET /v1/errands` - filters `sid` / `status` / `started_after` (RFC 3339) + pagination ([§ Pagination](../operator-api.md#pagination)). `GET /v1/errands/{errand_id}` — string by ULID (form like `POST .../exec` response + `started_by_aid`, `started_at`, `finished_at`).

**Errors:** `404 not-found` (`errand_id` does not exist).

#### `DELETE /v1/errands/{errand_id}` - cancellation of in-flight Errand

Permission: `errand.cancel`. MCP-tool: `keeper.errand.cancel`. Path-param: `errand_id` (ULID).

Cancel-flow (slice E5, best-effort): `DELETE /v1/errands/{errand_id}` sends `CancelErrand` to Soul via the EventStream channel - Soul-side `errandrunner` cancels the ctx of the active Run goroutine, it returns `ErrandResult{status: CANCELLED}` in the same channel, applybus-receiver on Keeper translates the line `errands` to `status=cancelled`. The operator views the final status through `GET /v1/errands/{errand_id}` (poll).

**Response:** `204 No Content` when the signal is sent successfully. **Errors:** `409 errand-not-cancellable` (Errand is already in terminal status), `404 not-found` (`errand_id` is unknown or the target Soul is not connected).
