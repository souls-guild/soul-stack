# ADR-069. Run visibility in the Operator UI — live incarnation progress + history + a unified All Runs

> **Status: proposed (DRAFT); §DECISIONS ACCEPTED by the user — final sign-off 2026-07-04.** Design session 2026-07-04 (`NIM-37` live progress + history + `NIM-38` unified All Runs). The adjacent `NIM-36` (rendering the keeper badge) — DONE, referenced below.
>
> **User decisions (2026-07-04):** Q2 ✓ (show keeper steps), Q3 ✓ (unified All Runs), C ✓ (endpoint path), D ✓ (one ADR), B → architect. **Q1 extended**: not only live, but also **persistent history** of runs (reconstruct the progress weeks/a month later) — while "soberly assessing the load on the DB". **★FINAL SIGN-OFF 2026-07-04:** the §DECISIONS package is accepted in full, impl unblocked; **A0 RESOLVED** (coordinator): SSE auth via `fetch` streaming with an `Authorization` header — a token in the URL / `sse-token` is not needed, no rotation.
>
> **Coordination:** the backend wave is already launched (S37-1 live infrastructure ∥ S38-1 sort). The live part (SSE route, applybus, `applying_apply_id`) is valid under any Q1/A0. Persistent history (Q1b, §A5) and A0=fetch-streaming (§A3) are refinements of this revision.
>
> **DRAFT** in the file name is now kept ONLY because of promotion into the index ([README](README.md)/[architecture.md](../architecture.md)/[naming-rules](../naming-rules.md)) — up to the coordinator (see §"Registration"). **The user's veto is lifted.**
>
> **★Number agreed (2026-07-04):** `0068` was ceded to **NIM-34** (versioning `upgrade/<slug>/`, already DONE — moving something implemented is costlier than moving a DRAFT); this ADR is renumbered to **`0069`** (free in the tree). The coordinator (the NIM-24 line / the single push gate) confirms the finalization when converging on the canon — if `0069` turns out to be taken by another branch, move only this file.
>
> **Numbering.** `0069` (was `0068`, but a collision with NIM-34 upgrade versioning, which also took `0068` — ceded, see above). `0069` is free in the tree; the coordinator confirms on convergence.

---

## DECISIONS — ACCEPTED by the user (final sign-off 2026-07-04)

| # | Fork | Decision | Status |
|---|---|---|---|
| **A0** | SSE auth handshake | **fetch-streaming CHOSEN** (coordinator 2026-07-04): `fetch` sends the ordinary `Authorization: Bearer` header, the stream via `ReadableStream` → **a token in the URL / `sse-token` is not needed, no rotation**. The query-token (`POST /v1/sse-token`) is **discarded** (the pain of rotation: TTL≪stream lifetime). Cookie rejected. Backend live infra **does not depend** on auth. | ✓ fetch-streaming (Authorization header) |
| **Q1a** | Live progress (while the run is in flight) | **SSE ephemeral**: per-task via applybus `task.executed` (`task_status ∈ OK/CHANGED/FAILED/TIMED_OUT` already flows). | ✓ |
| **Q1b** | History (reconstruct the progress weeks/a month later) | **From `audit_log`** — the per-task progress is **already persisted** there (`events_taskevent.go:91` writes `task.executed` to audit, not only to live), the index `audit_log_correlation_id_idx` **already exists**, retention **configurable** (`audit.retention_days`). Needed: a UI-timeline reader + a default retention. **Zero new write load** (the data is already written). §A5. | ✓ user extended Q1; load accounted for |
| **Q2** | keeper-side steps in live | **Publish to applybus** (currently only audit, `keeper_dispatch.go:145-146`) — so that cloud/vault/registered steps are visible live under `sid="keeper"`. | ✓ "yes, show" |
| **Q3** | All Runs form | **UNIFIED**: one page with all types (`apply_run`+`voyage`+`push`+`errand`) + a filter by type + column sorting. | ✓ "much more convenient" |

**Sub-forks:**

