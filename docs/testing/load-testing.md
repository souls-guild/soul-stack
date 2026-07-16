# Soul Stack Load Testing Plan

Regulatory **plan** (what and how we load, what we look at, with what and in what order). Tool `soul-legion` built; **F0 + F1 were run on a live bench and MEASURED up to N=25000** - actual numbers in section §8 "Measured results". Full ramp up to 100k (F2) remains calculated; true 50k+ on one host rests on the client harness limit (ephemeral ports) → a distributed generator is needed.

Load appears **as a separate level next to L0–L4** ([testing/README.md → Levels](README.md)). It differs from L3a/L3b/L3c in purpose: they check **correctness** (contract, apply realism, HA cases), and load checks **throughput and cliff point** at a scale that functional levels do not create (thousands to tens of thousands of streams, hundreds of simultaneous runs).

## 1. Purpose

1. **Validate calculated sizing numbers.** Table [scaling.md → Sizing for 100k VM](../operations/scaling.md) - orders of magnitude, not measurements; note in the same place: "real sizing - based on load tests with real workload." The plan closes this gap. **Status:** F0 + F1 MEASURED up to **N=25000 connections** (fleet-Voyage up to 10000 hosts - see §8); the full 100k remains **calculated** until F2.
2. **Find cliff along each load axis** - the scale level at which latency goes into the sky / failures appear / reconnect-storm begins. Before the cliff there is a working area, behind it there is degradation.
3. **Confirm or refute known bottlenecks** from [scaling.md → Growth bottlenecks](../operations/scaling.md): PG primary CPU on `apply_runs` claim, PG IO on `audit_log` ([known-limitations.md → Audit-scaling](../known-limitations.md)), Redis CPU on SID-lease, OTel-collector drop.
4. **Empirically show architectural gaps**, known as PLANNED/backlog: lack of Shepherd (new instance is idle after scale-out - [scaling.md → Shepherd](../operations/scaling.md)) and cliff audit-INSERT before partitioning.

**Beta profile vs plan.** For a closed small beta (a few operators, a fleet of up to hundreds of hosts - [known-limitations.md](../known-limitations.md)) the scale axis of this plan is **not required**: F0 is enough as sanity-validation of the calculated numbers. Full ramp up to 100k - post-beta backlog (see §6, F2).

## 2. What we are testing - two load axes + run

### Axis A - Souls side (streams)

