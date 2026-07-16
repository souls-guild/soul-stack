# The soul.yml format

The config of the `soul` agent on a managed host. One file per host, located by convention at `/etc/soul/soul.yml` (the exact path is at the operator's discretion, the binary accepts `--config <path>`). It applies both in the pull daemon and in the push mode — but in push most of the fields are not needed, because Keeper passes the `soul` a rendered run plan (`ApplyRequest` as protojson) through stdin with a single `soul apply` command, not through a long-lived configuration. Raw Destiny/Essence does not reach a push host.

A working example with all fields is [`examples/soul/soul.yml`](../../examples/soul/soul.yml). This document **normatively types** all fields — the parser is written against it.

**Soul has no `auth:` block.** Authentication to Keeper is only via mTLS (SoulSeed), see [`identity.md`](identity.md), [`onboarding.md`](onboarding.md). The JWT `auth:` block exists only on Keeper (for operators via OpenAPI/MCP, see [`docs/keeper/config.md → auth:`](../keeper/config.md#auth) and [ADR-014](../adr/0014-operator-identity.md)).

## Type conventions

| Entry | Meaning |
|---|---|
| `string` | an arbitrary UTF-8 string. |
| `int` | a signed 64-bit integer. |
| `bool` | `true` / `false`. |
| `duration` | a Go duration string (`5s` / `500ms` / `1h30m`). |
| `enum{a,b,c}` | a string from an explicitly enumerated set. |
| `string(host:port)` | a `host:port` string; `host` is an IP or DNS name, `port` is `1..65535`. |
| `fqdn` | a DNS name per RFC 1035/1123 hostname: a set of labels separated by dots, each label `^[a-z0-9-]{1,63}$`, not starting or ending with a hyphen. Example: `redis-01.prod.example.com`. |
| `path` | an absolute path in the host's local FS. |
| `list<T>` | as usual. |

`default: —` denotes a required field without a default. Optional fields are marked `optional`. The `enum{…}` values are lowercase ASCII, without spaces.

## Layout

```yaml
# sid: redis1.cache-test-dev.example         # OPT.: explicit SID; default = FQDN

paths:
  modules: /var/lib/soul-stack/modules        # cache of custom modules
  seed:    /var/lib/soul-stack/seed           # SoulSeed directory (versioned layout: current -> vN)

keeper:
  endpoints:                                  # see connection.md
    - host: k1.dc1.example
      event_stream_port: 9443                  # mTLS, `soul run`
      bootstrap_port: 9442                     # server-only TLS, `soul init`
  retry: {...}
  failback: {...}
  max_apply_size_mb: 8                         # recv limit of ApplyRequest, default 8 MiB
  tls:
    ca: /var/lib/soul-stack/seed/ca.crt

soulprint:
  refresh_interval: 5m

cleanup:
  modules_ttl_days: 30
  run_interval: 24h

logging:
  level: info
  format: json
  file: /var/log/soul/soul.log         # empty/omitted → stderr without rotation
  rotation: { max_size_mb: 50, max_age_days: 7, max_files: 5, compress: true }

metrics:
  enabled: true
  listen: "127.0.0.1:9091"
  basic_auth:                            # opt.; default — loopback-bind without auth
    enabled: true
    username: scrape
    password_file: /etc/soul/metrics-password   # mode 0400, one line

otel:
  enabled: true
  endpoint: "k1.dc1.example:4317"
```

## Top-level fields

### `sid:`

| Field | Type | Default | Meaning |
|---|---|---|---|
| `sid` | `fqdn` | the host FQDN | Optional. By default computed as the host FQDN (see [identity.md → Identity](identity.md)). Overriding is allowed, but is very rarely needed — for example, in environments where the FQDN is not stable enough. If the `sid` in the config mismatches the one the SoulSeed was issued for, the connection to Keeper will not pass the TLS level. |

### `paths:`

File paths on the host. If the host follows the convention layout `/var/lib/soul-stack/`, these fields may be omitted, relying on the binary defaults.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `paths.modules` | `path` | `/var/lib/soul-stack/modules` | The cache directory of custom modules. Inside — catalog slots `<ns>-<name>/{manifest.yaml, soul-mod-<name>}` ([ADR-065](../adr/0065-core-module-installed.md), see [modules.md](modules.md)). |
| `paths.seed` | `path` | `/var/lib/soul-stack/seed` | The directory with the SoulSeed in a versioned layout: the active version is the symlink `current` to the directory `vN/` with `cert.pem`/`key.pem`/`ca.pem` (normatively — [identity.md → On-disk format](identity.md)). The private key is generated locally at `soul init` and never leaves this directory. For the operator the value does not change: it is enough to specify the directory. See [onboarding.md](onboarding.md). |

In push mode `paths.seed` is not used — a push host has no SoulSeed.

### `keeper:`

The connection to the Keeper cluster: a list of endpoints, the retry policy, failback, mTLS material. **The full normative specification of the algorithm and the semantics of each field are in [connection.md](connection.md).** Here — the typing and a brief meaning.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `keeper.endpoints` | `list<KeeperEndpoint>` | — | A non-empty list of Keeper-cluster endpoints; see [connection.md → YAML config](connection.md). At least one. |
| `keeper.endpoints[].host` | `string` | — | **Required.** The host of a single Keeper instance (FQDN or IP), shared by both phases. Empty → diag `missing_required_field`. |
| `keeper.endpoints[].event_stream_port` | `int` (1..65535) | — | **Required.** The port of the EventStream listener (mTLS, the `soul run` phase). Absent → diag `event_stream_port_required`; out of range → `port_out_of_range`. |
| `keeper.endpoints[].bootstrap_port` | `int` (1..65535) | — | **Required.** The port of the Bootstrap listener (server-only TLS, the `soul init` phase). Absent → diag `bootstrap_port_required`; out of range → `port_out_of_range`. Required explicitly (ADR-012(b), "security first"): no silent fallback of bootstrap onto the event_stream port. |
| `keeper.endpoints[].priority` | `int` (≥1) | `1` | The priority (smaller = more preferred, [connection.md → Conventions](connection.md)). Orders **both** phases (the hosts match). |
| `keeper.retry.max_attempts` | `int` (≥1) | `2` | How many times in a row to try one endpoint on a retriable error (per-endpoint retries) before spraying to the next endpoint. Default `2` (not `5`): per-endpoint persistence is kept low, resilience is provided by spray + the external reconnect; see [connection.md → Parameters](connection.md) and [→ Error classification](connection.md). Omitted/`0` → `2`. |
| `keeper.retry.backoff.initial` | `duration` | `1s` | The initial interval of the exponential backoff **between full passes** over the fallback list (the external reconnect loop). **Also** reused as a **flat** (non-growing) pause between attempts to a single endpoint in the per-endpoint retry — there is no separate key for the inter-attempt pause. ⚠️ The inter-attempt pause is not picked up by hot-reload (restart-required), the reconnect backoff — is. See [connection.md → Parameters](connection.md). |
| `keeper.retry.backoff.max` | `duration` | `30s` | The upper bound of the backoff between full passes (does not affect per-endpoint retry — there the pause is flat). |
| `keeper.retry.backoff.jitter` | `bool` | `true` | Whether to apply a random jitter to the backoff. Per [connection.md → YAML config](connection.md) — a `bool` (`true`/`false`), not a duration: it is a **toggle** of "whether to use jitter", the concrete magnitude is internal to the backoff algorithm. |
| `keeper.retry.handshake_timeout` | `duration` | `10s` | The timeout for establishing the TLS+gRPC connection to a single endpoint. |
| `keeper.failback.enabled` | `bool` | `true` | Whether to try to return to a more preferred priority after switching down. |
| `keeper.failback.interval` | `duration` | `1h` | How often to launch a failback attempt. |
| `keeper.failback.spray` | `duration` | `10m` | The **amplitude** of the random jitter around `interval` (the actual moment = `interval ± spray`, uniformly). Protection against the herd effect with thousands of Souls. Per [connection.md → Parameters](connection.md) — a `duration`, not a bool: the type is consistent with "does not stretch the interval, protects against synchronous wakeups". |
| `keeper.tls.ca` | `path` | — | The path to the Keeper cluster's CA certificate. Soul uses it to validate the server side during the mTLS handshake. The client certificate and the private key itself lie in `paths.seed/`. |
| `keeper.max_apply_size_mb` | `int` (MiB, ≥1) | `8` | The ceiling on the size of a single incoming FromKeeper message, primarily `ApplyRequest` with a batch of rendered `RenderedTask` (Destiny rendering is Keeper-side, [ADR-012](../adr/0012-keeper-soul-grpc.md)). Applied as `grpc.MaxCallRecvMsgSize` in the dial of the EventStream client, replacing the small gRPC recv default (4 MiB), which is not enough for a large Destiny. `0`/omitted → the default `8`; `<1` → diag `value_out_of_range`. **Must be ≥ the Keeper-send limit** (`listen.grpc.event_stream.max_apply_size_mb` in [keeper/config.md](../keeper/config.md#listen)), otherwise Keeper will send something Soul rejects; the defaults on both sides match (8 MiB). In push mode it does not apply (the plan comes through the stdin of `soul apply`, not over gRPC). |

In push mode the `keeper:` block is ignored — the Soul host receives the rendered plan (`ApplyRequest` protojson) through the stdin of `soul apply` from Keeper. (The operator-side CLI form of the push operation — `keeper.push.apply` — is not yet normalized, a separate backlog; the host-side entry point `soul apply` is fixed — [keeper/push.md](../keeper/push.md).)

### `soulprint:`

The parameters of assembling the Soulprint (facts about the host). The typed schema MVP is [`soulprint.md`](soulprint.md), the record is [ADR-018](../adr/0018-soulprint-typed.md).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `soulprint.refresh_interval` | `duration` | `5m` | How often Soul reassembles the facts and (for pull) emits an update over the stream via `SoulprintReport`. |

The set of `SoulprintFacts` fields is normative in [`soulprint.md`](soulprint.md); it is collected by the Soul agent by a fixed table (`os.family`/`pkg_mgr`/`init_system`, etc.) and is **not declared** in the config. User-collectors (`/etc/soul/soulprint.d/*`) are deferred, see [open Q №22](../architecture.md) (requires decisions on the sandbox/rights/collector format — a separate ADR).

### `cleanup:`

Local cache cleanup on the host. Applied in pull mode (the daemon performs a periodic pass). In push mode the cleanup comes from the `keeper.push` side, not from `soul.yml`. See [modules.md → Local cleanup](modules.md).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `cleanup.modules_ttl_days` | `int` (days) | `30` | How many **days** an unused version of a module or a binary lives in `/var/lib/soul-stack/{bin,modules}/` before the daemon deletes it. The unit is fixed in the field name. |
| `cleanup.run_interval` | `duration` | `24h` | How often the daemon runs a pass over the cache. |

`modules_ttl_days` is a deliberate exception to the `duration` convention: the unit is fixed in the field name for the operator's convenience (module TTL is naturally measured in days).

### `logging:`

Soul's logs. Rotation is built-in (a cross-cutting requirement, see [requirements.md](../requirements.md)).

The behavior depends on `logging.file`:

- **`logging.file` not set** → output to `stderr` without rotation (dev mode, convenient under systemd/journald and in a container).
- **`logging.file` set** → writing to that file with rotation by size/age (a built-in rotator), the archives are placed next to it by the template `<file>-<timestamp>.<ext>`.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `logging.level` | `enum{debug,info,warn,error}` | `info` | The logging level. |
| `logging.format` | `enum{json,text}` | `json` | `json` for machine processing, `text` for a human. |
| `logging.file` | `string` (path) | — (stderr) | The path to the log file. Empty — output to `stderr` without rotation. |
| `logging.rotation.max_size_mb` | `int` (MB) | `50` | The size of a single log file before rotation. |
| `logging.rotation.max_age_days` | `int` (≥0) | `7` | How many days to keep a rotated file. Empty/`0` → the builder default (7 days); "no age limit" is not expressible in the current grammar (an MVP limitation — the field moved to a flat `int`, the distinction "0 vs not set" was removed). |
| `logging.rotation.max_files` | `int` | `5` | How many rotated files to keep. |
| `logging.rotation.compress` | `bool` | `true` | Whether to compress rotated files. In the MVP `false` does not disable compression (always `true`); disabling will appear later. |

The `logging.rotation.*` fields apply only when `logging.file` is set.

### `metrics:`

The publication of Soul's metrics (a Prometheus-compatible endpoint). A cross-cutting requirement.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `metrics.enabled` | `bool` | `true` | Enable publication. With `false` the `/metrics` listener does not come up. |
| `metrics.listen` | `string(host:port)` | `127.0.0.1:9091` | The local address of the dedicated `/metrics` listener. By default **loopback** (`127.0.0.1`), so as not to be exposed outward — the scraper goes through the node_exporter pattern or a sidecar. If `enabled: true` but `listen` is empty — the default `127.0.0.1:9091` applies. |
| `metrics.basic_auth.enabled` | `bool` | `false` | Enable HTTP Basic auth on `/metrics`. Disabled by default — the endpoint's protection is provided by the loopback bind. Needed when binding not on loopback (a scrape from another host). |
| `metrics.basic_auth.username` | `string` | — | The username for Basic auth. Required when `basic_auth.enabled: true`. |
| `metrics.basic_auth.password_file` | `string(path)` | — | The path to the file with the password (one line, the trailing newline is dropped). Required when `basic_auth.enabled: true`. The source is a **file**, not a vault-ref: Soul has no vault client ([ADR-012](../adr/0012-keeper-soul-grpc.md)). A plaintext password directly in the YAML is forbidden by the grammar — only a path. The file permissions are the operator's concern (recommendation `0400`). The existence of the file is checked at the start of `soul run` (fail-fast on absence/emptiness), not in `soul-lint`. |

On Soul, metrics are optional (unlike Keeper, where `listen.metrics` is required): not every managed host wants to open a port. The cross-cutting requirement "publication of metrics" from [`requirements.md`](../requirements.md) applies to the components as a whole — Keeper always exposes them, Soul at the operator's choice.

> **Auth on Soul's `/metrics` — via `password_file`.** Unlike Keeper (`metrics.auth.basic`, the password from a vault-ref, [keeper/config.md](../keeper/config.md#metrics)), Soul has no vault client ([ADR-012](../adr/0012-keeper-soul-grpc.md)) to resolve credentials. Therefore the password source is a file on disk (`metrics.basic_auth.password_file`); the constant-time check itself is the common helper `obs.ServeMetrics` (the same as on Keeper). With `basic_auth.enabled: false` (default) the `/metrics` protection is the loopback bind.

### `otel:`

OpenTelemetry tracing. A cross-cutting requirement.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `otel.enabled` | `bool` | `false` | Enable OTLP export. |
| `otel.endpoint` | `string(host:port)` | — | The address of the OTLP receiver (gRPC). Required when `enabled: true`. In the default delivery the receiving side is a Keeper instance with OTLP intake enabled. |
| `otel.export_metrics` | `bool` | `false` | Opt. push of metrics over OTLP in addition to the Prometheus scrape ([ADR-024 §1.2](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) / [observability.md §5](../observability.md)). **A stub for Slice 2:** the field is read, but the OTLP metrics pipeline does not yet come up — in Slice 0 only traces are exported. By default metrics go only through the Prometheus `/metrics`. |

With `enabled: true` the `endpoint` field is required; with `enabled: false` the block may be omitted entirely.

### `plugin_runtime:`

```yaml
plugin_runtime:
  socket_dir: /var/run/soul-stack/plugins
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false
```

The host-process lifecycle for plugins launched on the Soul side (`soul_module` — the `soul-mod-<name>` binaries): the handshake and shutdown timeouts, the capabilities whitelist and the resource-conflict policy, an optional TLS on the plugin socket. Applied both in the pull daemon and in push mode — the set of capabilities/conflicts is the same. The full lifecycle semantics, the handshake-string format, and the plugin-launch diagram are in [`../keeper/plugins.md → Lifecycle`](../keeper/plugins.md#lifecycle); the normative decision is [ADR-020(d/f/g/h)](../adr/0020-plugin-infrastructure.md).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `plugin_runtime.socket_dir` | `path` | `/var/run/soul-stack/plugins/` | The directory in which the Soul host creates the Unix-domain sockets of plugins (`<namespace>-<name>-<pid>.sock`). Created with mode `0700`, owned by the service user `soul` ([ADR-020(d)](../adr/0020-plugin-infrastructure.md)). The path differs for the Soul host (`soul-stack`) and the Keeper host (`soul-stack-keeper`), see [`../keeper/config.md → plugin_runtime`](../keeper/config.md#plugin_runtime). |
| `plugin_runtime.startup_timeout` | `duration` | `10s` | The time from the `fork()` of the plugin process to the appearance of the handshake string `"soul_stack":"plugin-v1"` in stdout. On exceedance — the host sends SIGTERM, then SIGKILL after `shutdown_grace` expires ([ADR-020(d)](../adr/0020-plugin-infrastructure.md), [`../keeper/plugins.md → Host behavior on handshake`](../keeper/plugins.md)). |
| `plugin_runtime.shutdown_grace` | `duration` | `10s` | The time from SIGTERM to SIGKILL. The SDK provides a signal handler, the plugin must close in-flight RPCs and terminate itself within this window ([ADR-020(d)](../adr/0020-plugin-infrastructure.md)). |
| `plugin_runtime.allowed_capabilities` | `list<enum>` | all 6 capabilities (see the YAML block above) | A closed enum (the full catalog — [`../keeper/plugins.md → required_capabilities table`](../keeper/plugins.md), [ADR-020(f)](../adr/0020-plugin-infrastructure.md)). A whitelist: `soul-lint` rejects a destiny **before launch** if the plugin's `manifest.required_capabilities` ⊄ this list. The default allows all six; the operator narrows it by the security policy. Values outside the closed enum are rejected by the parser with `unknown_capability`. |
| `plugin_runtime.conflict_policy` | `enum{warn,fail}` | `warn` | The policy for when two plugins in one run claim the same resource in `side_effects` (an identical `<resource_type>:<value>` pair). `warn` — the host writes an audit event and continues the run; `fail` — the step is marked `failed`, the reason `policy_violation` is reflected in the diagnostic channel `TaskEvent` / `RunResult` ([ADR-020(g)](../adr/0020-plugin-infrastructure.md), [`../keeper/plugins.md → Host behavior on side_effects`](../keeper/plugins.md)). |
| `plugin_runtime.enable_tls` | `bool` | `false` | Enabling mTLS on the plugin socket. In the MVP — `false`: security is provided by the `0700` file permissions on the Unix socket ([ADR-020(h)](../adr/0020-plugin-infrastructure.md)). Post-MVP — `true` uses the `server_cert` (base64-PEM) field of the handshake string, already reserved as a forward-compat reserve. Until a separate task is closed, the behavior on `true` is rejected by the parser with `tls_not_implemented`. |

#### Hot-reload of the plugin_runtime: block

The per-field policy (the general reload mechanism is normalized by [ADR-021](../adr/0021-hot-reload-config.md), see [Hot-reload](#hot-reload) below):

| Field | Reload without restarting the `soul` process | Rationale |
|---|---|---|
| `allowed_capabilities` | yes | A parameter of a specific plugin launch: the host reads the value at fork, new runs see the new value. |
| `conflict_policy` | yes | The `side_effects` conflict evaluation happens at run-assembly time, in-memory. |
| `startup_timeout` | yes | Applied to new plugin runs, does not affect already-launched ones. |
| `shutdown_grace` | yes | The same. |
| `socket_dir` | **no, requires a restart** | Changes the host's external surface (the file-system layout); already-launched plugin sockets lie in the old directory. |
| `enable_tls` | **no, requires a restart** | Changes the TLS-handshake chain of the plugin protocol. |

The rule: we change-without-restart what is used as a parameter of a specific plugin run; we require-a-restart for what changes the host's external surface. Symmetric with the Keeper side, see [`../keeper/config.md → Hot-reload of the plugin_runtime block`](../keeper/config.md).

In push mode the `plugin_runtime:` block applies the same way — the Soul host brings up a plugin from the artifact cache passed by Keeper through the same timeouts and the capabilities whitelist. (The operator-side CLI form of the push operation is not yet normalized, a separate backlog; the host-side entry point is `soul apply`.)

### `hot_reload:`

```yaml
hot_reload:
  enable_signal: true
  enable_inotify: false
  audit_correlation_id: true
```

The block controls the enablement of the hot-reload-mechanism triggers (`SIGHUP` / `inotify`) and the generation of a `correlation_id` for the audit events `config.reload_succeeded` / `config.reload_failed`. The semantics and invariants of the mechanism itself are [ADR-021](../adr/0021-hot-reload-config.md) and [Hot-reload](#hot-reload) below. The defaults are identical to the Keeper side ([`../keeper/config.md → hot_reload`](../keeper/config.md#hot_reload)). The block is entirely optional: when absent from `soul.yml` the defaults from the table apply. In push mode the block is ignored — the Soul host is one-shot, hot-reload is not applicable (see [Hot-reload](#hot-reload) below).

| Field | Type | Default | Meaning |
|---|---|---|---|
| `hot_reload.enable_signal` | `bool` | `true` | Enable the `SIGHUP` trigger of the file-edit path: the pull daemon catches the signal, re-reads `soul.yml` from disk, runs the validation pipeline, and does an atomic swap ([ADR-021(b)](../adr/0021-hot-reload-config.md)). With `false` the file-edit path is disabled. |
| `hot_reload.enable_inotify` | `bool` | `false` | Enable auto-reload via `inotify`/`fsnotify` (Linux-only) — reacting to a change of `soul.yml` without `SIGHUP`. A post-MVP optional extension ([ADR-021(b)](../adr/0021-hot-reload-config.md)): the watch-handle overhead and the Linux-only dependency are the reason not to make it a default. |
| `hot_reload.audit_correlation_id` | `bool` | `true` | Generate a `correlation_id` for the audit events `config.reload_succeeded` / `config.reload_failed` ([naming-rules.md → Audit-events](../naming-rules.md#audit-events)). |

#### Hot-reload of the hot_reload: block

All three fields **require a restart** of the `soul` process — because they control the hot-reload mechanism itself: changing `enable_signal` / `enable_inotify` without a restart = a race condition on installing/removing the signal handler or the `inotify` watch; changing `audit_correlation_id` on the fly would split one logical reload operation into two different audit-logging modes. Symmetric with the Keeper side, see [`../keeper/config.md → Hot-reload of the hot_reload block`](../keeper/config.md).

| Field | Reload without restarting the `soul` process | Rationale |
|---|---|---|
| `enable_signal` | **no, requires a restart** | Changes the binding of the signal handler to `SIGHUP`; a race on installing/removing the handler. |
| `enable_inotify` | **no, requires a restart** | Changes the registration of the `inotify` watch on the config path; a race on the handle. |
| `audit_correlation_id` | **no, requires a restart** | A parameter of the reload pipeline itself; changing it by one of the reloads would mean writing an audit event about its own mutation in two different modes. |

## Hot-reload

Config hot-reload with write-back of the changed value to disk is a cross-cutting requirement ([requirements.md](../requirements.md), [architecture.md → Cross-cutting requirements](../architecture.md)). The mechanism is normalized by **[ADR-021](../adr/0021-hot-reload-config.md)** — symmetric with the Keeper side (see [`../keeper/config.md → Hot-reload`](../keeper/config.md#hot-reload) for the full formulation); the implementation is the common package [`shared/config/`](../adr/0011-go-layout.md) (Tier 2).

**Pull mode (the daemon).** The `soul` daemon reads the local `soul.yml` — hot-reload works:

- **File-edit path** — the operator edits `soul.yml` on the host → sends `SIGHUP` to the `soul` process. Pipeline parse → schema-validate → semantic-validate → atomic swap → the audit event `config.reload_succeeded` / `config.reload_failed` ([ADR-021(c, g)](../adr/0021-hot-reload-config.md)).
- **API/MCP path** — **not provided in the MVP** for the Soul host. The Soul admin surface (a local HTTP/MCP listener) is deferred post-MVP ([open Q №8](../architecture.md)). Centralized rollout of `soul.yml` in the MVP is via CI / Ansible / SSH (the operator's choice); upon receiving a new file the operator sends `SIGHUP`. The API/MCP path on the Keeper side is normalized, see [`../keeper/config.md → Hot-reload`](../keeper/config.md#hot-reload).
- **Validation** — three stages (parse / schema-validate / semantic-validate); an error at any stage → the in-memory state is unchanged, the file is not modified, an audit `config.reload_failed` with `phase ∈ {parse, schema_validate, semantic_validate}`.
- **Scope** — the general principle: reload-able without a restart — parameters of a specific launch / run (timeouts, policies, thresholds, capabilities whitelist); require restart — the process's external surface (Keeper endpoints in `keeper.endpoints`, file TLS certificates, log-rotation paths). The summary table below covers all `soul.yml` blocks 1:1; blocks with non-trivial per-field semantics (`plugin_runtime`, `hot_reload`) are additionally normalized by separate tables in their sections.

**Summary per-block reload policy** (normative, one row per block; symmetric with the Keeper side, see [`../keeper/config.md → Hot-reload`](../keeper/config.md#hot-reload)):

| Block | Reload-able without restarting the `soul` process | Require restart | Note |
|---|---|---|---|
| `sid` | — | yes | SID is bound to the SoulSeed mTLS certificate. |
| `paths.*` (`modules`, `seed`) | — | yes | File-system layout, cache locations. |
| `keeper.endpoints` | — | yes | The open gRPC bidi stream connection. |
| `keeper.retry.*` / `keeper.failback.*` | yes | — | Parameters of the next retry / failback iteration. **Exception:** the per-endpoint inter-attempt pause (reuse of `backoff.initial`/`jitter`) is restart-required — it is read once when the EventStream client is assembled; the reconnect backoff and failback are picked up per-iteration. |
| `keeper.tls.ca` | — | yes | TLS-context init. |
| `keeper.max_apply_size_mb` | — | yes | The recv limit is set by a dial option of the open gRPC stream; the new value is picked up on the next reconnect. |
| `soulprint.refresh_interval` | yes | — | Applied to the next collection iteration. |
| `cleanup.modules_ttl_days` / `cleanup.run_interval` | yes | — | In-memory cleanup loop. |
| `logging.level` | yes | — | An in-memory variable. |
| `logging.format` / `logging.file` / `logging.rotation.*` | — | yes | Re-init the log writer. |
| `metrics.enabled` / `metrics.listen` / `metrics.basic_auth.*` | — | yes | The listener address + the basic-auth credentials (the `password_file` resolve at listener start). |
| `otel.*` | — | yes | Re-init the exporter. |
| **`plugin_runtime.*`** | per-field — see [§ Hot-reload of the plugin_runtime: block](#hot-reload-of-the-plugin_runtime-block) | | |
| **`hot_reload.*`** | per-field — see [§ Hot-reload of the hot_reload: block](#hot-reload-of-the-hot_reload-block) | | (all require restart) |
- **Per-host without coordination** — each Soul host reloads its own `soul.yml` independently; cross-host coordination via Keeper is post-MVP ([ADR-021(f)](../adr/0021-hot-reload-config.md)).
- **Audit events** — `config.reload_succeeded` / `config.reload_failed`, the catalog is in [`docs/naming-rules.md → Audit-events`](../naming-rules.md#audit-events). The Soul-side audit is written to a local journal and optionally streamed to Keeper into the common audit trail (the mechanism — backlog).
- **History** — git-blame of `soul.yml` (if the operator keeps configs in git/CI) + the local audit journal.

**Push mode (`keeper.push`).** In a push session the `soul.yml` on the remote host is **not used** (Soul comes up one-shot, the rendered run plan arrives from Keeper through the stdin of `soul apply` — see ["Push mode"](../architecture.md)). **Hot-reload is not applicable** in push: the process is short-lived, there is no file on disk.

**The optional `hot_reload:` block** in `soul.yml` (the fields `enable_signal`, `enable_inotify`, `audit_correlation_id`) — the normative typing of the fields is in [`### hot_reload:`](#hot_reload) above. When the block is absent, the defaults from there apply (built into `shared/config`), symmetric with the Keeper side.

## What does NOT live in soul.yml

- **The `auth:` block.** Soul does not authenticate via JWT — only mTLS / SoulSeed, see [identity.md](identity.md). JWT — for Keeper operators, not for Soul.
- **Destiny and Essence.** They do not reach the Soul host raw at all — Keeper renders them on its side (`vault-resolve → input-validation → CEL-render → text/template-render`, ADR-012(d)). Soul receives only the ready plan: in pull — `ApplyRequest` over the live stream, in push — `ApplyRequest` (protojson) through stdin. They are not on Soul's disk.
- **The SoulSeed token.** Used once in `soul init`: passed by the `--token` flag or via the env `SOUL_BOOTSTRAP_TOKEN` (the flag beats the env; the env form is preferable — the flag shows up in `ps`/shell history; stdin is not read). The file with the token, if the delivery channel put it on disk, is an artifact of the delivery channel, not of the config (see [onboarding.md → On the Soul side](onboarding.md)).
- **A list of modules or their sources.** The module registry lives on Keeper (the catalog `keeper.yml::plugins.soul_modules[]` + Sigil grants, [ADR-065](../adr/0065-core-module-installed.md)); Soul receives modules via the core module `core.module.installed` (pull, RPC `FetchModule`) or by a bulk transfer in a push session (see [modules.md](modules.md)).
- **The `version:` field.** The version of the Soul binary is a git ref / SHA of the artifact; it is not duplicated in `soul.yml` (see [ADR-007](../adr/0007-versioning-git-ref.md)).
- **The set of Soulprint collectors.** Fixed by the Soul binary per [ADR-018](../adr/0018-soulprint-typed.md); user-collectors are deferred ([open Q №22](../architecture.md)).

## The full example

A minimal valid `soul.yml` with all required and optional fields:

```yaml
# sid: redis1.cache-test-dev.example         # opt.: explicit SID; default = FQDN

paths:
  modules: /var/lib/soul-stack/modules
  seed:    /var/lib/soul-stack/seed

keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
    - host: k2.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
    - host: k3.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 2
    - host: k4.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 2
    - host: k1.dc2.example
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 3

  retry:
    max_attempts: 2          # per-endpoint attempts before spray; default 2
    backoff:
      initial: 1s
      max: 30s
      jitter: true
    handshake_timeout: 10s

  failback:
    enabled: true
    interval: 1h
    spray: 10m

  tls:
    ca: /var/lib/soul-stack/seed/ca.crt

soulprint:
  refresh_interval: 5m

cleanup:
  modules_ttl_days: 30
  run_interval: 24h

logging:
  level: info
  format: json
  file: /var/log/soul/soul.log         # empty/omitted → stderr without rotation
  rotation: { max_size_mb: 50, max_age_days: 7, max_files: 5, compress: true }

metrics:
  enabled: true
  listen: "127.0.0.1:9091"

otel:
  enabled: true
  endpoint: "k1.dc1.example:4317"

plugin_runtime:
  socket_dir: /var/run/soul-stack/plugins
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false

# Optional: the whole block can be omitted — defaults apply
hot_reload:
  enable_signal: true
  enable_inotify: false
  audit_correlation_id: true
```

The reference example is in the file [`examples/soul/soul.yml`](../../examples/soul/soul.yml).

## See also

- [connection.md](connection.md) — the normative specification of the `keeper:` block (the priority + failback algorithm).
- [modules.md](modules.md) — what lies in `paths.modules`, how `cleanup` works.
- [identity.md](identity.md) — what lies in `paths.seed`, why Soul has no `auth:` (mTLS / SoulSeed instead of JWT).
- [onboarding.md](onboarding.md) — how the SoulSeed appears in `paths.seed`.
- [soulprint.md](soulprint.md) — the typed Soulprint schema MVP, `refresh_interval`.
- [`docs/keeper/config.md`](../keeper/config.md) — the Keeper-side config (including `auth:` for operators).
- [`examples/soul/soul.yml`](../../examples/soul/soul.yml) — a working example.
- [architecture.md → ADR-018](../adr/0018-soulprint-typed.md) — the typed Soulprint MVP.
- [architecture.md → Cross-cutting requirements](../architecture.md) — why `logging.rotation`, `metrics`, `otel` are required.
