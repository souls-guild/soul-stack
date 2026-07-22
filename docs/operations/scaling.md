# Scaling Keeper

Horizontal scaling of a Keeper cluster: adding instances, Acolyte pool configuration, Conclave / Watchman behavior, refuse-guard, balancer, target scale of 100k VM.

Architectural context:
- [ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) - HA stateless cluster on top of a shared PG/Redis.
- [ADR-006](../adr/0006-cache-redis.md) — Conclave (presence Keeper instances) + SID-lease (presence Souls).
- [ADR-027](../adr/0027-apply-work-queue.md) - Acolyte-pool, Ward-claim, Watchman / soul-shedding.

## Basic model

```
   Souls ───────► L4-LB (least-conn TCP) ──────►  keeper-1   keeper-2   ...   keeper-N
                                                     │           │              │
                                                     └────────┬──┴──────────────┘
                                                              ▼
                                                       ┌────────────┐
                                                       │  Postgres  │ shared
                                                       └────────────┘
                                                              ▲
                                                              ▼
                                                       ┌────────────┐
                                                       │   Redis    │ shared
                                                       └────────────┘
```

Any instance serves any request. Specifics:

- **Distribution of Soul streams** - between LB (new connections are distributed least-conn) and SID-lease in Redis (one Soul → one Keeper instance for the duration of the session).
- **Apply execution distribution** - work-queue ([ADR-027](../adr/0027-apply-work-queue.md)): The Acolyte pool on each instance brands jobs from `apply_runs` through `SELECT … FOR UPDATE SKIP LOCKED`.
- **Distribution of background tasks** - Reaper leader is selected via Redis-lease `reaper:leader` (one live-Reaper in the cluster).
- **Load balancing distribution** (Shepherd, [ADR-002 amendment](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)) - **PLANNED/backlog**, not in the code. Before its appearance during scale-out, the new instance is idle until the natural churn of EventStreams (failback by `failback.interval` or stream break).

## Acolyte-pool

Acolyte = pool of apply execution workers on each Keeper instance. **Feature-flag** via `acolytes: N` to `keeper.yml`:

```yaml
acolytes: 4                         # 0 = pool is not raised (old synchronous path)
# acolyte_lease: 30s                # TTL Ward-capture
# acolyte_batch: 10                 # max. tasks for one claim tick
# acolyte_poll_interval: 2s         # poll-fallback period to Summons
# acolyte_drain_grace: 5s           # graceful-drain window when stopped
```

