# Push — push-mode endpoints (agentless SSH delivery)

Domain section of the [Operator API](../operator-api.md): the `/v1/push/*` endpoints (push run of Destiny over SSH via `keeper.push`, [ADR-004](../../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)). Conventions, error format, pagination, the mapping table — in the root [operator-api.md](../operator-api.md). The full model of push mode (orchestrator, cleanup, SshProvider) — [push.md](../push.md). The MCP side — [mcp-tools/push.md](../mcp-tools/push.md).

## Endpoint sections

Mapping endpoint ↔ MCP-tool ↔ permission (a 2-route table) — in the root [operator-api.md → Push (2)](../operator-api.md#push-2--pushmd).

| Method / Path | Permission | MCP-tool |
|---|---|---|
| `POST /v1/push/apply` | `push.apply` | [`keeper.push.apply`](../mcp-tools/push.md#keeperpushapply) |
| `GET /v1/push/{apply_id}` | `push.read` | — (REST polling) |

> **NB.** The MCP tool `keeper.push.cleanup` exists in the manifest, but there is **no** REST route `/v1/push/cleanup` — the declaration was removed from the spec on 2026-06-10 as dead (the route was never mounted in `router.go`). Cleanup in push mode is done in the same SSH session via the `cleanup_stale_versions` flag of the `POST /v1/push/apply` request (see below) — there is no separate cleanup REST endpoint. The full cleanup model — [push.md → Cleanup](../push.md#cleanup-on-the-host).

### `POST /v1/push/apply` — push run of Destiny over SSH

Permission: `push.apply`. MCP-tool: `keeper.push.apply`. Full model description — [push.md](../push.md).

**Request `PushApplyRequest`:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `inventory` | `list<string>` | yes | List of SIDs (FQDN) of the target hosts. The hosts must exist in the `souls` registry with `transport: ssh`. |
| `destiny` | `string` (git-ref form `<name>@<ref>`) | yes | Reference to Destiny — `<name>` from `default_destiny_source` ([config.md](../config.md#services--default_destiny_source--default_module_source)) + git-tag/branch. |
| `input` | `object` | optional | Input for the destiny (see [destiny/input](../../input.md)). |
| `ssh_provider` | `string` | optional | Name of the SshProvider from `keeper.yml → plugins.ssh_providers[].name` ([push.md → Authentication](../push.md#ssh-authentication--pluggable-provider)). Defaults to the first registered one. |
| `cleanup_stale_versions` | `bool` | optional | Remove stale versions of the `soul` binary/modules in the same SSH session ([push.md → Cleanup](../push.md#cleanup-on-the-host)). Default `false`. |

```json
{
  "inventory": ["redis-push-01.example.com", "redis-push-02.example.com"],
  "destiny": "redis-base@v1.4.0",
  "input": { "redis_password": "vault:secret/redis/prod#password" },
  "ssh_provider": "vault-ssh"
}
```

**Response `202 Accepted`:**

```json
{ "apply_id": "01HABCDEFGHJKMNPQRSTVWXYZ" }
```

**Errors:** `403 forbidden` (RBAC may filter by `coven=` of the inventory hosts), `404 not-found` (the SID is absent from the registry), `422 validation-failed`.

### `GET /v1/push/{apply_id}` — state of a push run

Permission: `push.read`. MCP-tool: no dedicated tool yet (polling is done via REST). Full model — [push.md](../push.md). The read endpoint of the Variant C orchestrator: returns the current state of the `push_runs` record (in-flight or terminal).

**Response `200 OK` `PushApplyView`:**

| Field | Type | Meaning |
|---|---|---|
| `apply_id` | `string` (ULID) | Run identifier. |
| `inventory_sids` | `list<string>` | List of SIDs passed to `POST /v1/push/apply`. |
| `destiny_ref` | `string` | The `<name>@<ref>` of the request. |
| `ssh_provider` | `string` | Name of the SshProvider (optional); empty = registry default. |
| `input` | `object` | Destiny input (as it arrived in the request). |
| `cleanup_stale` | `bool` | The `cleanup_stale_versions` flag of the request. |
| `status` | `string` (enum) | `pending` / `running` / `success` / `partial_failed` / `failed` / `cancelled`. |
| `started_at` | `string` (RFC3339) | Time the request was accepted. |
| `finished_at` | `string` (RFC3339), opt | Finalization time (absent while `pending`/`running`). |
| `started_by_aid` | `string` (AID), opt | The Archon initiator. |
| `summary` | `object`, opt | Per-host run outcomes (see below). |

`summary` (present ONLY for terminal statuses or when `cancelled`):

```json
{
  "hosts": [
    {"sid": "redis-push-01.example.com", "status": "success"},
    {"sid": "redis-push-02.example.com", "status": "failed", "error": "run_status=failed"}
  ],
  "total": 2,
  "success_count": 1,
  "fail_count": 1
}
```

For `cancelled` (Reaper purge_orphan_push_runs) — additionally `summary.orphan_purged: true` + `summary.reason`.

**Errors:** `404 not-found` (the apply_id is absent from the `push_runs` registry), `422 validation-failed` (a malformed apply_id).
