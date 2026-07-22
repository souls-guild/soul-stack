# Push — MCP tools for push mode (agentless SSH delivery)

Domain section of the [MCP-tools catalog](../mcp-tools.md): the `keeper.push.*` tools (push run of Destiny over SSH via `keeper.push`, [ADR-004](../../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)). Transport, auth, the tool declaration format, the async convention, error mapping — in the root [mcp-tools.md](../mcp-tools.md). The source of truth on semantics is [operator-api/push.md](../operator-api/push.md). The full model of push mode is [push.md](../push.md).

### Push (2)

#### `keeper.push.apply`

Push run of Destiny over SSH. Permission: `push.apply`. Endpoint: [`POST /v1/push/apply`](../operator-api/push.md#post-v1pushapply--push-run-of-destiny-over-ssh). Async: **yes**.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `inventory` | `array<string>` (SID) | yes | Target hosts. |
| `destiny` | `string` (`<name>@<ref>`) | yes | Destiny + git-ref. |
| `input` | `object` | optional | Destiny input. |
| `ssh_provider` | `string` | optional | Name of the SshProvider from `keeper.yml`. |
| `cleanup_stale_versions` | `boolean` | optional | Default `false`. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `_apply_id` | `string` (ULID) | Run ID. |

#### `keeper.push.cleanup`

Cleanup of `/var/lib/soul-stack/` on the host. Permission: `push.cleanup`. **Tool without a REST route** — the REST declaration `/v1/push/cleanup` was removed from the spec on 2026-06-10 as dead (the route was never mounted in `router.go`); the MCP tool remains in the manifest as a stub wrapper. There is no paired REST endpoint; cleanup in push mode is done via the `cleanup_stale_versions` flag of the [`POST /v1/push/apply`](../operator-api/push.md#post-v1pushapply--push-run-of-destiny-over-ssh) request. Async: **yes**.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `inventory` | `array<string>` (SID) | yes | Hosts for cleanup. |
| `ssh_provider` | `string` | optional | By analogy with `push.apply`. |
| `full` | `boolean` | optional | `true` — wipe `/var/lib/soul-stack/` entirely; `false` (default) — only stale versions. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `_apply_id` | `string` (ULID) | Run ID. |
