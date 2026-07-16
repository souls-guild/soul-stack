# Conductor / Conductor

The background leader-elected subsystem inside the `keeper`-binary is the executor of [Cadence](../naming-rules.md)-schedules ([ADR-046](../adr/0046-cadence.md)): according to its tick, it selects mature schedules and spawns a regular [Voyage](../adr/0043-voyage.md)-run. **Not a separate binary** (like Reaper). Full design fixation - [ADR-048](../adr/0048-conductor.md); this document is a guide to behavior and config.

> **Conductor executes, Cadence stores, Reaper cleans.** Cadence is a row in the Postgres table `cadences` with a run "recipe" + repetition rule ([ADR-046](../adr/0046-cadence.md)). Conductor only **executes the trigger** - Cadence spawn semantics (due fetch, three `overlap_policy`, recalculation `next_run_at`) belong to Cadence and have been transferred to Conductor **without changes**. Spawn historically (S0-design ADR-046) was planned by the Reaper rule `spawn_due_cadence` (`action: spawn`), but [ADR-048](../adr/0048-conductor.md) (2026-06-02) moved execution to a separate subsystem: the Reaper cleanup domain (`reaper.interval` 1h) and the Cadence scheduling domain require different natural rhythm (~15–30s). After removing Reaper, the domain is cleanup again ([reaper.md](reaper.md)).

## Properties

- Lives inside `keeper`. Not a separate binary.
- Works **only on one Keeper instance at a time**: the leader is selected via Redis-lease `conductor:leader` with TTL = `lock_ttl` ([ADR-006](../adr/0006-cache-redis.md)). Lease is **independent** of `reaper:leader` - the Conductor leader and the Reaper leader can be on different instances.
- Sits on a generic lease-loop primitive (`keeper/internal/leaderloop`, common with Reaper) - two independent lease keys, two independent leader-elections.
- **Adaptive polling step** (cron model, [ADR-048 "Adaptive interval"](../adr/0048-conductor.md)): tick is NOT fixed. Before each tick, the leader recalculates the step from the Cadence enabled register - `clamp(min(enabled schedule periods), poll_floor, poll_ceiling)`; empty registry of enabled rules → `poll_idle` (polling is not empty). Independent of `reaper.interval` (1h). Details - Adaptive polling step.
- **Default-ON with Redis** (footgun-guard, see Default-ON and degradation without Redis).
- `poll_floor` / `poll_ceiling` / `poll_idle` / `lock_ttl` (and backcompat-alias `interval`) - hot-reload without re-deploying the binary (end-to-end requirement, see [requirements.md](../requirements.md)). `enabled` is read at start (see Config).
- Metrics `keeper_conductor_*` - see Metrics.

## What Conductor does on tick

When an instance holds lease `conductor:leader`, on each tick it performs the same due-fetch and spawn that are normalized in [ADR-046 §4/§5](../adr/0046-cadence.md):

1. `SELECT … FROM cadences WHERE enabled AND next_run_at <= NOW() FOR UPDATE SKIP LOCKED` - selection of mature schedules.
2. For each due line **in one PG transaction**: apply `overlap_policy` (`skip` / `queue` / `parallel`) → with `INSERT` spawn allowed in `voyages` / `voyage_targets` with `cadence_id` → recalculation `next_run_at` (cron parser for `cron`, anchored `next_run_at + interval_seconds` for `interval`) → `last_run_at = NOW()`.

**Anchor of recalculation is the planned slot, not `NOW()`.** For `interval`, the next launch is counted from **the previous planned `next_run_at`** (the one for which the line became due), and not from the actual `last_run_at`/`NOW()` - otherwise tick-drift (Conductor worked a little later term) would accumulate and the slots would "go away". Anchoring to the planned grid makes the interval drift-free. See [ADR-046 §4](../adr/0046-cadence.md).

**Missed-slot after idle (anti-storm).** If the Keeper was idle and several slots were missed, `next_run_at` is transferred to **the first future grid slot strictly after `NOW()`** - one catch-up spawn for the current due, **without** additional spawn for each missed slot. Symmetrically for `interval` (roll `+ interval_seconds` until the slot becomes `> NOW()`) and for `cron` (the next cron slot after `NOW()`).
3. The spawned Voyage is included in the normal Voyage-lifecycle (`pending` → claim VoyageWorker → `running` → terminal); Conductor **doesn't lead him further** (one-shot spawn).

`FOR UPDATE SKIP LOCKED` + single-executor leadership gives **exactly one spawn per tick** without a race between Keeper instances.