Connecting **1k / 5k / 10k / 25k / 50k / 100k** stub agents, each holding a long-lived `EventStream` (gRPC bidi over mTLS, stream initiated by Soul - [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). Emulated: `Hello`-handshake → stream hold → periodic heartbeat (gRPC keepalive + app message update `last_seen_at`, [ADR-012](../adr/0012-keeper-soul-grpc.md)) → `SoulprintReport` → `RunResult` to `ApplyRequest`.

Axis question: **will Keeper / PG / Redis support a fleet of N connected streams** - by RAM/Keeper goroutines, number of active streams, SID-lease/presence-load on Redis, `last_seen_at`-flush on PG.

### B Axis - API Side (`/v1` handles)

Load on operator handles `/v1` (**~118 HTTP operations on ~82 path keys** in [openapi.yaml](../keeper/openapi.yaml); domains: `souls` / `incarnations` / `voyages` / `cadences` / `heralds`+`tidings` / `errands` / `push`+`push-providers`+`push-runs` / `oracle`(`decrees`/`vigils`) / `synods` / `roles`+`permissions` / `operators`+`me` / `services` / `audit`+`event-types` / `sigil` / `augur` / `plugins`+`modules`).

Key invariant of the B axis: **the cost of many handles depends on the size of the fleet.** Therefore, the B axis runs **on top of a background of N connected stub-souls** (A axis output), and not on an empty Keeper. Examples of float-dependent handles:

- `GET /v1/souls` with filter `coven`/`status`/`transport` + pagination - the cost increases with the number of entries in the registry and the presence resolve (batch-EXISTS SID-lease in Redis - [scaling.md → Target scale](../operations/scaling.md)).
- `POST /v1/voyages` — resolve roster (`on:` + `where:` for fleet): on a large roster — massive SID-lease check in Redis ([scaling.md → Bottlenecks](../operations/scaling.md), line "Redis CPU on SID-lease check"). Under Tempo-rate-limit ([config.md → tempo](../keeper/config.md#tempo); `voyage_create` bucket - [observability.md → Tempo](../observability.md#keeper--tempo-per-aid-rate-limiter-write-api-adr-050)) - separately check whether the limiter itself is cutting the load profile.
- `GET /v1/audit` - Reading the growing `audit_log` under the background run record.

### C-Axis - Run Load (Voyage / Cadence for Large Fleet)

The heaviest profile: **M incarnations × N stub hosts**, run through Voyage (one-time) and Cadence (on schedule, spawns Voyage - [ADR-046](../adr/0046-cadence.md), [conductor.md](../keeper/conductor.md)). Touches the whole hot way of Keeper:

```
render (CEL+text/template, Keeper-side) → apply_runs claim (Acolyte SELECT … FOR UPDATE SKIP LOCKED)
→ dispatch (SendApply to stream) → RunResult → state-commit (PG transaction) → audit-INSERT
```

This is the only axis that loads the Acolyte pool ([scaling.md → Acolyte pool](../operations/scaling.md)), render pipeline and audit-write at the same time. Stub responds `RunResult` instantly (does not actually apply), so the C axis measures the **Keeper-side throughput of the orchestration**, not the apply realism (realism is L3b).

## 3. Harness design - three components

Harness is built **on the foundation of the existing soulstub** ([`tests/e2e/internal/soulstub/soulstub.go`](../../tests/e2e/internal/soulstub/soulstub.go)): real gRPC bidi-mTLS-stream, goroutine-to-stream (`recvLoop`), `Hello`/`ApplyRequest`→`RunResult`/`ErrandRequest`→`ErrandResult`/`PortentEvent`. Now it is under `//go:build e2e` and lives in `internal/` of the E2E package - to load it, you need to move it to the reused load tool.

### Component 1 - `soul-legion` (mass of streams)

Tool name is **`soul-legion`** ([naming-rules.md → Soul Legion](../naming-rules.md#soul-legion); metaphor "legion" = many souls). Test-only artifact: package/binary `soul-legion` in directory `tests/load/` (next to `tests/e2e/`/`tests/e2e-live/`/`tests/e2e-k8s/`), **NOT** shipping binary ([ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) captures only `keeper`/`soul`/`soul-lint`).

- **Foundation** - `soulstub`: move it out of `//go:build e2e` into reusable code on which `soul-legion` stands.
- **Emulation contract (same as soulstub):** `Hello`/`EventStream`/heartbeat/`SoulprintReport`/`RunResult`. `soul-legion` **DOES NOT parse Destiny and DOES NOT apply** - this is deliberate: the A/C axis measures the load **on the Keeper**, and not the realism of apply on the host (realism - L3b, real-soul-in-container). Otherwise, the load host itself will become a bottleneck and the measurements will be about it, and not about Keeper.
- **Topology by scale:**
  - **1k–10k** — single-process, goroutine-to-stream (direct extension of the current soulstub model). Goal F0.
  - **50k–100k** — distributed (several load hosts, each holding a share of streams): one process will not hold 100k streams via FD/RAM. Goal F2 (backlog).
- **Each stub = unique SID + mTLS-leaf under that SID** (like a real Soul; soulstub already accepts `cert/key/caBundle` on `New`). Generating a mass of leaves for load is a separate subtask of the harness (batch-issue from dev-CA, not Vault-per-leaf on the hot generation path).

**How ​​to launch** (on a raised dev stand) - with one command `make stress` (alias `load-test`); the load profile is specified by ENV variables (`COUNT`/`RAMP`/`API`/`VOYAGE`/…). The target itself collects the binary, mints admin-JWT for the B/C axes (`make dev-jwt` mechanism) and clears the legion from the registry at the output. List of variables and examples - [`tests/load/README.md`](../../tests/load/README.md).

### Component 2 - API loader (B axis)

- **Tool:** k6 / vegeta (external scripted HTTP load, JWT-auth) **or** custom Go loader. The fork is at the implementation stage; for fleet-dependent handles with the roster resolve (`POST /v1/voyages`) custom Go is more convenient (precise control over the request body and the distribution of roster sizes).
- **Profile by domain:** separate set for read-heavy (`GET /v1/souls`, `GET /v1/audit`, `GET /v1/incarnations`) and write-heavy (`POST /v1/voyages`, `POST /v1/cadences`). Each handle has its own target RPS and distribution of parameters (filter size, roster size, pagination depth).
- **On top of the A-axis background:** The API loader starts when N stub-souls are already raised by component 1 - otherwise the float-dependent handles are measured on an empty register and give irrelevant numbers.

### Component 3 - Run Loader (C Axis)

- **M incarnations × N stub-hosts**, spawn Voyage (one-time, `POST /v1/voyages`) and Cadence (schedule, spawns Voyage Conductor-leader - [conductor.md](../keeper/conductor.md)).
- **Uses the stub generator of component 1** as a pool of hosts (they respond to `RunResult`).
- **Profile control:** number of simultaneous runs, roster size per run, Cadence spawn frequency, overlap mode (`skip`/`queue`/`parallel` - [ADR-046](../adr/0046-cadence.md)). Under load, `parallel` (wave overlay) is especially interesting - the stress of the Acolyte pool and render pipeline.

## 4. What to look for - metrics and observation gaps

Basis - **existing** `keeper_*`/`soul_*` metrics from [observability.md → Metrics catalog](../observability.md). We do not introduce new metrics for the sake of the load test (this would be considered a propose-and-wait); We measure by what is already instrumented, and 4 spaces are outside.

### 4.1. Metrics (available in code)

| What we measure | Metric | Axis |
|---|---|---|
| Active EventStreams | `keeper_grpc_streams_active` (gauge) | A |
| Stream App messages by direction | `keeper_grpc_messages_total{direction}` | A |
| Dispatch `ApplyRequest` (ok/failed) | `keeper_grpc_apply_dispatch_total{result}` | C |
| Onboarding rate via `Bootstrap` | `keeper_grpc_bootstrap_total{result}` | A (ramp-up) |
| Latency of HTTP handles p50/p99 | `keeper_http_request_duration_seconds` (histogram, route-pattern path) | B |
| In-flight HTTP | `keeper_http_in_flight_requests` (gauge) | B |
| Render latency (the heaviest Keeper phase) | `keeper_render_duration_seconds` p50/p99 + `keeper_render_errors_total` | C |
| Run duration/outcome scenario | `keeper_scenario_run_duration_seconds` + `keeper_scenario_runs_total{result}` | C |
| Vault-resolve secrets on render | `keeper_vault_read_duration_seconds{mount}` + `_errors_total` | C |
| Conductor-spawn Cadence | `keeper_conductor_spawn_duration_seconds` + `_spawned_total` + `_spawn_errors_total` | C |
| Rate-limiter write-API | `keeper_tempo_allowed_total` / `keeper_tempo_rejected_total{endpoint}` | B |
| RBAC Checks (API Hot Path) | `keeper_rbac_checks_total{result}` | B |
| Soul-side apply-loop (stub will not be filled; for real-soul-variant) | `soul_apply_*` | C |
| Reaper-leader (background under load) | `keeper_reaper_lease_held` | A/C |

### 4.2. Observational gaps - NO metrics, measure from outside (in the 1st phase)

These 4 values are critical for cliff analysis, but there are no Prometheus-collectors for them - on F0/F1 we remove them using external means (CLI/exporter), without waiting for instrumentation:

| Space | Why is important | How to measure outside |
|---|---|---|
| **Redis SID-lease / presence rate** | The A/B axis rests here (presence-resolve `GET /v1/souls`, roster `POST /v1/voyages`) | `redis-cli INFO commandstats` / `redis-cli --stat`, latency on `EXISTS`-batch; Redis CPU |
| **PG `apply_runs` claim latency** | Direct bottleneck([scaling.md](../operations/scaling.md)); `SELECT … FOR UPDATE SKIP LOCKED` on primary | `pg_stat_statements` by claim request; lock-wait |
| **PG `audit_log` INSERT-rate** | Known deferred cliff ([ADR-022](../adr/0022-audit-pipeline.md), [known-limitations.md → Audit-scaling](../known-limitations.md)): before partitioning it will hit INSERT-rate/size | `pg_stat_statements` INSERT-rate, table size growth, IO-wait |
| **Conclave live-count** | Coordination HA / refuse-guard / future Shepherd; collector `keeper_conclave_*` missing ([scaling.md → Conclave](../operations/scaling.md)) | `redis-cli KEYS 'keeper:instance:*'` + TTL |

Additionally externally: **PG connection pool** (wait-time / saturation - bottleneck under parallel Acolyte+API+claim), **RAM/Keeper goroutines** (`/metrics` go-runtime collectors + `pprof`), **OTel-collector dropped spans** (collector logs).

### 4.3. What will show architectural gaps

- **Shepherd-rebalance (NOT implemented).** Scale-out test under A-axis load: add a Keeper instance with N streams running → new instance **idle** (`keeper_grpc_streams_active` on it ≈0 to natural churn - [scaling.md → Shepherd](../operations/scaling.md)). The test will record this quantitatively (time before rebalancing / its absence).
- **Leader-election** (Reaper / Conductor / Toll) under load and with kill-leader: exactly one holder lease (`sum(keeper_reaper_lease_held)==1`, `sum(keeper_conductor_lease_held)==1`).

## 5. Cliff criteria

**Ramp 1k → 100k on A-axis** (and proportional to M×N on C-axis). At **each step** we fix the cut:

- `keeper_http_request_duration_seconds` p99 by key handles (B axis);
- `keeper_grpc_streams_active` (actual held vs target);
- Redis CPU + SID-lease rate (§4.2);
- PG CPU + `apply_runs` claim latency + audit-INSERT-rate (§4.2);
- PG connection-pool wait;
- RAM / number of Keeper goroutines.

**Cliff = stage** at which any of:

- **p99 goes into the sky** - the latency of the handle/render/dispatch increases abruptly (not linearly);
- **failed-rate is growing** - `keeper_grpc_apply_dispatch_total{result=failed}` / `keeper_scenario_runs_total{result=failed}` / HTTP 5xx appear under the standard profile;
- **reconnect-storm** — `soul_eventstream_reconnects_total` avalanche (for the real-soul version) or a massive drop `keeper_grpc_streams_active` with immediate recovery (streams are broken and reconnected in a circle).

Fixed cliff on each axis - **measured boundary**, replacing the calculated numbers in [scaling.md](../operations/scaling.md). The step up to the cliff is a supported zone for this infra-configuration.

## 6. Phasing

| Phase | Volume | Scale | Deadline | Infra |
|---|---|---|---|---|
| **F0** ✅ | soulstub stem → `soul-legion` (component 1); ramp single-process; sanity-validation of calculated numbers | 1k–10k | ~1–2 days | local dev stack (PG/Redis/Vault via docker-compose) |
| **F1** ✅ | Axis B (API loader) + axis C (run loader) on top of a background of N stub-souls; removing 4 observation spaces outside | up to 25k background + API/run | ~2–3 days | local / single dedicated host (24 vCPU/30 GiB) |
| **F2** | Distributed generator; full ramp up to cliff | 50k–100k | ≥1 week | **prod-grade infra + budget** (several load hosts, dedicated PG/Redis-cluster) |

(F0 and F1 were run on a live bench on 2026-06-17 - measured numbers in §8. F1 will actually reach 25k background, higher than the planned 10k.)

- **F0** - the minimum that validates the calculated sizing numbers and the harness itself; the only phase needed for small beta.
- **F1** - full load against the background of the fleet; gives the first cliff numbers by API and run on a moderate scale.
- **F2** - **BACKLOG.** Activate **together with** implementation of audit partitioning ([ADR-022](../adr/0022-audit-pipeline.md)) and Shepherd ([scaling.md → Shepherd](../operations/scaling.md)) - this is one "large fleet" operating mode: test 100k scale without these two subsystems = obviously run into known gaps. For closed small beta, F2 is **not needed**.

## 7. What's in the backlog (outside this plan)

- **F2 as a whole** (50k–100k distributed) - see §6.
- **Metrics for 4 observation spaces** (§4.2): `keeper_conclave_*`, explicit collectors Redis-lease-rate / PG-pool / audit-INSERT-rate. Their introduction is a separate slice with propose-and-wait by name (new metrics = catalog extension [observability.md](../observability.md)), not part of the load plan.
- **`soul-legion`-generator** (§3, component 1) - name fixed ([naming-rules.md → Soul Legion](../naming-rules.md#soul-legion)); tool construction (removal of soulstub from `//go:build e2e` + ramp single-process) - F0, not yet implemented.
- **Real-soul load option** (real `soul`-binary instead of stub) - outside the scope: stub is deliberately not used to measure the Keeper, not the host. Apply realism under load is a separate task based on L3b ([testing/README.md → L3b](README.md)).
- **CI integration of load run** - not in `make check` (Docker-dependent, expensive in terms of time and resources); similar to L3a/L3b/L3c - a separate on-demand target.

## 8. Measured results (F0 + F1, 2026-06-17)

Actual run of `soul-legion`: F0 (A-axis) + F1 (B/C axes against the background of the fleet), ramp up to **N=25000 MEASURED** (and probe N=50000, hitting the client limit - §8.1). The numbers below are **measured**, not calculated; they replace framing "sizing - calculated" for scale **up to 25k inclusive**. 100k remains the settlement/F2 backlog (§6). The run revealed and closed two bottlenecks: applybus maxclients-cliff (`fec7e02`) and Tempo-preview rate-limit (`34d85a9`) - see §8.5.

**Methodology.** `soul-legion` on a live dev bench (**24 vCPU / 30 GiB**): **one Keeper instance** (event-stream `:9443`, metrics `:9090`, API `:8080`), dev-PKI (batch-issued mTLS-leaf for each fake SID), real Souls background. Axis A - ramp **1k → 5k → 10k → 25k** connections in single-process (goroutine-per-stream), probe 50k. **NOT 100k** - the full ramp remains F2/backlog (§6); The purpose of the run is to measure the working area to the limit achievable on one host and sanity-validate the calculation orders.

### 8.1. Axis A - connections (ramp 1k → 25k, probe 50k)

Ramp single-process, each stub holds a long-lived `EventStream` (gRPC bidi/mTLS). All target streams are held at each stage (`keeper_grpc_streams_active` = N + real background), **0 errors** on ramp-up up to 25k.

| N | connect p99 | RSS Keeper | RSS/soul | Keeper Goroutines |
|---|---|---|---|---|
| **1 000** | 109 ms | 183 MiB | ≈ 0.18 | — |
| **5 000** | 108 ms | 690 MiB | ≈ 0.14 | — |
| **10 000** | 119 ms | 1 221 MiB | ≈ 0.12 | ≈ 90k |
| **25 000** | 185 ms | 2 930 MiB | ≈ 0.12 | ≈ 195k |

- **Linearity.** Connect latency stays flat (p99 109→185 ms) when N increases by 25×; RSS grows linearly. **RSS/soul falls** with increasing N (0.18 → 0.12 MiB) - depreciation of the base-overhead of the Keeper process.
- **Drain:** after the legion is turned off, `streams_active` returns to baseline - **no streams/goroutines leak**.

**Extrapolation to 100k by a factor of ≈ 0.12 MiB/capita: ≈ 11–12 GiB RSS** - within budget [scaling.md → Sizing for 100k VM](../operations/scaling.md) (3–4×8 GB; with 3+ instances goroutines and streams divided between them).

#### Single-host limit: ephemeral port exhaustion (probe N=50000)

50k probe **hit the ceiling ≈ 28222 simultaneous streams** - **not a keeper**, but **exhaustion of ephemeral loopback ports on the harness side** (one client source-IP → limited range `ip_local_port_range`). Keeper kept ≈ 28k streams calmly (RSS ≈ 3.3 GiB, PG idle / low CPU). True 50k+ on one host rests on the client limit, and not on Keeper → requires a **distributed harness** (several source-IPs / several load machines) - this is F2 (§6).

> **★ Disclaimer on per-soul RSS.** Take the **coefficient on the larger N**, and not the absolute on the small one. At small N, the absolute RSS/soul is overestimated by the base-overhead Keeper process (at N=1000 ≈ 0.18 MiB/soul; at N=300 - even higher, ≈ 0.46 → false extrapolation). An honest figure is an incremental coefficient of N=10k–25k (≈ 0.12 MiB/person). **Accurate per-soul under 100k - task F2** (only a real ramp to cliff gives the correct coefficient: at large N, PG/Redis-resolve, presence-batch, GC-pressure come into play).

### 8.2. Axis B - API for N souls

On top of a background of connected stub-souls; B axis expanded to **24 GET-collection-handles + write-axis** (create→delete).

**Read-path.** Fleet-dependent `GET /v1/souls` degrades **linearly with fleet size**, remaining in SLA: **3476 → 1488 req/s** as N grows, **p99 < 140 ms**, **0 errors**. Directories without presence resolution (`GET /v1/modules`, `/v1/event-types`, `/v1/me/permissions`) - **p99 < 5 ms** (do not depend on the fleet). Read pens are kept with a margin at a scale of up to 25k.

**Write-axis** (create→delete cycles on synod / role / push-provider / herald under **25k-fleet**): **≈ 234 req/s**, **p99 5–7 ms**, **0 errors**. After releasing the Tempo limit (`voyage_preview` bucket - §8.5), the write profile no longer hits the limiter on read-like operations.

> **★ Nakhodka (answers the question §2: "doesn't the limiter itself cut the load profile")** On the first run **yes** - `POST /v1/voyages/preview` rested on ≈ 10 rps, sharing the per-AID bucket `voyage_create` (10/20) with the creating route. Untied in the same session: separate bucket `voyage_preview` (dev-default `30/60`, [config.md → tempo](../keeper/config.md#tempo), [observability.md → Tempo](../observability.md#keeper--tempo-per-aid-rate-limiter-write-api-adr-050)), preview increased from ≈ 10 to ≈ 33 rps (`34d85a9`, §8.5). Full measurement of all write pens under a distributed load across several AIDs - F2.

### 8.3. Axis C - Voyage by Fleet

command-Voyage by `coven=legion`, e2e (creation → all `ErrandResult` → finalization), **succeeded 100% on all stages**:

| scope_size | end-to-end |
|---|---|
| **1 000** | 3.58 s |
| **5 000** | 8.34 s |
| **10 000** | 11.6 s |

- Time grows **sublinearly** to fleet size (10× scope → ≈ 3.2× time) - dispatch/finalization is amortized.
- **Audit:** ≈ **2 INSERT/host** (`errand.invoked` + `errand.completed`) → linear increase in the number of INSERTs with the fleet.

> **★ CRITICAL: 10k-Voyage did not finalize AT ALL before applybus-fix.** On the first run, command-Voyage on ~10k hosts did not reach finalization (`succeeded=0` in 5 min at idle PG / low CPU). Root - **maxclients-cliff applybus**: cluster-mode applybus raised a separate Redis pub/sub-subscription for **each** applyID → ~10k concurrent Errands exhausted Redis `maxclients` (`ERR max number of clients reached`). Fixed in the same session (`fec7e02`, §8.5): holder-skip (local-publisher of the same instance does not raise Redis-bridge) + channel sharding `apply:<id>` → `events:shard:<fnv32a(id)%256>` (constant number of subscriptions instead of O(N)). The numbers above are **after the fix**; before him, the 10k level was unattainable.

> **★ Confirmation.** Audit-INSERT grows **linearly with fleet size** - direct empirical confirmation of deferred cliff before partitioning ([ADR-022](../adr/0022-audit-pipeline.md), [known-limitations.md → Audit-scaling](../known-limitations.md)). Based on the measured N, this is still far from a collapse, but the growth coefficient is fixed.

### 8.4. Confirmed vs remains on F2

| Confirmed (measured up to N=25k) | Remains calculated / on F2 (backlog) |
|---|---|
| Streams are linear up to 25k (connect p99 ≤ 185 ms, RSS/soul ≈ 0.12 MiB) | Accurate per-soul RSS under 100k (you need a real ramp to the cliff) |
| RSS coefficient in the scaling.md budget (≈ 11–12 GiB@100k) | True 50k+ on a single host - **distributed harness required** (single-host limit = ephemeral ports, §8.1) |
| Read-API degrades linearly, in SLA (3476→1488 rps, p99 < 140 ms); directories p99 < 5 ms | Real cliff on each axis (at ≤ 25k not reached) |
| Write axis ≈ 234 rps p99 5–7 ms under 25k fleet, 0 errors | Full write-API measurement across multiple AIDs at full scale |
| command-Voyage e2e up to 10k (succeeded 100%, after applybus fix) | 4 observational spaces §4.2 under full scale |
| Audit-INSERT is linear across fleet (≈ 2/host) | |

### 8.5. Found and corrected in this session

The load revealed two bottlenecks, **both were closed in the same session** - the measured numbers §8.2/§8.3 were removed after the fixes:

- **applybus maxclients-cliff** (`fec7e02`). Cluster-mode applybus raised a separate Redis pub/sub-subscription for each applyID → ~10k simultaneous Errands exhausted Redis `maxclients`, and command-Voyage at ~10k did not finalize at all. Fix: holder-skip (lease-holder == self → the event goes through local-bus, Redis-bridge does not rise) + channel sharding `apply:<id>` → `events:shard:<fnv32a(id)%256>` (K=256, constant number of subscriptions instead of O(N), scale to 100k). The same mechanism symmetrically hit scenario-run / RunResult / TaskEvent / SSE. ADR-006(c) amendment.
- **Tempo-preview rate-limit untied** (`34d85a9`). `POST /v1/voyages/preview` shared the per-AID bucket `voyage_create` (10/20) with the creating route → preview limited to ≈ 10 rps, although dry-resolve scope read-like in effect (without persist/audit). Fix: separate bucket `voyage_preview` (dev-default 30/60), preview increased ≈ 10 → 33 rps. ADR-050 amendment + ADR-043 §4.

## See also

- [testing/README.md](README.md) - levels L0–L4; load appears next to it as a separate level.
- [operations/scaling.md](../operations/scaling.md) - sizing calculation table, bottlenecks, Shepherd/Conclave/Acolyte.
- [observability.md](../observability.md) - directory of `keeper_*`/`soul_*`-metrics (base for §4).
- [`tests/load/README.md`](../../tests/load/README.md) - how to run `make stress` (ENV variables, what it measures, precondition).
- [`tests/e2e/internal/soulstub/soulstub.go`](../../tests/e2e/internal/soulstub/soulstub.go) - stub generator foundation (component 1).
- [known-limitations.md → Audit-scaling](../known-limitations.md) - deferred audit-cliff (context §4.2 / F2).
