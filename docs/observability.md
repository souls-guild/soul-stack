# Observability: metrics and OpenTelemetry

Regulatory spec of the end-to-end observability Soul Stack layer - publishing metrics and tracing. Fixes **conventions and architecture** common to all binaries (`keeper` / `soul` / `soul-lint`), but does not list specific metrics - this catalog is filled in when the corresponding subsystems are implemented.

Solution source - [ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge). The end-to-end requirement that this lands under is "everyone publishes metrics" + "OpenTelemetry support out of the box" ([requirements.md](requirements.md), section "General Requirements"). The observability stack code lives in [`shared/obs/`](adr/0011-go-layout.md) ([ADR-011](adr/0011-go-layout.md)) - a single module for both server binaries, so that metrics and traces are collected in one stack without duplication.

> **Why cross-cutting (one file) and not per-binary.** In the table ["Cross-cutting requirements"](architecture.md#end-to-end-requirements-and-where-they-land) metrics and OTel are marked "All three binaries". Conventions (namespace, units, suffixes, resource-attrs) - general; the distinction between Keeper and Soul is made by the **metric prefix** and the `service.name` value, and not by a separate spec for each binary. Therefore, there is only one spec, like `shared/obs` - one Go module for both binaries.

## 1. Architecture: Prometheus-primary + OTel-bridge

Two telemetry channels with clearly separated roles:

| Channel | What does it carry | Model | Role |
|---|---|---|---|
| **Prometheus** | Metrics (counters / gauges / histograms). | **Pull** (scrape `/metrics`). | **Primary** metrics channel. De-facto monitoring standard; Keeper already has a Prometheus-registry in [`shared/obs`](adr/0011-go-layout.md). |
| **OpenTelemetry** | Trace (spans) + optional push metrics. | **Push** (OTLP to collector). | **Bridge**: end-to-end tracing (operator → Keeper → Soul via propagation in gRPC metadata) + optional export of metrics to an OTLP collector for installations without Prometheus-scrape. |

**Not "OTel-primary".** Metrics are exposed primarily by the Prometheus endpoint; OTLP-push metrics is an optional bridge, not a replacement for the scrape channel. Traces are always via OTel (Prometheus does not have a trace model).

### 1.1. Prometheus - what is on display

- **Endpoint `/metrics`** on server binaries (`keeper`, `soul`), on a **dedicated listener** (separate port), not on an openapi router. Exposition-format - `text/plain; version=0.0.4` (Prometheus 2.x scrape-compatible), OpenMetrics-format not yet enabled.
  - **Keeper:** `listen.metrics.addr` (usually `0.0.0.0:9090`). The endpoint was removed from the openapi router - scrape goes to a separate port without the auth-chain Operator API. keeper_http_* metrics are still collected by middleware on `/v1/*` and displayed here (the same registry). Opt. protection - HTTP Basic-auth (`metrics.auth.basic`, password from vault-ref, constant-time comparison; [keeper/config.md](keeper/config.md#metrics)).
  - **Soul:** `metrics.listen` (default loopback `127.0.0.1:9091`). Opt. protection - HTTP Basic-auth (`metrics.basic_auth`, [soul/config.md](soul/config.md#metrics)), but **password source is a file on disk** (`password_file`), and not vault-ref: Soul does not have a vault client ([ADR-012](adr/0012-keeper-soul-grpc.md)) for resolution. The constant-time check itself is the same `obs.ServeMetrics` helper as in Keeper (helper source-agnostic, §1.1 above). When basic-auth is disabled (default), the protection `/metrics` is loopback-bind.
- **Dedicated registry** ([`prometheus.NewRegistry()`](adr/0011-go-layout.md)), not global `DefaultRegisterer` - so that two instances in the same process (for example, in a test) do not share the state and do not fall into re-register. On both binaries, one registry shuffles between the instrumentation (middleware / apply-loop) and the metrics-listener's exposition-handler.
- **Helper `obs.ServeMetrics(addr, reg, auth)`** ([`shared/obs`](adr/0011-go-layout.md)) - reused by both binaries: raises a dedicated listener for `GET /metrics`, opt. basic-auth (`auth=nil` → open). Helper **source-agnostic**: caller passes the already resolved password (`BasicAuth{Username, Password}`); helper vault itself does not resolve ([ADR-011](adr/0011-go-layout.md): `shared/obs` does not support vault-client). Graceful Shutdown in the main defer chain.
- **Basic collectors** are registered explicitly: go-runtime (memory / goroutines / gc) and process (fds / cpu). Without them, scrape is useless in production: application metrics alone do not answer the question "who is leaking".
- `soul-lint` is an offline tool, `/metrics` does not have an endpoint (no long-lived process).

### 1.2. OTel - what is exported

- **Traces** - always if the `otel:` config block is enabled. End-to-end propagation trace-context via gRPC metadata of the EventStream: the archon-call span on Keeper is associated with the apply span on Soul.
- **OTLP metrics** - **optional**. It is enabled by a separate flag in the `otel:` block of the config (standard typing of the block is in the config.md of each binary, a separate task). By default, metrics go only through Prometheus-scrape; OTLP metrics - for installations without Prometheus.
- **Endpoint of the OTLP collector** is set in the `otel:` config block; locally raised via docker-compose ([dev-infra](adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster), see §1.3 below).

### 1.3. OTel in the dev stack (otel-collector + Jaeger)

Local dev stack ([`dev/docker-compose.yml`](dev/local-setup.md)) picks up a trace receiver from two services:

| Service | Image | Role | Port (host) |
|---|---|---|---|
| **otel-collector** | `otel/opentelemetry-collector-contrib` | Accepts OTLP gRPC from keeper/soul, batch → export to Jaeger + debug log. The pipeline config is [`dev/otel-collector.yaml`](dev/local-setup.md). | `4317` (OTLP gRPC) |
| **jaeger** | `jaegertracing/all-in-one` | Storage + UI traces (in-memory storage, lost upon restart). Receives from the OTLP collector inside the docker network. | `16686` (UI) |

- **keeper / soul → collector** - insecure-gRPC on `127.0.0.1:4317`. Both the keeper dev config ([`dev/keeper.dev.yml`](dev/local-setup.md): `otel.enabled: true`, `endpoint: 127.0.0.1:4317`) and the soul dev config ([`dev/soul.dev.yml`](dev/local-setup.md)) point to this endpoint. Insecure - because dev-collector is raised without TLS (`SetupOTel` is always `WithInsecure`, see [`shared/obs/otel.go`](adr/0011-go-layout.md)).
- **See traces** - Jaeger UI http://127.0.0.1:16686, service `keeper` / `soul`. Alternative without UI: `docker compose -f dev/docker-compose.yml logs -f otel-collector` (debug-exporter prints accepted spans).
- **Cont - NOT this stack.** `examples/keeper/keeper.yml` leaves the `otel:` block configurable (real collector endpoint, TLS), without dev-hardcode. all-in-one Jaeger with in-memory storage - for local debugging only.

## 2. Namespace of metrics: `soul_*` / `keeper_*`

Separate prefixes by component:

| Prefix | Who is exhibiting | Example |
|---|---|---|
| **`keeper_*`** | Keeper-side metrics. | `keeper_grpc_streams_active`, `keeper_http_requests_total` |
| **`soul_*`** | Soul-side metrics. | `soul_apply_tasks_total` |

**Differentiation by prefix, not by label.** We do not enter a common prefix with label `component="keeper|soul"` - individual prefixes are shorter, more catchy (`grep '^keeper_'`), and coincide with the Prometheus standard (per-exporter namespace). One scrape target = one component, there is no mixing of metrics in one process.

> **Soul-side metric-naming note.** The `soul_` prefix refers to the **component role** (Soul agent), not to the module namespace dictionary (`core` / `wb` / ...). These are different spaces: `soul_apply_tasks_total` - agent metric; `core.pkg.installed` — module address. There is no intersection.

### 2.1. Naming convention

Prometheus naming standard (aka the way the metrics are already named in [`shared/obs`](adr/0011-go-layout.md)):

- **`snake_case`**, ASCII, lowercase. Name = `<prefix>_<subsystem>_<name>_<unit>[_suffix]`.
- **Unit suffix** is required where there is a unit: `_seconds` (durations are **always seconds**, not milliseconds), `_bytes` (sizes are **always bytes**). Not `_ms`, not `_mb` in the metric name (Soulprint internal MB fields are data, not metrics).
- **Type suffix:**
  - `_total` — for counters (monotonically growing): `keeper_http_requests_total`.
  - `_seconds` / `_bytes` without `_total` - for gauges/histograms.
  - Histogram metric is named by the measured value + unit: `keeper_http_request_duration_seconds` (Prometheus itself adds `_bucket` / `_sum` / `_count`).
- **Gauge instantaneous** - without `_total`: `keeper_http_in_flight_requests`, `keeper_grpc_streams_active`.

### 2.2. Labels - cardinality under control

- Labels - for sections for which operator questions are actually asked (`method` / `status` / `coven` / ...), not for unique identifiers.
- **You cannot put unbounded values ​​in label**: `sid` (FQDN, thousands of hosts), `aid`, `apply_id`, `audit_id`, raw URL-path. This cardinality-blow-up is a place for such trace/log cuts, not a metric.
  - Example from the code: HTTP-`path` is taken from route-pattern (`/v1/operators/{aid}/revoke`), not from raw URL - otherwise each AID will generate a new series.
- The domain identity of the instance (`kid` / `sid`) lives in **OTel resource-attributes** (see §3), not in metrics-labels.

## 3. OTel resource-attributes

Each span/metric-export carries resource-attributes identifying the source:

| Attribute | Meaning | Destination |
|---|---|---|
| **`service.name`** | `"keeper"` \| `"soul"` | Standard OTel semconv attribute. Service name = binary name (according to the Soul Stack dictionary). |
| **`service.instance.id`** | generic instance-id | Standard OTel semconv attribute (if set by runtime). |
| **`soulstack.kid`** | [KID](naming-rules.md#identifiers) (Keeper ID) | **Custom** attribute: domain identity of the Keeper instance in the HA cluster. Only on `service.name="keeper"`. |
| **`soulstack.sid`** | [SID](naming-rules.md#identifiers) (Soul ID = FQDN) | **Custom** attribute: domain identity of the Soul agent. Only on `service.name="soul"`. |

**Why custom `soulstack.kid` / `soulstack.sid`, and not just generic `service.instance.id`.** Soul Stack domain identity is KID and SID (lease, audit, targeting are built on them). Generic `service.instance.id` does not provide a filter for "traces of a specific Soul by its FQDN" or "events of a single Keeper instance by its KID" in terms of the system dictionary. The prefix `soulstack.` is the namespace of custom project attributes, so as not to conflict with OTel semconv-reserved names.

**Wired.** `SetupOTel(ctx, OTelConfig{...})` ([`shared/obs`](adr/0011-go-layout.md)) collects resource from `service.name` + custom `ResourceAttrs`. Keeper-main sends `ServiceName: "keeper"` + `{"soulstack.kid": cfg.KID}`; Soul-main - `ServiceName: "soul"` + `{"soulstack.sid": sid}`. `SetupOTel` is called **once per process** (sets global TracerProvider + propagator); `otel.*` config block - restart-required, hot-reload does not re-read it.

> **Why `soulstack.kid` / `soulstack.sid` are resource-attrs and not metric-labels.** High cardinality (SID = FQDN, thousands of hosts): in Prometheus-labels this is a blow-up (§2.2). In OTel resource-attrs, instance identity is a standard place, and traces for one host are filtered without inflating the metrics series.

## 4. Communication with requirements and code

- **requirements.md** - "everyone publishes metrics" is closed by Prometheus-`/metrics` on server binaries; "OpenTelemetry support out of the box" - OTel traces + optional OTLP metrics bridge. Both are end-to-end, in [`shared/obs`](adr/0011-go-layout.md).
- **`shared/obs`** - a single Go module of the observability stack: `Registry` (+go/process-collectors), `MetricsHandler`, HTTP-middleware instrumentation, `ServeMetrics` (dedicated listener + basic-auth option), `SetupOTel` (trace-provider, OTLP-exporter, resource-attrs). Both server binaries wire-up it to `runDaemon` (keeper/soul `cmd`). OTLP-**metrics**-pipeline (push metrics) is another extension point, see §5.
- **A specific list of metrics** (what exactly Keeper and Soul measures) is **not standardized here**: it is filled in during the implementation of each subsystem (gRPC-stream, apply-cycle, Reaper, RBAC, ...), as well as the [audit-events directory](naming-rules.md#audit-events). Each metric follows the conventions of §2.
- **Entered in-process spans** (via the global TracerProvider from `SetupOTel`; with OTel disabled - no-op, the code does not branch). Span attributes carry domain identifiers (`sid` / `apply_id` / incarnation/scenario name), which are prohibited in metric-labels (§2.2):
  - **Keeper:** `scenario.run` and `scenario.destroy_teardown` (tracer `keeper/scenario`; the second is a child of the teardown-final destroy: archive + DELETE lines incarnation), `grpc.bootstrap` and `grpc.apply_dispatch` (tracer `keeper/grpc`, pilot), `augur.request` (tracer `keeper/augur`; resolve + fetch `AugurRequest`, attributes without secrets/query), `sigil.anchors_reload` (tracer `keeper/sigil`; runtime rotation of trust-anchor signature keys - re-build Signer + re-broadcast).
  - **Soul:** `apply.run` (tracer `soul/runtime`).
  - **Cross-process trace-propagation Keeper→Soul** - **implemented** via only-add proto-field `ApplyRequest.trace_context` (W3C traceparent, ADR-012(c) forward-compat). Keeper injects trace-context span `grpc.apply_dispatch` into `req.TraceContext` (`SendApply`); Soul retrieves it before `runner.Run`, and `apply.run` is raised as a child - through operator → Keeper → Soul (ADR-024) route. In cluster-mode, the field travels inside protobuf bytes via Redis pub/sub without separate processing. Empty `trace_context` (old Keeper) → Extract noop, `apply.run` remains the root of its own route (forward-compat degradation). EventStream metadata is not used for this - traceparent in payload.

### 4.0. Where does the Prometheus-collector subsystem live (placement rule)

The normative rule under which all subsystems are instrumented (the circulation of instrumentation is strictly according to it):

- **The subsystem collector lives next to the subsystem**, in `<bin>/internal/<subsys>/metrics.go`, with a signature logger `Register<Subsys>Metrics(reg *obs.Registry) *<Subsys>Metrics` if it is specific to one binary/subsystem. All `keeper_*` / `soul_*` subsystem metrics land here, not in `shared/obs`. Pilot - [`keeper/internal/grpc/metrics.go`](adr/0011-go-layout.md) (`RegisterGRPCMetrics`).
- **Only the end-to-end foundation** remains in `shared/obs`, needed by both binaries without reference to their internal types: registry-core (go/process collectors + exposition handler), `ServeMetrics`, `SetupOTel`, and parameterizable HTTP-middleware (`HTTPMetrics` via injected path-extractor).
- **The criterion is NOT the metric prefix, but the collector's dependencies:** pulls internal types / heavy init subsystems → subsystem-local (`<bin>/internal/<subsys>/metrics.go`); neutral and needed by both → `shared/obs`. This is the same boundary as between `shared/vault` (client-only) and `keeper/internal/vault` (server-side) according to [ADR-011](adr/0011-go-layout.md).

> **`HTTPMetrics` in `shared/obs` is a correct application of the rule, NOT an exception.** Middleware is parameterized by an injected path-extractor (`MiddlewareForPath(func(*http.Request) string)`) and does not know any keeper-types - it is neutral and needed by both binaries, so its place is in `shared/obs`. The distributor of the instrumentation **should not** "comb" it into `<bin>/internal/...`: according to the dependency criterion, it is already placed correctly.

> **`ReaperMetrics` lives in `keeper/internal/reaper` - an example of the executed rule §4.0.** Reaper - a keeper-only subsystem that draws internal types; its collector is located next to the subsystem (`RegisterReaperMetrics` in [`keeper/internal/reaper/metrics.go`](adr/0011-go-layout.md)), and not in `shared/obs`. This is an exemplary application of the "collector next to subsystem" criterion: the migration from `shared/obs` is completed, there is no longer a separate backlog task.

### 4.1. Metrics catalog (filled by subsystems)

Metrics land when the corresponding subsystem is instrumented. Name/type/labels - according to the conventions of §2. Subsystems that have not yet been instrumented are not listed in the catalog.

#### Keeper · EventStream (gRPC, ADR-012)

Reference Instrumentation-Pilot ([ADR-024](adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge)). Recorder - `RegisterGRPCMetrics(reg *obs.Registry) *GRPCMetrics` in [`keeper/internal/grpc/metrics.go`](adr/0011-go-layout.md); the descriptor is rummaged between EventStream-handler (streams/messages) and Outbound (apply-dispatch).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_grpc_streams_active` | gauge | — | Number of open EventStreams of Keeper↔Soul right now. Inc after handshake (HelloReply delivered), Dec in defer handler. Without labels: cut according to `sid` - cardinality-blow-up (§2.2); "how many Souls are on the instance" can be seen from the `instance` Prometheus-target label. |
| `keeper_grpc_messages_total` | counter | `direction` (`from_soul` / `to_soul`) | App messages of the stream in the direction: `from_soul` - received in the receive-loop (incl. handshake-Hello), `to_soul` - sent (HelloReply + send-loop). The payload type is not included in the label - this is a question for trace, not for metrics. |
| `keeper_grpc_apply_dispatch_total` | counter | `result` (`ok` / `failed`) | Attempts to send `ApplyRequest` to Soul (`Outbound.SendApply`): `ok` - enqueue/publish successful, `failed` - `ErrSoulNotConnected` / `ErrOutboundQueueFull`. Growth `failed` — Souls are unavailable / queues are full. |
| `keeper_grpc_bootstrap_total` | counter | `result` (`ok` / `failed`) | Soul onboarding attempts via unary `Bootstrap` (separate listener, server-only TLS - **not** EventStream): `ok` - seed released and Soul joined, `failed` - any non-ok outcome (token/CSR/Vault/tx). Anti-enum of onboarding: detailing the reason - in trace/log, not in label. |

**Spans (in-process).** Dispatch `ApplyRequest` is wrapped in span `grpc.apply_dispatch`; onboarding via `Bootstrap`-RPC - span `grpc.bootstrap` (both via global tracer `otel.Tracer("keeper/grpc")`, provider raised `SetupOTel`). The span attributes are `sid` / `apply_id` (domain identifiers, prohibited in metric-labels §2.2, in trace attributes - normally); carries no secrets. Span - per unit of dispatch/onboarding, not for the entire long-lived stream. When OTel is disabled, the global tracer is no-op - spans are free, the code does not branch. Cross-process trace-propagation Keeper→Soul (via gRPC metadata, §1.2) is a separate task (proto-field `traceparent` is required), not included in this slice.

#### Keeper · scenario (runner, ADR-009)

Registrar - `RegisterScenarioMetrics(reg *obs.Registry) *ScenarioMetrics` in [`keeper/internal/scenario/metrics.go`](adr/0011-go-layout.md). Instruments the Keeper-side scenario run (run-goroutine).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_scenario_runs_total` | counter | `result` (`ok` / `failed` / `locked`) | Completed scenario runs on the terminal: `ok` — state is committed; `failed` - run failed, incarnation transferred to `error_locked`; `locked` — run rejected before start (incarnation already `applying` / `error_locked`). We DO NOT put the names of incarnation/scenario in the label (cardinality-blow-up according to the number of incarnations/scenarios) - their section carries span `scenario.run`. |
| `keeper_scenario_run_duration_seconds` | histogram(`DefBuckets`) | — | Duration of the scenario run (from the start of run-goroutine to the terminal). Filled only with actually started runs; `locked` (rejected before start) does not appear in the histogram. A section based on the result is not needed - for p99 the general series is enough. |

**Spans (in-process).** The run is wrapped in span `scenario.run` via tracer `otel.Tracer("keeper/scenario")`. Attributes are domain identifiers of the run (incarnation/scenario name), prohibited in metric-labels (§2.2).

#### Keeper · RBAC (ADR-028)

Registrar - `RegisterRBACMetrics(reg *obs.Registry) *RBACMetrics` in [`keeper/internal/rbac/metrics.go`](adr/0011-go-layout.md). Instruments the RBAC subsystem: directory snapshot rebuild (`Holder.Refresh`), permission checks (`Holder.Check` is the only check point, both api-middleware and MCP go through it) and cluster invalidation (`Holder.WatchInvalidations`). The descriptor is injected by the setter `Holder.SetMetrics` into the `setupMetricsRegistry` daemon (after creating the registry, which is raised later `NewHolder` - init-order, pattern `vault.Client.SetMetrics`).

**Cardinality (invariant).** Not a single label with `aid` / `permission` / `role_name` / `resource` / `action` - only closed-enum (`kind`: `load`/`parse`; `result`: `allow`/`deny`). Who checked what and what - in the audit-log, not in the metric.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_rbac_snapshot_rebuild_duration_seconds` | histogram(`DefBuckets`) | — | The duration of one snapshot rebuild (`Holder.Refresh`: `src.Load` from the database → `NewEnforcerFromSnapshot`). It detects success and failure. |
| `keeper_rbac_snapshot_rebuild_errors_total` | counter | `kind` (`load` / `parse`) | Unsuccessful rebuilds: `load` - `src.Load` (database unavailable / SELECT error), `parse` - `NewEnforcerFromSnapshot` (invalid permission after directory versions were out of sync). The phase is known to the caller exactly - it is not guessed by the type of error. |
| `keeper_rbac_snapshot_last_success_timestamp_seconds` | gauge (Unix-time) | — | Time of the last SUCCESSFUL rebuild. **The age of a photo is calculated in PromQL as `time() - keeper_rbac_snapshot_last_success_timestamp_seconds`** - we deliberately do not create a separate `_age_seconds` metric (the gauge age would "rotate" between scrapes). |
| `keeper_rbac_snapshot_roles` | gauge | — | Number of roles in the current snapshot (per successful `Holder.Refresh`). |
| `keeper_rbac_snapshot_operators` | gauge | — | Number of agents with ≥1 role assignment in the current snapshot. AID without bindings = default-deny, does not count. |
| `keeper_rbac_checks_total` | counter | `result` (`allow` / `deny`) | Permission checks `Holder.Check`: `allow` - `err==nil`; `deny` - any non-nil error (explicit `ErrPermissionDenied` and misconfigured-call are reduced to `deny`, both outside = 403). Hot path admin-API/MCP - increment only one nil-safe `Inc`. |
| `keeper_rbac_invalidations_received_total` | counter | — | Accepted cluster-wide RBAC validations (pub/sub-signals in `Holder.WatchInvalidations`, before re-reading starts). Self-origin is filtered by source. |

#### Keeper service-registry (Service registry/keeper_settings, ADR-029)

Registrar - `RegisterRegistryMetrics(reg *obs.Registry) *RegistryMetrics` in [`keeper/internal/serviceregistry/metrics.go`](adr/0011-go-layout.md). Mirror of RBAC-Holder metrics for a snapshot of the Service registry: snapshot rebuild (`Holder.Refresh`: `ListServices` + `GetSetting` from the database) and cluster invalidation (`Holder.WatchInvalidations`). The descriptor is injected by the setter `Holder.SetMetrics` into the `setupMetricsRegistry` daemon (init-order, pattern `rbac.Holder.SetMetrics`). Getter `Holder.Resolve` - synchronous hot path consumers scenario; per-`Resolve` we do NOT deliberately create a metric/span (overhead for each resolve; snapshot metrics for rebuild are sufficient).

**Cardinality (invariant).** In the labels we DO NOT put the name of the Service / git / ref / setting value - only closed-enum `kind` (`load`/`parse`). The registry snapshot is a public directory, it's not a secret, but cardinality in terms of the number of Services in the metrics is unacceptable.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_serviceregistry_snapshot_rebuild_duration_seconds` | histogram(`DefBuckets`) | — | Duration of one rebuild of the registry snapshot (`Holder.Refresh`: `src.Load` from the database). It detects success and failure. |
| `keeper_serviceregistry_snapshot_rebuild_errors_total` | counter | `kind` (`load` / `parse`) | Unsuccessful rebuilds: `load` - `src.Load` (DB unavailable / SELECT error); `parse` is reserved for a future typed string decoder (the current `PoolSource.Load` does not have a separate parse phase). |
| `keeper_serviceregistry_snapshot_last_success_timestamp_seconds` | gauge (Unix-time) | — | Time of the last SUCCESSFUL rebuild. The snapshot age is `time() - keeper_serviceregistry_snapshot_last_success_timestamp_seconds` in PromQL (symmetry with RBAC). |
| `keeper_serviceregistry_snapshot_services` | gauge | — | Number of Services in the current snapshot (for each successful `Holder.Refresh`). |
| `keeper_serviceregistry_invalidations_received_total` | counter | — | Accepted cluster-wide registry invalidations (pub/sub-signals in `Holder.WatchInvalidations`, before rereading starts). Self-origin is filtered by source. |

#### Keeper · render (CEL+text/template-pipeline, ADR-010)

Registrar - `RegisterRenderMetrics(reg *obs.Registry) *RenderMetrics` in [`keeper/internal/render/metrics.go`](adr/0011-go-layout.md). Instruments the Keeper-side render pipeline scenario (vault-resolve → CEL-render → resolve `on`/`where` → plan assembly). The descriptor is injected with the constructor parameter `NewPipeline` (one Pipeline per keeper instance).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_render_duration_seconds` | histogram(`DefBuckets`) | — | The duration of one pass `Pipeline.Render` in seconds (vault-resolve → CEL-render → resolve `on`/`where` → plan assembly) is the heaviest Keeper-side phase of the run (the same horizon as the span `render.pipeline`). A section based on the result is not needed - for p99 the general series is sufficient; We do NOT put the names incarnation/scenario in the label (cardinality-blow-up) - their section is carried by the span. |
| `keeper_render_errors_total` | counter | — | Failed passes `Pipeline.Render` (any non-nil error: `ErrUnsupportedDSL`/vault-resolve-fail/CEL-fail/host-invariant-fail). Without a label for a reason - the detail goes to trace/log (span `render.pipeline` sets `codes.Error`); We hold counter for an alert on the rate of rendering errors. |

#### Keeper · vault (reading KV v1/v2, ADR-017)

Registrar - `RegisterVaultMetrics(reg *obs.Registry) *VaultMetrics` in [`keeper/internal/vault/metrics.go`](adr/0011-go-layout.md). Instruments the keeper-side Vault client (reading KV v1/v2, mount version is determined automatically: CEL `vault()`, `vault:`-ref, `core.vault.kv-read`, reading JWT-signing-key). The descriptor is injected by the `Client.SetMetrics` setter (init-order: registry is raised later than the client, the same pattern as `Holder.SetMetrics`).

**Cardinality (invariant, ADR-024 §2.2 + "security comes first")** We DO NOT put either the secret value or the logical KV path in the labels (the path often carries the name of the secret and a high cardinality). Section - only `mount` (closed enum, 1-2 values ​​on keeper: `secret`-default) and `kind` errors (closed enum `notfound`/`error`).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_vault_read_duration_seconds` | histogram(`DefBuckets`) | `mount` | Latency of one `Client.ReadKV` in seconds (round-trip to Vault), divided by `mount`. The hot way to resolve secrets. |
| `keeper_vault_read_errors_total` | counter | `mount`, `kind` (`notfound` / `error`) | Unsuccessful `Client.ReadKV`: `notfound` - `ErrVaultKVNotFound` (path missing/deleted, normal outcome), `error` - transport/other read error. The detail of the reason (the path itself) is in the caller's log/trace, not in the metric; You need to alert to `notfound` and `error` differently. |

#### Keeper · Augur (broker AugurRequest, ADR-025)

Registrar - `RegisterBrokerMetrics(reg *obs.Registry) *BrokerMetrics` in [`keeper/internal/augur/metrics.go`](adr/0011-go-layout.md). Instruments the Keeper-side broker `AugurRequest` from Soul (EventStream-handler `handleAugurRequest`: authorization resolve + fetch from vault/prometheus/elk). The descriptor is registered in `setupMetricsRegistry`, injected into `keepergrpc.AugurDeps.Metrics` into `setupGRPCEventStream` (pattern `grpcMetrics`/`scenarioMetrics`).

**Cardinality + security (invariant, augur.md §8).** In labels we DO NOT put `omen_name` / `query` / `sid` / `apply_id` / `request_id` / secret value - only closed-enum `source` (`vault`/`prometheus`/`elk`/`unknown`) and `decision` (`ok`/`denied`/`error`). Who requested what - in audit-log/trace.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_augur_fetch_total` | counter | `source` (`vault` / `prometheus` / `elk` / `unknown`), `decision` (`ok` / `denied` / `error`) | Processed `AugurRequest`s: `ok` - access allowed AND fetch successful; `denied` - resolver denied access; `error` - infrastructure failure (resolve/fetch has fallen, concurrency-limit). `source=unknown` — the type of Omen is not yet defined at the time of registration (Omen was not found / the semaphore is full before resolution). |
| `keeper_augur_fetch_duration_seconds` | histogram(`DefBuckets`) | `source` | Duration of processing one `AugurRequest` (resolve + fetch), according to `source` (vault-KV is cheap, prom/elk is external HTTP). The decision section is not needed - for p99 the per-source series is enough. |

**Spans (in-process).** Processing `AugurRequest` is wrapped in span `augur.request` via tracer `otel.Tracer("keeper/augur")`. Attributes - `sid` + closed-enum `source_type` / `decision`; `omen_name` / `query` / secret value is NOT placed in span (augur.md §8, ADR-024 §2.2).

#### Keeper · Oracle (reactor-router beacons, ADR-030)

Recorder - `RegisterOracleMetrics(reg *obs.Registry) *OracleMetrics` in [`keeper/internal/oracle/metrics.go`](adr/0011-go-layout.md). Instruments the Keeper-side reactor-router Oracle (EventStream-handler `handlePortentEvent`: receiving `PortentEvent` → match according to the Decree registry → setting named-scenario to work-queue, ADR-030 S2/S4). The descriptor is registered in `setupMetricsRegistry`, injected into `keepergrpc.OracleDeps.Metrics` into `setupGRPCEventStream` (pattern `augurMetrics`).

**Cardinality + security (ADR-024 §2.2).** We DO NOT put `decree` / `sid` / `apply_id` / `beacon` / payload in the labels - this is high-cardinality (tens of thousands of hosts × rules) and/or untrusted input (Soul can be compromised). All collectors are without labels (one Oracle stream). Who exactly worked - in audit-log (`oracle.fired` / `decree.circuit_tripped`) and trace.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_oracle_portents_received_total` | counter | — | Accepted `PortentEvent`s (with non-empty `beacon_name`). The denominator of others: how many beacon events actually reached the reactor. |
| `keeper_oracle_decrees_matched_total` | counter | — | Decree triggers that have passed the entire filter (subject-match + membership + where-CEL + NOT in cooldown) and reached the stage. Per-Decree is incremented (one Portent can match several). Less `portents_received` due to default-deny. |
| `keeper_oracle_scenarios_enqueued_total` | counter | — | Named-scenario, successfully delivered to work-queue (ADR-027) by Oracle reaction. Equal to the number of recorded fires; discrepancy with `decrees_matched` - enqueue failures. |
| `keeper_oracle_cooldown_blocked_total` | counter | — | Decree triggers cut off by cooldown per-(decree, subject) (loop-prevention, ADR-030(a)). Growth - frequent edge events on one rule. |
| `keeper_oracle_circuit_tripped_total` | counter | — | Auto-disable Decree circuit-breaker (ADR-030(a): N operations per window → `enabled=false` + alert). Any non-zero increase is an abnormal situation (the rule has fallen into a loop), an alert candidate. |

#### Keeper · Sigil (signing keys + rotation, ADR-026(h))

Registrar - `RegisterKeyMetrics(reg *obs.Registry) *KeyMetrics` in [`keeper/internal/sigil/keymetrics.go`](adr/0011-go-layout.md). Instruments the registry of Sigil signature trust-anchor keys (R3 multi-anchor rotation). The descriptor is registered in `setupMetricsRegistry` (nil when Sigil is disabled - collectors are not published), one instance is rummaged: gauge active keys are updated by `KeyService` (after registry mutation), re-broadcast observability is daemon from `reloadAnchors` (pub/sub-signal AND TTL-fallback-tick).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_sigil_signing_keys_active` | gauge | — | The current number of active trust-anchor signing keys (`status='active'`). Closed set (units of keys per cluster), without label cuts. |
| `keeper_sigil_anchors_rebroadcast_total` | counter | — | Re-broadcast passes of a set of anchors to connected Souls - for **every** `reloadAnchors` (pub/sub-signal `sigil:anchors-changed` + TTL-fallback tick), regardless of how many Souls the set actually reached. The signal "the node re-read the set and sent it out." |
| `keeper_sigil_anchors_last_delivered` | gauge | — | Number of Souls to which the **last** re-broadcast of the anchor set was successfully delivered (`Outbound.RebroadcastTrustAnchors` delivered). Operational signal "a new set has been sent to connected Souls, **BEFORE** Retire the old key" (Retire-invariant ADR-026(h), R3-S7). |

**Spans (in-process).** Runtime rotation (re-build Signer + update verify sets + re-broadcast) is wrapped in span `sigil.anchors_reload` via tracer `otel.Tracer("keeper/sigil")`. Attributes `active_anchors` / `rebroadcast_souls` are operational counts (not label-cardinality, not key material).

#### Keeper · Toll (cluster-wide outflow-detector, [ADR-038](adr/0038-toll.md))

Implementation - a separate slice (ADR fixed 2026-05-26, code outside this ADR). Metrics are reserved here for directory consistency.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_cluster_degraded` | gauge (0/1) | — | `1` — Toll-leader activated the `cluster:degraded` flag (rate disconnect > threshold for 60s window); `0` - normal state. Set ONLY by the leader (Redis-lease `cluster:toll:leader` guarantees an exclusive setter; non-leader instances do not publish this gauge). Not a fanout metric - closed recruitment by cluster level. |
| `keeper_toll_disconnects_total` | counter | `coven` | Non-graceful EventStream-disconnects observed by tollwatcher (post-filter graceful-shutdown / warmup-immunity). Per-coven cardinality is safe on counter - this is not the fanout flag of cluster:degraded, but the observation rate source of the Toll window. |

**Spans (in-process).** Fire-event (flag `cluster:degraded` platoon) is wrapped in span `toll.degraded_fired` via tracer `otel.Tracer("keeper/toll")` - cheap, single triggers. Attributes `rate` / `baseline_connected` / `window_seconds` are Toll window operational numbers.

#### Keeper · Conductor (performer Cadence, [ADR-048](adr/0048-conductor.md))

Registrar - `RegisterConductorMetrics(r *obs.Registry) *ConductorMetrics` in [`keeper/internal/conductor/metrics.go`](adr/0011-go-layout.md). Instruments the leader-elected executor of Cadence schedules ([conductor.md](keeper/conductor.md)): spawn tick of mature Cadence → Voyage. Collectors are registered **only in the branch of the raised Conductor** (default-ON for Redis and not `cadence_scheduler.enabled: false`) - when the Conductor is not raised, they are not published at all (cardinality-safe, parity Reaper). Without labels (one Conductor thread per instance; section by instance - Prometheus label `instance`).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_conductor_lease_held` | gauge | — | `1` if this instance runs Redis-lease `conductor:leader`, otherwise `0`. Cluster-wide invariant: `sum(keeper_conductor_lease_held) == 1` (with exactly one leader). Set ONLY when leadership changes (OnLeaseChange). Independent of `keeper_reaper_lease_held` - lease holders may vary. |
| `keeper_conductor_spawn_executions_total` | counter | — | Conductor leader spawn ticks for instance uptime. Increments by **every** tick, regardless of the presence of due schedules. "Efficiency" denominator: many ticks with zero `spawned_total` = no schedules or all `skip`/`queue`. |
| `keeper_conductor_spawned_total` | counter | — | Voyage, **actually spawned** from matured Cadence (`skip`/`queue` ticks are not counted - affected = "how many runs created"). |
| `keeper_conductor_spawn_errors_total` | counter | — | Spawn tick errors (`Spawner.Run` returned error: PG failure / resolve target). Selected from `spawn_executions_total` to alert without a histogram. Any non-zero increase is an alert candidate. |
| `keeper_conductor_spawn_duration_seconds` | histogram(`0.005…30s`) | — | Spawn tick duration (`Spawner.Run`): SELECT due + per-row insert — units or tens of ms; The top 30s catches an abnormally long tick. `_count` is the same as `spawn_executions_total`. |

#### Keeper · Tempo (per-AID rate-limiter write-API, [ADR-050](adr/0050-tempo.md#adr-050-tempo--per-aid-rate-limiting-write-api))

Registrar - `RegisterTempoMetrics(reg *obs.Registry) *TempoMetrics` in [`keeper/internal/api/tempo_metrics.go`](adr/0011-go-layout.md). Counters are incremented by middleware on each resolved/rejected request of the resolver-heavy write endpoint (config - [config.md → tempo](keeper/config.md#tempo)). Registered **unconditionally** (registry is always raised): with Tempo turned off (no Redis / `tempo.enabled: false`) counters remain at 0 - a valid signal "limiter is not active".

Label `endpoint` = boolean bucket-name(`voyage_create`); **NO AID label** - the number of operators is not limited, AID in the label would explode the cardinality of time-series. Who exactly exceeds is visible in the audit/logs for `claims.Subject`, not in the metrics.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_tempo_allowed_total` | counter | `endpoint` | Requests missed by the Tempo limiter (token taken from the per-AID bucket). `endpoint` = bucket name (`voyage_create` serves `POST /v1/voyages` + `/v1/voyages/preview`). |
| `keeper_tempo_rejected_total` | counter | `endpoint` | Requests rejected by the Tempo limiter (bucket empty → `429 tempo-exceeded` + `Retry-After`). Growth - the operator hits the API more often `rate`+`burst`. Fail-open passes during a Redis failure are NOT considered rejected (passthrough) and do NOT increase either of the two counters. |

#### Soul · apply (apply-cycle, ADR-012/ADR-015)

Registrar - `RegisterApplyMetrics(reg *obs.Registry) *ApplyMetrics` in [`soul/internal/runtime/metrics.go`](adr/0011-go-layout.md). Instruments the soul demon's apply loop.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `soul_apply_tasks_total` | counter | `result` (`ok` / `changed` / `failed`) | Completed run tasks: `changed` - the task made changes; `failed` - crashed / timeout / canceled (terminal failures result in `failed`); `ok` - no changes. We DO NOT put `apply_id` / `sid` in the label (cardinality §2.2) - their section carries the span `apply.run`. |
| `soul_apply_duration_seconds` | histogram(`0.05…300s`) | — | Duration of one run (`Run` entire). Buckets are wider `keeper_http` - apply is heavier than an HTTP request (packets / files / services), the top up to 300s catches the tail (compilation / large archive). |
| `soul_apply_task_retries_total` | counter | — | Repeated attempts `runTask` (DSL core `retry:` / `until:`, [tasks.md §9](destiny/tasks.md)), without taking into account the first attempt. Growth - unstable tasks / flaky hosts. Without labels: cut by task/apply_id - cardinality (§2.2). |
| `soul_apply_task_skipped_total` | counter | `reason` (`when` / `requisite` / `failed_run`) | Tasks skipped by gating flow-control (`mod.Apply` was not called): `when` — `when:` gave false; `requisite` - `onchanges:` / `onfail:` did not work; `failed_run` - the run has already failed, the non-`onfail` task was skipped by a fail-stop. Closed enum - Soul's gating chain. |
| `soul_apply_task_timed_out_total` | counter | — | Tasks that ended with a timeout (`TASK_STATUS_TIMED_OUT`), according to the FINAL outcome (after exhaustion of retry, not for each attempt). Isolated from the general `failed`-result `soul_apply_tasks_total`: timeout = a special "hanging" signal, a separate series is convenient for alerts. |

**Spans (in-process).** The run is wrapped in span `apply.run` via tracer `otel.Tracer("soul/runtime")`. Attributes - `apply_id` / `sid` (not allowed in metric-labels §2.2).

#### Soul · EventStream (gRPC client, ADR-002/ADR-012)

Registrar - `RegisterEventStreamMetrics(reg *obs.Registry) *EventStreamMetrics` in [`soul/internal/grpc/metrics.go`](adr/0011-go-layout.md). Instruments the Soul-side EventStream client (connection-state).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `soul_eventstream_connected` | gauge (0/1) | — | `1` — Soul↔Keeper EventStream session established (handshake completed); `0` - break / reconnect. The connection-state of the Soul agent is one-dimensional (one Keeper stream at a time) - there are no labels, the KID/session section goes to trace/log. |
| `soul_eventstream_reconnects_total` | counter | — | Attempts to reconnect after the initial connection (every `Dial` reconnect-loop). Growth is a signal of an unstable channel / unavailability of the Keeper cluster. |

#### Soul · soulprint (fact collection, ADR-018)

Registrar - `RegisterSoulprintMetrics(reg *obs.Registry) *SoulprintMetrics` in [`soul/internal/soulprint/metrics.go`](adr/0011-go-layout.md). Instruments the collection of facts about the host.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `soul_soulprint_collections_total` | counter | `result` (`ok` / `failed`) | Snapshots of facts about the host. `Collect` best-effort and error does not return (ADR-018) → now only `ok` is incremented; `failed` is reserved for future fatal collection scenarios (closed enum). We do NOT put `sid` in the label - a cut to the host in OTel resource-attrs (§3). |
| `soul_soulprint_collect_duration_seconds` | histogram(`0.001…5s`) | — | Duration of one fact snapshot (`Collect`). Collection is easy (reading `/proc`, `/etc/os-release`, `net.*`) - narrow buckets at the bottom catch the norm; top to 5s - in case of slow FQDN/DNS resolution on a problematic host. |

#### Soul · beacon (scheduler beacons, ADR-030)

Registrar - `RegisterBeaconMetrics(reg *obs.Registry) *BeaconMetrics` in [`soul/internal/beacon/metrics.go`](adr/0011-go-layout.md). Instruments per-process beacon-scheduler Soul-demon (Vigil-checks → edge-triggered `PortentEvent` into channel, ADR-030 S1/S4). The descriptor is registered on the soul-registry in `main`, injected into `beacon.SchedulerConfig.Metrics`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `soul_beacon_portents_dropped_total` | counter | — | `PortentEvent`s discarded when the channel buffer overflows (`Scheduler.emit` drop-branch): writer-loop EventStream lags or there is no active session for a long time. Drop of edge-triggered events - loss of one transition (the next State shift will raise Portent again); non-zero growth - signal "reactions are lost", alert candidate. Without labels: cut by vigil-name - high-cardinality (§2.2). |

## 5. What's on hold

- **Specific catalog of metrics** for subsystems - upon implementation (see §4).
- **OpenMetrics exposition-format** - not enabled; will turn on with an understandable triple-test from the user.
- **OTLP-push metrics** (`otel.export_metrics: bool`) - the config field is set up in both binaries ([keeper/config.md](keeper/config.md#otel) / [soul/config.md](soul/config.md)), but **OTLP-metrics-pipeline is not raised yet**: in the current slice only traces are exported via OTel metrics - only through Prometheus-scrape. Real push metrics are a separate task.
- **Sampling-config** (`otel.sampler`) - field not entered; the default is hardwired into the code (`ParentBased(AlwaysSample)` in [`shared/obs`](adr/0011-go-layout.md)). Configurable sampler - on the first real request.
- **Exemplars** (histogram-bucket ↔ trace-id) - post-MVP, does not block MVP.