Full typing is [`docs/keeper/config.md` → acolytes](../keeper/config.md#acolytes).

### When what-how much-Acolyte

| `acolytes` | Suitable for |
|---|---|
| `0` (default) | **Single-keeper-only** ([ADR-027(h)](../adr/0027-apply-work-queue.md) amendment). The old run-goroutine path: incarnation owns one in-memory instance. Run performed on K1 with Soul on K2 stream - **will forever freeze** in `applying`. |
| `>0` | **HA-required**. Work-queue: claim+dispatch via PG, completion via a common PG, regardless of the instance with the stream. |
| `2-8` per instance | Typical prod range. |
| `>16` | Large installations - limited by PG (`SELECT … FOR UPDATE SKIP LOCKED` loads primary). |

**Default `0` - not for cont.** With `acolytes: 0` + `Conclave.CountLive > 1` Keeper **refuses to start** (see [§ Refuse-guard](#refuse-guard)).

### Calculation of Acolyte number

Rough estimate: ≥1 Acolyte needed per simultaneous run. The run takes Acolyte for the rendering time (Vault-resolve + CEL + text/template) + SendApply (fast, ms-seconds) + waiting for the barrier (with a serial frame - the duration of all tasks + inter-host sync).

For a typical installation of up to 100 incarnations with a peak frequency of 5-10 simultaneous apply per cluster - `acolytes: 2-4` per instance is sufficient. For more active use (dozens of simultaneous apply) - increase proportionally, monitoring `keeper_render_duration_seconds.p99` and PG latency.

## Conclave — presence of Keeper instances

[Conclave](../adr/0006-cache-redis.md) - Redis registry of live Keeper instances (new Redis role `e`):

- Each instance writes the key `keeper:instance:<kid>` with TTL `DefaultConclaveTTL=30s` at startup.
- Renew every `DefaultConclaveRenewInterval=10s` lease-goroutine.
- Removes the key to graceful-shutdown.
- `RegisterInstance` rejects busy KID (`ErrConclaveKIDTaken`) - protection against double KID during misconfiguration.

`Conclave.LiveKIDs()` / `Conclave.CountLive()` provide an up-to-date set of live instances. Used:

- **Refuse-guard** at start at `acolytes: 0` (see below).
- **Watchman / soul-shedding** - to coordinate load balancing (planned through Shepherd).

### Verify Conclave

```sh
redis-cli KEYS 'keeper:instance:*'
# 1) "keeper:instance:keeper-prod-01"
# 2) "keeper:instance:keeper-prod-02"
# 3) "keeper:instance:keeper-prod-03"

redis-cli TTL 'keeper:instance:keeper-prod-01'
# (integer) 27       # ≤ 30, renew every 10s
```

Prometheus-metric - not available at the time of writing (count Live instances through `len(Conclave.LiveKIDs())` is not currently exposed as `keeper_conclave_*`-collector; see [open question in `disaster-recovery.md`](disaster-recovery.md#open-questions-runbook)).

## Refuse-guard: `acolytes: 0` in HA - refusal to start

**Invariant**: `N > 1` live Keeper instances **requires** `acolytes > 0` ([ADR-027(h)](../adr/0027-apply-work-queue.md) amendment). Implemented in two layers:

1. **Refuse at startup.** Keeper checks `acolytes: 0` with `Conclave.CountLive() > 1`; if other instances are alive - **refuses to start** with `refusing to start: acolytes:0 unsafe under multi-keeper (Conclave reports N=X live instances); set acolytes>0 or set allow_unsafe_single_path_multi_keeper=true`, exit 1. Protects against misconfiguration.
2. **Dispatch-time WARN.** Runtime safety-net: when trying to dispatch on a non-Acolyte path with SID-lease for another KID - `WARN` in the logs (footgun will appear here).

### Explicit opt-out

For rare cases (one node in the HA cluster is temporarily running on the old path) - `allow_unsafe_single_path_multi_keeper: true` or env `KEEPER_ALLOW_UNSAFE_MULTI_KEEPER=true`:

```yaml
acolytes: 0
allow_unsafe_single_path_multi_keeper: true   # DANGER, see ADR-027(h)
```

**Do not use** in normal operating model. Known footgun ([ADR-027(h)](../adr/0027-apply-work-queue.md)): run hangs at `applying`, incarnation does not exit locked state.

### Fail-open: Redis is not available

If Redis is unavailable (Conclave does not respond) refuse-guard **fail-open** - start is not blocked (see [ADR-027(h)](../adr/0027-apply-work-queue.md) Consequences). This is a deliberate choice: blocking start in the absence of Redis would turn any incident with Redis into a catastrophic outage (no one will start). Price - theoretical misconfig window at the time of upgrade from downtime Redis; In a production installation with HA Redis, the risk is acceptable.

## Watchman / soul-shedding - isolation-detect

[Watchman](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) - a subsystem that responds to loss of connection with the general state (PG and/or Redis):

- **Probe-loop** periodically pings PG + Redis.
- **Debounce-state-machine** transfers the instance to `isolated` after N consecutive failures (default 3).
- **`StreamManager.CloseAll()`** — closes all local EventStream streams (cancels per-stream `ctx`).
- Soul receives EOF → performs a normal failback to the next endpoint from the priority list.
- Reverse transition `isolated → healthy` does not "call back" - Souls return themselves by priority.

Watchman works **only with real isolation** (debounce), and not with a one-time timeout. A healthy cluster does not float.

In the code - `keeper/internal/watchman/`. Configurable (at the time of writing, the thresholds are default, the config block is not allocated in `keeper.yml`).

## Shepherd - load balancing with scale-out

⚠️ **PLANNED/backlog**, not implemented ([ADR-002 amendment](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)).

Problem: when adding a new instance behind LB, existing long-lived EventStream streams **stick** on old instances. LB balances only new connections; Soul will not reconnect on its own. Failback only fires when `priority > 1`. The new instance remains idle until the natural churn (hours or days).

Solution (when it appears): the instance through Conclave sees its share of the load above its fair share and **dumps the excess** of its streams (partial `StreamManager.CloseAll`) with jitter / cap.

**Before implementation** - workaround for scale-out:

- **Forced rolling-restart of existing instances** into the maintenance window - Souls failback is rebalanced including the new instance. Safe with `acolytes > 0` (apply will survive recovery / other Acolyte).
- **Long-tail** - natural churn will resolve sticking in days.

## L4 balancer: settings

EventStream + Bootstrap-RPC - TCP, set L4-LB (haproxy, IPVS, AWS NLB, Yandex L4-LB):

```haproxy
# HAProxy example for EventStream (port 8443) - L4 TCP, least-conn
frontend eventstream_in
    bind *:8443
    mode tcp
    option tcplog
    default_backend eventstream_backends

backend eventstream_backends
    mode tcp
    balance leastconn
    # Health check - simple TCP-probe of a port
    option tcp-check
    server keeper-1 keeper-1.internal:8443 check inter 5s
    server keeper-2 keeper-2.internal:8443 check inter 5s
    server keeper-3 keeper-3.internal:8443 check inter 5s
    # Timeout margin for long-lived streams (gRPC bidi)
    timeout server 24h
    timeout client 24h
```

Key parameters:

- **`balance leastconn`** - distributes new streams evenly. Round-robin gives skew during long-lived streams of different intensity.
- **`timeout server 24h` / `timeout client 24h`** - gRPC keepalive keeps the connection through NAT/firewall, but LB should give it a window without idle reset.
- **TCP-probe**, not HTTP. gRPC via L4 is not parsed - LB does not know healthz; TCP-probe port `8443` is sufficient.
- **Sticky session is NOT needed** - SID-lease in Redis already gives a unique "one Soul → one instance" state.

OpenAPI (`8080`) + MCP (`8081`) - L7-proxy with TLS termination, the usual path (nginx / traefik / envoy http2 mode).

## Target scale 100k VM

Accordingly, invariant "hot → Redis, not PG" (see [ADR-002 amendment](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster) and [ADR-006](../adr/0006-cache-redis.md)). Validated by mega-acceptance on 3 keeper + 9-node Redis-cluster:

- Presence Souls is derived from Redis SID-lease (batch-EXISTS), **not from PG-`souls.status`** (no synchronous recording on the hot path).
- Heartbeat - Redis role (a), PG-`last_seen_at` flush throttled (default once every `stale_after/3 = 30s` on SID).
- Conclave / leader-lease / pub/sub — Redis.

### Sizing infrastructure for 100k VM (approx.)

| Component | Host | RAM | CPU |
|---|---|---|---|
| Keeper | 3+ instances | 8 GB | 8 vCPU |
| Postgres primary | dedicated | 32 GB | 8 vCPU + NVMe |
| Postgres replica (Patroni) | dedicated | 32 GB | 8 vCPU + NVMe |
| Redis cluster | 3+ master + replicas | 4 GB | 4 vCPU |
| Vault | 3 instances (raft) | 4 GB | 2 vCPU |
| OTel-collector | 1-3 instances | 2 GB | 4 vCPU |

Numbers are orders of magnitude for the starting point. Real sizing - based on load tests with real workload.

### Growth bottlenecks

| Bottleneck | Symptom | Solution |
|---|---|---|
| PG primary CPU on `apply_runs` claim | Claim latency is growing, `keeper_grpc_apply_dispatch_total{result=ok}.rate` is falling | Decrease `acolyte_batch` / distribute `acolyte_poll_interval` jitter; increase PG-CPU; consider read-replica for `Holder.Resolve` requests |
| PG primary IO on `audit_log` | Backup tool is lagging behind, `INSERT` is lagging | Partitioning `audit_log` to `created_at` (post-MVP); run audit-write on async worker (post-MVP) |
| Redis CPU on SID-lease check | Scenarios with a large roster (`on:` + `where:` over thousands of hosts) slow down | Pipeline batching SID-lease check (available in the code); increase Redis-node resources, consider cluster-mode |
| OTel-collector | dropped spans in OTel-collector logs | Increase resource collector, sampling in Keeper (sampler is still configurable - backlog) |

## Scale-out procedure (adding an instance)

1. **Prepare host** using [`deployment.md`](deployment.md) (same deb/rpm, same config with new `kid`).
2. **`keeper.yml`** — change only `kid:` (plus local cert paths if unique); the rest is a copy of the working config.
3. **L4-LB** - add backend (see above).
4. **Run** `systemctl start keeper`. Conclave-presence will appear in Redis in 1-2s.
5. **Verify**: `redis-cli KEYS 'keeper:instance:*'` shows N+1 keys.
6. **Balancing** - see § Shepherd (at the time of writing - without auto-balance after scale-out; rolling-restart of existing instances or wait for natural churn).

## Scale-in procedure (graceful instance deletion)

1. **Drain LB** — remove an instance from the active pool (`server keeper-3 ... disabled` in HAProxy, or health-check fail).
2. **`systemctl stop keeper`** — graceful shutdown:
   - Acolyte pool stops claiming new jobs (`acolyte_drain_grace` default 5s).
   - In-flight claims - canceled by ctx; Ward remains in the database (`claimed`/`running`), lease will expire in `acolyte_lease` (30s), recovery-scan will pick up (if enabled, see [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md)).
   - Conclave-presence is clearly removed.
   - Soul streams are closing → Souls failback to the remaining instances.
3. **Remove host** from inventory.

## See also

- [`docs/keeper/config.md` → acolytes](../keeper/config.md#acolytes) — Acolyte pool config.
- [`docs/keeper/reaper.md` → recovery-enable](../keeper/reaper.md) — enable gate `reclaim_apply_runs`.
- [`docs/architecture.md` → ADR-002 / ADR-006 / ADR-027](../architecture.md) - justifications.
- [`monitoring.md`](monitoring.md) - scale metrics: `keeper_grpc_streams_active`, `keeper_reaper_lease_held`, etc.
- [`upgrade.md`](upgrade.md) - rolling upgrade over scale-out / scale-in.
