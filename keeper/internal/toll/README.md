# Toll — cluster-wide detector of mass Souls attrition

Implementation of [ADR-038](../../../docs/adr/0038-toll.md#adr-038-toll--a-cluster-wide-detector-of-mass-souls-attrition).

## Purpose

A passive observer of the rate-of-disconnect of Soul agents at the cluster level. When a
threshold is exceeded (default 20% of the baseline `souls.status='connected'` over a
sliding 60s window) it sets a cluster-wide flag `cluster:degraded` (Redis key,
TTL 60s) → the Operator API returns 503 + Retry-After on sensitive mutating
endpoints. The goal is to cut the operator off from launching a scenario / push-apply on
an obviously sick cluster with an explicit error, rather than serving `partial_failed` or a
silently failing run.

Toll is a **passive observer**: it doesn't close streams (that's `watchman`,
soul-shedding S2 — a different entity), doesn't perform recovery actions, only
observes and blocks the write API.

## Architecture

```
                  ┌──────────────┐
                  │  Toll Leader │   (single Keeper instance via Redis-lease)
                  │  (aggregator)│
                  └──────┬───────┘
                         │ reads
            ┌────────────┴────────────┐
            │ Redis sorted-set        │
            │ toll:disconnects        │   ← all per-instance Watchers publish
            │ (score=unix-sec,        │      disconnect events here
            │  value=sid|kid|coven)   │
            └─────────────────────────┘
                         │ leader writes
                         ▼
                   cluster:degraded   (Redis key, TTL 60s)
                         │
                  middleware reads
                         ▼
               POST /v1/incarnations/{name}/scenarios/{scenario} → 503 Retry-After
               POST /v1/push/apply                                 → 503 Retry-After
               (read-API, RBAC, unlock, destroy, Errand           — NOT blocked)
```

## Package components

| File | What's inside |
|---|---|
| `publisher.go` | The `Publisher` interface, `DegradedReader` interface, `NoopDegradedReader`, `EncodeDisconnect` (the form of a sorted-set member). |
| `degraded.go` | The `degradedWriter` interface (internal, for the Leader). |
| `watcher.go` | `Watcher` — per-instance hook, warmup-immunity + graceful-shutdown filters, ZADD into the shared sorted set via Publisher. |
| `leader.go` | `Leader` — background goroutine, Redis lease + aggregation loop + set/clear with asymmetric hysteresis. Sentinels `ErrLeaseTaken` / `ErrLeaseLost`. |
| `baseline.go` | The `BaselineReader` interface + `PGBaselineReader` (PG impl) + a cached wrapper with TTL. |
| `audit.go` | Thin wrappers writing `cluster.degraded_set` / `cluster.degraded_cleared`. |
| `metrics.go` | `Metrics` — `keeper_cluster_degraded` (gauge) + `keeper_toll_disconnects_total` (counter, label `coven`) + warmup/graceful/leader_active. |
| `middleware.go` | `DegradedMiddleware` — chi/net-http middleware, blocked routes → 503 + Retry-After + RFC 7807 problem+json. |

## Invariants (see ADR-038)

- **Single-leader aggregation** — the `cluster:degraded` set is written ONLY by the leader
  via the Redis lease `cluster:toll:leader`. Two flags are never set simultaneously.
- **Warmup immunity 60s** after instance start — disconnects are counted (metric
  grows) but NOT published (defense against cluster cold-start false positives).
- **Graceful-shutdown filter** — closures initiated by Keeper itself
  (Watchman shedding / `ctx.Done()` graceful keeper shutdown) are dropped.
- **Asymmetric hysteresis** — trips on the first threshold breach, only clears after a
  sustained 60s grace period below the threshold.
- **Fail-OPEN middleware** — on a reader error (Redis flap) the middleware lets the
  request through; availability matters more than caution, the flag expires via TTL if the
  leader dies.
- **Baseline=0 → no degraded** — an empty cluster is not evaluated (protection against
  division by zero).

## Wire-up in the daemon

Set up in `setupToll` (keeper/cmd/keeper/daemon.go) AFTER `setupRedis` and BEFORE
`setupGRPCEventStream`. Gates (any one triggered → Toll fully disabled, the
EventStream hook is a no-op, middleware passthrough):

- `keeper.toll.enabled: false` in keeper.yml;
- `d.redisClient == nil` (single-instance/dev without Redis).

The wire-up uses thin adapters (`keeperRedisToll*`) over primitives
in `keeper/internal/redis/tolldetector.go` (ZADD/ZCOUNT/ZREMRANGEBYSCORE/
SET-DEL-EXISTS), so the `toll` package doesn't depend on `*redis.Client` directly and
remains unit-testable through fake interfaces.

## Configuration

See `KeeperToll` in [shared/config/keeper.go](../../../shared/config/keeper.go).
Optional `toll:` block in keeper.yml; all fields have defaults from `DefaultToll*` constants:

```yaml
toll:
  enabled: true              # default true; false — disable
  threshold: 0.20            # fraction of the baseline souls.status='connected'
  window_size: 60s
  degraded_ttl: 60s
  clear_grace: 60s           # asymmetric hysteresis
  lease_ttl: 30s             # cluster:toll:leader, renewed every 10s
  warmup_delay: 60s          # cluster cold-start immunity
```

## Metrics

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `keeper_cluster_degraded` | gauge (0/1) | — | set ONLY by the leader; a cluster-level closed set. |
| `keeper_toll_disconnects_total` | counter | `coven` | non-graceful disconnects (post-filter). |
| `keeper_toll_warmup_skipped_total` | counter | — | disconnects dropped by warmup immunity. |
| `keeper_toll_graceful_skipped_total` | counter | — | disconnects dropped as graceful shutdown. |
| `keeper_toll_leader_active` | gauge (0/1) | — | 1 = this instance holds the lease. Cluster-wide sum = 1. |

## Audit events

`cluster.degraded_set` / `cluster.degraded_cleared` (scope `cluster.*`), source
`keeper_internal`, `archon_aid: NULL`. Payload — numeric parameters (rate,
baseline, threshold, leader_kid, window/grace seconds). Written ONLY by the leader.

## Tests

- `watcher_test.go` — warmup-immunity / graceful filter / publisher-error
  not-fatal / nil-safe receiver.
- `leader_test.go` — acquire-retry / set-degraded / clear-grace / baseline=0 /
  lease-lost recovery / sorted-set error skips tick / cached baseline.
- `middleware_test.go` — passthrough / 503 / fail-open / nil-reader.

L1 / L3 (integration with a real Redis cluster) — a separate slice.