**Authorship and audit.** The child Voyage spawns "on behalf of" the creator of Cadence (`voyages.started_by_aid` inherits `created_by_aid` Cadence). Spawn/pass audit-event (`cadence.spawned` / `cadence.skipped_overlap`) is written with **`source: background`** and `archon_aid: NULL` - this is an autonomous background initiative of the keeper, and not an operator call. Source `background` **saved after moving to Conductor** (new source `scheduler` is NOT entered, see [ADR-048 → ADR-022](../adr/0048-conductor.md)). Event-types directory - [naming-rules.md → Audit-events](../naming-rules.md#audit-events).

## Adaptive polling step

Conductor polling step **not fixed** ([ADR-048 "Adaptive interval"](../adr/0048-conductor.md)). Before each tick, the leader removes a step from the Cadence enabled register and pins it into the `[poll_floor, poll_ceiling]` corridor of the "Calm" profile (30s / 60s / 120s by default):

```
derivedMinPeriod = min(periods of all enabled Cadence) # interval rules carry interval_seconds; cron rules - 60s contribution
step = clamp(derivedMinPeriod, poll_floor, poll_ceiling)
if the enabled registry is empty → step = poll_idle
```

How `derivedMinPeriod` is considered (implementation - [`cadence.MinPeriod.DerivedMinPeriod`](../../keeper/internal/cadence/crud.go), [`conductor.AdaptivePollInterval`](../../keeper/internal/conductor/poll.go)):

- **interval rules** give `MIN(interval_seconds)` on enabled lines (one easy aggregate-SELECT `SELECT MIN(interval_seconds), bool_or(schedule_kind='cron') FROM cadences WHERE enabled`, partial index `cadences_enabled_interval_idx`).
- **cron rules** do not carry `interval_seconds` (NULL, do not go into MIN), but cron granularity is a minute: if there is at least one enabled cron rule, the contribution is fixed **60s**. In order not to miss the nearest cron slot, Conductor with cron rules is polled at least once a minute.
- The **more frequent** of the two is taken: `min(MIN(interval_seconds), 60s when cron is present)`.
- **Empty enabled registry** (neither interval nor cron rules): `derivedMinPeriod` is not defined - Conductor is polled with `poll_idle` (lazy-baseline). The "empty" signal carries the same MIN request - there is no separate Redis channel.

Why a corridor:

- **`poll_floor` (30s)** - lower bound: frequent scheduling (`interval=5s`) will NOT cause Conductor to thresh PG every 5 seconds. This is a defense-in-depth backstop to the floor limit of the minimum Cadence period (see below) - even if the sub-floor line has bypassed the write-path and DB-CHECK, the poll will still not drop below 30s.
- **`poll_ceiling` (60s)** - upper limit: the rare schedule (`interval=1h`) does not stretch the polling so that the only insurance against missing a slot is the missed-slot anchored recalculation mechanism.
- **`poll_idle` (120s)** - empty registry: there is nothing to spawn, we poll less often than the usual corridor, without loading the PG idle.

**Failover-safe by construction.** Step is a pure function from the current enabled register in the PG, recalculated at every tick from the leader. The new leader after failover does not carry the in-memory polling state: the same registry → the same step. Non-leaders do not perform aggregate-SELECT (only the lease holder calls it).

**Degradation due to fetch error.** If the MIN request has fallen (PG-glitch), the leader does NOT fall - fallback to `poll_ceiling` (infrequent edge of the corridor, not floor - so as not to thresh the PG in a storm) + warn in the log. The next tick repeats the request.

**Why not event-driven.** Reactive (push notification "schedule is ripe") would not give any benefit: downstream chain (claim tasks by Acolyte pool + EventStream to Soul + apply on the host) throttles the accuracy itself. Cadence is a rough rhythm ("once every N minutes/hours"), and the reactive sub-30s domain belongs to [Beacons](../adr/0030-vigil-oracle.md) (Vigil/Portent/Oracle, ADR-030), not Cadence.

## Floor minimum period Cadence

Minimum interval-Cadence period is **30s** ([ADR-046](../adr/0046-cadence.md), Pass B). Creating or changing a Cadence with `interval_seconds < 30` is rejected with **HTTP 422** and the text "minimum Cadence period is 30s; for a response faster than 30s, use Beacons (Vigil/Oracle, ADR-030)." cron-Cadence does not fall under floor (cron-granularity is a minute, ≥ 60s).

Three levels of protection (defence-in-depth):

1. **Write-path validate** ([`cadence.ValidateIntervalFloor`](../../keeper/internal/cadence/crud.go), `POST`/`PATCH /v1/cadences`) - friendly 422 to PG. Floor is taken from the same config key `cadence_scheduler.poll_floor` as the lower limit of the survey (single minimum, not hardcode `30` in two places).
2. **DB-CHECK `cadences_interval_seconds_floor`** (`interval_seconds IS NULL OR interval_seconds >= 30`, migration 068) - invariant at the table level, catches a record bypassing the API.
3. **Pre-flight data-guard** in migration 068 - before `ADD CONSTRAINT` checks if there are already lines with sub-30 `interval_seconds` (for example, a dev stand with the old 10s schedule), and `RAISE EXCEPTION` with clear text, if found. Not silent UPDATE: silently changing the period of someone else's schedule is unacceptable - the operator decides for himself (raise to 30s / transfer to cron / delete).

Floor-limit and lower polling limit `poll_floor` coincide in value (30s) and in source for a reason: the sub-30s period is meaningless (downstream will not handle it more accurately), and the reactive domain is Beacons.

## Default-ON and degradation without Redis

Conductor is enabled **by default if Redis is configured** (lease leadership is possible). This is **footgun-guard**: Cadence without a working scheduler will not silently spawn Voyage - the operator created the schedule, but it is "dead" without a visible error. Therefore, the scheduler is active out of the box on any Redis installation.

- **Turning off Cadence - per-Cadence** via `enabled: false` of the schedule line itself ([ADR-046 §3](../adr/0046-cadence.md)), not by global blanking of Conductor.
- **Global Quenching** - Explicit `cadence_scheduler.enabled: false` (the operator deliberately disables the entire scheduler on this instance).
- **Without Redis** (single-instance dev without Redis) leader-election is impossible - Conductor **degrades in the same way as the Reaper leader on a single instance** (does not rise; `keeper_conductor_*` metrics are not published).

## Config

Block `cadence_scheduler:` in `keeper.yml` (optional - if absent, defaults apply + default-ON for Redis):

```yaml
cadence_scheduler:
  enabled: true        # nil/omitted → ON when Redis is configured (footgun-guard); false → OFF
  poll_floor: 30s      # lower limit of the adaptive polling step (Calm profile)
  poll_ceiling: 60s    # upper bound of adaptive polling step
  poll_idle: 120s      # polling step when Cadence enabled registry is empty
  lock_ttl: 5m         # TTL Redis-lease conductor:leader
  # interval: 60s # backcompat-alias poll_ceiling (see below); new configs write poll_*
```

| Field | Type | Default | Hot-reload | Meaning |
|---|---|---|---|---|
| `cadence_scheduler.enabled` | `bool` (optional, tri-state) | `nil` → ON with Redis | no (read at start) | Enable Conductor. **Omitted / `null`** → default-ON if Redis is present (footgun-guard [ADR-048 §5](../adr/0048-conductor.md)); explicit **`false`** → Conductor does not rise; explicit **`true`** → rises (requires Redis for lease leadership, like Reaper). Disabling an individual schedule - per-Cadence `enabled: false` (ADR-046), not here. |
| `cadence_scheduler.poll_floor` | `duration` | `30s` | yes | The lower bound of the adaptive polling step (see Adaptive polling step). Coincides with the floor limit of the minimum Cadence period (same key - single source 30s). **Absolute minimum**: `< 30s` → config error `value_out_of_range` at start (sub-30s period is meaningless, reactive domain - Beacons). Empty/invalid → default. It is re-read at every tick from the latest Store snapshot. |
| `cadence_scheduler.poll_ceiling` | `duration` | `60s` | yes | Upper bound on adaptive polling step: the sparse schedule (`interval=1h`) does not stretch the polling so that the missed-slot mechanism becomes the only insurance. Invariant `poll_floor ≤ poll_ceiling` (aka `value_out_of_range`). Empty/invalid → default. Hot-reload. |
| `cadence_scheduler.poll_idle` | `duration` | `120s` | yes | Polling step with **empty enabled registry** Cadence (there is nothing to spawn - we poll less often than the corridor, we don't idly hammer PG). Invariant `poll_idle ≥ poll_ceiling` (otherwise `value_out_of_range`: idle should not be more frequent than normal polling). Empty/invalid → default. Hot-reload. |
| `cadence_scheduler.interval` | `duration` | — (alias) | yes | **Backcompat-alias** `poll_ceiling`. Before the amendment, 2026-06-07 was a fixed tick period; Now the step is adaptive, and `interval` is left only for the sake of the old `keeper.yml`. If `poll_ceiling` is **not** set → `poll_ceiling = max(interval, poll_floor)` (clamp up to floor). Sub-floor `interval` (for example dev-config with `5s`) **does not drop the config**: rises to floor with WARNING (`value_clamped`, text about Beacons for sub-30s). If both `interval` and `poll_ceiling` are specified, `poll_ceiling` wins (alias is ignored). New configs write `poll_*`, not `interval`. |
| `cadence_scheduler.lock_ttl` | `duration` | `5m` | yes | TTL Redis-lease `conductor:leader` ([ADR-006](../adr/0006-cache-redis.md)). Parity `reaper.lock_ttl`: large enough to survive the leader's temporary stall, short enough for a quick failover. renew goes to `lock_ttl/3`. Empty/`0`/invalid → default. Applicable between re-acquire leases. |

The format `poll_floor` / `poll_ceiling` / `poll_idle` / `interval` / `lock_ttl` is validated in the semantic phase of the parser (`checkDuration`, like `reaper.interval` / `acolyte_*`); invalid duration rejects the config at the start, the range (`>0`) is achieved by default. The mutual order of the corridor (`poll_floor ≥ 30s ≤ poll_ceiling ≤ poll_idle`) is checked against **resolved** values ​​(taking into account alias-clamp), therefore it also catches implicit violations through `interval`.

> **The previous dev recommendation `interval: 5s` has been cancelled.** At floor 30s, sub-30s polling is unattainable by design. For a frequent rhythm, set `poll_floor`/`poll_ceiling` to 30–60s; for a response faster than 30s is not a Cadence task, use [Beacons](../adr/0030-vigil-oracle.md) (Vigil/Oracle, ADR-030).

> **`enabled` is not a hot-reload.** Enabling/disabling Conductor is read when the instance starts (unlike `poll_floor` / `poll_ceiling` / `poll_idle` / `lock_ttl`, which are re-read on the fly). To turn off or turn on the scheduler, you need to restart the instance. This is deliberate: hot-toggle subsystems (raise/kill goroutine + lease on the fly) are a separate complexity not needed for normal operation (per-Cadence `enabled` covers operational management of schedules without restart).

## Metrics

Registered in the Prometheus-registry Keeper **only in the branch of the raised Conductor** (default-ON for Redis and not `enabled: false`) - if the Conductor is not raised, collectors are not published at all (cardinality-safe, parity Reaper). Implementation - [`keeper/internal/conductor/metrics.go`](../../keeper/internal/conductor/metrics.go), wire-up from `keeper/cmd/keeper/daemon.go`.

| Metric | Type | Tags | Meaning |
|---|---|---|---|
| `keeper_conductor_lease_held` | gauge | — | `1` if this instance runs Redis-lease `conductor:leader`, otherwise `0`. One gauge per keeper instance. Cluster-wide invariant: `sum(keeper_conductor_lease_held) == 1` (with exactly one leader). Independent of `keeper_reaper_lease_held` - holders may vary. |
| `keeper_conductor_spawn_executions_total` | counter | — | Number of Conductor leader spawn ticks per instance uptime. Increments for every tick, **regardless** of whether due schedules are found. Comparison with `spawned_total` shows "efficiency": many ticks with zero spawn = no schedules or all `skip`/`queue`. |
| `keeper_conductor_spawned_total` | counter | — | Number of Voyages **actually spawned** from matured Cadence. `skip`/`queue` ticks (the policy did not give spawn) do not go here - this is "how many runs were created", parity affected-semantics of Reaper. |
| `keeper_conductor_spawn_errors_total` | counter | — | Number of spawn tick errors (Spawner returned error: PG failure, resolve target, etc.). Selected from `spawn_executions_total` to alert without a histogram. |
| `keeper_conductor_spawn_duration_seconds` | histogram | — | Duration of one spawn tick (`Spawner.Run`). Buckets `0.005…30s` (parity reaper-rule-duration): a typical tick is units or tens of ms (SELECT due + per-row insert), the top 30s catches an abnormally long tick in a separate bucket. `_count` is the same as `spawn_executions_total`. |

**Dashboard / alert guidelines** ([ADR-048 §5](../adr/0048-conductor.md)): "leader is alive" (`sum(keeper_conductor_lease_held) == 1`), "spawn is on schedule" (non-zero `spawned_total` if there are active schedules), surge `spawn_errors_total` - abnormal situation.

## See also

- [operator-api/cadences.md](operator-api/cadences.md) - Operator API schedules (`/v1/cadences*`): creating/editing/toggle/runs Cadence that this Conductor executes.
- [reaper.md](reaper.md) - Reaper / Reaper: cleanup domain (spawn rules **no**, it's here).
- [config.md](config.md) → block `cadence_scheduler:` in `keeper.yml`.
- [ADR-048](../adr/0048-conductor.md) - Conductor design (rationale, rejected options).
- [ADR-046](../adr/0046-cadence.md) - Cadence: scheduling model, due-fetch, `overlap_policy`, recalculation `next_run_at`.
- [ADR-043](../adr/0043-voyage.md) - Voyage: what exactly Conductor spawns.
- [ADR-006](../adr/0006-cache-redis.md) - Redis-lease, single-executor, leader-election.
- [naming-rules.md → Modules and subsystems inside `keeper`](../naming-rules.md) - Conductor in the dictionary.
- [observability.md → Keeper · Conductor](../observability.md) - metrics in the general catalog.
