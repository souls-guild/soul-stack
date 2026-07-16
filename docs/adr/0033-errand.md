# ADR-033. Errand — pull-ad-hoc exec outside a scenario.

**Context.** The pull stack (ADR-002, ADR-012) can apply a scenario to an incarnation; the push stack (ADR-032) can do ad-hoc multi-host destiny over SSH. **Pull-ad-hoc exec of a single command through an already-running Soul agent** (uptime / cat /proc/… / status of a specific service / one-off shell fix) falls outside both branches: the incarnation/scenario scaffolding is redundant here and semantically wrong (incarnation.state is not mutated), and push over SSH requires credentials and bypasses the already-trusted mTLS channel. We need a **separate pull-ad-hoc path**: "invoke a single module on a specific SID through EventStream and get the result back."

**Decision.**
1. **Entity — `Errand`** (transl. "an errand / a one-off task"). Operator → Keeper → a specific Soul → a single module with a restricted input → result back. **NOT a scenario, NOT an apply, NOT incarnation-bound.** The name is fixed via propose-and-wait.
2. **Module whitelist — capability + list.** The Soul-side Errand runner calls `SoulModule.Apply` **only** if the module satisfies one of:
   - the hard-coded list `core.cmd.shell` / `core.exec.run` (verb modules, by design they do not mutate incarnation.state);
   - implements the marker interface `ErrandReadSafe` in sdk/module/ (by analogy with PlanReadSafe from ADR-031(f), default-deny).
   Any other module → reject BEFORE the call with error code `errand_module_not_allowed`.
3. **Sync/Async — hybrid.** POST /v1/souls/{sid}/exec blocks up to timeout_seconds (default 30s, server-cap 300s). On cap → 202 + errand_id + Location: /v1/errands/{errand_id} + continuation in a background goroutine. GET /v1/errands/{errand_id} — async polling.
4. **State invariant.** **Errand does NOT mutate incarnation.state.** Structurally: (a) whitelist (shell/exec do not write to state; ErrandReadSafe modules declare read-safe semantics); (b) the Errand runner does NOT aggregate state_changes and does NOT send a RunResult (instead — a separate ErrandResult); (c) the Incarnation is not touched at all (no apply_id in apply_runs, no state-commit by the Keeper).
5. **RBAC — its own area `errand.*`.** Permissions: `errand.run` / `errand.cancel` (post-MVP) / `errand.list`. Selectors: `host=<sid>` / `coven=<label>`; bare — unrestricted scope.

**Contract.**

| Endpoint | Method | Body | Response |
|---|---|---|---|
| `/v1/souls/{sid}/exec` | POST | `{module, input, timeout_seconds?, dry_run?}` | sync: 200 + ErrandResult; async escalation: 202 + {errand_id} + Location: |
| `/v1/errands/{errand_id}` | GET | — | 200 + ErrandResult or 202 + {status: running} |
| `/v1/errands` | GET | filter `sid=`/`status=`/`started_after=` | 200 + {items[]}, RBAC filter |

- `module` — fully-qualified `<ns>.<name>.<state>` (core.cmd.shell / core.exec.run / other read-safe ones).
- `input` — a JSON object, validated against the module's input_schema (the input-validation phase, templating.md).
- `timeout_seconds` — `1..300`, default 30.
- `dry_run` — bool, only for modules with `PlanReadSafe`; for verb modules (shell/run/probe) → 400 `errand_dry_run_unsupported`.

**proto-only-add.** File `proto/keeper/v1/errand.proto`:
- `ErrandRequest{errand_id, module, input(Struct), timeout_seconds, dry_run}` — only-add to `FromKeeper.oneof`.
- `ErrandResult{errand_id, status, exit_code, stdout, stderr, stdout_truncated, stderr_truncated, duration_ms, error_message, output(Struct)}` — only-add to `FromSoul.oneof`.
- `CancelErrand{errand_id}` — only-add to `FromKeeper.oneof` (slice E5, field 11).
- `ErrandStatus` enum: UNSPECIFIED(0) / RUNNING(1) / SUCCESS(2) / FAILED(3) / TIMED_OUT(4) / CANCELLED(5) / MODULE_NOT_ALLOWED(6).

