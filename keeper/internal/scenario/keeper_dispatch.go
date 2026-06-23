package scenario

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// dispatchKeeperTasks исполняет keeper-side задачи прогона (`on: keeper`,
// docs/keeper/modules.md) ЛОКАЛЬНО на этом keeper-инстансе — без work-queue/Soul.
// Контракт исполнения симметричен Soul-side пути (events_taskevent.go /
// events_runresult.go), но без сети: модуль вызывается in-process через
// keeper-side core-Registry, его ApplyEvent-ы собираются in-proc-стримом и
// агрегируются в те же таблицы прогона:
//   - apply_runs (apply_id, sid=[render.KeeperTargetSID]) — терминал success/failed;
//   - apply_task_register — register-результат задачи (loadRegisterByHost его читает);
//   - error_summary — причина падения (RecordTaskFailure), как у Soul-side задач.
//
// Все keeper-задачи прогона делят ОДНУ apply_runs-строку sid=keeper (модель
// «один row на (apply_id, sid)»): по task_idx они различаются в
// apply_task_register, а строка прогона keeper-target-а success ⇔ все keeper-
// задачи прошли. Первая упавшая keeper-задача → строка failed + возврат ошибки
// (run() уходит в abort, host-fan-out не стартует).
//
// Исполняется ДО host-dispatch-а (run.go): keeper-шаги в реальных сценариях идут
// первыми (provision/coven-bind → затем apply на хостах, см.
// redis/create). Cross-task chaining keeper-register → host-render
// в пилоте недоступен (in.Register пуст — future), поэтому порядок «keeper, потом
// hosts» функционально достаточен.
//
// keeperPlans пуст → no-op (прогон без keeper-side задач — обычный Soul-side путь).
func (r *Runner) dispatchKeeperTasks(ctx context.Context, spec RunSpec, log *slog.Logger, tasks []*render.RenderedTask, plans []render.DispatchPlan) error {
	keeperTasks := keeperTasksOf(tasks, plans)
	if len(keeperTasks) == 0 {
		return nil
	}
	if r.keeperModules == nil {
		return ErrKeeperModulesNotConfigured
	}

	// Одна строка apply_runs на keeper-target прогона: вставляем running до первого
	// исполнения. Терминал (success/failed) — после прохода по всем keeper-задачам.
	if err := applyrun.Insert(ctx, r.deps.DB, &applyrun.ApplyRun{
		ApplyID:         spec.ApplyID,
		SID:             render.KeeperTargetSID,
		IncarnationName: spec.IncarnationName,
		Scenario:        spec.ScenarioName,
		Status:          applyrun.StatusRunning,
		StartedByAID:    startedByPtr(spec.StartedByAID),
	}); err != nil {
		return fmt.Errorf("scenario: insert keeper apply_run: %w", err)
	}

	for _, rt := range keeperTasks {
		changed, failed, output, msg := r.applyKeeperTask(ctx, rt)
		log.Info("scenario: keeper-side задача исполнена",
			slog.String("module", rt.Module),
			slog.Int("task_idx", rt.Index),
			slog.Bool("changed", changed),
			slog.Bool("failed", failed))

		// task.executed для КАЖДОЙ keeper-задачи — симметрично Soul-side handler-у
		// (events_taskevent.go). Без него changed-keeper-задачи (on: keeper) не
		// попадают в свёртку changed_tasks (auditpg) и task:-подписка Tiding на них
		// молча мёртвая (ADR-052 §k). Эмитим ДО failed-return, чтобы упавшая
		// keeper-задача тоже отметилась в журнале (status FAILED, не CHANGED).
		r.emitKeeperTaskExecuted(ctx, spec.ApplyID, rt, changed, failed, msg, log)

		if failed {
			summary := composeKeeperFailure(rt, msg)
			// keeper-target — single-passage (passage 0): keeper-side задачи
			// исполняются ДО host-fan-out-а одной строкой apply_runs (ADR-056 не
			// дробит keeper-target по Passage). rt.Index — глобальный сквозной
			// индекс по плану; для keeper-target локальный==глобальный (нет
			// per-host where:), поэтому task_idx и plan_index совпадают (=rt.Index).
			if rerr := applyrun.RecordTaskFailure(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, 0, rt.Index, rt.Index, summary); rerr != nil {
				log.Warn("scenario: запись причины падения keeper-задачи провалена",
					slog.Int("task_idx", rt.Index), slog.Any("error", rerr))
			}
			if uerr := applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, 0, applyrun.StatusFailed, &summary); uerr != nil {
				log.Warn("scenario: перевод keeper apply_run в failed провален",
					slog.Any("error", uerr))
			}
			return fmt.Errorf("scenario: keeper-side задача %q (%s) провалена: %s", rt.Name, rt.Module, msg)
		}

		// register: keeper-задачи аккумулируется под KeeperTargetSID — симметрично
		// Soul-side accumulateRegister. Без register: (rt.Register=="") / no_log
		// задача в накопитель не пишется (loadRegisterByHost их и так не резолвит).
		r.accumulateKeeperRegister(ctx, spec.ApplyID, rt, changed, failed, output, log)
	}

	if err := applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, 0, applyrun.StatusSuccess, nil); err != nil {
		return fmt.Errorf("scenario: перевод keeper apply_run в success: %w", err)
	}
	return nil
}