- **A (SSE auth) — RESOLVED: fetch-streaming.** `fetch(url,{headers:{Authorization}})` + `response.body.getReader()` + manual parsing of SSE frames. An ordinary Bearer (like all requests) → **no token in the URL, no rotation**. The cost — auto-reconnect/event parsing by hand (EventSource gives them for free), acceptable. **Discarded:** a short-lived query-token (`EventSource('…?access_token=')` + `POST /v1/sse-token`) — a token in the URL settles in logs + TTL≪stream lifetime → rotation. Cookie rejected (`tokenStore.ts`).
- **B (linking incarnation→run) → architect.** Candidate: expose the already-written column `applying_apply_id` (`lockRun` [`state.go:45`](../../keeper/internal/scenario/state.go), cleared [`crud.go:849,1197`](../../keeper/internal/incarnation/crud.go)) in `GET /v1/incarnations/{name}` — non-null while the run is in flight. "The last completed" — top-1 from `GET /v1/incarnations/{name}/runs`. Implemented in S37-1; the final form is confirmed by architect.
- **C (live endpoint) ✓.** `GET /v1/incarnations/{name}/runs/{apply_id}/events` — symmetry with the `RunDetail` path + the precedent `/v1/voyages/{id}/events`.
- **D (one ADR) ✓.** Both tickets — one ADR-0069.

---

## Context (what the operator sees now)

**NIM-37.** On the incarnation page there is **no runs tab** — "History" shows `state_history` (state snapshots), not the apply progress. The operator sees neither the live nor the history of runs (which scenario ran, which task, whether anything changed). Meanwhile:

- The ready client `keeperApi.incarnations.runs(name)` → `GET /v1/incarnations/{name}/runs` (`keeper.ts:541`) **is called nowhere**.
- **A live stream already exists**: SSE `GET /mcp/events?apply_id=<ULID>` ([`sse.go:145`](../../keeper/internal/mcp/sse.go)); applybus ([`bus.go`](../../keeper/internal/applybus/bus.go)) publishes `task.executed` ([`events_taskevent.go:322`](../../keeper/internal/grpc/events_taskevent.go)) `{apply_id, sid, task_idx, task_status, passage, error?}` and `apply.completed/failed/cancelled` ([`events_runresult.go:248`](../../keeper/internal/grpc/events_runresult.go)). **There is no consumer in web**: the only `new EventSource` — `errandRuns.events` (`keeper.ts:996`), a stub, called by no one. Even the live Voyage pages (`VoyageDetail.tsx:586`, `VoyageTargets.tsx:36`) go by **polling**. → our SSE consumer is the **first authorized** one in web (a handshake from scratch).
- **History is already written** (the key fact for Q1b): each task's `task.executed` (Soul + keeper) is **persisted to `audit_log`** — [`events_taskevent.go:91`](../../keeper/internal/grpc/events_taskevent.go) (`AuditWriter.Write`, EventType `task.executed`) + keeper-side [`keeper_dispatch.go`](../../keeper/internal/scenario/keeper_dispatch.go); plus `incarnation.run_completed` with `changed_tasks[]` ([ADR-052 §k](0052-herald-notifications.md)). The `audit_log` table (migration 001) has `correlation_id` (= apply_id) **with an index** `audit_log_correlation_id_idx` and retention `purge_audit_old(max_age)` from `audit.retention_days` ([ADR-022](0022-audit-pipeline.md)). That is, a run's progress is reconstructable from the journal — only a UI reader is missing.
- **What is NOT in PG (apply_runs)**: per-task progress is not stored in the operational runs tables ([ADR-012](0012-keeper-soul-grpc.md); [`runsview.go:13-18`](../../keeper/internal/applyrun/runsview.go)) — there is only per-host status + the failed task. **This is separate storage from `audit_log`** — the journal writes per-task, the operational table does not.
- incarnation detail **does not return** the current apply_id (`applying_apply_id` is in the DB, but not in `IncarnationGetView` [`incarnation_view.go:34-51`](../../keeper/internal/api/handlers/incarnation_view.go)).
- keeper-side `task.executed` is not published to **live SSE** (only to audit; [`keeper_dispatch.go:145-146`](../../keeper/internal/scenario/keeper_dispatch.go)).

