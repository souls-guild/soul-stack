# ADR-041. ErrandRun — multi-target wrapper over Errand.

> **Status: Superseded by [ADR-043](0043-voyage.md#adr-043-voyage--unified-batch-run) (Voyage), 2026-05-29.**
>
> **The ErrandRun implementation is REMOVED (Wave 5, 2026-05-30).** The `errand_runs` table was dropped in migration `062`; packages `internal/errandrun` + `internal/errandrunorch` (incl. ErrandRunWorker), endpoints `/v1/errand-runs`, proto messages `EventErrandRun*`, and the audit family `errand_run.*` were removed from the code. Multi-target command runs now live in Voyage `kind=command` ([ADR-043](0043-voyage.md#adr-043-voyage--unified-batch-run)): batch = N hosts, `incarnation.state` is not touched; the whitelist [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario), RBAC `errand.run`, claim+lease failover, and snapshot-scope are preserved. The audit family `errand_run.*` is semantically replaced by `command_run.*`.
>
> **Only the multi-target ErrandRun wrapper is Superseded.** The single **Errand** ([ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario): `POST /v1/souls/{sid}/exec`, audit `errand.*`) is **NOT removed** — it remains sugar for a one-off exec on a single host without the Voyage/`runs` wrapper.
>
> **The (multi-target) implementation is removed — see [ADR-043](0043-voyage.md#adr-043-voyage--unified-batch-run).** Below is the original decision record (a historical record, not an active contract).

**Context.** ADR-033 introduced `Errand` as single-SID pull-ad-hoc exec. Pattern usage:
an operator wants "uptime on coven=prod-eu" (~50 hosts) or "rm /var/cache/foo on
glob web-*" (~200 hosts). Currently this is N separate POST /v1/souls/{sid}/exec calls —
there is no shared ULID for auditing, no cancel-all, no shared summary. Symmetry with
Tide↔Surge ([ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)): `Surge` is a unit-of-work inside `Tide`; here we need a
parallel analog.

**Decision.**

1. **Entity `ErrandRun`** (top-level entity over N `Errand`). Name
   propose-and-wait passed 2026-05-27 (`Convoy`/`Dispatch` rejected).
2. **Link:** `ErrandRun` → N `Errand` via FK `errands.errand_run_id`
   (NULLABLE — single-target POST /v1/souls/{sid}/exec remains sugar
   without ErrandRun).
3. **Endpoint `POST /v1/errand-runs`** body `{module, input, target:{sids?,
   coven?, where?, concurrency?}, timeout_seconds?, on_failure?}`. Target
   resolution — **AND-merge** (inherits the security invariant [ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override): invocation
   narrows scope, does not widen it; RBAC selector via `errand.run` is mandatory).
4. **Module whitelist — the same as [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)** (`core.cmd.shell` / `core.exec.run` /
   `ErrandReadSafe` marker). The state invariant is not violated.
5. **Concurrency** — per-host fan-out with a semaphore cap (default 50, max 500).
   Unlike Tide-Surge — no sequential waves, all Errands start in
   parallel under the cap.
6. **PG table `errand_runs`** (schema — slice E6-1).
7. **Failure policy `on_failure`** — `continue` (default) / `abort`. abort
   = first Errand failed → cancel the remaining pending ones. parity Tide.FailureAbort.
8. **Async by default.** Sync escalation (parity [ADR-033 §3](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)) for a single-Errand
   ErrandRun — none. Multi-target ErrandRun — always 202 + `errand_run_id` +
   SSE progress.
9. **RBAC** — reuses `errand.run` (selector `coven=` / `host=` /
   bare). No new permissions are introduced.
10. **Audit events** — `errand_run.invoked` / `errand_run.completed` /
    `errand_run.failed` / `errand_run.cancelled`, the `errand_run.*` area (a new
    area of the audit catalog).
11. **Reaper rule** — we reuse `purge_old_errands` (TTL cascades to
    `errand_runs` via FK).
12. **Cancel** — `DELETE /v1/errand-runs/{id}` cancels all `running` Errands
    via the existing `CancelErrand` ([ADR-033 amendment 2026-05-27](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario), slice E5).

**Names.**

| Name | Role |
|---|---|
| `ErrandRun` | top-level invocation instance over N Errand |
| `Errand` | unit-of-work (unchanged, [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)) |
| `ErrandRunWorker` | claim-loop orchestrator (`keeper/internal/errandrunorch/`) |
| `errand_runs` | PG table |
| `errands.errand_run_id` | nullable FK |

**Invariants.**

- ErrandRun does NOT mutate incarnation.state (inherits the [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario) invariant).
- AND-merge of target resolution (inherits the [ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override) security invariant).
- Whitelist enforced on the Soul side per-Errand (defense-in-depth [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)).
- One ErrandRun → one ULID → grouping in UI/audit/SSE.
- Single-target POST /v1/souls/{sid}/exec continues to work without the
  ErrandRun wrapper (errand_run_id NULL).

**Rejected alternatives.**

- (a) Names `Convoy`/`Dispatch` — see PM decisions 2026-05-27.
- (b) Reuse Tide for ad-hoc multi-target — rejected: Tide is bound to
  incarnation+scenario; Errand is by-design out-of-incarnation.
- (c) N independent Errands without a parent — no cancel-all, no shared audit.

**Deferred.**

- per-coven concurrency cap (sub-pools) — will be added on first request.
- ErrandRun for keeper-side core (parallel to [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario) "Deferred").
- Hybrid sync-first for ErrandRun with N=1 — async-by-default is simpler.

**Slice map.** E6-0 (this ADR + dictionary) → E6-1 (PG schema) → E6-3
(errandrunorch) → E6-4 (HTTP+MCP) → E6-6 (audit) → C1 (soulctl) → W1 (UI
Wizard). E6-5 (CEL glob) — in parallel.
