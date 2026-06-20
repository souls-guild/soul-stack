package grpc

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// handleTaskEvent — обработчик payload-а [keeperv1.TaskEvent] (M2.4).
//
// По PM-decision (3): пишем единственный audit-event `task.executed` с
// `source: soul_grpc`, `correlation_id = apply_id`, payload-ом — статус,
// task_idx, error (если есть) и register_data (если есть). Сами register_data
// маскируются общим [audit.MaskSecrets] на write-path-е (auditpg).
//
// no_log-suppression: для задачи с TaskEvent.no_log=true (эхо RenderedTask.no_log,
// apply.proto) register_data и error.message в audit НЕ пишутся — это корень утечки
// произвольного секрета, который MaskSecrets по vault-ref не ловит. В payload едет
// маркер suppressed:"no_log". Подавление строго по эхо-флагу, без обращения к
// []RenderedTask: на multi-Keeper (ADR-002) этот TaskEvent мог прийти не на тот
// инстанс, что держит run-goroutine.
//
// TaskStatus enum (включая `TASK_STATUS_CANCELLED`) сериализуется в payload
// через `Status().String()` единым полем `status` — расширение enum-а
// (CANCELLED, SKIPPED, …) обрабатывается без отдельных веток. Отдельный
// audit-event `task.cancelled` — post-MVP (фильтр по `payload->>'status'`
// в `audit_log` покрывает практический use-case без удвоения enum-а).
//
// Сохранение в `incarnation.run_state` (PM-decision 3 «optional») — пост-MVP:
// в MVP run-time-state не материализуется отдельной таблицей, итог фиксируется
// одним движением через `RunResult` (см. [handleRunResult]).
func (h *eventStreamHandler) handleTaskEvent(ctx context.Context, sid, sessionID string, ev *keeperv1.TaskEvent) {
	if ev == nil {
		h.logger.Warn("eventstream: TaskEvent payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	// no_log-задача: register_data (params/output) и error.message (= stderr) —
	// корень утечки произвольного секрета, который MaskSecrets по vault-ref не
	// ловит. Подавляем их в долгоживущем audit. Решение строго по эхо-флагу
	// TaskEvent.no_log (apply.proto): []RenderedTask держит run-goroutine, а этот
	// TaskEvent на multi-Keeper (ADR-002) мог прийти на другой инстанс. Маркер
	// suppressed:"no_log" кладёт сам helper.
	noLog := ev.GetNoLog()
	in := audit.TaskExecutedInput{
		SID:     sid,
		ApplyID: ev.GetApplyId(),
		TaskIdx: int(ev.GetTaskIdx()),
		// plan_index (ADR-056 §S1 fix Variant B): ГЛОБАЛЬНЫЙ сквозной индекс по всему
		// плану (= RenderedTask.Index) — ключ корреляции CHANGED-задачи с планом в
		// auditpg.SelectChangedTaskKeys (state_changes-whitelist + audit). Локальный
		// TaskIdx под staged/per-host-where ≠ глобальному. N=1 → plan_index==task_idx.
		PlanIndex: int(ev.GetPlanIndex()),
		Status:    ev.GetStatus().String(),
		NoLog:     noLog,
		Passage:   int(ev.GetPassage()),
	}
	if e := ev.GetError(); e != nil {
		in.Error = &audit.TaskExecutedError{
			Code:    e.GetCode(),
			Module:  e.GetModule(),
			Message: e.GetMessage(),
		}
	}
	if rd := ev.GetRegisterData(); rd != nil && !noLog {
		// google.protobuf.Struct → JSON через protojson — единственный способ
		// корректно сериализовать NullValue / NumberValue / nested-Struct.
		// Bytes сразу в payload — auditpg-writer прокинет через MaskSecrets.
		// no_log → register_data не пишем вовсе (произвольный секрет в output).
		if b, err := protojson.Marshal(rd); err != nil {
			h.logger.Warn("eventstream: register_data marshal failed",
				slog.String("sid", sid),
				slog.String("apply_id", ev.GetApplyId()),
				slog.Any("error", err),
			)
		} else {
			in.RegisterData = string(b)
		}
	}
	payload := audit.BuildTaskExecutedPayload(in)

	if err := h.deps.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: ev.GetApplyId(),
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: audit write task.executed failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Any("error", err),
		)
	}

	h.recordTaskFailure(ctx, sid, ev)
	h.accumulateRegister(ctx, sid, ev)
	h.publishTaskExecuted(sid, ev)
}