**PG table `errands`** (see the migration schema).

**Cross-keeper routing — reuse of infra.**
- Request: outbound:<sid> pub/sub (existing).
- Reply: errand events go through the sharded applybus channel `events:shard:<n>` ([ADR-006](0006-cache-redis.md) c.1); the forward loop filters out foreign applyIDs by `envelope.ApplyID`. The TODO-rename `apply:<errand_id>` → `events:*` was done in S2 (the per-applyID channel was collapsed to a fixed set of shards).

**Audit events.** `errand.invoked` / `errand.completed` / `errand.failed` / `errand.timed_out` / `errand.cancelled` — the last one is written both when an operator initiates a cancel (`source: api`/`mcp`, slice E5) and when the terminal `ErrandResult{CANCELLED}` is received from the Soul (`source: soul_grpc`); the UI groups by `correlation_id=errand_id`.

**Reaper rule `purge_old_errands`** (TTL 7 days, configured via `reaper.errands.ttl`).

**Render context for Errand input.** Available: `vault(...)`. Not available: `register.*`, `incarnation.state.*`, `essence.*`, `soulprint.self.*`, `soulprint.hosts` (an Errand has no scenario/incarnation/cross-host runtime).

**Invariants.** Errand does NOT mutate incarnation.state; the whitelist is enforced Soul-side (defense-in-depth); sync-first, async escalation; output cap 64 KiB / channel; secret-masking of output.

**Rejected alternatives.** (a) reuse ApplyRequest (drift), (b) everything through an RBAC selector (a soft boundary), (c) pure async (redundant for uptime), (d) a separate gRPC RPC (breaks ADR-012).

**Deferred.** per-module Errand mapper; Errand for keeper-side core.

**Amendment 2026-05-27. Errand E5 closure — slice complete.** The CancelErrand RPC is implemented: a new `CancelErrand{errand_id}` message in `proto/keeper/v1/errand.proto`, added to `FromKeeper.oneof` (field 11, only-add); `DELETE /v1/errands/{errand_id}` with permission `errand.cancel` (selector NoSelector — the SID is known only after the row lookup); MCP tool `keeper.errand.cancel` (catalog count 58 → 59); soulctl `errand cancel <id>`. Cancel flow: the Keeper-side `errand.Dispatcher.Cancel` reads the row, checks `status='running'` (409 `errand-not-cancellable` on a terminal state), sends CancelErrand to the Soul via `Outbound.SendCancelErrand`/`PublishCancelErrand` (local/remote depending on the lease), writes audit `errand.cancelled` (initiated). The Soul-side `errandrunner.Runner` maintains `active map[errand_id]→cancel-fn`; on receiving CancelErrand it calls Cancel(id) → ctx.Cancel → Run returns `ErrandResult{CANCELLED}` over the same EventStream channel, and the applybus receiver moves the row to `status=cancelled` via MarkTerminal. Series E1–E5 is closed. Deferred: L3a/L3b/L3c cancel-test in the Vigil harness (requires a cancel helper after the M2.5 harness extension).

**Amendment 2026-05-29 (Voyage `command`-kind reuses the Errand infra).** [ADR-043](0043-voyage.md#adr-043-voyage--unified-batch-run) (Voyage) `kind=command` **reuses** the whitelist and marker interface of this ADR: the hard-coded list `core.cmd.shell` / `core.exec.run` + `ErrandReadSafe` (the Soul-side per-Errand defense-in-depth is preserved). The state invariant (does NOT mutate `incarnation.state`) carries over into Voyage unchanged. **Single-SID `/v1/souls/{sid}/exec` remains sugar** — a short form for a one-off exec on a single host, without the Voyage/`runs` scaffolding (as Errand used to be without an ErrandRun). Multi-target — through `kind=command` Voyage. RBAC for `command`-kind = `errand.run` ([ADR-043](0043-voyage.md#adr-043-voyage--unified-batch-run) item 6).
