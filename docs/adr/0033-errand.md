# ADR-033. Errand — pull-ad-hoc exec вне scenario.

**Контекст.** Pull-стек (ADR-002, ADR-012) умеет применять scenario к incarnation; push-стек (ADR-032) умеет ad-hoc multi-host destiny по SSH. **Pull-ad-hoc exec одиночной команды через уже стоящий Soul-agent** (uptime / cat /proc/… / status конкретного сервиса / разовый shell-fix) — выпадает из обеих веток: incarnation/scenario-обвязка тут избыточна и семантически неверна (incarnation.state не мутируется), push через SSH требует креденшелов и обходит уже доверенный mTLS-канал. Нужен **отдельный pull-ad-hoc контур** «дёрнуть один модуль на конкретном SID через EventStream и получить результат».

**Решение.**
1. **Сущность — `Errand`** (пер. «поручение / разовое задание»). Operator → Keeper → конкретный Soul → один модуль с ограниченным input-ом → результат назад. **НЕ scenario, НЕ apply, НЕ incarnation-bound.** Имя зафиксировано propose-and-wait.
2. **Whitelist модулей — capability + список.** Errand-runner Soul-side зовёт `SoulModule.Apply` **только** если модуль удовлетворяет одному из:
   - жёсткий список `core.cmd.shell` / `core.exec.run` (verb-модули, by-design не мутируют incarnation.state);
   - реализует marker-интерфейс `ErrandReadSafe` в sdk/module/ (по аналогии с PlanReadSafe из ADR-031(f), default-deny).
   Любой другой модуль → reject ДО вызова с error code `errand_module_not_allowed`.
3. **Sync/Async — гибрид.** POST /v1/souls/{sid}/exec блокируется до timeout_seconds (default 30s, server-cap 300s). При cap → 202 + errand_id + Location: /v1/errands/{errand_id} + продолжение в background-горутине. GET /v1/errands/{errand_id} — async polling.
4. **State-инвариант.** **Errand НЕ мутирует incarnation.state.** Структурно: (a) whitelist (shell/exec не пишут в state; ErrandReadSafe модули декларируют read-safe-semantics); (b) Errand-runner НЕ агрегирует state_changes и НЕ шлёт RunResult (вместо — отдельный ErrandResult); (c) Incarnation вообще не трогается (нет apply_id в apply_runs, нет state-commit Keeper-ом).
5. **RBAC — собственная область `errand.*`.** Permissions: `errand.run` / `errand.cancel` (post-MVP) / `errand.list`. Селекторы: `host=<sid>` / `coven=<label>`; bare — unrestricted scope.

**Контракт.**

| Endpoint | Метод | Body | Ответ |
|---|---|---|---|
| `/v1/souls/{sid}/exec` | POST | `{module, input, timeout_seconds?, dry_run?}` | sync: 200 + ErrandResult; async-эскалация: 202 + {errand_id} + Location: |
| `/v1/errands/{errand_id}` | GET | — | 200 + ErrandResult либо 202 + {status: running} |
| `/v1/errands` | GET | filter `sid=`/`status=`/`started_after=` | 200 + {items[]}, RBAC-фильтр |

- `module` — fully-qualified `<ns>.<name>.<state>` (core.cmd.shell / core.exec.run / другие read-safe).
- `input` — JSON-объект, валидируется против input_schema модуля (фаза input-validation, templating.md).
- `timeout_seconds` — `1..300`, default 30.
- `dry_run` — bool, только для модулей с `PlanReadSafe`; для verb-модулей (shell/run/probe) → 400 `errand_dry_run_unsupported`.

**proto-only-add.** Файл `proto/keeper/v1/errand.proto`:
- `ErrandRequest{errand_id, module, input(Struct), timeout_seconds, dry_run}` — only-add в `FromKeeper.oneof`.
- `ErrandResult{errand_id, status, exit_code, stdout, stderr, stdout_truncated, stderr_truncated, duration_ms, error_message, output(Struct)}` — only-add в `FromSoul.oneof`.
- `CancelErrand{errand_id}` — only-add в `FromKeeper.oneof` (slice E5, field 11).
- `ErrandStatus` enum: UNSPECIFIED(0) / RUNNING(1) / SUCCESS(2) / FAILED(3) / TIMED_OUT(4) / CANCELLED(5) / MODULE_NOT_ALLOWED(6).