// recordTaskFailure фиксирует причину падения первой упавшей задачи хоста в
// строке `apply_runs` (BUG-3): индекс задачи, имя модуля и текст
// `TaskError.Message`. Так оператор по `GET /v1/incarnations/<name>` видит
// конкретный шаг и причину (`task 0 core.pkg.installed: E: Version '7.2.4' not
// found`), а не голый `RUN_STATUS_FAILED`.
//
// Хранилище — Postgres (НЕ in-memory): TaskEvent мог прийти не на тот
// Keeper-инстанс, что держит run-goroutine (ADR-002); общая таблица переживает
// cross-Keeper-роутинг. first-failure-wins обеспечивает [applyrun.RecordTaskFailure]
// (COALESCE) — гонок при нескольких упавших задачах нет.
//
// Маскинг: error_summary читается наружу через barrier/status_details (GET
// incarnation, без маскинга на том канале), поэтому MaskSecrets применяется
// здесь, на write-path-е — vault-ref / секрет-shaped значения в stderr задачи
// не утекут. Для no_log-задачи (эхо TaskEvent.no_log, apply.proto) message
// (= stderr) может нести произвольный plaintext-секрет, MaskSecrets его не
// ловит — поэтому summary целиком заменяется нейтральным «(no_log task failed)»
// прямо здесь, на write-path-е. Это defense-in-depth: run-goroutine
// (scenario.dispatch) делает то же, держа []RenderedTask с NoLog, но на
// multi-Keeper (ADR-002) мог оказаться на другом инстансе; floor в dispatch
// сохраняется.
//
// Срабатывает только на FAILED/TIMED_OUT (TaskError заполнен лишь там, см.
// apply.proto). ApplyRunDB=nil (unit без PG / ad-hoc push) → no-op.
// ErrApplyRunNotFound (push без scenario-runner-а / TaskEvent опередил Insert)
// → log+skip: причина пропадёт, но апплай-стрим не валим.
func (h *eventStreamHandler) recordTaskFailure(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if h.deps.ApplyRunDB == nil {
		return
	}
	if !isFailedStatus(ev.GetStatus()) {
		return
	}
	taskIdx := int(ev.GetTaskIdx())
	if taskIdx < 0 {
		return
	}

	// no_log-задача: error.message (= stderr) может нести произвольный plaintext-
	// секрет, который MaskSecrets по vault-ref не ловит. На write-path-е (источник
	// error_summary) подавляем его нейтральным текстом — defense-in-depth, не
	// полагаясь на floor в run-goroutine (scenario.dispatch), который держит
	// []RenderedTask и на multi-Keeper мог оказаться на другом инстансе (ADR-002).
	var summary string
	if ev.GetNoLog() {
		summary = fmt.Sprintf("task %d %s: (no_log task failed)", taskIdx, ev.GetError().GetModule())
	} else {
		summary = composeTaskErrorSummary(taskIdx, ev.GetError())
	}
	// passage (ADR-056): причина падения пишется в строку (apply_id, sid, passage)
	// этого Passage; N=1 → 0. Soul эхает passage из ApplyRequest.
	//
	// plan_index (ADR-056 §S1 fix Variant B): ГЛОБАЛЬНЫЙ сквозной индекс упавшей
	// задачи по всему плану (= RenderedTask.Index). Пишется в apply_runs.
	// failed_plan_index — ключ корреляции module/action упавшей задачи на сборке
	// DriftReport (checkdrift.buildHostReport) и no_log-подавления в barrier
	// (dispatch.failureReason). Локальный taskIdx (поле task_idx) под staged/
	// per-host-where ≠ глобальному — на корреляцию с планом не годится (тот же
	// дефект, что register-канал закрыл миграцией 079). N=1 → plan_index==task_idx.
	if err := applyrun.RecordTaskFailure(ctx, h.deps.ApplyRunDB, ev.GetApplyId(), sid, int(ev.GetPassage()), taskIdx, int(ev.GetPlanIndex()), summary); err != nil {
		h.logger.Warn("eventstream: record task failure failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Int("task_idx", taskIdx),
			slog.Int64("plan_index", int64(ev.GetPlanIndex())),
			slog.Any("error", err),
		)
	}
}