**NIM-38.** Two runs pages, different sources:

- `/runs` → `RunsFeed.tsx` = client-union `voyages+push+errands` (`:159-212`). Scenario `apply_run` — **absent**. Sorting is hardcoded `startedAt DESC`, no pagination (LIMIT=50/source).
- `/incarnation-runs` → `IncarnationRunsList.tsx` = scenario `apply_run` via `GET /v1/runs`. Server-side pagination. No column sorting.
- Backend `GET /v1/runs` ([`runs.go`](../../keeper/internal/api/handlers/runs.go)) — `status/incarnation/offset/limit`, the order is hardcoded `ORDER BY started_at DESC, apply_id DESC` ([`runsglobal.go:131-133`](../../keeper/internal/applyrun/runsglobal.go)).
- Sorting example: server-side `IncarnationsList.tsx` (`sort`/`sort_dir` in query+queryKey, `toggleSort`, `↑↓`); client-side `SoulsList.tsx` (`sortItems`/`▲▼`).

---

## Decision §A — Live progress + run history on the incarnation (NIM-37)

### A1. Linking incarnation → current run (B)

`GET /v1/incarnations/{name}` returns a nullable `applying_apply_id` (the `incarnation.applying_apply_id` column, written in `lockRun`; non-null while the run is in flight). The UI immediately knows the apply_id to subscribe to — without a race on `/v1/runs?status=applying`. "The last completed" — top-1 from `/v1/incarnations/{name}/runs`.

### A2. keeper-side task.executed → applybus (Q2)

keeper-side tasks (`on: keeper`) are executed in-process in `dispatchKeeperTasks`; `emitKeeperTaskExecuted` writes only audit. We add a **symmetric** publication to applybus (next to the audit write):

- Thread `ApplyBus *applybus.EventBus` into the `scenario.Runner` deps (the type is already in `grpc.eventStreamHandler.deps.ApplyBus`).
- Payload **identical in form to the Soul-side one** ([`events_taskevent.go:301-326`](../../keeper/internal/grpc/events_taskevent.go)): `{apply_id, kind:"task.executed", sid:"keeper", task_idx, task_status, passage}`.
- **Secret hygiene as on the Soul-side**: no `output`/`register`/`message`; on failed — only `error{code,module}`. `MaskSecrets` on the SSE write path ([`sse.go:327`](../../keeper/internal/mcp/sse.go)) — a second barrier.
- Cluster mode: `Publish` already forwards to the Redis shard channel (cross-Keeper) — no extra code.
- The audit write does not change (a separate source `changed_tasks`/Tiding).

The `apply.started` publisher is absent in the canon — not required for live (the UI starts from `applying`, renders on the first `task.executed`).

### A3. SSE endpoint of the Operator API (A, C)

A new route **`GET /v1/incarnations/{name}/runs/{apply_id}/events`** (`text/event-stream`), the stream via `applybus.Subscribe(ctx, applyID)`; frame `event:/id:/data:`, heartbeat 30s, max-lifetime 30min, stream limits (the common SSE helper from `mcp/sse.go` or a narrow duplicate).

**RBAC** — like `/mcp/events` `authorizeSSE` ([`sse.go:286`](../../keeper/internal/mcp/sse.go)): the run initiator OR `incarnation.get`/`incarnation.history`. A nonexistent/foreign apply_id → **403** (anti-enum). Backend RBAC **does not depend** on the auth method (A0).

**Auth (A0) — RESOLVED: fetch-streaming (coordinator 2026-07-04).** Backend RBAC/route do not depend on the auth method; web uses the `Authorization` header:

