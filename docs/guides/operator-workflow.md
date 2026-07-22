# Day-2: Operator duty cycle

This guide is the next step after [first-service.md](first-service.md). There you assembled a service, registered it and created an incarnation; here you **exploit** it: you run it again, check the drift, target part of the fleet, upgrade the version, scale it and sort out incidents.

This is a task-oriented guide: each section is a specific task "how to do X" with a real team. The full normative grammar is in the links, and the runbook level of operation (HA, recovery, sizing) is in [docs/operations/](../operations/README.md). This guide is a bridge between them: what an operator does on a normal day without going into SRE details.

## Prerequisites

Everything from [first-service.md](first-service.md): worker Keeper, Archon token in `TOKEN`, incarnation `hello-demo` under coven `demo`. CLI `soulctl` - thin wrapper over the Operator API ([ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)); where the option is not available in the CLI, a direct API call is shown.

> **About real covens.** dev-stand from [getting-started.md](../getting-started.md) raises a fleet under coven `demo`. The targeting examples below include `prod` / `staging` / `dev` - this is an illustration of a multi-environment fleet of 9 souls: covens are simply stable labels that you assign to hosts during onboarding ([ADR-008](../adr/0008-coven-stable-tags.md)). At your stand, insert `demo`.

## 1. Rerun/update status

Creating an incarnation runs the `create` script once. To run the script again (the same `create` with a new `input` or another service operation) on an **existing** incarnation - `soulctl incarnation run`:

```sh
# repeat create with new greeting (re-apply: hosts will converge to new state)
soulctl incarnation run hello-demo create --input '{"greeting":"hi again"}' --wait
```

`--wait` polls the status until apply completes and prints the final `status` + `history_id`. Without `--wait`, the command returns `apply_id` immediately (the operation is asynchronous). Flags:

- `--input '<json>'` — script input (validated by Keeper against the `input:` contract **before** launch);
- `--dry-run` — run the script in dry-run mode without mutations (see section 2 - this is the mechanics of Scry);
- `--wait-timeout 5m` is the waiting ceiling for `--wait`.

Under the hood it's `POST /v1/incarnations/{name}/scenarios/{scenario}` ([operator-api/incarnations.md](../keeper/operator-api/incarnations.md)). Changing the input of the incarnation is a re-run with the new `--input`: state will be rewritten only after success on **all** hosts (cross-host barrier, [orchestration.md → §7](../scenario/orchestration.md)), otherwise the incarnation goes to `error_locked` (section 6).

View what is recorded in state and the history of runs:

```sh
soulctl incarnation get hello-demo          # spec / state / status / covens
soulctl incarnation history hello-demo      # state_history: apply_id / scenario / who launched
```

Summary of all ways to start work (single run / batch via Voyage / push) - [run-flavors.md](../keeper/run-flavors.md).

## 2. Checking drift (check-drift / Scry)

**What is drift.** Between runs, someone could change the host manually, another system, or the service itself rewrote its config - and until the next apply, no one will know about it. **Scry** ([ADR-031](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)) answers the question "would at least one resource be reapplied if we ran reconcile right now." Regulatory: **drift = dry-run reconcile would show `changed=true` on at least one resource** for **declared** resources. This is a declarative drift, and **not** a full host dump: Scry sees the discrepancy only in what is declared in the script, unmanaged resources are invisible to it by design.

**What you need from the service.** The drift check reads `scenario/converge/main.yml` - this is the "desired state" of the service. In [first-service.md](first-service.md) this script is already located next to `create` (section "Service repo layout"). If there is no file, `check-drift` will return `422 ErrConvergeMissing` and will not touch the incarnation ([faq.md](../operations/faq.md)).

**How to start:**

```sh
soulctl incarnation check-drift hello-demo
```

`converge` parameters are auto-resolved: for each input, the value is taken from the `--input` override → `incarnation.state.<name>` → default schema (this gives "auto-drift from state" without manual transmission). The command is synchronous, CLI timeout is 5 minutes (Soul bypasses the entire script in Plan mode). The API equivalent is `POST /v1/incarnations/{name}/check-drift`.

**How to read the result.** The CLI prints a summary and a table by host:

```
incarnation: hello-demo
scenario:    converge
checked_at:  2026-06-16T12:30:00Z
summary:     drifted=1 clean=3 unsupported=0 failed=0

SID                STATUS    TASKS_DRIFTED
host-01.internal   drifted   1/2
host-02.internal   clean     0/2
```

Per-host `status`: `clean` (everything matches), `drifted` (at least one resource showed `changed`), `unsupported` (module without read-safe-Plan - for example verb modules `core.exec.run`/`core.cmd.shell`, they do not have a "desired state", this normal, not an error), `failed` (Plan fell). For a complete per-task analysis (`idx`/`module`/`action`/`changed`/`message`), take `-o json`.