// isFailedStatus — true для терминальных статусов задачи, при которых заполнен
// TaskError (FAILED / TIMED_OUT, см. apply.proto). TIMED_OUT — частный случай
// failed.
func isFailedStatus(s keeperv1.TaskStatus) bool {
	return s == keeperv1.TaskStatus_TASK_STATUS_FAILED || s == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
}

// composeTaskErrorSummary строит operator-facing строку причины падения задачи:
// `task <idx> <module>: <message>`. message пропускается через MaskSecrets
// (vault-ref / секрет-shaped значения из stderr не утекают в наблюдаемый
// канал). Пустой module/message опускаются, чтобы не плодить `task 3 : `.
func composeTaskErrorSummary(taskIdx int, te *keeperv1.TaskError) string {
	module := ""
	message := ""
	if te != nil {
		module = te.GetModule()
		message = maskString(te.GetMessage())
	}

	head := fmt.Sprintf("task %d", taskIdx)
	if module != "" {
		head += " " + module
	}
	if message == "" {
		return head
	}
	return head + ": " + message
}

// maskString прогоняет одиночную строку через [audit.MaskSecrets] (vault-ref /
// секрет-shaped substring → ***MASKED***). audit публикует маскинг только для
// map-payload-а, поэтому оборачиваем строку в map и достаём обратно.
func maskString(s string) string {
	if s == "" {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"v": s})
	if v, ok := masked["v"].(string); ok {
		return v
	}
	return s
}

// accumulateRegister копит register_data задачи в `apply_task_register`
// (миграция 022): scenario-runner после барьера читает накопленное и строит
// RenderInput.Register per-host для рендера state_changes.sets (слайс 2,
// orchestration.md §7.1).
//
// register_name тут не известен (proto несёт только task_idx, ADR-012(d)) —
// храним по task_idx, имя резолвит scenario-runner при чтении из своих
// []RenderedTask. Хранилище — Postgres (НЕ in-memory): на multi-Keeper
// (ADR-002) этот TaskEvent мог прийти не на тот инстанс, что держит
// run-goroutine; общая таблица переживает cross-Keeper-роутинг.
//
// ApplyRunDB=nil (unit-сборка без PG / ad-hoc push) → no-op. Пустой
// register_data (задача без register:) → no-op. Ошибка записи только
// логируется: register-канал best-effort на уровне аккумуляции, недостающую
// строку scenario-runner трактует как отсутствие register-значения; провал
// этого write-а не должен валить апплай-стрим.
func (h *eventStreamHandler) accumulateRegister(ctx context.Context, sid string, ev *keeperv1.TaskEvent) {
	if h.deps.ApplyRunDB == nil {
		return
	}
	rd := ev.GetRegisterData()
	if rd == nil {
		return
	}
	if err := applyrun.UpsertTaskRegister(ctx, h.deps.ApplyRunDB, &applyrun.TaskRegister{
		ApplyID: ev.GetApplyId(),
		SID:     sid,
		// plan_index (ADR-056 §S1 fix Variant B): ГЛОБАЛЬНЫЙ сквозной индекс задачи
		// по всему плану (все Passage) — ключ register-корреляции. На нём ключуется
		// apply_task_register (миграция 079); task_idx (локальная позиция в
		// ApplyRequest своего Passage) неуникален между Passage и между хостами,
		// поэтому ключом быть не может. N=1 / старый Soul → plan_index=0=task_idx.
		PlanIndex:    int(ev.GetPlanIndex()),
		TaskIdx:      int(ev.GetTaskIdx()),
		RegisterData: rd.AsMap(),
		// passage (ADR-056): register копится per-(apply_id, sid, passage). FK на
		// apply_runs требует совпадения passage со строкой задания этого Passage.
		Passage: int(ev.GetPassage()),
	}); err != nil {
		h.logger.Warn("eventstream: accumulate register_data failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Int64("task_idx", int64(ev.GetTaskIdx())),
			slog.Any("error", err),
		)
	}
}