- **fetch-streaming (CHOSEN).** web does `fetch(url, {headers:{Authorization: Bearer <jwt>}})` and reads `response.body.getReader()` + `TextDecoderStream`, parsing SSE frames by hand. **No token in the URL** (an ordinary header, like all requests) → **no leak into logs, no rotation**. The cost: auto-reconnect and event parsing (which EventSource gives for free) are written by hand — acceptable. The backend route is the same; the middleware reads `Authorization` in the standard way (the `access_token`/`sse-token` transport is not needed).
- **short-lived query-token (DISCARDED — for the record).** `EventSource('…/events?access_token=<short-jwt>')` + `POST /v1/sse-token` (TTL ~60s). Rejected: a token in the URL settles in the reverse-proxy/access log (a security floor), and TTL 60s ≪ the stream lifetime (up to 30 min) → **rotation** (reissue/reopen) — the very pain the user pointed out. The `access_token` transport in the canon (`auth.go:80-108`, `:32`) remains, but we do not use this path.

**Why not `/mcp/events`**: the MCP plane (JSON-RPC tool-call streaming), its own auth code. Operator UI in the MCP channel = mixing planes.

### A4. Live progress (Q1a) — what is visible while the run is in flight

- **Live per-task**: SSE `task.executed` → a badge `OK/CHANGED/FAILED/TIMED_OUT` by `task_idx` (+`sid`, keeper-side `sid="keeper"`).
- **Aggregate status**: polling `GET /v1/runs`/`RunDetail` (already) + SSE `apply.completed/failed/cancelled`.
- **Reload while in flight**: the in-memory bus does not backfill past live events → fallback to per-host `RunDetail` (polling) + history from audit (§A5). Acceptable.

### A5. Run history (Q1b) — reconstruct the progress weeks/a month later

The user's goal: open a run completed a week/a month ago and see the detailed progress. **The data is already persisted** — we implement a UI reader over `audit_log`, without new storage.

- **Source** — `GET /v1/audit?correlation_id=<apply_id>` (the endpoint exists, [`handlers/audit.go`](../../keeper/internal/api/handlers/audit.go)): each task's `task.executed` (ok/changed/failed, `task_idx`, `sid`, `at`) + `incarnation.run_completed` (`changed_tasks[]`). It gives the full per-task timeline of any run within the retention bounds.
- **Reading is cheap**: the index `audit_log_correlation_id_idx` (migration 001:29-31, partial) → a single run's timeline = an index scan, not a full scan.
- **Retention configurable**: `purge_audit_old(max_age)` from `keeper.yml → audit.retention_days`. "A month ago" = retention ≥ 30 days (an operator setting, not a hardcode). Set a reasonable default + document it.
- **UI**: `RunDetail` for a completed run renders a timeline from audit (in addition to the live mode §A4 for one in flight). One screen, two sources: applybus (in flight) / audit (completed).

**Load on the DB (the user asked for sobriety):**

- **Writing — zero delta.** `task.executed` is written to audit **already today** — Q1b does not add a single INSERT, only reads.
- **Reading — cheap.** The index on `correlation_id` already exists; a single run's timeline is a narrow index scan.
- **Storage/scale.** The audit volume grows with runs×tasks×hosts. Up to ~10k VM the current audit works without optimizations. At **100k VM** audit hits its own ceiling (INSERT rate, table size) — this is an **existing** problem ([audit-scaling backlog](0022-audit-pipeline.md): partitioning + retention + hot/cold split), **not created** by this feature. "A month in detail" is achievable by tuning retention; the 100k scale inherits the general audit epic.

---

## Decision §B — Unified All Runs + sorting (NIM-38) ✓

### B1. Backend: `sort`/`sort_dir` in `GET /v1/runs`