**What to do with drift.** Status of `drift` on incarnation **informational, non-blocking** ([ADR-031(d)](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile)): it does not block incarnation. Remediation - normal apply: run `converge` as an operational script, the hosts will converge to the declaration, the status will return to `ready`:

```sh
soulctl incarnation run hello-demo converge --wait
```

(`converge` is an operational script with a dual role: like `run` it actually reduces hosts to a state, like target `check-drift` it does a declarative dry-run, [ADR-031 amendment 2026-06-10](../adr/0031-scry-drift.md#adr-031-scry--drift-detection-declarative-dry-run-reconcile).) If you prefer to repeat the original operation - `run hello-demo create --input ...` (section 1). There is a background periodic scan, but it is turned off by default (`reaper.scry_background.enabled=false`) - it is turned on by a conscious decision in production.

## 3. Targeting by fleet

By default, `run` without `on:` in the step hits **the entire incarnation** - all **member** hosts (via the membership relation `incarnation_membership`; `incarnation.name` is not a Coven, [ADR-008 amendment 2026-07-17](../adr/0008-coven-stable-tags.md#amendment-2026-07-17-nim-124-incarnationname-is-not-a-coven--membership-is-a-first-class-relation)). To drive only part of the fleet, there are two mechanisms ([orchestration.md → §3–§4](../scenario/orchestration.md)):

**Stable target is `on:` (covens).** In the scenario, the step targets the intersection of stable covens, ⊆ incarnation:

```yaml
tasks:
  - name: Apply only on prod hosts
    module: core.file.present
    on: [prod]            # 4 hosts with coven prod out of 9 in the fleet
    params: { path: /tmp/marker, content: "prod only" }
```

Covens - stable logical labels of the host (environment / data center / project), host role **not Coven** ([ADR-008](../adr/0008-coven-stable-tags.md)). What covens souls have can be seen in `soulctl souls list` (or `GET /v1/souls`).

**Volatile predicate - `where:` (late-binding by facts).** Per-host filter, which is calculated at the time of running by host facts (`soulprint.self.*`) or by the result of the probe step (`register.*`). This is late-binding: a new host that appeared in coven at the time of the run automatically falls under the predicate; it does not need to be listed:

```yaml
tasks:
  - name: Debian family only
    module: core.pkg.present
    where: "soulprint.self.os.family == 'debian'"
    params: { name: nginx }
```

The difference between the positions of the `where:` step key and the `soulprint.where(...)` function is [orchestration.md → §4](../scenario/orchestration.md). The complete list of facts for predicates (os-family, pkg_mgr, primary_ip, ...) is [soulprint.md](../soul/soulprint.md).

> **Cross-incarnation targeting is prohibited by grammar** - `on:`/`where:` only hit the hosts of their incarnation. Data about other run hosts is via `soulprint.hosts` ([orchestration.md → §4.1](../scenario/orchestration.md)).

## 4. Upgrade the service version

The service version is the git-ref (tag or branch) under which its files are committed; there is no `version:` field in the manifest ([ADR-007](../adr/0007-versioning-git-ref.md)). Upgrading an incarnation to a new version - `POST /v1/incarnations/{name}/upgrade` with a target `to_version` (this operation is not available in `soulctl` - via the API):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations/hello-demo/upgrade \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"to_version": "v2.0.0"}'
```

Reply `202 Accepted` with `apply_id` is an asynchronous operation. `to_version` - any git-ref (tag `v2.0.0` or branch `main`); if it matches the current version of the incarnation, Keeper will return a "nothing to upgrade" error.

**When `state_schema_version` changes.** If a new version of the service raised `state_schema_version` (breaking change in the `incarnation.state` structure), upgrade will apply the `migrations/<NNN>_to_<MMM>.yml` migrations atomically in one PG transaction: on failure - rollback and status `migration_failed` (section 6). This is an **explicit statement step**, not lazy; `state_history` stores snapshot per-change for recovery. Backup before upgrade with schema change, rollback and migration form - [operations/upgrade.md → State_schema migrations](../operations/upgrade.md#state_schema-migrations) and DSL standard grammar ([migrations.md](../migrations.md), [ADR-019](../adr/0019-state-migration-dsl.md#adr-019-state_schema-migration-dsl)).

Available refs for upgrade (tags + service branches) - `GET /v1/services/{name}/refs`.

## 5. Scale - add host to coven

Adding a soul to the fleet is onboarding a new Soul (as in [getting-started.md → Step 6](../getting-started.md)): register a host with the necessary covens, issue a bootstrap token, apply `soul init` on the host. On the dev stand this is done by `make dev-souls` (re-raises the fleet in the database registry, covens are saved).

Key for day-2: **the new host is picked up by late-binding automatically**. If you onboarded a host in coven `prod`, then:

- the next run with `where:`/`on: [prod]` (section 3) will enable it without editing the script - the target will resolve at the time of run;
- regular scheduled runs (**Cadence**, [ADR-046](../adr/0046-cadence.md)) and event-driven reactions (**Vigil**, [ADR-030](../adr/0030-vigil-oracle.md)) will cover it on the next tick - both resolve the target at the time of triggering, and not at the time of setting.

That is, after onboarding a new host, you don't need to edit anything in the services/schedules - the right covens are enough for the soul. Fleet sizing, Acolyte pool for load, target scale 100k VM, balancing at scale-out - [operations/scaling.md](../operations/scaling.md).

## 6. Incidents - `error_locked` and unlocking

If apply fails on at least one host, the incarnation goes to **`error_locked`** and is blocked: new runs on it will not work until you explicitly unlock it. This is by design - so that the discrepancy does not accumulate silently ([architecture.md → Atomicity and error_locked](../architecture.md)). Close blocking statuses: `migration_failed` (state migration failed during upgrade, section 4), `destroy_failed` (destroy failed).

**Where to see what happened:**

```sh
soulctl incarnation get hello-demo          # status: error_locked
soulctl incarnation history hello-demo      # last runs (who/when/scenario)
# transaction audit log:
curl -s "http://127.0.0.1:8080/v1/audit?correlation_id=<apply_id>" \
  -H "Authorization: Bearer $TOKEN"
```

**How to unlock.** `POST /v1/incarnations/{name}/unlock` with the required field `reason` (this command is not in `soulctl` - via API):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations/hello-demo/unlock \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
-d '{"reason": "checked host-01 manually, the file was created, the reason was a full disk - cleaned"}'
```

- **`reason` is required and limited to 500 characters** (`ReasonMaxLen`, considered in Unicode runes - the Cyrillic alphabet fits completely). Blank or longer - `422`. This is recorded in the audit, so write to the point: what you checked and why it is safe to remove the lock.
- The response `200` returns `previous_status` (`error_locked`) and a new `status`. After unlocking, the incarnation again accepts runs - but **unlocking does not fix the host**: first eliminate the cause, then unlock and run the script again (section 1).

Triage of typical problems (`apply` hangs in `applying`, souls in `disconnected`, `409 already applying`) - [operations/faq.md](../operations/faq.md). Stuck incarnations/`apply_runs` after a disaster and recovery - [operations/disaster-recovery.md → Adjustment after recovery](../operations/disaster-recovery.md).

## 7. What to watch

Prometheus metrics - on `:9090/metrics`, namespace `keeper_*` (Keeper-side) and `soul_*` (Soul-side). In normal day-2 practice, keep track of a few things:

- **`keeper_grpc_streams_active`** — how many souls are online; drop below expected number = part of the fleet has fallen off (check `souls.status = 'disconnected'`).
- **Apply failure rate** — `rate(keeper_scenario_runs_total{result="failed"}[15m]) / rate(keeper_scenario_runs_total[15m])`; growth = runs fail, incarnations go to `error_locked`.
- **`keeper_reaper_lease_held`** - must be exactly `1` across the cluster; `0` = database cleaning is up, `>1` = split-brain.
- **`keeper_conductor_lease_held`** - the same for the Conductor leader ([ADR-048](../adr/0048-conductor.md)); Cadence schedules will be spawned by the scheduler (`0` = schedules will not spawn Voyage).

OTel traces cover the end-to-end path of the run. A complete list of metrics, ready-made alerts (Critical/Warning/Info), dashboards and where to view traces - [operations/monitoring.md](../operations/monitoring.md).

## 8. What's next

- **More service operations** - add scripts `scenario/<op>/main.yml`, see [first-service.md → What's next](first-service.md).
- **Run orchestration** (`serial:` / `run_once:` / probe-idiom, batch via Voyage) - [orchestration.md](../scenario/orchestration.md), [run-flavors.md](../keeper/run-flavors.md).
- **Cluster operation** (HA, rolling upgrade Keeper/Soul fleet, disaster recovery, sizing) - [operations/](../operations/README.md).
- **RBAC and Archons** (roles, scoped-visibility, permission `incarnation.unlock`/`upgrade`/`check-drift`) - [operations/bootstrap-rbac.md](../operations/bootstrap-rbac.md), [keeper/rbac.md](../keeper/rbac.md).
