# Soul — MCP-tools host registry

Domain section [MCP-tools directory](../mcp-tools.md): tools `keeper.soul.*` (host registration, bootstrap tokens, bulk assignment of Coven tags, one-shot command execution). Transport, auth, tool declaration format, async-convention, error mapping - in the root [mcp-tools.md](../mcp-tools.md). The source of truth in semantics is [operator-api.md → Soul](../operator-api/souls.md).

### Soul (8)

`keeper.soul.list` remains the transport of the future M2 registry (in `manifest.go` it is marked stub); the others are Implemented. The `manifest.go` directory contains eight Soul-tools (`create` / `issue-token` / `coven-assign` / `traits-assign` / `list` / `ssh-target.update` / `run-command` / `forget`). A ninth declaration, `keeper.soul.errand.run`, carries the `keeper.soul.` prefix but belongs to the Errand family and is documented in [mcp-tools/errands.md](errands.md). Read registry routes (`GET /v1/souls/{sid}`, `/soulprint`, `/history`) - **REST-only** (there are no MCP tools, covered by permission `soul.list`).

`keeper.soul.run-command` is the one Soul-tool NOT paired with a `soul.<action>` permission and the one with no REST twin: it is the non-interactive console ([ADR-0074](../../adr/0074-interactive-console-pty.md) amendment, NIM-147) and is gated by `soul.console`.

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

#### `keeper.soul.forget`

**Irreversibly** erases a host from the registry and releases what it held. Permission: **`soul.forget`**; selector `host=<sid>`. Endpoint: [`DELETE /v1/souls/{sid}`](../operator-api/souls.md). Async: no.

This is the only destructive action on the resource, and the only one an agent can take that no later call can undo — worth reading the [endpoint section](../operator-api/souls.md) before wiring it into anything that decides on its own. It is legal in **any** status, including `connected` (the call tears the stream down) and a SID this cluster has never heard from, because a dead host cannot consent to its own removal. There is **no `force` flag** and no dry-run.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (regex SID) | yes | FQDN of the host to forget. The only argument; an unknown one is a `malformed-request`. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `sid` | `string` | Mirror input. |
| `status_before` | `string` | The status the host had when it went, read under the row lock inside the deleting transaction. |
| `seeds_revoked` | `integer` | SoulSeeds that were still **live** (`active` + `superseded`) — credentials that may exist on a disk somewhere. Terminal rows (`expired`/`revoked`) are not counted. |
| `bootstraps_burned` | `integer` | Unredeemed bootstrap tokens invalidated. |
| `memberships_severed` | `integer` | Incarnation memberships that went with the host. |
| `choir_voices_removed` | `integer` | Choir Voices that went with the host. |
| `local_stream_closed` | `boolean` | This instance held the `EventStream` and closed it. `false` only means the stream lived elsewhere. |
| `broadcast` | `boolean` | The cluster-wide teardown notice went out. Never inferred from "we tried". |
| `cache_keys_purged` | `integer` | Per-SID Redis keys removed (`soul:<sid>:hb`, `:util`, `:util:win`). |
| `warnings` | `array<string>` | Resources that could **not** be released after the row was already gone. |

**Check the counts before reporting success.** The delete reaches rows nobody named: every foreign key on `souls(sid)` is `ON DELETE CASCADE`, so `memberships_severed` and `choir_voices_removed` are how much of a running fleet's topology just changed. A host that looked idle can still be the third member of an incarnation.

**`warnings[]` is not decoration.** A non-empty list means the host was erased **and something it held was not released** — the record is gone and the resource is still there. `soul:<sid>:hb` has no TTL and the Reaper keys off `souls` rows, so an unpurged key after the row is gone leaks permanently. Report the warnings; do not fold a `200` with warnings into "done".

**Errors:** `forbidden` (no `soul.forget`, or not on this host), `not-found` (SID is not in the registry), `validation-failed` (missing or malformed `sid`), `malformed-request` (unknown argument), `teardown-unavailable`, `internal-error`. `teardown-unavailable` is the retryable one and it is machine-readable, not just readable in the message: the cluster-wide teardown notice could not be sent, so **nothing was deleted** — the host is still registered and the call is safe to repeat once Redis is reachable. Erasing it while a stream on an unreachable instance stayed open would be worse than failing. `internal-error` means the opposite: a defect, with no promise about what was or was not written.

**Audit.** `soul.forgotten`, carrying the full count set — the only audit record that is the **last surviving copy** of what it describes, since every row it names is gone by the time it is written.

#### `keeper.soul.ssh-target.update`