// emitKeeperTaskExecuted пишет audit-событие task.executed для keeper-side
// задачи — симметрично Soul-side handler-у (events_taskevent.go). Это
// единственный источник, по которому свёртка changed_tasks (auditpg) и
// task:-подписка Tiding (ADR-052 §k) видят keeper-задачи (on: keeper): без него
// changed-задача keeper-target-а молча выпадала из run_completed.changed_tasks.
//
// Форма payload — общий [audit.BuildTaskExecutedPayload] (та же, что Soul-side),
// чтобы свёртка (payload->>'sid'/'task_idx'/'status') одинаково видела обе
// стороны. sid = render.KeeperTargetSID (адрес keeper-target-а прогона),
// correlation_id = apply_id (совпадает с фильтром SelectChangedTaskKeys).
//
// Секрет-гигиена: register_data/output в payload НЕ кладётся (keeper-задачи могут
// нести vault-резолвленный output); message — только на провале и только для
// не-no_log (для no_log подавляется helper-ом). SSE/applybus НЕ публикуется —
// keeper-side прогресс на operator-SSE не идёт (только запись в audit).
//
// Audit=nil (unit-сборка без аудита) → no-op. Ошибка записи только логируется:
// потеря события = деградация наблюдаемости, прогон не валим.
func (r *Runner) emitKeeperTaskExecuted(ctx context.Context, applyID string, rt *render.RenderedTask, changed, failed bool, message string, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	in := audit.TaskExecutedInput{
		SID:     render.KeeperTargetSID,
		ApplyID: applyID,
		TaskIdx: rt.Index,
		// keeper-side задачи (`on: keeper`) исполняются до host-fan-out (один
		// KeeperTargetSID, passage=0) — локальная позиция всегда совпадает с
		// глобальным RenderedTask.Index. plan_index == task_idx == rt.Index.
		PlanIndex: rt.Index,
		Status:    keeperTaskStatus(changed, failed).String(),
		NoLog:     rt.NoLog,
	}
	if failed {
		in.Error = &audit.TaskExecutedError{
			Module:  rt.Module,
			Message: message,
		}
	}
	ev := &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: applyID,
		Payload:       audit.BuildTaskExecutedPayload(in),
	}
	if err := r.deps.Audit.Write(ctx, ev); err != nil {
		log.Warn("scenario: запись audit task.executed keeper-задачи провалена",
			slog.Int("task_idx", rt.Index), slog.String("module", rt.Module), slog.Any("error", err))
	}
}

// keeperTaskStatus маппит исход keeper-задачи (changed/failed) в keeperv1-enum,
// чтобы task.executed-payload нёс ту же status-строку, что Soul-side
// (Status().String()). Свёртка changed фильтрует по литералу
// "TASK_STATUS_CHANGED" (auditpg). keeper-side не различает timed_out (модуль
// либо вернул failed-event, либо gRPC-error) — failed достаточно.
func keeperTaskStatus(changed, failed bool) keeperv1.TaskStatus {
	switch {
	case failed:
		return keeperv1.TaskStatus_TASK_STATUS_FAILED
	case changed:
		return keeperv1.TaskStatus_TASK_STATUS_CHANGED
	default:
		return keeperv1.TaskStatus_TASK_STATUS_OK
	}
}

// applyKeeperTask вызывает keeper-side core-модуль in-process и сворачивает его
// ApplyEvent-стрим в финальный результат (changed/failed/output/message),
// симметрично Soul-side runTask (selfRegisterData). Адрес модуля делится на
// (base, state) тем же config.SplitModuleAddr, что и Soul-side plantask/
// applyrunner: Registry индексирует модули по base (`core.cloud`), state
// (`created`) уходит в ApplyRequest.state. Бракованный адрес или модуль не
// найден в Registry → failed (как Soul на неизвестный модуль). Apply вернул
// gRPC-error (не failed-event) → failed с текстом ошибки.
func (r *Runner) applyKeeperTask(ctx context.Context, rt *render.RenderedTask) (changed, failed bool, output map[string]any, message string) {
	base, state, ok := config.SplitModuleAddr(rt.Module)
	if !ok {
		return false, true, nil, fmt.Sprintf("invalid keeper-side module address %q (want <namespace>.<module>.<state>)", rt.Module)
	}
	mod, ok := r.keeperModules.Lookup(base)
	if !ok {
		return false, true, nil, fmt.Sprintf("unknown keeper-side module %q", rt.Module)
	}

	req := &pluginv1.ApplyRequest{
		State:  state,
		Params: rt.Params,
	}
	sink := newKeeperApplyStream(ctx)
	if err := mod.Apply(req, sink); err != nil {
		return false, true, nil, err.Error()
	}

	last := sink.last()
	if last == nil {
		// Модуль не прислал финального события — трактуем как ошибку контракта
		// (Soul-side тоже считает отсутствие финала аномалией).
		return false, true, nil, "keeper-side module produced no final event"
	}
	if last.GetFailed() {
		return false, true, nil, last.GetMessage()
	}
	var out map[string]any
	if o := last.GetOutput(); o != nil {
		out = o.AsMap()
	}
	return last.GetChanged(), false, out, last.GetMessage()
}