- `AllRunsInput` ([`runs.go`](../../keeper/internal/api/handlers/runs.go)) += `Sort`, `SortDir`. Whitelist: **`started_at|finished_at|status|incarnation|scenario`**; `sort_dir ∈ asc|desc`; default `started_at`/`desc` (byte-exact). Invalid → **422**.
- `buildRunsQuery`/`ListRuns` ([`runsglobal.go:118-159`](../../keeper/internal/applyrun/runsglobal.go)): `ORDER BY` from the whitelist (the column via a `switch` over an enum, not a raw string — SQL injection). **Tie-break `, apply_id DESC`**.
- **total/offset are safe**: `sort` only in `listSQL` (131-133); `countSQL` (125) is untouched. `finished_at` nullable → `NULLS LAST`.
- Naming — `sort`/`sort_dir` (reusing `/v1/incarnations` 1:1). openapi + `npm run gen:api`.

### B2. Web: unified All Runs (Q3) + sortable columns

One page `/runs` with all 4 types; `/incarnation-runs` → redirect; Sidebar — remove the duplicate. Architecture — a **type filter**:

- A segment `All | Scenario | Voyage | Push | Errand` (`RunsFeed` already has type-chips — `+scenario`).
- **`Scenario`** (`apply_run`) — `keeperApi.runs.list()`, **server-side** sorting+pagination (B1) by the `IncarnationsList` pattern (`sort`/`sort_dir` in query+queryKey, `toggleSort`, `↑↓`, **+ offset reset** — absent in the sample, to be added).
- **`Voyage|Push|Errand`** — client-union, **client-side** sorting (`SoulsList` `▲▼`).
- **`All`** — client-union of the first N of each type (+ the 4th source apply_runs), client sorting; a caption "first N per type" (do not pass truncation off as completeness).

A single server-union endpoint (one SQL over 4 tables) — expensive (different tables/statuses/total), a **follow-up**. The current default closes the pain without it.

---

## Breakdown into implementation slices

The pipeline — `developer → review → qa (→ docs-writer)`. S37-1/S38-1 are large → an **architect-reconnaissance pattern**. Tracks NIM-37 ∥ NIM-38.

### S37-1 — backend live (NIM-37) · IN PROGRESS (backend wave)

