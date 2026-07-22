# Conductor

A background, leader-elected subsystem inside the `keeper` binary — the executor of [Cadence](../naming-rules.md) schedules ([ADR-046](../adr/0046-cadence.md)): on each tick it selects matured schedules and spawns an ordinary [Voyage](../adr/0043-voyage.md) run. **Not a separate binary** (like Reaper). The full design record is [ADR-048](../adr/0048-conductor.md); this document is a reference for behavior and config.

> **Conductor executes, Cadence stores, Reaper cleans up.** A Cadence is a row in the Postgres table `cadences` with a run "recipe" + a repetition rule ([ADR-046](../adr/0046-cadence.md)). Conductor only **executes the trigger** — the Cadence spawn semantics (due-selection, the three `overlap_policy` values, recomputing `next_run_at`) belong to Cadence and were carried into Conductor **unchanged**. Spawning was historically (the S0 design of ADR-046) planned as a Reaper rule `spawn_due_cadence` (`action: spawn`), but [ADR-048](../adr/0048-conductor.md) (2026-06-02) moved execution into a separate subsystem: Reaper's cleanup domain (`reaper.interval` 1h) and Cadence's scheduling domain need different natural rhythms (~15–30s). After the move, Reaper is again a pure cleanup domain ([reaper.md](reaper.md)).

## Properties