// accumulateKeeperRegister пишет register-результат keeper-задачи в
// apply_task_register под KeeperTargetSID — тем же путём, что Soul-side
// accumulateRegister (events_taskevent.go). Payload — {changed, failed,
// timed_out, skipped} + output-поля модуля (симметрия selfRegisterData
// applyrunner.go). Задача без register: либо no_log → no-op (loadRegisterByHost
// их не резолвит в state_changes). FK на apply_runs(apply_id, sid=keeper) уже
// удовлетворён вставленной строкой. Ошибка только логируется (best-effort, как у
// Soul-side accumulateRegister).
func (r *Runner) accumulateKeeperRegister(ctx context.Context, applyID string, rt *render.RenderedTask, changed, failed bool, output map[string]any, log *slog.Logger) {
	if rt.Register == "" || rt.NoLog {
		return
	}
	data := map[string]any{
		"changed":   changed,
		"failed":    failed,
		"timed_out": false,
		"skipped":   false,
	}
	for k, v := range output {
		data[k] = v
	}
	if err := applyrun.UpsertTaskRegister(ctx, r.deps.DB, &applyrun.TaskRegister{
		ApplyID: applyID,
		SID:     render.KeeperTargetSID,
		// keeper-side задача исполняется локально с глобальным rt.Index — он же и
		// ключ корреляции (PlanIndex), и информационный TaskIdx: на keeper-стороне
		// нет per-Passage среза ApplyRequest, поэтому локальный==глобальный индекс
		// (ADR-056 §S1 fix Variant B). buildRegisterByHost резолвит имя по PlanIndex.
		PlanIndex:    rt.Index,
		TaskIdx:      rt.Index,
		RegisterData: data,
	}); err != nil {
		log.Warn("scenario: аккумуляция register keeper-задачи провалена",
			slog.Int("task_idx", rt.Index), slog.Any("error", err))
	}
}

// keeperTasksOf отбирает RenderedTask-и, чей DispatchPlan помечен Keeper=true
// (render.IsKeeperTask), в порядке Index (= порядок scenario.tasks[]).
func keeperTasksOf(tasks []*render.RenderedTask, plans []render.DispatchPlan) []*render.RenderedTask {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	out := make([]*render.RenderedTask, 0)
	for _, p := range plans {
		if !p.Keeper {
			continue
		}
		if t := byIndex[p.TaskIndex]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

// composeKeeperFailure формирует operator-facing причину падения keeper-задачи
// для apply_runs.error_summary (формат `task <idx> <module>: <message>`,
// симметрично composeTaskErrorSummary Soul-side). no_log keeper-задача → текст
// подавляется (как failureReason).
func composeKeeperFailure(rt *render.RenderedTask, message string) string {
	if rt.NoLog {
		return fmt.Sprintf("task %d %s: (no_log task failed)", rt.Index, rt.Module)
	}
	head := fmt.Sprintf("task %d %s", rt.Index, rt.Module)
	if message == "" {
		return head
	}
	return head + ": " + maskErrText(fmt.Errorf("%s", message))
}

// keeperApplyStream — in-proc реализация grpc.ServerStreamingServer[ApplyEvent]
// для локального вызова keeper-side core-модуля (симметрично inProcApplyStream
// Soul-side runtime). Буферизует ApplyEvent-ы; исполнитель смотрит на финальный.
type keeperApplyStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*pluginv1.ApplyEvent
}

func newKeeperApplyStream(ctx context.Context) *keeperApplyStream {
	return &keeperApplyStream{ctx: ctx}
}

func (s *keeperApplyStream) Context() context.Context { return s.ctx }

func (s *keeperApplyStream) Send(ev *pluginv1.ApplyEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *keeperApplyStream) SetHeader(metadata.MD) error  { return nil }
func (s *keeperApplyStream) SendHeader(metadata.MD) error { return nil }
func (s *keeperApplyStream) SetTrailer(metadata.MD)       {}

func (s *keeperApplyStream) SendMsg(m any) error {
	ev, ok := m.(*pluginv1.ApplyEvent)
	if !ok {
		return fmt.Errorf("keeper apply stream: SendMsg got %T, want *pluginv1.ApplyEvent", m)
	}
	return s.Send(ev)
}

func (s *keeperApplyStream) RecvMsg(any) error {
	return fmt.Errorf("keeper apply stream: RecvMsg not supported")
}

func (s *keeperApplyStream) last() *pluginv1.ApplyEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}