- **Files:** `incarnation.go`(+`crud.go` SELECT) — `ApplyingApplyID *string`; `incarnation_view.go` — the field in `IncarnationGetView` + projection; `keeper_dispatch.go` — `publishKeeperTaskExecuted` + `ApplyBus` in the `Runner` deps; the new SSE route `/v1/incarnations/{name}/runs/{apply_id}/events` (under `RequireJWT`); `vendor/openapi/keeper.yaml`. `POST /v1/sse-token` is **NOT needed** (A0=fetch-streaming: web sends the `Authorization` header; if already committed in S37-1 — remove it or leave it dead, at the coordinator's discretion).
- **Guard invariants:** (1) `applying_apply_id` non-null during applying, null on terminal; (2) keeper-side publishes `task.executed` with `sid="keeper"`, the payload symmetric to the Soul-side one; (3) secret hygiene of the keeper-side SSE (no output/message, failed → `error{code,module}`); (4) SSE-route RBAC deny without `incarnation.get`; a foreign apply_id → 403; accepts `Authorization: Bearer` (fetch-streaming).
- **Dependencies:** none.

### S37-2 — web live (NIM-37)

- **Files:** `IncarnationDetail` — the "Runs" section (a list of `incarnations.runs(name)` + live status); `api/keeper.ts` — the SSE client **fetch-streaming** (`fetch`+the `Authorization` header + `response.body.getReader()`, manual parsing of SSE frames + reconnect); `RunDetail` — live per-task badges over the per-host slice.
- **Guard invariants:** (1) the section renders runs of all kinds; (2) a live event updates the badge without a reload; (3) **graceful fallback to polling** on stream interruption; (4) a reload does not break things (per-host + history from audit); (5) auth — via the `Authorization` header (the token is not in the URL).
- **Dependencies:** S37-1. A0=fetch-streaming (resolved).

### S37-3 — web run history from audit (Q1b) · NEW

- **Files:** `RunDetail` — for a completed run a timeline from `GET /v1/audit?correlation_id=<apply_id>` (`task.executed` + `run_completed`); `api/keeper.ts` — an audit-timeline client (if not yet present). Backend: confirm the default `audit.retention_days` in `keeper.yml` (≥30 days) + doc. The `correlation_id` index already exists — no new migrations/writes.
- **Guard invariants:** (1) a completed run from N days ago shows the per-task progress from audit; (2) a run beyond retention — an explicit "history expired", not an empty screen; (3) reading goes by the `correlation_id` index (not a full scan) — a test on the query plan/latency; (4) **zero new INSERTs** (Q1b only reads).
- **Dependencies:** S37-2 (the shared `RunDetail`). The backend audit endpoint — already exists.

### S38-1 — backend sort (NIM-38) · IN PROGRESS

- **Files:** `runs.go` (`Sort`/`SortDir` + validation → 422); `runsglobal.go` (`ORDER BY` from the whitelist + tie-break `apply_id DESC` + `NULLS LAST`); openapi.
- **Guard invariants:** (1) sort by 5 columns asc/desc; (2) tie-break `apply_id DESC`; (3) invalid → 422; (4) `total` independent of `sort`; (5) `finished_at NULLS LAST`; (6) default byte-exact.
- **Dependencies:** none.

### S38-2 — web consolidation + columns (NIM-38)

- **Files:** `RunsFeed.tsx` — the 4th source apply_runs + type filter `+scenario` + sortable columns (server for scenario, client for union); `App.tsx` — redirect `/incarnation-runs`→`/runs`; `Sidebar.tsx` — remove the duplicate; fold in `IncarnationRunsList.tsx`.
- **Guard invariants:** (1) scenario runs are visible in All Runs; (2) a click sorts (server in scenario — resets offset; client in union); (3) tie-break; (4) filter by type; (5) redirect; (6) "first N" are marked.
- **Dependencies:** S38-1.

---

## What is NOT included (boundaries)

- Persisting per-task in the **operational runs tables** (`apply_runs`) — not needed: history comes from `audit_log` (§A5). ADR-012 (per-task not in apply_runs) stays in force.
- A single server-union All Runs endpoint (follow-up).
- The `apply.started`/`apply.progress` publisher (not needed for live).
- Optimizing audit for 100k VM (the existing audit-scaling epic, not this feature).
- Rendering the keeper badge — **NIM-36** (DONE).
- A cookie session; MCP `/mcp/events` is not touched.

## Open questions / follow-up

- ~~A0 finalization~~ — **RESOLVED** (coordinator 2026-07-04): fetch-streaming via the `Authorization` header; query-token/`sse-token` discarded.
- **Server-union All Runs** — when client-union hits volume limits.
- **Audit-scaling for 100k** — partitioning + retention + hot/cold ([backlog](0022-audit-pipeline.md)); directly affects the depth of "run history" on large Souls.
- ~~Per-resource / one-time SSE token~~ — removed: A0=fetch-streaming, no token in the URL, the question is moot.

## Registration (confirmation received 2026-07-04 — only the coordinator's mechanics remain)

Remove `-DRAFT` → `0069-run-visibility.md`; a line in [README](README.md); a stub in [architecture.md](../architecture.md); [naming-rules](../naming-rules.md) — no new names (`events`/`sort`/`sort_dir` — existing DevOps contracts). `make check-doc-links` before the commit.

## Amends / Related

- **[ADR-012](0012-keeper-soul-grpc.md)** — per-task not in the operational `apply_runs`; **but** the per-task progress is in `audit_log` (§A5 reads it for history — it does not violate the ADR-012 boundaries).
- **[ADR-022](0022-audit-pipeline.md)** — retention of `audit_log` (`purge_audit_old`); the depth of "run history" = `audit.retention_days`.
- **[ADR-027](0027-apply-work-queue.md)** (m-S1) — `applying_apply_id` as the read-source for linking (the write path is not changed).
- **[ADR-052 §k](0052-herald-notifications.md)** — audit `task.executed`/`changed_tasks` is not touched; the applybus publication of Q2 is a separate channel alongside.
- **NIM-36** (DONE) — rendering `sid="keeper"` into a badge; this ADR is the backend prerequisite.