**PG-таблица `errands`** (см. схему миграции).

**Cross-keeper routing — reuse инфры.**
- Request: outbound:<sid> pub/sub (existing).
- Reply: errand-события идут через шардированный applybus-канал `events:shard:<n>` ([ADR-006](0006-cache-redis.md) c.1); forward-loop отсеивает чужие applyID по `envelope.ApplyID`. TODO-rename `apply:<errand_id>` → `events:*` выполнен в S2 (per-applyID-канал свёрнут к фиксированному множеству шардов).

**Audit-events.** `errand.invoked` / `errand.completed` / `errand.failed` / `errand.timed_out` / `errand.cancelled` — последний пишется и при инициации cancel-а оператором (`source: api`/`mcp`, slice E5), и при получении терминала `ErrandResult{CANCELLED}` от Soul (`source: soul_grpc`); UI группирует по `correlation_id=errand_id`.

**Reaper-rule `purge_old_errands`** (TTL 7 дней, конфигурируется `reaper.errands.ttl`).

**Render-context для Errand input.** Доступно: `vault(...)`. Недоступно: `register.*`, `incarnation.state.*`, `essence.*`, `soulprint.self.*`, `soulprint.hosts` (Errand не имеет scenario/incarnation/cross-host runtime).

**Инварианты.** Errand НЕ мутирует incarnation.state; whitelist enforced на Soul-side (defense-in-depth); sync-первичный, async-эскалация; output cap 64 KiB / channel; secret-masking output.

**Отвергнутые альтернативы.** (а) reuse ApplyRequest (drift), (б) всё через RBAC selector (мягкая граница), (в) чистый async (избыточно для uptime), (г) отдельный gRPC RPC (ломает ADR-012).

**Что отложено.** per-module Errand-mapper; Errand для keeper-side core.

**Amendment 2026-05-27. Errand E5 closure — slice complete.** CancelErrand RPC реализован: новое `CancelErrand{errand_id}` message в `proto/keeper/v1/errand.proto`, добавлено в `FromKeeper.oneof` (field 11, only-add); `DELETE /v1/errands/{errand_id}` с permission `errand.cancel` (selector NoSelector — SID известен только после lookup-а строки); MCP-tool `keeper.errand.cancel` (catalog count 58 → 59); soulctl `errand cancel <id>`. Cancel-flow: Keeper-side `errand.Dispatcher.Cancel` читает row, проверяет `status='running'` (409 `errand-not-cancellable` на терминал), отправляет CancelErrand Soul-у через `Outbound.SendCancelErrand`/`PublishCancelErrand` (local/remote по lease-у), пишет audit `errand.cancelled` (initiated). Soul-side `errandrunner.Runner` ведёт `active map[errand_id]→cancel-fn`, при получении CancelErrand вызывает Cancel(id) → ctx.Cancel → Run возвращает `ErrandResult{CANCELLED}` тем же EventStream-каналом, applybus-receiver переводит строку в `status=cancelled` через MarkTerminal. Series E1–E5 закрыта. Что отложено: L3a/L3b/L3c cancel-test в Vigil-harness (требует cancel-helper после M2.5 harness extension).

**Amendment 2026-05-29 (Voyage `command`-kind переиспользует Errand-инфру).** [ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) (Voyage) `kind=command` **переиспользует** whitelist и marker-интерфейс этого ADR: жёсткий список `core.cmd.shell` / `core.exec.run` + `ErrandReadSafe` (defense-in-depth на Soul-side per-Errand сохраняется). State-инвариант (НЕ мутирует `incarnation.state`) переносится в Voyage без изменений. **Single-SID `/v1/souls/{sid}/exec` остаётся sugar** — short-form для разового exec на одном хосте, без обвязки Voyage/`runs` (как раньше Errand без ErrandRun). Multi-target — через `kind=command` Voyage. RBAC `command`-kind = `errand.run` ([ADR-043](0043-voyage.md#adr-043-voyage--унифицированный-батчевый-прогон) пункт 6).
