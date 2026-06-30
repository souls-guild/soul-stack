package scenario

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
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
// keeper-задачи каждого Passage делят ОДНУ apply_runs-строку (apply_id, sid=keeper,
// passage): по task_idx они различаются в apply_task_register, а строка success ⇔ все
// keeper-задачи ЭТОГО Passage прошли. Первая упавшая keeper-задача → строка failed +
// возврат ошибки (run() уходит в abort). staged-прогон с keeper-задачами в нескольких
// Passage пишет N keeper-строк — по одной на (apply_id, keeper, passage) (PK тройной,
// миграция 078). N=1 (или все keeper-задачи в Passage 0) → одна строка passage 0,
// БИТ-В-БИТ как до Слайса 2.
//
// Исполняется per-Passage ВНУТРИ stage-loop (run.go), на ПЕРЕ-рендеренных при
// ActivePassage=passage tasks, СТРОГО ДО host-dispatch-а ЭТОГО Passage: keeper-fail
// Passage>0 → abort ДО host-fan-out-а этого Passage (host-dispatch не стартует), как и
// на Passage 0. keeper→keeper register-chaining: keeper-задача Passage N видит
// `register.<prev>.*` keeper-задач прошлых Passage через renderIn.KeeperRegister (перелив
// stage-loop, см. keeperRegisterBucket). Ordering критичен для refresh-границы: keeper-
// dispatch Passage P исполняется ДО того, как re-resolve P+1 прочитает его эффект
// (например core.soul.registered{refresh_soulprint} пишет souls+coven).
//
// keeperTasks ЭТОГО Passage пуст → no-op (Passage без keeper-задач: host-only Passage
// или прогон вовсе без keeper-side задач — обычный Soul-side путь).
func (r *Runner) dispatchKeeperTasks(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, tasks []*render.RenderedTask, plans []render.DispatchPlan) error {
	keeperTasks := keeperTasksOf(tasks, plans, passage)
	if len(keeperTasks) == 0 {
		return nil
	}
	if r.keeperModules == nil {
		return ErrKeeperModulesNotConfigured
	}

	// Одна строка apply_runs на keeper-target ЭТОГО Passage: вставляем running до
	// первого исполнения. Терминал (success/failed) — после прохода по всем keeper-
	// задачам Passage. Passage в PK (apply_id, sid, passage) — N keeper-строк на
	// staged-прогоне с keeper-задачами в разных Passage.
	if err := applyrun.Insert(ctx, r.deps.DB, &applyrun.ApplyRun{
		ApplyID:         spec.ApplyID,
		SID:             render.KeeperTargetSID,
		IncarnationName: spec.IncarnationName,
		Scenario:        spec.ScenarioName,
		Status:          applyrun.StatusRunning,
		StartedByAID:    startedByPtr(spec.StartedByAID),
		Passage:         passage,
	}); err != nil {
		return fmt.Errorf("scenario: insert keeper apply_run (passage %d): %w", passage, err)
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
		r.emitKeeperTaskExecuted(ctx, spec.ApplyID, passage, rt, changed, failed, msg, log)

		if failed {
			summary := composeKeeperFailure(rt, msg)
			// keeper-target адресуется тройкой (apply_id, keeper, passage) — Passage
			// этой keeper-задачи (Слайс 2: keeper-задачи стратифицируются по Passage,
			// строка failed пишется в строку ИМЕННО этого Passage). rt.Index —
			// глобальный сквозной индекс по плану; для keeper-target локальный==
			// глобальный (нет per-host where:), поэтому task_idx и plan_index совпадают
			// (=rt.Index).
			if rerr := applyrun.RecordTaskFailure(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, passage, rt.Index, rt.Index, summary); rerr != nil {
				log.Warn("scenario: запись причины падения keeper-задачи провалена",
					slog.Int("passage", passage), slog.Int("task_idx", rt.Index), slog.Any("error", rerr))
			}
			if uerr := applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, passage, applyrun.StatusFailed, &summary); uerr != nil {
				log.Warn("scenario: перевод keeper apply_run в failed провален",
					slog.Int("passage", passage), slog.Any("error", uerr))
			}
			return fmt.Errorf("scenario: keeper-side задача %q (%s) провалена: %s", rt.Name, rt.Module, msg)
		}

		// register: keeper-задачи аккумулируется под KeeperTargetSID — симметрично
		// Soul-side accumulateRegister. Без register: (rt.Register=="") / no_log
		// задача в накопитель не пишется (loadRegisterByHost их и так не резолвит).
		// passage ОБЯЗАТЕЛЕН (Слайс 2): FK apply_task_register→apply_runs идёт по
		// тройке (apply_id, sid, passage) (миграция 078) — register keeper-задачи
		// Passage P обязан ссылаться на keeper apply_runs-строку ИМЕННО passage P
		// (она вставлена выше), иначе FK на (apply_id, keeper, 0) промахнётся (для
		// P>0 строки passage 0 у keeper-target-а нет) и register потеряется.
		r.accumulateKeeperRegister(ctx, spec.ApplyID, passage, rt, changed, failed, output, log)

		// Sync-hook bind-пути (ADR-060 amend, R1): после успешного
		// core.soul.registered хост(ы) привязан(ы) к coven инкарнации — проецируем
		// incarnation.traits в souls.traits хостов-членов, чтобы новопривязанный
		// подхватил traits своей инкарнации. Гейтится именно registered-модулем
		// (bind-граница); прочие keeper-задачи (cloud/vault) членство не меняют.
		r.syncTraitsOnRegistered(ctx, spec.IncarnationName, rt, log)
	}

	if err := applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, passage, applyrun.StatusSuccess, nil); err != nil {
		return fmt.Errorf("scenario: перевод keeper apply_run (passage %d) в success: %w", passage, err)
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
func (r *Runner) emitKeeperTaskExecuted(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, changed, failed bool, message string, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	in := audit.TaskExecutedInput{
		SID:     render.KeeperTargetSID,
		ApplyID: applyID,
		TaskIdx: rt.Index,
		// keeper-side задачи (`on: keeper`) исполняются ОДНОЙ строкой на (apply_id,
		// keeper, passage) — нет per-host where:, локальная позиция всегда совпадает с
		// глобальным RenderedTask.Index. plan_index == task_idx == rt.Index (сквозной
		// по всему плану, всем Passage → ключ корреляции уникален между Passage).
		PlanIndex: rt.Index,
		Status:    keeperTaskStatus(changed, failed).String(),
		NoLog:     rt.NoLog,
		// passage этой keeper-задачи (Слайс 2: keeper-задачи стратифицируются по
		// Passage). В payload для триажа per-Passage; корреляцию changed_tasks НЕ
		// меняет (та идёт по sid/plan_index). 0 на N=1 / keeper-задачах Passage 0.
		Passage: passage,
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
// их не резолвит в state_changes). Ошибка только логируется (best-effort, как у
// Soul-side accumulateRegister).
//
// passage (Слайс 2): FK apply_task_register→apply_runs идёт по тройке (apply_id,
// sid, passage) (миграция 078). register keeper-задачи Passage P ОБЯЗАН ссылаться
// на keeper apply_runs-строку ИМЕННО passage P (вставлена в dispatchKeeperTasks
// перед циклом по keeper-задачам этого Passage). Иначе FK на (apply_id, keeper, 0)
// промахнётся для P>0 (строки passage 0 у keeper-target-а нет) → register потеряется
// и keeper→keeper цепочка Passage P→P+1 оборвётся.
func (r *Runner) accumulateKeeperRegister(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, changed, failed bool, output map[string]any, log *slog.Logger) {
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
		// passage keeper-задачи — компонент FK на apply_runs(apply_id, sid, passage)
		// + накопительный фильтр loadRegisterByHostUpToPassage (register Passage<P).
		Passage: passage,
	}); err != nil {
		log.Warn("scenario: аккумуляция register keeper-задачи провалена",
			slog.Int("passage", passage), slog.Int("task_idx", rt.Index), slog.Any("error", err))
	}
}

// registeredModuleBase — base-адрес keeper-side core-модуля core.soul.registered
// (coremod/soul.Name). Локальная константа: scenario НЕ импортирует coremod/soul
// (тот тянет PG-store/presence-deps) — bind-граница распознаётся по адресу задачи,
// а не по типу модуля.
const (
	registeredModuleBase  = "core.soul"
	registeredModuleState = "registered"
)

// syncTraitsOnRegistered — sync-hook bind-пути релокации Trait (ADR-060 amend,
// R1). После УСПЕШНОГО core.soul.registered хост(ы) привязан(ы) к coven
// инкарнации; проецируем incarnation.traits в souls.traits хостов-членов, чтобы
// новопривязанный подхватил traits своей инкарнации. Гейтится именно
// registered-модулем — прочие keeper-задачи (cloud/vault) членство не меняют.
//
// Лучше-эффортно: incName пуст (прямой keeper-test без incarnation) / инкарнация
// без traits / сбой загрузки → лог, прогон не валим (traits — организационная
// метка, не блокирует apply). Идемпотентно: повторный bind пере-проецирует тот же
// источник.
func (r *Runner) syncTraitsOnRegistered(ctx context.Context, incName string, rt *render.RenderedTask, log *slog.Logger) {
	base, state, ok := config.SplitModuleAddr(rt.Module)
	if !ok || base != registeredModuleBase || state != registeredModuleState {
		return
	}
	if incName == "" || r.deps.DB == nil {
		return
	}
	inc, err := incarnation.SelectByName(ctx, r.deps.DB, incName)
	if err != nil {
		log.Warn("scenario: bind-sync traits — загрузка инкарнации провалена (best-effort)",
			slog.String("incarnation", incName), slog.Any("error", err))
		return
	}
	if len(inc.Traits) == 0 {
		return
	}
	if serr := incarnation.SyncTraitsToHosts(ctx, r.deps.DB, incName, inc.Traits); serr != nil {
		log.Warn("scenario: bind-sync traits → souls провален (best-effort)",
			slog.String("incarnation", incName), slog.Any("error", serr))
	}
}

// keeperRegisterBucket извлекает плоский register-bucket keeper-задач из per-host
// register-таблицы прогона: записи под синтетическим хостом KeeperTargetSID
// ("keeper"), куда accumulateKeeperRegister складывает register-результаты
// keeper-side задач. Возвращает register-name → payload (та же форма, что плоская
// RenderInput.Register), либо nil, если keeper-bucket пуст / отсутствует.
//
// Назначение (staged-render, keeper→keeper register-chaining): stage-loop run.go
// кладёт результат в ИЗОЛИРОВАННЫЙ renderIn.KeeperRegister перед per-passage render-ом
// keeper-задач активного Passage. keeperVars (render/dispatch.go) читает именно
// KeeperRegister — так keeper-задача Passage N видит `register.<prev>.*` от keeper-
// задач предыдущих Passage, а host-fallback (hostRegister) при этом keeper-register НЕ
// получает (канал отделён от плоской Register). registerByHost — то, что вернул
// loadRegisterByHostUpToPassage (register Passage < активного), поэтому bucket
// несёт только УЖЕ завершённые keeper-задачи (forward-only).
func keeperRegisterBucket(registerByHost map[string]map[string]any) map[string]any {
	bucket := registerByHost[render.KeeperTargetSID]
	if len(bucket) == 0 {
		return nil
	}
	return bucket
}

// keeperTasksOf отбирает RenderedTask-и, чей DispatchPlan помечен Keeper=true
// (render.IsKeeperTask) И чей RenderedTask.Passage == passage, в порядке Index
// (= порядок scenario.tasks[]).
//
// Фильтр по Passage (staged-render, ADR-056, Слайс 2): keeper-задачи стратифицируются
// по register-зависимости как host-задачи (core.bootstrap.delivered читает
// register.provision.* → Passage строго ПОСЛЕ core.cloud.created). dispatchKeeperTasks
// зовётся per-Passage на ПЕРЕ-рендеренных при ActivePassage=p tasks; здесь отбираются
// только keeper-задачи ИМЕННО этого Passage. keeper-задача будущего Passage (>p) на
// этом render — placeholder без Params (pipeline.go placeholder-gate) И без Keeper=true
// в плане, поэтому фильтр по Keeper её и так отсекает; фильтр по Passage — вторая
// граница (на свежем render Passage p staged keeper-задача p несёт Keeper=true).
// N=1-прогон: единственный Passage 0, все keeper-задачи passage==0 → отбираются как до
// эпика (БИТ-В-БИТ).
func keeperTasksOf(tasks []*render.RenderedTask, plans []render.DispatchPlan, passage int) []*render.RenderedTask {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	out := make([]*render.RenderedTask, 0)
	for _, p := range plans {
		if !p.Keeper {
			continue
		}
		t := byIndex[p.TaskIndex]
		if t == nil || t.Passage != passage {
			continue
		}
		out = append(out, t)
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
	// sealed-пути этого прогона keeper-task summary не получает (узкая точка
	// сборки текста ошибки task'а) → nil: vault+regex слои + regex-аларм (ADR-010
	// §7.4). Зарезолвленный keeper-task message — message модуля, не значение по
	// sealed-пути; vault-ref/sensitive-by-name закрыты этими слоями.
	return head + ": " + maskErrText(fmt.Errorf("%s", message), nil)
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
