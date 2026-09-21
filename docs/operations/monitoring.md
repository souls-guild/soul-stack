# Monitoring & alerts

What and how to monitor in a Soul Stack installation. Full observability spec - [`docs/observability.md`](../observability.md) (ADR-024). Here is the operational part: key alerts, dashboards instructions on how to read critical metrics.

## Telemetry channels

| Channel | What does it carry | Endpoint |
|---|---|---|
| **Prometheus `/metrics`** (pull) | Keeper and Soul metrics (counters / gauges / histograms) | Keeper: `listen.metrics.addr` (default `:9090`); Soul: `metrics.listen` (default `127.0.0.1:9091`) |
| **OpenTelemetry** (push OTLP gRPC) | Trace; optional OTLP metrics (post-MVP) | `otel.endpoint` (`*:4317`) |
| **Logs** (stdout/file) | Structured JSON logs | `logging.file` or stderr (systemd journal); built-in rotation |

Full rationale for Prometheus-primary + OTel-bridge - [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge).

### Protection `/metrics`

| Binar | Protection | How to set up |
|---|---|---|
| Keeper | HTTP Basic-auth (optional) | `metrics.auth.basic` + password from `password_ref` (Vault); see [config.md → metrics](../keeper/config.md#metrics). |
| Soul | Loopback bind default + opt. Basic-auth | `metrics.listen: 127.0.0.1:9091` (default) - not accessible from outside. If you need external scrape, bind to the interface and Basic-auth via `password_file` (see [`docs/soul/config.md` → metrics](../soul/config.md#metrics)). |

## Namespace metrics

| Prefix | Who is exhibiting |
|---|---|
| `keeper_*` | Keeper-side metrics (gRPC, scenario, RBAC, render, vault, reaper, ...) |
| `soul_*` | Soul-side metrics (apply, eventstream, soulprint, beacon) |

Distinguishing by prefix, not by label. Full conventions (snake_case, `_total`/`_seconds`/`_bytes`, label-cardinality rules) - [`docs/observability.md` § 2](../observability.md). A detailed catalog of subsystems is [`docs/observability.md` § 4.1](../observability.md).

OTel resource-attributes identify the instance:
- `service.name = "keeper" | "soul"` (OTel semconv standard).
- `soulstack.kid = <KID>` (Keeper only).
- `soulstack.sid = <SID>` (Soul only).

## Key alerts

Conditional expressions - for Prometheus / alertmanager. Exact thresholds - select for installation.

### Critical (immediate attention)

| Alert | Expression | What does |
|---|---|---|
| **Keeper down** | `up{job="keeper"} == 0 for 1m` | Keeper instance is not responding. Investigate via journalctl/logs. |
| **Reaper not running** | `sum(keeper_reaper_lease_held) == 0 for 5m` | No instance supports Reaper-lease. Database cleaning has stopped. If it drags on, `bootstrap_tokens` / `apply_runs` / `audit_log` will grow. |
| **Reaper split-brain** | `sum(keeper_reaper_lease_held) > 1` | Two instances simultaneously consider themselves the Reaper leader. **Shouldn't happen** (Redis-lease with TTL). Log the incident, restart Redis. |
| **Conductor not running** | `sum(keeper_conductor_lease_held) == 0 for 5m` | No instance holds lease `conductor:leader` - [Cadence](../naming-rules.md)-schedules **do not spawn Voyage** ([conductor.md](../keeper/conductor.md), [ADR-048](../adr/0048-conductor.md)). If the scheduler is intentionally disabled (`cadence_scheduler.enabled: false` / no Redis) - the alert should not be raised (collectors are not published). |
| **Conductor split-brain** | `sum(keeper_conductor_lease_held) > 1` | Two instances in parallel consider themselves Conductor leaders. **Shouldn't happen** (Redis-lease with TTL). Log the incident, restart Redis. |
| **PG connection failure** | `keeper_postgres_connection_errors_total[5m] > 0` (when the metric appears) | Keeper cannot connect to PG. Most operations will fail. |
| **Vault unavailable** | `keeper_vault_read_errors_total{kind="error"}[5m] > 10` | Vault is not available. New operations (resolving secrets at the start) will fall; in-flight will survive. |
| **`incarnation` stuck in `applying`** | (no native metric, via SQL: `SELECT COUNT(*) FROM incarnation WHERE status = 'applying' AND (NOW() - applying_started_at) > '15 minutes'`) | Run hangs → hole owned-by-dead-instance ([ADR-027](../adr/0027-apply-work-queue.md)). With `acolytes:0` under HA - known footgun. Diagnostics — [`faq.md` → applying-stuck](faq.md). |
| **Conclave split / no presence** | (no native metric - via redis `KEYS keeper:instance:*` count) | If `KEYS` is empty, all instances have lost Redis connectivity; if less than the real number, the part is isolated. |

### Warning (triage during business hours)

| Alert | Expression | What does |
|---|---|---|
| **gRPC streams below baseline** | `keeper_grpc_streams_active < expected_souls_count * 0.95 for 10m` | The Souls part is not connected. Check: `souls.status = 'disconnected'`, reasons (network outage / Soul binary has fallen / SoulSeed has expired). |
| **Drift in `souls.status` from lease** | (requires cross-source: SQL count `souls.status = 'connected'` vs Redis `KEYS soul:*:lock` count) | Large discrepancy → `mark_disconnected` reconcile does not have time. Check Reaper logs, lock_ttl. |
| **Apply failure rate** | `rate(keeper_scenario_runs_total{result="failed"}[15m]) / rate(keeper_scenario_runs_total[15m]) > 0.05` | >5% of runs fail. Investigate scenario/hosts. |
| **Apply tasks high retry rate** | `rate(soul_apply_task_retries_total[15m]) > 0.5` | Tasks are often retracted. Unstable hosts / flaky tasks. |
| **Vault read latency** | `histogram_quantile(0.99, rate(keeper_vault_read_duration_seconds_bucket[5m])) > 1.0` | Vault responds slowly. Investigate Vault instance. |
| **RBAC snapshot stale** | `time() - keeper_rbac_snapshot_last_success_timestamp_seconds > 300` | The RBAC snapshot did not reassemble for > 5 minutes. Check `keeper_rbac_snapshot_rebuild_errors_total{kind=...}` for details. |
| **Service-registry snapshot stale** | `time() - keeper_serviceregistry_snapshot_last_success_timestamp_seconds > 300` | The same for the service registry. |
| **Augur deny rate** | `rate(keeper_augur_fetch_total{decision="denied"}[15m]) / rate(keeper_augur_fetch_total[15m]) > 0.1` | >10% of Augur requests are rejected - possibly misconfigured Omens or an escalation attempt. |
| **Sigil last delivered drops** | `keeper_sigil_anchors_last_delivered < keeper_grpc_streams_active * 0.8` | Re-broadcast trust-anchor-set did not reach most Souls - check EventStream-state before `Retire` old key. |
| **Apply timed-out rate** | `rate(soul_apply_task_timed_out_total[15m]) > 0` | Tasks started to time out. |
| **Beacon portents dropped** | `rate(soul_beacon_portents_dropped_total[15m]) > 0` | Soul loses beacon events. Beacon reactions are skipped in Oracle. |
| **Oracle circuit tripped** | `rate(keeper_oracle_circuit_tripped_total[15m]) > 0` | Some Decree fell into a loop and was auto-disabled. Investigate. |
| **Conductor spawn errors** | `rate(keeper_conductor_spawn_errors_total[15m]) > 0` | Conductor tick drops when Cadence spawns (PG failure/resolve target schedule). Schedules don't give birth to Voyage. Investigate — [conductor.md → Metrics](../keeper/conductor.md). |
| **dispatched stuck (post-recovery-enable)** | (via SQL: `SELECT COUNT(*) FROM apply_runs WHERE status = 'dispatched' AND claim_at < NOW() - INTERVAL '1 hour'`) | After enabling `reclaim_apply_runs` - lines `dispatched`, not confirmed by Soul (S6 Soul-reconcile should orphan them). If they freeze, Soul-reconcile does not work / Soul old. |

### Info (for capacity planning, not alerts)

- `keeper_grpc_apply_dispatch_total{result="ok"}` - apple throughput. Trend → planning.
- `soul_apply_duration_seconds` - duration of runs. p95/p99 → user expectations.
- `keeper_reaper_rule_purged_total{rule=*}` — cleanup volume. Trend `audit_log` retention → plan partitioning.

## Dashboards

### Keeper overview (one per cluster)

Grouping by `instance` (KID):

- `keeper_grpc_streams_active` — sum + per-instance breakdown.
- `keeper_grpc_messages_total` — rate by `direction`.
- `keeper_grpc_apply_dispatch_total` — rate by `result`.
- `keeper_scenario_runs_total` — rate by `result`.
- `keeper_scenario_run_duration_seconds` — p50/p95/p99.
- `keeper_render_duration_seconds` — p95.
- `keeper_reaper_lease_held` — gauge per-instance (sum=1).
- `keeper_reaper_rule_*` — per-rule purged + duration.
- `keeper_conductor_lease_held` — gauge per-instance (sum=1 if Conductor is raised).
- `keeper_conductor_spawned_total` / `keeper_conductor_spawn_errors_total` - Cadence spawning is on schedule / errors.
- `keeper_vault_read_duration_seconds` — p99 by `mount`.
- `keeper_rbac_checks_total` — rate by `result`.

### Soul fleet (one per coven / entire fleet)

- `soul_eventstream_connected` — sum / count Souls.
- `soul_eventstream_reconnects_total` — rate.
- `soul_apply_tasks_total` — rate by `result`.
- `soul_apply_duration_seconds` — p95.
- `soul_apply_task_skipped_total` — rate by `reason`.

### Audit / RBAC (compliance)

- `rate(keeper_rbac_checks_total{result="deny"}[5m])` — base-rate denied requests.
- SQL queries to `audit_log` - separate dashboard via PG datasource (Grafana supports PG as a source). Filter by `event_type`, `archon_aid`, `correlation_id`.

## Logs

### Format

`logging.format: json` (default) - structured JSON, parsed by any log-aggregator:

```json
{
  "time": "2026-05-26T14:30:00.123Z",
  "level": "info",
  "msg": "soul connected",
  "sid": "host-01.example.com",
  "kid": "keeper-prod-01",
  "trace_id": "...",
  "span_id": "..."
}
```

`logging.format: text` - for interactive debug via journalctl.

### Rotation

Built-in (see [config.md → logging](../keeper/config.md#logging) / [soul/config.md → logging](../soul/config.md#logging)):

```yaml
logging:
  file: /var/log/keeper/keeper.log
  rotation:
    max_size_mb: 100      # rotation when reaching 100 MB
    max_age_days: 7       # delete archives older than 7 days
    max_files: 10         # keep max 10 archives
    compress: true        # gzip
```

Archives - `<file>-<timestamp>.<ext>` next to `file`. Without dependency on `logrotate` ([requirements.md](../requirements.md): "built-in default log rotation").

### What to look for in logs during an incident

| Symptom | Rake |
|---|---|
| Soul won't connect | `bootstrap` / `mTLS` / `SoulSeed verify` / `unauthorized` |
| RBAC deny / 403 | `permission denied` / `rbac` / `denied for aid` |
| Vault resolution fell | `vault` / `kv-read` / `ErrVaultKVNotFound` |
| Hot-reload failed | `config.reload_failed` |
| Reaper rule dropped | `reaper` / `dispatch_error` |
| Apply failed | `apply` + `apply_id=<ULID>` for a specific run |
| Conclave has not registered KID | `Conclave` / `ErrConclaveKIDTaken` |

## OTel traces

End-to-end traces operator → Keeper → Soul via trace-context in `ApplyRequest.trace_context` (W3C traceparent, [ADR-012(c)](../adr/0012-keeper-soul-grpc.md) only-add). Implemented spans:

| Span | Tracer | Where |
|---|---|---|
| `scenario.run` | `keeper/scenario` | Running a scenario on Keeper. |
| `grpc.bootstrap` | `keeper/grpc` | Bootstrap RPC onboarding Soul. |
| `grpc.apply_dispatch` | `keeper/grpc` | One dispatch `ApplyRequest`. |
| `render.pipeline` | `keeper/render` | CEL+text/template-render phase. |
| `augur.request` | `keeper/augur` | Processing `AugurRequest`. |
| `sigil.anchors_reload` | `keeper/sigil` | Runtime rotation of trust-anchor-set. |
| `apply.run` | `soul/runtime` | Apply to Soul (child from `grpc.apply_dispatch`). |

Attributes carry domain identifiers (`apply_id`, `sid`, `scenario`, `incarnation`) that are prohibited in metric-labels (cardinality).

### Where to watch traces

- **dev** - Jaeger UI on `http://127.0.0.1:16686` ([`docs/dev/local-setup.md`](../dev/local-setup.md)).
- **cont** - real OTel-backend (Jaeger / Tempo / DataDog / Honeycomb / ...), endpoint at `otel.endpoint`.

### Sampling

`ParentBased(AlwaysSample)` is hardcoded (at the time of writing). Configurable sampler - at the first real request ([`docs/observability.md` § 5](../observability.md)). For a production installation with high traffic, sampling will have to be configured via OTel-collector (tail-based sampling in the collector).

## Capacity planning

Triggers to consider scaling:

| Metric | Trigger | Solution |
|---|---|---|
| `keeper_grpc_streams_active` per-instance | >5000 on one instance | Scale-out: more Keeper instances; LB rebalances (see [`scaling.md`](scaling.md)). |
| `keeper_scenario_run_duration_seconds.p99` | grows for no apparent reason | Investigate render stage (`keeper_render_duration_seconds`), then PG (claim-bottleneck). |
| `keeper_reaper_rule_duration_seconds{rule="purge_audit_old"}` | grows | `audit_log` is growing, consider partitioning. |
| Table size `apply_runs` / `state_history` / `audit_log` | grows beyond planning | See retention in [`infra.md`](infra.md). |
| OTel collector dropped spans | >0 | Increase collector resources or add sampling. |

## See also

- [`docs/observability.md`](../observability.md) - full regulatory observability spec (ADR-024).
- [`docs/keeper/reaper.md` → Metrics](../keeper/reaper.md) - `keeper_reaper_*`.
- [`docs/keeper/conductor.md` → Metrics](../keeper/conductor.md) - `keeper_conductor_*`.
- [`docs/keeper/config.md` → metrics / otel / logging](../keeper/config.md) - configuration of observability blocks.
- [`docs/soul/config.md` → metrics / otel / logging](../soul/config.md) - the same for Soul.
- [`faq.md`](faq.md) - typical triage scenarios.
