# ADR-021. Hot-reload of config with write-back YAML

- **Context.** [`docs/requirements.md`](../requirements.md) formulates a **two-part** end-to-end requirement: (1) "hot configuration change" — a runtime change without a restart; (2) "rewrite the configuration on disk after applying it" — after an operator's in-memory mutation, the new value is written back to YAML. This is a **non-standard hot-reload**: besides the classic `file → reload-on-signal`, an API-driven mutation with write-back YAML is needed, preserving comments and key order. [`docs/keeper/config.md → ## Hot-reload`](../keeper/config.md#hot-reload) and [`docs/soul/config.md → ## Hot-reload`](../soul/config.md#hot-reload) were marked, before this ADR, as **"mechanism not architecturally fixed"** — TBD. The section [`§ Hot-reload of the plugin_runtime: block`](../keeper/config.md#hot-reload-block-plugin_runtime) ([ADR-020(d)](0020-plugin-infrastructure.md#adr-020-plugin-infrastructure-manifest-format-handshake-lifecycle)) already fixes a per-field policy for one block, but there is no general mechanism. Without a norm, the implementation of [`shared/config`](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules) (ADR-011) and the keeper-side config-server would have to guess the behavior.

- **Decision.**

  **(a) Two ways to change the config.** Both are supported in the MVP and normalized uniformly:

  | Path | Trigger | Pipeline |
  |---|---|---|
  | **File-edit** | The operator edits `keeper.yml` / `soul.yml` on the host → sends `SIGHUP` to the process. | parse → schema-validate → semantic-validate → atomic swap → audit. |
  | **API/MCP** | The operator calls an OpenAPI/MCP config-mutation endpoint → the host changes it in memory, validates, atomic swap, **write-back YAML**, audit. | mutate → schema-validate → semantic-validate → atomic swap → write-back → audit. |

  Specific API endpoints / MCP tool names for the API path are **not normalized in this ADR**, deferred until the Operator API is implemented (see Consequences).

  **(b) File-edit trigger — SIGHUP in the MVP.** A standard POSIX signal, always works, minimal overhead. **inotify / fsnotify** is post-MVP, an optional extension for auto-reload without a signal; enabled via the flag `hot_reload.enable_inotify: true` (the normative typing of the block is [`docs/keeper/config.md → hot_reload`](../keeper/config.md#hot_reload) and [`docs/soul/config.md → hot_reload`](../soul/config.md#hot_reload)). The Linux-only dependency and watch-handle overhead are the reason not to make it the default.

  **(c) Validation pipeline + atomic swap.** Atomicity is guaranteed by three validation stages **before** the swap; the swap is a separate step.

  Validation pipeline:

  1. **Parse YAML** — syntax, YAML node types. Error → audit event `config.reload_failed` with `phase: parse`, the in-memory state is not touched.
  2. **Schema-validate** — field types, regexes, enum values per the tables in [`docs/keeper/config.md`](../keeper/config.md) / [`docs/soul/config.md`](../soul/config.md). Error → `phase: schema_validate`.
  3. **Semantic-validate** — cross-field and network checks (for example, the new `vault.addr` is reachable; the new Postgres DSN responds). Error → `phase: semantic_validate`.

  After all three stages pass — an **atomic swap** via `sync/atomic.Pointer[Config]` or an equivalent. All readers after the swap see the new snapshot, readers before the swap finish reading from the old one without blocking.

  Any error at any stage → the in-memory state is unchanged, the file on disk is not modified (even for the API path — see (d) and Trade-offs), audit event `config.reload_failed` with `validation_errors[]` (error type + path + message).

  **(d) Write-back YAML — for the API/MCP path only.** The file-edit path already has a valid file on disk, there is nothing to rewrite. The API path must:

  - **Round-trip preservation** — comments, key order, anchors are preserved. The specific Go library (`gopkg.in/yaml.v3` / `goccy/go-yaml` / other) is an implementation choice of `shared/config` in Tier 2; ADR-021 fixes only the round-trip invariant.
  - **Atomic rename** — write-to-tmp in the same directory + `rename(2)` → atomic replacement. Protection against a partially written file on crash or power loss. The tmp name has no ABI guarantees, but the live file name after `rename` is the target one.
  - **Permissions** — inherited from the source file (`stat` of the source + `chmod` of the new one before rename).
  - **Write-back order:** swap → write-back. If the write-back fails (disk full, permission denied), the in-memory state **has already changed** — `config.reload_succeeded` records this. **MVP behavior:** a write-back failure is reflected by an additional `config.reload_failed` with `phase: semantic_validate` and an explicit message in `validation_errors[]` ("write-back failed: <reason>") — there is **no** separate `phase: write_back` in the MVP. **Backlog:** adding `phase: write_back` as a separate enum value — on the first real request after `shared/config` is implemented.

  **(e) Scope: reload-able vs require restart — the general principle.** Per-block tables are normalized in [`docs/keeper/config.md`](../keeper/config.md) / [`docs/soul/config.md`](../soul/config.md) for each block (as already done for `plugin_runtime:` — [`Hot-reload of the plugin_runtime: block`](../keeper/config.md#hot-reload-block-plugin_runtime)). ADR-021 fixes the general principle:

  - **Reload-able without a restart** — parameters of a specific run / operation / execution: timeouts, thresholds, policies, capabilities whitelist, retry-backoff, the Reaper's batch size, etc. Changed in memory, new operations see the new value, in-flight ones finish with the old one.
  - **Require restart** — the external surface of the process: listener address, socket paths, TLS certificate keystore files (without on-disk observation), DB connection strings, log-rotation file paths. Changing these while running without a restart breaks the invariants of open connections.

  Full per-block tables for all `keeper.yml` / `soul.yml` blocks are **deferred to a separate task** (see Consequences). In this ADR — only the principle + a brief summary in the `## Hot-reload` section of both config.md files.

  **(f) Multi-host coordination — per-host without cross-host.** Each Keeper instance in the HA cluster ([ADR-002](0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) reads its own local `keeper.yml` independently. Config files are stored per-host (not in shared storage), reload happens separately on each instance. Same for Soul: each Soul host reloads its own `soul.yml` independently.

  Cross-host coordination (cluster-wide "reload by event" via Redis pub/sub, a two-phase config commit between Keeper instances) is **deferred post-MVP**. In the MVP, the operator updates the config on each instance explicitly (via CI, an SSH rollout script, or sequential API calls). Config consistency across instances is an operational concern, not a runtime invariant.

  **(g) Audit trail — two audit-event names.** One event per reload attempt:

  - **`config.reload_succeeded`** — fields `source` (`signal|api|mcp`, a closed enum normalized in [ADR-022(b)](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention); `signal` — an operator at the keyboard on the host, `api`/`mcp` — an Archon via HTTP/MCP), `archon.aid` (for `source: api|mcp`; empty for `source: signal`), `changed_paths` (a list of YAML paths of the changed fields, e.g. `["postgres.pool.max", "reaper.interval"]`), `correlation_id`.
  - **`config.reload_failed`** — fields `source`, `archon.aid` (if applicable), `validation_errors[]` (error type + path + message), `phase` (`parse | schema_validate | semantic_validate`).

  Convention — `<area>.<action>` (lowercase, dots), like permissions ([rbac.md → Permissions format](../keeper/rbac.md#format-permissions)). The audit-event-name directory is [`docs/naming-rules.md`](../naming-rules.md). The `correlation_id` field is a ULID, the general business-correlation mechanism of the audit chain (not an OTel trace-id); its form and the general audit pipeline (storage, schema, write-path, retention) are normalized in [ADR-022](0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention).

  **(h) The `shared/config` package — shared across all three binaries.** Per [ADR-011](0011-go-layout.md#adr-011-go-code-layout-gowork-with-per-side-modules), `shared/config/` is a Go module shared by `keeper` / `soul` / `soul-lint`:

  - YAML parsing with a round-trip-aware structure (if write-back is implemented).
  - Schema validation against the tables in config.md.
  - Atomic swap helpers (a `sync/atomic.Pointer[T]` wrapper with reader helpers).
  - Write-back helpers (used only server-side by `keeper`/`soul`, not by `soul-lint`).
  - `soul-lint` uses the same parser + validator without atomic swap / write-back — static config validation without spinning up a process.

  The specific implementation and public API of `shared/config/` is a separate Tier 2 task.

  **(i) History of changes — git-blame + audit in the MVP.** For the file-edit path — the YAML file lives in the operator's git repo (CI-managed deploy), history is built from git-blame. For the API path — the audit trail in Postgres (`config.reload_succeeded` records with `archon.aid` / `changed_paths`) gives "who changed what, when."

  A separate `config_history` DB table with snapshots is **deferred post-MVP** — it does not block the MVP, and is added without breaking changes on the first real request (compliance audit / rollback by timestamp).

  **(j) Symmetry for Soul.** A Soul host in **pull mode** reads its local `soul.yml` — the hot-reload mechanism is symmetric to the Keeper side: SIGHUP / atomic swap / audit. **API-driven mutation on a Soul host is not provided in the MVP** — the Soul admin surface (a local HTTP/MCP listener or a push command `ConfigReload` over Keeper↔Soul gRPC) is deferred post-MVP, see [open Q No. 8](../architecture.md#current). Centralized `soul.yml` rollout in the MVP is via CI / Ansible / SSH delivery of a new file + `SIGHUP` — sufficient for typical config-management scenarios.

  In **push mode** ([Push mode (`keeper.push`)](../architecture.md#push-mode-keeperpush)), `soul.yml` is not used on the remote host (Soul is brought up one-shot, configuration arrives from Keeper via stdin) — hot-reload is **not applicable** in push.

- **Consequences.**
  - **Two new audit-event names** in [`docs/naming-rules.md`](../naming-rules.md) — `config.reload_succeeded`, `config.reload_failed`. They open the audit-events directory; names for the other subsystems are added as they appear.
  - **The `shared/config` package** — a separate Tier 2 implementation task (parser, schema-validator, atomic swap, write-back). Used by all three binaries.
  - **Subscribe API on reload-swap.** On top of the poll-style `Store[T].Get()`, `shared/config` provides an opt-in callback API: `Store[T].OnReload(fn ReloadCallback[T]) (unsubscribe func())`. The callback is called **only** on `Swapped=true`, in a separate goroutine per subscriber (a slow subscriber does not block the others + recover-panic), order is not guaranteed. Arguments — `old`/`new *T`, snapshot pointers, mutation is forbidden. Application: removes the "next tick / next request" latency where a component must react to a reload **immediately** (for example, `keeper/internal/reaper.Runner` — caches cfg in an `atomic.Pointer` and updates it from the callback). Does not break backward compat: existing consumers on `Get()` keep working unchanged. **RBAC is excluded from this path ([ADR-028](0028-rbac-storage.md#adr-028-rbac-storage--postgres)):** after moving RBAC storage to Postgres, reloading RBAC = a DB mutation + Redis pub/sub invalidation of the enforcer's snapshot, not `SIGHUP` / config-swap; `keeper/internal/rbac.Holder` is rebuilt from DB invalidation, not from a config-reload callback.
  - **Per-block reload-policy tables** — a separate normalization task. In this cycle only the `## Hot-reload` sections of both config.md files are supplemented (normative text + a link to ADR-021).
  - **Optional `hot_reload:` block** in `keeper.yml` / `soul.yml` (fields `enable_signal: bool default true`, `enable_inotify: bool default false`, `audit_correlation_id: bool default true`) — normalized in [`docs/keeper/config.md → hot_reload`](../keeper/config.md#hot_reload) and [`docs/soul/config.md → hot_reload`](../soul/config.md#hot_reload). All three fields require a restart (they control the hot-reload mechanism itself — a race on the signal handler / fsnotify watch). Defaults are built into `shared/config`, the block is optional.
  - **API/MCP endpoints for config mutation** (`config.set` and similar) — deferred until the Operator API (Tier 2 / post-MVP).
  - The ["End-to-End Requirements"](../architecture.md#end-to-end-requirements-and-where-they-land) section spells out "hot-reload + write-back to disk" via a link to ADR-021.

- **Trade-offs.**
  - **YAML round-trip vs full regeneration.** Round-trip requires a round-trip-aware parser (heavier than a plain `yaml.Unmarshal`), and some YAML edge cases (e.g. exotic anchors with aliases to complex structures) may lose fidelity. Full regeneration gives deterministic output, but loses the comments the operator wrote to explain "why this value is here." Round-trip was chosen — comments are operationally more valuable than perfect determinism.
  - **Per-host vs cross-host coordination.** Per-host is simpler (no distributed protocol), but allows temporary inconsistency between the cluster's Keeper instances (one has already reloaded the config, others haven't yet). We accept this: config parameters are mostly "runtime tunables," not correlation-dependent invariants; a short window of divergence is operationally safe. Cross-host (via Redis pub/sub) will be added post-MVP on the first real request.
  - **`config.reload_failed` rolls back in memory.** On the API path, after a validation error, **the file on disk is not changed** (write-back only on success). The operator sees the error in the HTTP/MCP response and in the audit; the in-memory state is unchanged. This is a deliberate cost: simpler than "write, then roll back the file" (catastrophic on power loss between write and rollback). The alternative "write-first, validate-from-disk" was rejected: it breaks atomicity.