- Lives inside `keeper`. Not a separate binary.
- Runs on **only one Keeper instance at a time**: the leader is elected via the Redis lease `conductor:leader` with TTL = `lock_ttl` ([ADR-006](../adr/0006-cache-redis.md)). The lease is **independent** of `reaper:leader` — the Conductor leader and the Reaper leader may be on different instances.
- Sits on the generic lease-loop primitive (`keeper/internal/leaderloop`, shared with Reaper) — two independent lease keys, two independent leader elections.
- **Adaptive poll step** (cron model, [ADR-048 "Adaptive interval"](../adr/0048-conductor.md)): the tick is NOT fixed. Before each tick the leader recomputes the step from the enabled Cadence registry — `clamp(min(periods of enabled schedules), poll_floor, poll_ceiling)`; an empty registry of enabled rules → `poll_idle` (do not poll idly). Independent of `reaper.interval` (1h). Details — [Adaptive poll step](#adaptive-poll-step).
- **Default-ON when Redis is present** (footgun guard, see [Default-ON and degradation without Redis](#default-on-and-degradation-without-redis)).
- `poll_floor` / `poll_ceiling` / `poll_idle` / `lock_ttl` (and the backcompat alias `interval`) — hot-reload without redeploying the binary (a cross-cutting requirement, see [requirements.md](../requirements.md)). `enabled` is read at startup (see [Config](#config)).
- Metrics `keeper_conductor_*` — see [Metrics](#metrics).

## What Conductor does on a tick

When an instance holds the lease `conductor:leader`, on each tick it performs the same due-selection and spawn normalized in [ADR-046 §4/§5](../adr/0046-cadence.md):

1. `SELECT … FROM cadences WHERE enabled AND next_run_at <= NOW() FOR UPDATE SKIP LOCKED` — selecting matured schedules.
2. For each due row **in a single PG transaction**: apply `overlap_policy` (`skip` / `queue` / `parallel`) → on an allowed spawn, `INSERT` into `voyages` / `voyage_targets` with `cadence_id` → recompute `next_run_at` (cron parser for `cron`, anchored `next_run_at + interval_seconds` for `interval`) → `last_run_at = NOW()`.

   **The recompute anchor is the planned slot, not `NOW()`.** For `interval`, the next run is computed from the **previous planned `next_run_at`** (the one by which the row became due), not from the actual `last_run_at`/`NOW()` — otherwise tick drift (Conductor fired a bit past the deadline) would accumulate and slots would "walk off". Anchoring to the planned grid makes the interval drift-free. See [ADR-046 §4](../adr/0046-cadence.md).

   **Missed slot after downtime (anti-storm).** If Keeper was idle and several slots were missed, `next_run_at` is advanced to the **first future grid slot strictly after `NOW()`** — one catch-up spawn for the current due, **without** re-spawning each missed slot. Symmetric for `interval` (winding `+ interval_seconds` until the slot becomes `> NOW()`) and for `cron` (the next cron slot after `NOW()`).
3. The spawned Voyage enters the ordinary Voyage lifecycle (`pending` → claimed by a VoyageWorker → `running` → terminal); Conductor does **not** drive it further (a one-shot spawn).

`FOR UPDATE SKIP LOCKED` + single-executor leadership give **exactly-one-spawn per tick** without a race between Keeper instances.

**Authorship and audit.** The child Voyage is spawned "on behalf of" the Cadence creator (`voyages.started_by_aid` inherits the Cadence's `created_by_aid`). The spawn/skip audit event (`cadence.spawned` / `cadence.skipped_overlap`) is written with **`source: background`** and `archon_aid: NULL` — this is an autonomous background initiative of the keeper, not an operator call. The `background` source is **preserved after the move into Conductor** (a new `scheduler` source is NOT introduced, see [ADR-048 → ADR-022](../adr/0048-conductor.md)). The catalog of event types — [naming-rules.md → Audit-events](../naming-rules.md#audit-events).

## Adaptive poll step

Conductor's poll step is **not fixed** ([ADR-048 "Adaptive interval"](../adr/0048-conductor.md)). Before each tick the leader derives the step from the enabled Cadence registry and clamps it into the `[poll_floor, poll_ceiling]` corridor of the "Calm" profile (30s / 60s / 120s by default):

```
derivedMinPeriod = min(periods of all enabled Cadences)   # interval rules carry interval_seconds; cron rules contribute 60s
step = clamp(derivedMinPeriod, poll_floor, poll_ceiling)
if the enabled registry is empty → step = poll_idle
```

How `derivedMinPeriod` is computed (implementation — [`cadence.MinPeriod.DerivedMinPeriod`](../../keeper/internal/cadence/crud.go), [`conductor.AdaptivePollInterval`](../../keeper/internal/conductor/poll.go)):

- **interval rules** give `MIN(interval_seconds)` over enabled rows (one lightweight aggregate SELECT `SELECT MIN(interval_seconds), bool_or(schedule_kind='cron') FROM cadences WHERE enabled`, partial index `cadences_enabled_interval_idx`).
- **cron rules** carry no `interval_seconds` (NULL, not counted in MIN), but cron granularity is a minute: if there is at least one enabled cron rule, its contribution is a fixed **60s**. To not miss the nearest cron slot, Conductor with cron rules polls at least once a minute.
- The **more frequent** of the two is taken: `min(MIN(interval_seconds), 60s if cron present)`.
- **Empty enabled registry** (neither interval nor cron rules): `derivedMinPeriod` is undefined — Conductor polls at `poll_idle` (lazy baseline). The "empty" signal is carried by the same MIN query — there is no separate Redis channel.

Why the corridor:

- **`poll_floor` (30s)** — the lower bound: a frequent schedule (`interval=5s`) will NOT make Conductor hammer PG every 5 seconds. This is the defence-in-depth backstop to the Cadence minimum-period floor limit (see below) — even if a sub-floor row bypassed the write path and the DB CHECK, polling still will not drop below 30s.
- **`poll_ceiling` (60s)** — the upper bound: a rare schedule (`interval=1h`) does not stretch polling so far that the anchored-recompute missed-slot mechanism becomes the only safeguard against a missed slot.
- **`poll_idle` (120s)** — empty registry: there is nothing to spawn, poll less often than the normal corridor, without loading PG idly.

**Failover-safe by construction.** The step is a pure function of the current enabled registry in PG, recomputed on each tick from the leader. A new leader after failover carries no in-memory polling state: the same registry → the same step. Non-leaders do not execute the aggregate SELECT (only the lease holder calls it).

**Degradation on a fetch error.** If the MIN query fails (a PG glitch), the leader does NOT crash — it falls back to `poll_ceiling` (the infrequent edge of the corridor, not the floor — so as not to hammer PG in a storm) + a warn in the log. The next tick retries the query.

**Why not event-driven.** A reactive approach (a push notification "a schedule matured") would give no gain: the downstream chain (an Acolyte pool claiming the job + EventStream to the Soul + apply on the host) throttles the precision itself. Cadence is a coarse rhythm ("once every N minutes/hours"), while the reactive sub-30s domain belongs to [Beacons](../adr/0030-vigil-oracle.md) (Vigil/Portent/Oracle, ADR-030), not Cadence.

## Cadence minimum-period floor

The minimum period of an interval Cadence is **30s** ([ADR-046](../adr/0046-cadence.md), Pass B). Creating or modifying a Cadence with `interval_seconds < 30` is rejected with **HTTP 422** and the text "the minimum Cadence period is 30s; for reaction faster than 30s use Beacons (Vigil/Oracle, ADR-030)". cron Cadences do not fall under the floor (cron granularity is a minute, ≥ 60s).

Three levels of protection (defence-in-depth):

1. **Write-path validate** ([`cadence.ValidateIntervalFloor`](../../keeper/internal/cadence/crud.go), `POST`/`PATCH /v1/cadences`) — a friendly 422 before PG. The floor is taken from the same config key `cadence_scheduler.poll_floor` as the lower polling bound (a single minimum, not a hardcoded `30` in two places).
2. **DB CHECK `cadences_interval_seconds_floor`** (`interval_seconds IS NULL OR interval_seconds >= 30`, migration 068) — a table-level invariant, catching writes that bypass the API.
3. **Pre-flight data guard** in migration 068 — before `ADD CONSTRAINT` it checks whether rows with sub-30 `interval_seconds` already exist (e.g. a dev stand with an old 10s schedule), and does `RAISE EXCEPTION` with clear text if any are found. Not a silent UPDATE: silently changing someone's schedule period is inadmissible — the operator decides (raise to 30s / switch to cron / delete).

The floor limit and the lower polling bound `poll_floor` coincide in value (30s) and in source not by accident: a sub-30s period is meaningless (downstream will not act on it any more precisely), and the reactive domain is Beacons.

## Default-ON and degradation without Redis

Conductor is enabled **by default if Redis is configured** (lease leadership is possible). This is a **footgun guard**: a Cadence without a working scheduler silently fails to spawn Voyages — the operator created a schedule and it is "dead" without a visible error. So the scheduler is active out of the box on any Redis installation.

- **Disabling a Cadence — per-Cadence** via `enabled: false` on the schedule row itself ([ADR-046 §3](../adr/0046-cadence.md)), not by globally shutting down Conductor.
- **Global shutdown** — an explicit `cadence_scheduler.enabled: false` (the operator deliberately disables the whole scheduler on this instance).
- **Without Redis** (single-instance dev without Redis) leader election is impossible — Conductor **degrades the same way as the Reaper leader on a single instance** (does not come up; the `keeper_conductor_*` metrics are not published).

## Config

The `cadence_scheduler:` block in `keeper.yml` (optional — if absent, the defaults apply + default-ON with Redis):

```yaml
cadence_scheduler:
  enabled: true        # nil/omitted → ON when Redis is configured (footgun guard); false → OFF
  poll_floor: 30s      # lower bound of the adaptive poll step ("Calm" profile)
  poll_ceiling: 60s    # upper bound of the adaptive poll step
  poll_idle: 120s      # poll step when the enabled Cadence registry is empty
  lock_ttl: 5m         # TTL of the Redis lease conductor:leader
  # interval: 60s      # backcompat alias of poll_ceiling (see below); new configs write poll_*
```

| Field | Type | Default | Hot-reload | Meaning |
|---|---|---|---|---|
| `cadence_scheduler.enabled` | `bool` (opt., tri-state) | `nil` → ON with Redis | no (read at startup) | Enabling Conductor. **Omitted / `null`** → default-ON when Redis is present (footgun guard [ADR-048 §5](../adr/0048-conductor.md)); explicit **`false`** → Conductor does not come up; explicit **`true`** → comes up (requires Redis for lease leadership, like Reaper). Disabling an individual schedule is per-Cadence `enabled: false` (ADR-046), not here. |
| `cadence_scheduler.poll_floor` | `duration` | `30s` | yes | Lower bound of the adaptive poll step (see [Adaptive poll step](#adaptive-poll-step)). Coincides with the Cadence minimum-period floor limit (the same key — a single 30s source). **Absolute minimum**: `< 30s` → config error `value_out_of_range` at startup (a sub-30s period is meaningless, the reactive domain is Beacons). Empty/invalid → default. Re-read on each tick from a fresh Store snapshot. |
| `cadence_scheduler.poll_ceiling` | `duration` | `60s` | yes | Upper bound of the adaptive poll step: a rare schedule (`interval=1h`) does not stretch polling so far that the missed-slot mechanism becomes the only safeguard. Invariant `poll_floor ≤ poll_ceiling` (otherwise `value_out_of_range`). Empty/invalid → default. Hot-reload. |
| `cadence_scheduler.poll_idle` | `duration` | `120s` | yes | Poll step when the **enabled registry is empty** (nothing to spawn — poll less often than the corridor, do not hammer PG idly). Invariant `poll_idle ≥ poll_ceiling` (otherwise `value_out_of_range`: idle must not be more frequent than normal polling). Empty/invalid → default. Hot-reload. |
| `cadence_scheduler.interval` | `duration` | — (alias) | yes | **Backcompat alias** of `poll_ceiling`. Before the 2026-06-07 amendment it was a fixed tick period; now the step is adaptive, and `interval` is kept only for old `keeper.yml`. If set and `poll_ceiling` is **not** set → `poll_ceiling = max(interval, poll_floor)` (clamped up to the floor). A sub-floor `interval` (e.g. a dev config with `5s`) **does not fail the config**: it is raised to the floor with a WARNING (`value_clamped`, text about Beacons for sub-30s). If both `interval` and `poll_ceiling` are set — `poll_ceiling` wins (the alias is ignored). New configs write `poll_*`, not `interval`. |
| `cadence_scheduler.lock_ttl` | `duration` | `5m` | yes | TTL of the Redis lease `conductor:leader` ([ADR-006](../adr/0006-cache-redis.md)). Parity with `reaper.lock_ttl`: large enough to survive a temporary leader stall, short enough for fast failover. Renewal runs at `lock_ttl/3`. Empty/`0`/invalid → default. Applied between lease re-acquires. |

The format of `poll_floor` / `poll_ceiling` / `poll_idle` / `interval` / `lock_ttl` is validated in the parser's semantic phase (`checkDuration`, like `reaper.interval` / `acolyte_*`); an invalid duration rejects the config at startup, the range (`>0`) is enforced by the default. The mutual corridor order (`poll_floor ≥ 30s ≤ poll_ceiling ≤ poll_idle`) is checked against the **resolved** values (accounting for the alias clamp), so it also catches implicit violations via `interval`.

> **The former dev recommendation `interval: 5s` is cancelled.** With a 30s floor, sub-30s polling is unattainable by design. For a frequent rhythm set `poll_floor`/`poll_ceiling` to 30–60s; for reaction faster than 30s — that is not Cadence's job, use [Beacons](../adr/0030-vigil-oracle.md) (Vigil/Oracle, ADR-030).

> **`enabled` is not hot-reload.** Enabling/disabling Conductor is read at instance startup (unlike `poll_floor` / `poll_ceiling` / `poll_idle` / `lock_ttl`, which are re-read on the fly). To shut down or bring up the scheduler, an instance restart is required. This is deliberate: hot-toggling a subsystem (bringing up/killing a goroutine + lease on the fly) is a separate complexity, not needed for routine operation (per-Cadence `enabled` covers operational schedule management without a restart).

## Metrics

Registered in Keeper's Prometheus registry **only in the branch where Conductor is up** (default-ON with Redis and not `enabled: false`) — if Conductor is not up, the collectors are not published at all (cardinality-safe, parity with Reaper). Implementation — [`keeper/internal/conductor/metrics.go`](../../keeper/internal/conductor/metrics.go), wired up from `keeper/cmd/keeper/daemon.go`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `keeper_conductor_lease_held` | gauge | — | `1` if this instance holds the Redis lease `conductor:leader`, otherwise `0`. One gauge per keeper instance. Cluster-wide invariant: `sum(keeper_conductor_lease_held) == 1` (with exactly one leader). Independent of `keeper_reaper_lease_held` — the holders may differ. |
| `keeper_conductor_spawn_executions_total` | counter | — | The number of spawn ticks of the Conductor leader over the instance uptime. Incremented on every tick, **regardless** of whether any due schedules were found. Comparison with `spawned_total` shows "efficiency": many ticks with zero spawns = no schedules or all `skip`/`queue`. |
| `keeper_conductor_spawned_total` | counter | — | The number of Voyages **actually spawned** from matured Cadences. `skip`/`queue` ticks (the policy allowed no spawn) do not count here — this is "how many runs were created", parity with Reaper's affected semantics. |
| `keeper_conductor_spawn_errors_total` | counter | — | The number of spawn-tick errors (the Spawner returned an error: PG failure, target resolution, etc.). Split out of `spawn_executions_total` so it can be alerted on without a histogram. |
| `keeper_conductor_spawn_duration_seconds` | histogram | — | The duration of a single spawn tick (`Spawner.Run`). Buckets `0.005…30s` (parity with reaper-rule-duration): a typical tick is single/tens of ms (SELECT due + per-row insert), the top 30s catches an abnormally long tick in a separate bucket. `_count` matches `spawn_executions_total`. |

**Dashboard / alert guidance** ([ADR-048 §5](../adr/0048-conductor.md)): "the leader is alive" (`sum(keeper_conductor_lease_held) == 1`), "spawning is on schedule" (nonzero `spawned_total` when active schedules exist), a spike in `spawn_errors_total` is an abnormal situation.

## See also

- [operator-api/cadences.md](operator-api/cadences.md) — the Operator API for schedules (`/v1/cadences*`): creating/editing/toggling/runs of the Cadences this Conductor executes.
- [reaper.md](reaper.md) — Reaper: the cleanup domain (there is **no** spawn rule there, it is here).
- [config.md](config.md) → the `cadence_scheduler:` block in `keeper.yml`.
- [ADR-048](../adr/0048-conductor.md) — the Conductor design (rationale, rejected alternatives).
- [ADR-046](../adr/0046-cadence.md) — Cadence: the schedule model, due-selection, `overlap_policy`, recomputing `next_run_at`.
- [ADR-043](../adr/0043-voyage.md) — Voyage: what exactly Conductor spawns.
- [ADR-006](../adr/0006-cache-redis.md) — Redis lease, single-executor, leader election.
- [naming-rules.md → Modules and subsystems inside `keeper`](../naming-rules.md) — Conductor in the vocabulary.
- [observability.md → Keeper · Conductor](../observability.md) — metrics in the general catalog.