// publishTaskExecuted транслирует TaskEvent в SSE-канал через applybus.
// Pure best-effort: ApplyBus=nil (single-Keeper dev без SSE) → no-op.
//
// Payload — SSE-контракт: snake_case-ключи, фиксированные в
// docs/keeper/mcp-tools.md → § SSE event payloads.
//
// Подавление raw stderr на operator-SSE (BUG-3 floor): флаг `no_log` живёт в
// []RenderedTask у run-goroutine (scenario.dispatch), а НЕ в proto TaskEvent
// (ADR-012(d)); на multi-Keeper (ADR-002) этот TaskEvent мог прийти не на тот
// инстанс, что держит run-goroutine — значит grpc-слой здесь не знает no_log-
// статус задачи. Поэтому для УПАВШЕЙ задачи `error.message` (= stderr, может
// нести plaintext-пароль no_log-задачи, который MaskSecrets по vault-ref не
// ловит) в SSE НЕ кладётся вовсе: фрейм несёт только code/module для триажа.
// Детальную безопасную причину оператор получает через `status_details`/GET
// (там подавление no_log + двойной MaskSecrets, см. scenario.failureReason).
// Симметрично «(no_log task failed)» в dispatch, но строже — floor для всех
// упавших задач, без зависимости от cross-Keeper-проброса состояния прогона.
//
// Для НЕ-упавших задач (ok/changed) `error` отсутствует (TaskError заполнен
// лишь на FAILED/TIMED_OUT, см. apply.proto), полезные поля статуса сохраняются.
// Финальный MaskSecrets на SSE-write-path-е (writeSSEEvent) остаётся как
// второй барьер для register/state_changes-секретов по vault-ref/ключам.
//
// no_log-задача дополнительно несёт маркер suppressed:"no_log" — чтобы клиент
// видел причину «тихого» фрейма (error без message), а не трактовал как пропажу.
func (h *eventStreamHandler) publishTaskExecuted(sid string, ev *keeperv1.TaskEvent) {
	if h.deps.ApplyBus == nil {
		return
	}
	idx := ev.GetTaskIdx()
	payload := map[string]any{
		"apply_id":    ev.GetApplyId(),
		"kind":        string(applybus.KindTaskExecuted),
		"sid":         sid,
		"task_idx":    idx,
		"task_status": ev.GetStatus().String(),
		// passage (ADR-056): индекс Passage staged-render. 0 = единственный Passage.
		"passage": ev.GetPassage(),
	}
	// Маркер намеренного подавления для UX (SSE и так флорит error.message и не
	// кладёт register_data; маркер — чтобы клиент видел причину «тихого» фрейма).
	if ev.GetNoLog() {
		payload["suppressed"] = "no_log"
	}
	if e := ev.GetError(); e != nil {
		// message (stderr) намеренно не транслируется на SSE: см. doc-comment.
		payload["error"] = map[string]any{
			"code":   e.GetCode(),
			"module": e.GetModule(),
		}
	}
	h.deps.ApplyBus.Publish(applybus.Event{
		ApplyID: ev.GetApplyId(),
		Kind:    applybus.KindTaskExecuted,
		Payload: payload,
	})
}