Updates per-host SSH push-flow details (`souls.ssh_target` jsonb: `ssh_port`/`ssh_user`/`soul_path`, [ADR-032](../../adr/0032-push-orchestrator.md) amendment 2026-05-26, S7-1). Source-of-truth for `PGFallbackTargetResolver`; `keeper.yml::push.targets[]` - legacy fallback under the `push.allow_legacy_push_targets` flag. Permission: `soul.ssh-target-update`; selectors `host=<sid>` and `coven=<label>` (the host's own labels, resolved before the tool body runs - [rbac.md](../rbac.md)). Endpoint: [`PUT /v1/souls/{sid}/ssh-target`](../operator-api/souls.md). Async: no.

3-segment MCP-tool `keeper.soul.ssh-target.update` ↔ 2-segment permission `soul.ssh-target-update` (permission grammar is exactly `<resource>.<action>`; parallel `keeper.sigil.key.introduce` ↔ `sigil.key-introduce`).

**Input** (`required: sid, ssh_port, ssh_user, soul_path`):

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (FQDN, echo path-param) | yes | Host SID. |
| `ssh_port` | `integer` (1..65535) | yes | SSH port. |
| `ssh_user` | `string` (≥1) | yes | SSH user. |
| `soul_path` | `string` (abs. Unix path `^/.+`) | yes | Path to the `soul` binary on the host. |

**Output:** `{sid, ssh_target: {ssh_port, ssh_user, soul_path}}`. Errors: `not-found` (SID missing from registry `souls`).

#### `keeper.soul.run-command`

Runs ONE command line on a host and returns a machine-readable result. This is the **non-interactive console** ([ADR-0074](../../adr/0074-interactive-console-pty.md) amendment, NIM-147), not an Errand: the command is arbitrary and executes as the Soul daemon's user, typically **root**. Permission: **`soul.console`**; selector `host=<sid>` - the same right and the same scope-aware check the interactive WebSocket [`/v1/console`](../console.md) runs per `open` frame, **not `errand.run`**. No REST twin. Async: **possible** (server-cap 30s; `async=true` with `status=running` → poll `keeper.errand.get` by `errand_id`).

Use `keeper.soul.errand.run` instead whenever a named module with declared params does the job: that one can be authorized by what it is about to do, this one cannot.

The command is carried on the Errand transport with the module pinned to `core.cmd.shell` ([core.cmd](../../module/core/cmd/README.md)) - request/response is what makes the answer machine-readable (split channels, an integer exit code, the 64 KiB cap and its truncation flags), none of which a pty can express. The module is **not an argument**; an unknown argument is a `malformed-request`. The scenario-only idempotency guards of `core.cmd.shell` (`creates`/`unless`/`onlyif`) are not exposed - a one-shot agent call has no desired state to converge on.

**Input:**

| Field | Type | Required | Meaning |
|---|---|---|---|
| `sid` | `string` (regex SID) | yes | FQDN of the target Soul. |
| `command` | `string` (≥1) | yes | Shell line, executed as `sh -c` on the host. Pipes, redirects and globs work; there is no allow-list, which is why the tool needs `soul.console`. |
| `cwd` | `string` | optional | Working directory of the command. |
| `env` | `object<string,string>` | optional | Extra environment variables. |
| `timeout_seconds` | `integer` (1..300) | optional | Full timeout. Default 30. |

**Output:**

| Field | Type | Meaning |
|---|---|---|
| `errand_id` | `string` (ULID) | Run id; also the key for `keeper.errand.get` when `async=true`. |
| `sid` | `string` | Mirror input. |
| `status` | `string` | `running` / `success` / `failed` / `timed_out` / `cancelled` / `module_not_allowed`. The tool pins the module to `core.cmd.shell`, whose accepted exit codes default to `[0]` (NIM-687) — **a command that exits non-zero comes back `failed`**, with `exit_code`/`stdout`/`stderr` filled in. The tool's arguments carry no `exit_codes` knob; a caller who needs another code treated as success runs the command through an Errand ([`POST /v1/souls/{sid}/exec`](../operator-api/errands.md)), whose free-form `input` takes the module's own params. |
| `async` | `boolean` | `true` → server-cap exceeded, follow up via `keeper.errand.get`. |
| `exit_code` | `integer` | Exit status of the command. A `failed` result carries its code and both streams, not just an error message — that is the point of the `[0]` default above. Present on **every** terminal result the Soul reported, which includes the ones that never ran a process to completion: `cancelled` and `module_not_allowed` report `0` because nothing set a code, and so does a `failed` that stopped at a bad param or an executable that would not launch. Absent in three cases, none of them a real exit: while `running`; for a `timed_out` the Keeper declared on its own timer without waiting for the Soul (if the Soul's own timeout report wins the race, the field is there and reads `0`); and for a `failed` the Keeper synthesised because the Soul's result payload would not decode, whose `error_message` says so. So the code alone never separates success from "no code was ever produced": read `status` first, always. |
| `stdout`, `stderr` | `string` | Masked output (cap 64 KiB per channel). |
| `stdout_truncated`, `stderr_truncated` | `boolean` | Cap exceeded. |
| `duration_ms` | `integer` | Duration on the Soul side. |
| `error_message` | `string` | Masked reason for `failed` / `timed_out`. |

The command line is **not echoed back** in the response: the caller already has it, and a request field mirrored into a response is one more channel for a credential typed into an argument to travel through.

Errors: `forbidden` (no `soul.console`, or not on this host), `not-found` (Soul is not connected to the cluster), `validation-failed` (empty `sid`/`command`, malformed SID, `timeout_seconds` outside [1, 300]), `malformed-request` (unknown argument), `internal-error` (errand stack not configured - reported only **after** the permission check passes).

**Masking.** stdout/stderr/`error_message` are masked on this boundary as well as on the transport's ([ADR-010 §7.4](../../adr/0010-templating.md)): an agent reply is an observable channel like a log line. The bound is worth stating - on a free-form byte stream only the content layer (vault provenance, any KV mount) can fire; a credential the command prints in plaintext is indistinguishable from any other text, since recognizing it would require knowing in advance what the command was going to do.

**Audit.** Every execution writes **`console.command`** (`source: mcp`, `archon_aid`, `correlation_id` = `errand_id`, payload `{sid, status}`) on top of the transport's own `errand.invoked` / `errand.completed` chain. The command line is not in the payload, exactly as `console.opened` holds no keystrokes.
