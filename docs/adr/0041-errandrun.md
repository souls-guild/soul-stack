# ADR-041. ErrandRun — multi-target обвязка над Errand.

> **Статус: Superseded by [ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) (Voyage), 2026-05-29.**
>
> **Реализация ErrandRun УДАЛЕНА (Wave 5, 2026-05-30).** Таблица `errand_runs` дропнута migration `062`; пакеты `internal/errandrun` + `internal/errandrunorch` (вкл. ErrandRunWorker), эндпоинты `/v1/errand-runs`, proto-сообщения `EventErrandRun*` и audit-семейство `errand_run.*` удалены из кода. Multi-target command-прогон теперь живёт в Voyage `kind=command` ([ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон)): батч = N хостов, `incarnation.state` не трогается; сохранены whitelist [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario), RBAC `errand.run`, claim+lease failover, snapshot-scope. Audit-семейство `errand_run.*` семантически заменено `command_run.*`.
>
> **Superseded только multi-target ErrandRun-обвязка.** Single **Errand** ([ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario): `POST /v1/souls/{sid}/exec`, audit `errand.*`) **НЕ удалён** — остаётся sugar для разового exec на одном хосте без обвязки Voyage/`runs`.
>
> **Реализация (multi-target) удалена — см. [ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон).** Ниже — исходная фиксация решения (запись истории, не действующий контракт).

**Контекст.** ADR-033 ввёл `Errand` как single-SID pull-ad-hoc exec. Pattern usage:
оператор хочет «uptime на coven=prod-eu» (~50 хостов) или «rm /var/cache/foo на
glob web-*` (~200 хостов). Сейчас это N отдельных POST /v1/souls/{sid}/exec —
нет общего ULID-а для аудита, нет cancel-all, нет общего summary. Симметрия с
Tide↔Surge ([ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override)): `Surge` — unit-of-work внутри `Tide`; здесь нужен
parallel-аналог.

**Решение.**

1. **Сущность `ErrandRun`** (top-level entity над N `Errand`). Имя
   propose-and-wait пройден 2026-05-27 (отвергнуты `Convoy`/`Dispatch`).
2. **Связь:** `ErrandRun` → N `Errand` через FK `errands.errand_run_id`
   (NULLABLE — single-target POST /v1/souls/{sid}/exec остаётся sugar
   без ErrandRun).
3. **Endpoint `POST /v1/errand-runs`** body `{module, input, target:{sids?,
   coven?, where?, concurrency?}, timeout_seconds?, on_failure?}`. Резолв
   target — **AND-merge** (наследует security-инвариант [ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override): invocation
   сужает scope, не расширяет; RBAC-selector через `errand.run` обязательный).
4. **Whitelist модулей — тот же [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)** (`core.cmd.shell` / `core.exec.run` /
   `ErrandReadSafe`-marker). State-инвариант не нарушается.
5. **Concurrency** — per-host fan-out с semaphore-cap (default 50, max 500).
   В отличие от Tide-Surge — нет sequential waves, все Errand-ы стартуют
   параллельно под cap-ом.
6. **PG-table `errand_runs`** (схема — slice E6-1).
7. **Failure-policy `on_failure`** — `continue` (default) / `abort`. abort
   = первый Errand-failed → cancel остальных pending. parity Tide.FailureAbort.
8. **Async по умолчанию.** Sync-эскалация (parity [ADR-033 §3](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)) для single-Errand
   ErrandRun-а — нет. Multi-target ErrandRun — всегда 202 + `errand_run_id` +
   SSE-progress.
9. **RBAC** — переиспользует `errand.run` (selector `coven=` / `host=` /
   bare). Новых permissions не вводим.
10. **Audit-events** — `errand_run.invoked` / `errand_run.completed` /
    `errand_run.failed` / `errand_run.cancelled` область `errand_run.*` (новая
    область audit-catalog).
11. **Reaper-rule** — переиспользуем `purge_old_errands` (TTL каскадит на
    `errand_runs` через FK).
12. **Cancel** — `DELETE /v1/errand-runs/{id}` отменяет все `running` Errand-ы
    через существующий `CancelErrand` ([ADR-033 amendment 2026-05-27](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario), slice E5).

**Имена.**

| Имя | Роль |
|---|---|
| `ErrandRun` | top-level invocation-instance над N Errand |
| `Errand` | unit-of-work (без изменений, [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)) |
| `ErrandRunWorker` | claim-loop orchestrator (`keeper/internal/errandrunorch/`) |
| `errand_runs` | PG-table |
| `errands.errand_run_id` | nullable FK |

**Инварианты.**

- ErrandRun НЕ мутирует incarnation.state (наследует [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario) invariant).
- AND-merge target-резолва (наследует [ADR-040](0040-tide.md#adr-040-tide--invocation-time-scope-chunking--target-override) security invariant).
- Whitelist enforced на Soul-side per-Errand (defense-in-depth [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario)).
- Один ErrandRun → один ULID → группировка в UI/audit/SSE.
- Single-target POST /v1/souls/{sid}/exec продолжает работать без
  ErrandRun-обвязки (errand_run_id NULL).

**Отвергнутые альтернативы.**

- (а) Имена `Convoy`/`Dispatch` — см. PM-decisions 2026-05-27.
- (б) Reuse Tide для ad-hoc multi-target — отвергнуто: Tide bound to
  incarnation+scenario; Errand by-design out-of-incarnation.
- (в) N independent Errand-ов без parent — нет cancel-all, нет общего audit.

**Что отложено.**

- per-coven concurrency-cap (sub-pools) — добавится при первом запросе.
- ErrandRun для keeper-side core (parallel [ADR-033](0033-errand.md#adr-033-errand--pull-ad-hoc-exec-вне-scenario) «что отложено»).
- Hybrid sync-первичный для ErrandRun с N=1 — async-by-default проще.

**Slice-карта.** E6-0 (этот ADR + словарь) → E6-1 (PG-schema) → E6-3
(errandrunorch) → E6-4 (HTTP+MCP) → E6-6 (audit) → C1 (soulctl) → W1 (UI
Wizard). E6-5 (CEL glob) — параллельно.
