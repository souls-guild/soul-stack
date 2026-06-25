// Package runtime — apply-цикл Soul-демона: получение ApplyRequest от
// Keeper-а, диспетчер на Registry, агрегация ApplyEvent → TaskEvent,
// финальный RunResult.
//
// Core-модули (ADR-015) вызываются in-process через
// [inProcApplyStream] — адаптер `grpc.ServerStreamingServer[pluginv1.ApplyEvent]`
// поверх Go-канала. Кастомные модули (ADR-020, soul-mod-*) поднимаются как
// sub-process через [soul/internal/pluginhost] (M2.3+: пока wire-up
// принимает Registry, не делая различий).
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// tracer для in-process span-ов apply-цикла. Берёт глобальный TracerProvider,
// поднятый [obs.SetupOTel] в cmd/soul; при OTel disabled провайдер no-op —
// span-ы бесплатны и код не ветвится (ADR-024 §1.2).
var tracer = otel.Tracer("soul/runtime")

// Registry — узкий интерфейс над [coremod.Registry] / любым store-ом
// модулей. Здесь только Lookup — это всё, что нужно apply-циклу.
type Registry interface {
	Lookup(name string) (module.SoulModule, bool)
}

// EventSink — куда runtime шлёт сообщения для Keeper-а. Реализуется
// EventStream-клиентом ([soul/internal/grpc.StreamSession]); в тестах —
// fake-имплементация для проверки последовательности TaskEvent/RunResult.
type EventSink interface {
	SendTaskEvent(*keeperv1.TaskEvent) error
	SendRunResult(*keeperv1.RunResult) error
}

// ApplyRunner — состояние apply-цикла одного Soul-демона.
//
// Concurrent-прогонов на одном демоне нет — ADR-012(a) гарантирует, что
// Keeper не пошлёт второй ApplyRequest, пока не получен RunResult по
// текущему. Тем не менее runner держит map активных apply_id → cancel,
// чтобы CancelApply от Keeper-а мог адресно отменить in-flight прогон.
type ApplyRunner struct {
	registry Registry
	metrics  *ApplyMetrics

	// flowEngine — Soul-side sandboxed CEL-движок для flow-control-предикатов
	// (when:/changed_when:/failed_when:, ADR-012(d)). Один на runner: env
	// неизменен, compile-cache переиспользуется между задачами и прогонами
	// (concurrent-прогонов нет, ADR-012(a)). Без vault-client: внешний доступ
	// keeper-only, Soul-CEL чистый. flowEngineErr — ошибка сборки движка
	// (программная несовместимость cel-go, «не должно случаться»); если ненулевая,
	// [ApplyRunner.Run] завершает прогон internal-ошибкой, а не молча игнорирует
	// предикаты.
	flowEngine    *cel.Engine
	flowEngineErr error

	// hostFacts — собранный Soul-агентом soulprint-снимок хоста (pkg-mgr /
	// init-система), инжектится в core-модули перед Apply (Вариант A, ADR-018(b)).
	// Заполняется один раз на старте через [ApplyRunner.SetHostFacts]; пустое
	// значение безопасно — core-модули откатываются на runtime-детект. Только
	// чтение в Run (после старта не меняется), доп. синхронизации не требует.
	hostFacts util.HostFacts

	mu     sync.Mutex
	active map[string]context.CancelFunc

	// recentlyFinished — short-TTL in-memory набор apply_id, завершённых Run-ом
	// в последние [recentlyFinishedTTL] секунд (Soul-reconcile, ADR-027(g), S6).
	// Назначение — закрыть гонку «RunResult отправлен, но стрим порвался ДО того,
	// как Run успел unregister-нуть apply_id из active»: на reconnect-е
	// [ApplyRunner.ActiveSet] обязан всё ещё объявить этот apply ведомым, иначе
	// Keeper-sweep ложно осиротил бы строку, для которой результат уже в полёте/
	// доставлен. Запись живёт TTL после завершения Run и вычищается лениво в
	// ActiveSet (set небольшой — один-в-полёте apply, ADR-012(a)).
	//
	// Переживает reconnect/failback-swap (кеш в ApplyRunner, один на процесс, как
	// lastSeenAttempt), НО НЕ переживает рестарт процесса — это корректно: после
	// рестарта in-flight apply физически нет, и его dispatched-строки ЗАКОННО
	// сиротятся (пустой ActiveSet → Keeper терминалит их в orphaned).
	//
	// Под тем же mu, что active/lastSeenAttempt: операции короткие, конкурентных
	// apply на одном Soul нет (ADR-012(a)) — отдельный lock избыточен. nowFn —
	// инъекция времени для детерминизма TTL-тестов (в проде time.Now).
	recentlyFinished map[string]time.Time
	nowFn            func() time.Time

	// lastSeenAttempt — fencing-кеш Soul-guard-а (ADR-027(g), Phase 2): apply_id →
	// максимальный attempt, уже принятый к исполнению. [ApplyRunner.AcceptAttempt]
	// отвергает ApplyRequest с attempt < виденного — это отсекает stale-дубль,
	// когда recovery-скан вернул протухший Ward в очередь, а его оригинальный
	// apply ещё в полёте (пере-claim приедет с БОЛЬШИМ attempt и победит).
	//
	// Кеш живёт в ApplyRunner (per-process), а не в StreamSession: он обязан
	// ПЕРЕЖИВАТЬ reconnect/failback-swap стрима (cmd/soul пересоздаёт сессию,
	// runner один на процесс) — иначе после reconnect-а Soul забыл бы виденные
	// attempt-ы и пропустил бы stale-дубль. Рестарт Soul-процесса кеш обнуляет,
	// но это безопасно: после рестарта in-flight apply физически нет (процесс,
	// исполнявший его, мёртв), поэтому stale-дубль с меньшим attempt после
	// рестарта невозможен — фенсить нечего.
	//
	// Под тем же mu, что active: обе операции короткие, конкурентных apply на
	// одном Soul нет (ADR-012(a)) — отдельный lock избыточен.
	lastSeenAttempt map[string]int32
}

// recentlyFinishedTTL — окно, в течение которого завершённый Run остаётся в
// наборе [ApplyRunner.ActiveSet] (Soul-reconcile, ADR-027(g), S6). Должно с
// запасом перекрывать окно «SendRunResult → unregister → reconnect → WardRoster»
// (реально доли секунды), чтобы гонка «результат в полёте, стрим порвался до
// cleanup» не дала ложного orphan. 30s — щедрый потолок: даже после нескольких
// backoff-итераций reconnect-loop-а apply остаётся объявленным; излишне долгий
// TTL лишь отсрочил бы законное сиротство dispatched-строки после реального
// краха Soul (но крах = рестарт процесса, который набор и так обнуляет).
const recentlyFinishedTTL = 30 * time.Second

// NewApplyRunner собирает runner с зарегистрированными модулями.
//
// metrics — soul_apply_*-collectors (ADR-024); nil → инструментация выключена
// (nil-safe методы [ApplyMetrics] — no-op): push-режим (soul apply) и unit-тесты
// поднимаются без obs-стека.
func NewApplyRunner(reg Registry, metrics *ApplyMetrics) *ApplyRunner {
	engine, err := cel.NewFlowControl()
	return &ApplyRunner{
		registry:         reg,
		metrics:          metrics,
		flowEngine:       engine,
		flowEngineErr:    err,
		active:           make(map[string]context.CancelFunc),
		recentlyFinished: make(map[string]time.Time),
		nowFn:            time.Now,
		lastSeenAttempt:  make(map[string]int32),
	}
}

// SetHostFacts задаёт soulprint-снимок хоста, который runner инжектит в
// core-модули, реализующие [util.SoulprintAware] (core.pkg / core.service), перед
// каждым Apply (Вариант A, ADR-018(b)). Вызывается из cmd/soul один раз на старте
// после первого сбора soulprint; до вызова hostFacts пуст — модули детектят
// backend в рантайме (fallback). Конкурентных Run на одном Soul нет (ADR-012(a)),
// факт после старта не меняется — отдельная синхронизация не нужна.
func (r *ApplyRunner) SetHostFacts(f util.HostFacts) { r.hostFacts = f }

// Cancel пытается отменить активный apply с указанным id. Возвращает true,
// если apply был зарегистрирован и cancel вызван; false — если apply уже
// завершился или не существует. После cancel-а Run-горутина увидит ctx.Err()
// и завершит цикл, выслав RunResult со статусом CANCELLED.
func (r *ApplyRunner) Cancel(applyID string) bool {
	r.mu.Lock()
	cancel, ok := r.active[applyID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// AcceptAttempt — attempt-fencing-guard (ADR-027(g), Phase 2): решает, принимать
// ли ApplyRequest по его (apply_id, attempt) к исполнению. Вызывается ПЕРЕД
// [ApplyRunner.Run] на каждый входящий ApplyRequest.
//
// Правило:
//   - attempt == 0 → принять и НЕ фенсить: 0 = старый Keeper без fencing-поля
//     (apply.proto field 4 forward-compat, ADR-012(c) only-add). Кеш не трогаем,
//     чтобы пустой attempt не «отравил» seen для последующих fencing-запросов.
//   - attempt < seen[apply_id] → ОТВЕРГНУТЬ (вернуть false): это stale-дубль —
//     протухший Ward, чей оригинальный (больший attempt) apply уже принят. Кеш
//     не обновляем.
//   - attempt >= seen[apply_id] → принять, seen[apply_id] = attempt. Равенство
//     принимается (повторная доставка того же attempt — не stale; SID-lease уже
//     отсекает истинный дубль того же epoch-а, фенсить по «==» было бы ложным
//     отказом валидного re-deliver).
//
// Возврат true = исполнять (caller вызывает Run); false = молча дропнуть
// (ADR-027 barrier-B1: отвергнутый дубль НЕ шлёт RunResult, барьер Keeper-а
// закрывает оригинальный apply своим RunResult, runTimeout — нижняя страховка).
func (r *ApplyRunner) AcceptAttempt(applyID string, attempt int32) bool {
	// attempt=0 (старый Keeper) — fencing выключен, исполняем без записи в кеш.
	if attempt == 0 {
		return true
	}
	r.mu.Lock()
	seen := r.lastSeenAttempt[applyID]
	if attempt < seen {
		r.mu.Unlock()
		// B1 (ADR-027): отвергнутый stale-дубль НИЧЕГО не шлёт Keeper-у — debug-лог
		// + метрика, RunResult не отправляется (барьер закроет оригинальный apply
		// с большим attempt своим RunResult; runTimeout — нижняя страховка).
		r.metrics.ObserveFenced()
		slog.Default().Debug("runtime: ApplyRequest отвергнут attempt-fencing-guard-ом (stale-дубль)",
			slog.String("apply_id", applyID),
			slog.Int("attempt", int(attempt)),
			slog.Int("last_seen", int(seen)))
		return false
	}
	r.lastSeenAttempt[applyID] = attempt
	r.mu.Unlock()
	return true
}

func (r *ApplyRunner) register(applyID string, cancel context.CancelFunc) {
	r.mu.Lock()
	r.active[applyID] = cancel
	r.mu.Unlock()
}

// unregister снимает apply из in-flight-набора active и переводит его в
// recently-finished ring (Soul-reconcile, ADR-027(g), S6): apply_id остаётся
// объявленным ещё [recentlyFinishedTTL] после завершения Run, чтобы reconnect в
// окне «RunResult в полёте, стрим порвался до cleanup» не дал ложного orphan.
func (r *ApplyRunner) unregister(applyID string) {
	r.mu.Lock()
	delete(r.active, applyID)
	r.recentlyFinished[applyID] = r.nowFn()
	r.mu.Unlock()
}

// ActiveSet — снимок ведомых Soul-ом apply-прогонов для [WardRoster] (R-B
// транспорт, Soul-reconcile ADR-027(g), S6). Объединение трёх источников:
//   - active — in-flight прогоны (Run ещё исполняется);
//   - recentlyFinished — завершённые в последние [recentlyFinishedTTL] (анти-гонка
//     «результат в полёте, стрим порвался до unregister»); протухшие вычищаются
//     лениво здесь же;
//   - lastSeenAttempt — apply_id с известным fencing-epoch (attempt-эхо). Этот
//     слой даёт авторитетный attempt для записи; для in-flight/finished без него
//     attempt=0 (старый Keeper без fencing либо ещё не виденный epoch).
//
// Возвращает по одной [keeperv1.ActiveApply] на apply_id (дедуп по объединению).
// Пустой результат (nil) — явная декларация «ничего не ведётся»: caller шлёт
// WardRoster с пустым active[], Keeper по нему терминалит все dispatched-строки
// SID-а. Это правильно после рестарта (наборы пусты) и после штатного завершения
// единственного прогона за пределами TTL.
func (r *ApplyRunner) ActiveSet() []*keeperv1.ActiveApply {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.nowFn()
	// Ленивая чистка протухших finished-записей (set мал — один-в-полёте apply).
	for id, at := range r.recentlyFinished {
		if now.Sub(at) >= recentlyFinishedTTL {
			delete(r.recentlyFinished, id)
		}
	}

	ids := make(map[string]struct{}, len(r.active)+len(r.recentlyFinished))
	for id := range r.active {
		ids[id] = struct{}{}
	}
	for id := range r.recentlyFinished {
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		return nil
	}

	out := make([]*keeperv1.ActiveApply, 0, len(ids))
	for id := range ids {
		// attempt-эхо: авторитетный epoch берём из lastSeenAttempt (последний
		// принятый attempt). Нет записи (старый Keeper / attempt=0) → 0: Keeper
		// epoch-guard трактует 0 как «без fencing» и не фенсит по нему.
		out = append(out, &keeperv1.ActiveApply{
			ApplyId: id,
			Attempt: r.lastSeenAttempt[id],
		})
	}
	return out
}

// Run выполняет все tasks из req последовательно, шлёт TaskEvent для
// каждой, затем RunResult с агрегированным статусом.
//
// Gating перед Apply (ADR-012(d)): задача исполняется только при
// `when && onchanges-satisfied && onfail-satisfied`. when:-предикат вычисляется
// Soul-side sandboxed cel-go-движком ([ApplyRunner.evalWhen]) из
// RenderedTask.flow_context + register предыдущих задач (по register-имени).
// when:false ИЛИ onchanges не сработал ИЛИ onfail не сработал → SKIPPED
// (mod.Apply не вызывается). changed_when/failed_when вычисляются Soul-side ПОСЛЕ
// Apply ([ApplyRunner.runTask]): override changed/failed по результату
// (changed_when сначала, failed_when потом).
//
// Fail-stop с rescue (destiny/tasks.md §8): первая FAILED/TIMED_OUT задача
// НЕОБРАТИМО помечает RunResult как FAILED, но цикл НЕ прерывается. После провала
// все последующие ОБЫЧНЫЕ (не-onfail) задачи пропускаются (SKIPPED, Apply не
// вызывается); отрабатывают ТОЛЬКО onfail-задачи, чей источник упал
// (register.failed==true) — rescue/cleanup. onfail-задачи провал НЕ отменяют:
// RunResult остаётся FAILED. В нормальном (без провалов) прогоне onfail-задачи
// всегда SKIPPED. failed_when:false (ignore_errors) делает задачу OK — она НЕ
// триггерит ни fail-stop, ни onfail.
//
// Стратегия ошибок:
//   - ошибка вычисления when: → TaskEvent.status=FAILED (runtime-error CEL —
//     штатно по templating.md §10; compile-error — internal-расхождение
//     keeper↔soul, defensive FAILED + warn); прогон помечается FAILED, rescue-хвост
//     (onfail на эту задачу) отрабатывает, остальные tasks skip.
//   - модуль не найден в Registry → TaskEvent.status=FAILED (как провал модуля:
//     fail-stop с rescue), RunResult.status=FAILED.
//   - SoulModule.Apply вернул error → TaskEvent.status=FAILED.
//   - Apply прислал ApplyEvent.failed=true → TaskEvent.status=FAILED.
//   - Apply прислал ApplyEvent.changed=true (failed=false) → CHANGED.
//   - ctx был отменён до/во время задачи → RunResult.status=CANCELLED,
//     текущая задача — TaskEvent.status=CANCELLED, остальные не выполняются
//     (TaskEvent не шлётся). CancelApply прерывает цикл безусловно — это не
//     fail-stop, rescue на отмену НЕ срабатывает.
//   - иначе → OK.
//
// state_changes пока не агрегируется (заполнится в M2.3+); поле
// RunResult.state_changes остаётся nil.
//
// Возврат — error только при I/O-ошибках Sink-а (stream порвался). Все
// бизнес-ошибки задач уезжают через TaskEvent.error.
func (r *ApplyRunner) Run(ctx context.Context, req *keeperv1.ApplyRequest, sink EventSink) error {
	if req == nil {
		return fmt.Errorf("runtime: ApplyRequest is nil")
	}
	if sink == nil {
		return fmt.Errorf("runtime: sink is nil")
	}
	// flow-control CEL-движок обязателен: when:-предикаты вычисляются Soul-side
	// (ADR-012(d)). Ошибка его сборки — программная несовместимость cel-go (а не
	// runtime-данные), завершаем прогон явной internal-ошибкой, а не игнорируем
	// предикаты молча (это исказило бы gating: when:false-задача выполнилась бы).
	if r.flowEngineErr != nil || r.flowEngine == nil {
		return fmt.Errorf("runtime: flow-control CEL-движок недоступен: %w", r.flowEngineErr)
	}

	// Локальный ctx для прогона — нужен Cancel-у, чтобы прервать ровно этот
	// apply, не убивая родительский ctx Soul-демона.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	applyID := req.GetApplyId()
	// passage — индекс Passage staged-render (ADR-056). Эхается в КАЖДОМ
	// TaskEvent/RunResult прогона как есть (0 для N=1 — БИТ-В-БИТ как до staged):
	// keeper коррелирует терминал per-(apply_id, sid, passage) и накапливает
	// register для render следующего Passage. Захватываем один раз — все события
	// этого ApplyRequest относятся к одному Passage.
	passage := req.GetPassage()
	if applyID != "" {
		r.register(applyID, cancel)
		defer r.unregister(applyID)
	}

	// In-process span на весь прогон. apply_id — атрибут для фильтрации трейса
	// (в metric-labels нельзя — cardinality, ADR-024 §2.2); секретов нет (params
	// рендерятся Keeper-side и сюда как span-атрибуты не идут). sid в
	// ApplyRequest не передаётся (authority — mTLS peer cert, ADR-012), поэтому
	// разрез по хосту — на стороне Keeper-span-а. При OTel disabled tracer
	// no-op — Start/End бесплатны.
	runCtx, span := tracer.Start(runCtx, "apply.run",
		trace.WithAttributes(attribute.String("apply_id", applyID)),
	)
	defer span.End()

	start := time.Now()
	defer func() { r.metrics.ObserveApplyDuration(time.Since(start).Seconds()) }()

	// registerByIdx копит register-payload (TaskEvent.register_data) уже
	// выполненных задач прогона ПО ИНДЕКСУ — нужен gating-у requisites
	// (`onchanges:`): задача с onchanges_idx исполняется только если хотя бы у
	// одного источника register.changed == true. Soul применяет задачи строго
	// последовательно, поэтому к моменту gating источники (всегда раньше по
	// плану) уже здесь.
	registerByIdx := make(map[int32]*structpb.Struct, len(req.GetTasks()))

	// registerByName копит register-payload по register-ИМЕНИ (RenderedTask.register)
	// — нужен flow-control-предикатам (when:/…), которые пишут `register.<name>.*`
	// (ADR-012(d)). Параллелен registerByIdx (тот для onchanges-gating по индексам).
	// Задача без register-имени в этот map не попадает (адресуется только своим idx).
	registerByName := make(map[string]any, len(req.GetTasks()))

	runStatus := keeperv1.RunStatus_RUN_STATUS_SUCCESS
	// runFailed — прогон уже провален (была FAILED/TIMED_OUT задача). После этого
	// fail-stop НЕ делает немедленный break (как раньше): цикл продолжается, но
	// ПРОПУСКАЕТ все последующие обычные задачи, исполняя ТОЛЬКО onfail-задачи,
	// чьи источники упали (rescue/cleanup, destiny/tasks.md §8). RunResult при
	// этом остаётся FAILED — onfail это компенсация, а не отмена провала.
	runFailed := false
	for idx, task := range req.GetTasks() {
		// Cancel мог прийти между задачами — проверяем до запуска модуля.
		if err := runCtx.Err(); err != nil {
			runStatus = keeperv1.RunStatus_RUN_STATUS_CANCELLED
			break
		}

		// После провала прогона обычные (не-onfail) задачи не исполняются: они
		// пропускаются БЕЗ вычисления when: (when упавшей-цепочки мог бы дать
		// ложный новый FAILED). Исключение — onfail-задачи: для них gating
		// (источник упал? + when/onchanges) считается ниже. Это и есть новая
		// fail-stop-семантика: rescue-хвост отрабатывает, остальное skip.
		//
		// Терминал applier-register (aggregate_of) — ТОЖЕ исключение: он не исполняет
		// модуль (синтетическая свёртка дочерних, побочных эффектов нет), а его
		// register.<applier> ОБЯЗАН отражать реальный итог destiny даже при провале —
		// иначе внешний onfail:[<applier>] / when: register.<applier>.failed разорвётся
		// (failed-агрегат потерялся бы под generic-skipped). Эмитим агрегат СРАЗУ:
		// дочерние раньше по плану и уже в registerByIdx (терминал последний в группе).
		if runFailed && len(task.GetOnfailIdx()) == 0 {
			var ev *keeperv1.TaskEvent
			if agg := task.GetAggregateOf(); len(agg) > 0 {
				ev = &keeperv1.TaskEvent{
					ApplyId:      applyID,
					TaskIdx:      int32(idx),
					Status:       keeperv1.TaskStatus_TASK_STATUS_OK,
					RegisterData: aggregateRegisterData(agg, registerByIdx),
				}
			} else {
				ev = skippedTaskEvent(applyID, int32(idx))
			}
			recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			r.metrics.ObserveTask(taskResult(ev.GetStatus()))
			if len(task.GetAggregateOf()) == 0 {
				r.metrics.ObserveSkipped(skipReasonFailedRun)
			}
			continue
		}

		// Gating: задача исполняется только при `when && onchanges-satisfied &&
		// onfail-satisfied` (ADR-012(d)). Порядок — when ПЕРВЫМ:
		//   - when:"" → true (безусловно); when:false → SKIPPED, Apply не вызывается;
		//   - when:true, но onchanges/onfail не сработал → тоже SKIPPED.
		// Оба пути дают одинаковый skipped-payload (changed=false — не триггерит
		// onchanges последующих, как и onchanges-skip).
		when, whenErr := r.evalWhen(task, registerByName)
		if whenErr != nil {
			// Ошибка вычисления when: runtime-error CEL (например, register.x нет)
			// → задача FAILED по таблице ошибок templating.md §10; compile-error на
			// Soul = internal (Keeper пропустил невалидный предикат) → defensive
			// FAILED + warn (предикат уже провалидирован на Keeper-е перед рендером).
			r.logFlowControlError("when", task, whenErr)
			ev := &keeperv1.TaskEvent{
				ApplyId: applyID,
				TaskIdx: int32(idx),
				Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
				Error: &keeperv1.TaskError{
					Code:    "flowcontrol.when_error",
					Module:  task.GetModule(),
					Message: fmt.Sprintf("when %q: %v", task.GetWhen(), whenErr),
				},
			}
			ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
			recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			r.metrics.ObserveTask(applyResultFailed)
			// when-error = задача FAILED → fail-stop с rescue (как провал модуля):
			// помечаем прогон проваленным, но не прерываем цикл — onfail-задачи на
			// эту задачу отработают, остальные пропустятся (runFailed-ветка).
			runStatus = keeperv1.RunStatus_RUN_STATUS_FAILED
			runFailed = true
			continue
		}

		// when=false ЛИБО onchanges не сработал ЛИБО onfail не сработал → SKIPPED
		// (mod.Apply не вызывается). onchanges фиксит restart-flap (restart только
		// когда конфиг изменился); onfail — rescue-gating: onfail-задача в нормальном
		// (без провалов) прогоне всегда SKIPPED, исполняется только при упавшем
		// источнике (skipOnFail). Связка — AND (когда задано несколько requisite-ов).
		if !when || skipOnChanges(task.GetOnchangesIdx(), registerByIdx) ||
			skipOnFail(task.GetOnfailIdx(), registerByIdx) {
			ev := skippedTaskEvent(applyID, int32(idx))
			recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			// SKIPPED — терминальный не-успех-нейтральный исход; closed-enum
			// soul_apply_tasks_total (ok/changed/failed) сводит его к ok (не fail).
			r.metrics.ObserveTask(taskResult(ev.GetStatus()))
			// when ПЕРВЫМ в gating-цепочке: !when → reason=when, иначе skip
			// вызван requisite-ом (onchanges/onfail не сработал).
			if !when {
				r.metrics.ObserveSkipped(skipReasonWhen)
			} else {
				r.metrics.ObserveSkipped(skipReasonRequisite)
			}
			continue
		}

		ev := r.runTaskWithRetry(runCtx, applyID, int32(idx), task, registerByName, req.GetDryRun())
		// Если cancel случился внутри runTask (модуль уважает ctx), мы хотим
		// одиночный TaskEvent со статусом CANCELLED и RunResult=CANCELLED.
		// TaskError.code сохраняется как `apply.cancelled` для удобства фильтра
		// в audit / логах — сам факт отмены уже несёт TaskStatus, но строковый
		// код упрощает grep по audit_log без enum-resolution.
		if runCtx.Err() != nil {
			ev.Status = keeperv1.TaskStatus_TASK_STATUS_CANCELLED
			ev.Error = &keeperv1.TaskError{
				Code:    "apply.cancelled",
				Module:  ev.GetError().GetModule(),
				Message: "apply cancelled by Keeper",
			}
			ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
			if err := sendTaskEvent(sink, ev, task, passage); err != nil {
				return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
			}
			// Отменённая задача — терминальный не-успех; в closed enum
			// soul_apply_tasks_total (ok/changed/failed) сводится к failed.
			r.metrics.ObserveTask(applyResultFailed)
			runStatus = keeperv1.RunStatus_RUN_STATUS_CANCELLED
			break
		}
		// Материализация applier-register (orchestration.md §2.1.1, Вариант B):
		// терминальная core.noop.run с непустым aggregate_of несёт СВОДНЫЙ итог
		// destiny-прогона applier-а. Её собственный ApplyEvent тривиален (noop →
		// changed=false), поэтому register_data ПЕРЕЗАПИСываем агрегатом по дочерним
		// задачам (OR changed/failed/timed_out). Дочерние — раньше по плану и в этом
		// же ApplyRequest (терминал последний в группе), поэтому уже в registerByIdx.
		// Override стоит ПОСЛЕ cancel-ветки (отменённая задача сохраняет CANCELLED) и
		// ДО sendTaskEvent/recordRegister — и отправленный Keeper-у TaskEvent, и
		// register для последующих gating-ей несут агрегат.
		if agg := task.GetAggregateOf(); len(agg) > 0 {
			ev.RegisterData = aggregateRegisterData(agg, registerByIdx)
		}
		if err := sendTaskEvent(sink, ev, task, passage); err != nil {
			return fmt.Errorf("runtime: send TaskEvent[%d]: %w", idx, err)
		}
		// register выполненной задачи доступен последующим: gating-у onchanges (по
		// индексу) и flow-control-предикатам when:/… (по register-имени, ADR-012(d)).
		recordRegister(registerByIdx, registerByName, int32(idx), task.GetRegister(), ev.GetRegisterData())
		r.metrics.ObserveTask(taskResult(ev.GetStatus()))
		if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_FAILED ||
			ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
			// fail-stop с rescue (destiny/tasks.md §8): провал фиксирует RunResult
			// как FAILED НЕОБРАТИМО (onfail-задачи это cleanup, не отмена провала),
			// но цикл НЕ прерывается. Последующие обычные задачи пропускаются
			// (runFailed-ветка в начале итерации), отрабатывают только onfail-задачи,
			// чьи источники упали. TIMED_OUT — частный случай failed: тоже триггерит
			// rescue и тоже помечает прогон проваленным.
			if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT {
				// Таймаут считаем отдельной серией по ФИНАЛЬНОМУ исходу (после
				// исчерпания retry), не на каждую попытку — soul_apply_task_timed_out_total.
				r.metrics.ObserveTimedOut()
			}
			runStatus = keeperv1.RunStatus_RUN_STATUS_FAILED
			runFailed = true
		}
	}

	return sink.SendRunResult(&keeperv1.RunResult{
		ApplyId: applyID,
		Status:  runStatus,
		// attempt — эхо fencing-epoch запроса (ADR-027(g), gate-1): Keeper на
		// приёме (correlateRunResult) сверит его с apply_runs.attempt и отвергнет
		// результат устаревшей попытки. Soul возвращает значение как есть; 0 (старый
		// Keeper без fencing) уезжает 0 → проверка актуальности деградирует штатно.
		Attempt: req.GetAttempt(),
		// passage — эхо ApplyRequest.passage (ADR-056): barrier этого Passage ждёт
		// терминал по (apply_id, sid, passage). 0 для N=1 — БИТ-В-БИТ как до staged.
		Passage: passage,
	})
}

// sendTaskEvent проставляет в TaskEvent эхо no_log из RenderedTask и шлёт его в
// sink. Флаг едет на Keeper, чтобы тот подавил register_data/error.message в
// долгоживущем audit для no_log-задач, не обращаясь к []RenderedTask (этот
// TaskEvent мог прийти на другой Keeper-инстанс, ADR-002). Soul знает no_log из
// плана прогона — исполняет задачу, не логируя её params/output.
func sendTaskEvent(sink EventSink, ev *keeperv1.TaskEvent, task *keeperv1.RenderedTask, passage int32) error {
	ev.NoLog = task.GetNoLog()
	// Эхо ApplyRequest.passage (ADR-056): keeper коррелирует терминал per-
	// (apply_id, sid, passage) и копит register для render следующего Passage.
	// Единая точка проставления для всех TaskEvent прогона.
	ev.Passage = passage
	// Эхо RenderedTask.plan_index (ADR-056 §S1 fix Variant B): ГЛОБАЛЬНЫЙ сквозной
	// индекс задачи по всему плану (все Passage). Keeper корелирует register
	// именно по нему (apply_task_register.plan_index), НЕ по локальному
	// TaskEvent.task_idx (позиция в ApplyRequest.tasks[] — она локальна для
	// passage/host). N=1-прогон / старый Keeper без поля → plan_index=0=task_idx,
	// поведение БИТ-В-БИТ. Единая точка проставления для всех TaskEvent прогона.
	ev.PlanIndex = task.GetPlanIndex()
	return sink.SendTaskEvent(ev)
}

// defaultRetryDelay — пауза между попытками, если retry_delay не задан/невалиден
// (DSL-ядро retry.delay default, destiny/tasks.md §9).
const defaultRetryDelay = 5 * time.Second

// runTaskWithRetry — обёртка над runTask, реализующая DSL-ядро retry:/until:
// (destiny/tasks.md §9, Soul-side flow-control). Делает до retry_count попыток
// runTask (каждая — «одна попытка → один TaskEvent», контракт runTask не меняется);
// промежуточные попытки наружу НЕ эмитятся — caller получает TaskEvent ПОСЛЕДНЕЙ
// попытки (контракт «один TaskEvent на task_idx» сохранён, attempts-счётчик не вводим).
//
// Семантика (per architect):
//   - retry_count 0/1/пусто → одна попытка (обратная совместимость: без retry).
//   - БЕЗ until: повтор пока попытка FAILED/TIMED_OUT; первый не-FAILED исход
//     (OK/CHANGED) → выход; все исчерпаны → финальный статус ПОСЛЕДНЕЙ попытки как
//     есть (FAILED или TIMED_OUT — TIMED_OUT НЕ схлопывается в FAILED). failed_when:
//     false (ignore_errors) делает попытку OK → «не-FAILED исход» → выход на первой
//     попытке (ignore_errors побеждает retry).
//   - С until: until вычисляется ПОСЛЕ каждой попытки (после changed_when/failed_when
//     override). until-true → выход, финальный статус = статус попытки КАК ЕСТЬ (until
//     НЕ override-ит failed). until-false → delay → следующая попытка. Все попытки с
//     until-false → задача FAILED (flowcontrol.until_exhausted), ДАЖЕ если попытка
//     OK/CHANGED. На TIMED_OUT-попытке until НЕ вычисляется (таймаут = «неуспех,
//     повторить если попытки остались»).
//   - delay (retry_delay, default 5s) применяется ТОЛЬКО между попытками (не перед
//     первой, не после последней); прерывается отменой прогона по runCtx.
//   - cancel во время delay/попытки → выход из петли; CANCELLED-разбор делает caller
//     (Run проверяет runCtx.Err() после возврата).
func (r *ApplyRunner) runTaskWithRetry(runCtx context.Context, applyID string, idx int32, task *keeperv1.RenderedTask, registerByName map[string]any, dryRun bool) *keeperv1.TaskEvent {
	// dry_run (Scry, ADR-031): pure-read Plan ВМЕСТО Apply. Retry/until не
	// применяются — read детерминирован (ресурс либо расходится с желаемым, либо
	// нет; повтор чтения смысла не имеет), а Apply на dry_run не вызывается вовсе.
	// Поэтому dry_run обрабатывается одной попыткой planTask, минуя retry-петлю.
	if dryRun {
		return r.planTask(runCtx, applyID, idx, task)
	}

	maxAttempts := int(task.GetRetryCount())
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	until := task.GetUntil()

	// Fast-path: одна попытка без until → поведение в точности как раньше (runTask
	// напрямую), без лишней delay-обвязки.
	if maxAttempts == 1 && until == "" {
		ev, _ := r.runTask(runCtx, applyID, idx, task, registerByName)
		return ev
	}

	delay := parseRetryDelay(task)

	var ev *keeperv1.TaskEvent
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Попытки со второй — это retry (повтор после неуспеха/until-false).
		// soul_apply_task_retries_total: считаем именно повторы, не первую попытку.
		if attempt > 1 {
			r.metrics.ObserveRetry()
		}
		var self map[string]any
		ev, self = r.runTask(runCtx, applyID, idx, task, registerByName)

		// Cancel во время попытки → немедленный выход; CANCELLED-разбор — в Run.
		if runCtx.Err() != nil {
			return ev
		}

		status := ev.GetStatus()
		timedOut := status == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT

		if until == "" {
			// retry БЕЗ until: первый не-FAILED исход (OK/CHANGED) → выход.
			// FAILED/TIMED_OUT → повтор, если попытки остались. На последней попытке
			// возвращаем статус как есть (TIMED_OUT не схлопываем).
			if status != keeperv1.TaskStatus_TASK_STATUS_FAILED && !timedOut {
				return ev
			}
		} else if self == nil && !timedOut {
			// Терминально-ошибочная НЕ-таймаут-ветка runTask (selfRegister==nil):
			// bad address / module not found / flow-control compile-/runtime-error в
			// changed_when/failed_when. ev уже несёт точный код причины
			// (flowcontrol.changed_when_error / failed_when_error / …) и статус FAILED.
			// until-eval тут не имеет смысла (нет register.self) и затёр бы исходный
			// код на flowcontrol.until_error — возвращаем ev как есть, без повтора.
			// TIMED_OUT (тоже self==nil) сюда НЕ попадает: таймаут = «неуспех, повторить
			// если попытки остались» — он проваливается в общую ветку повтора ниже.
			return ev
		} else if !timedOut {
			// until (+retry): на TIMED_OUT-попытке until НЕ вычисляется — это «неуспех,
			// повторить если попытки остались». Иначе until-eval ПОСЛЕ override.
			ok, err := r.evalUntil(until, task, registerByName, self)
			if err != nil {
				// Runtime-/compile-error CEL в until → задача FAILED (как when/
				// changed_when/failed_when, templating.md §10). Терминально, без повтора.
				r.logFlowControlError("until", task, err)
				return flowControlErrorEvent(applyID, idx, "flowcontrol.until_error", task, until, err)
			}
			if ok {
				// until-true → выход; финальный статус = статус попытки КАК ЕСТЬ
				// (until НЕ override-ит failed: failed остаётся failed).
				return ev
			}
		}

		// Попытка неуспешна (или until-false): delay перед следующей, ТОЛЬКО если
		// она будет (не после последней попытки). Delay interruptible по runCtx
		// (taskCtx уже истёк через defer внутри runTask).
		if attempt < maxAttempts {
			select {
			case <-time.After(delay):
			case <-runCtx.Done():
				return ev
			}
		}
	}

	// Попытки исчерпаны. С until: until так и не стал truthy → FAILED
	// (until_exhausted), ДАЖЕ если последняя попытка OK/CHANGED. Без until:
	// финальный статус последней попытки уже терминальный FAILED/TIMED_OUT —
	// возвращаем как есть (TIMED_OUT не схлопываем в FAILED).
	if until != "" {
		return untilExhaustedEvent(applyID, idx, task, until, maxAttempts)
	}
	return ev
}

// parseRetryDelay парсит retry_delay тем же config.ParseDuration, что и timeout
// (единая convention `duration` Soul Stack). Пусто/невалид/неположительная →
// defaultRetryDelay (5s): defensive (формат провалидирован validateRetryField при
// парсе destiny), без падения на служебной ошибке.
func parseRetryDelay(task *keeperv1.RenderedTask) time.Duration {
	rd := task.GetRetryDelay()
	if rd == "" {
		return defaultRetryDelay
	}
	d, err := config.ParseDuration(rd)
	if err != nil || d <= 0 {
		slog.Default().Warn("runtime: невалидный/неположительный retry delay, применён default",
			slog.String("task", task.GetName()),
			slog.String("retry_delay", rd),
			slog.Duration("default", defaultRetryDelay))
		return defaultRetryDelay
	}
	return d
}

// evalUntil вычисляет until-предикат той же sandboxed-песочницей и активацией, что
// failed_when (flow_context + register.* предыдущих задач + register.self свежей
// попытки с применёнными changed_when/failed_when). self — selfRegister от runTask
// (nil на терминально-ошибочных ветках; туда until не доходит).
func (r *ApplyRunner) evalUntil(expr string, task *keeperv1.RenderedTask, registerByName map[string]any, self map[string]any) (bool, error) {
	return r.evalFlowPredicate(expr, task, mergeRegisterSelf(registerByName, self))
}

// untilExhaustedEvent — TaskEvent для исчерпания retry-петли с until-false на всех
// попытках: задача FAILED (flowcontrol.until_exhausted), даже если последняя попытка
// была OK/CHANGED (until — обязательное условие успеха, destiny/tasks.md §9).
func untilExhaustedEvent(applyID string, idx int32, task *keeperv1.RenderedTask, until string, attempts int) *keeperv1.TaskEvent {
	ev := &keeperv1.TaskEvent{
		ApplyId: applyID,
		TaskIdx: idx,
		Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error: &keeperv1.TaskError{
			Code:    "flowcontrol.until_exhausted",
			Module:  task.GetModule(),
			Message: fmt.Sprintf("until %q не стал truthy за %d попыток", until, attempts),
		},
	}
	ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
	return ev
}

// runTask диспетчит ОДНУ попытку задачи и собирает финальный TaskEvent.
// Возвращает заполненный event без отправки в sink — отправляет caller, и
// selfRegister — register.self ПОСЛЕДНЕЙ попытки (DSL-ядро changed/failed/timed_out
// + output:-поля, с применёнными changed_when/failed_when override). selfRegister
// нужен обёртке [ApplyRunner.runTaskWithRetry] для until-eval (он смотрит на
// register.self после override). На терминально-ошибочных ветках (nil task / bad
// address / module not found / timed_out / flow-control compile-error) selfRegister
// = nil — until туда не доходит (retry-обёртка трактует TIMED_OUT/FAILED по статусу).
//
// retry/until здесь НЕ обрабатываются — это «одна попытка → один TaskEvent» (контракт
// не меняется); петлю накручивает runTaskWithRetry.
func (r *ApplyRunner) runTask(ctx context.Context, applyID string, idx int32, task *keeperv1.RenderedTask, registerByName map[string]any) (*keeperv1.TaskEvent, map[string]any) {
	ev := &keeperv1.TaskEvent{
		ApplyId: applyID,
		TaskIdx: idx,
	}
	if task == nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "internal.nil_task",
			Message: "RenderedTask is nil",
		}
		return ev, nil
	}

	// `module:` приходит как `<namespace>.<module>.<state>` (например,
	// "core.pkg.installed"). Plugin-вызов принимает `state` отдельно от
	// имени, поэтому делим адрес.
	modName, state, ok := config.SplitModuleAddr(task.GetModule())
	if !ok {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "module.bad_address",
			Module:  task.GetModule(),
			Message: fmt.Sprintf("expected <namespace>.<module>.<state>, got %q", task.GetModule()),
		}
		return ev, nil
	}

	mod, found := r.registry.Lookup(modName)
	if !found {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "module.not_found",
			Module:  modName,
			Message: fmt.Sprintf("module %q is not registered (task %q)", modName, task.GetName()),
		}
		return ev, nil
	}

	// Per-task timeout (DSL-ядро timeout:, destiny/tasks.md §9): дочерний от ctx
	// контекст с дедлайном на одну попытку Apply. taskCtx ДОЧЕРНИЙ от runCtx —
	// при истечении его дедлайна runCtx.Err() остаётся nil, поэтому ветка cancel
	// в Run (if runCtx.Err() != nil → CANCELLED) не срабатывает, а статус
	// TIMED_OUT, выставленный ниже, сохраняется. Пусто timeout → лимита нет
	// (только scenario-ceiling); общий per-task дефолт сознательно НЕ вводим —
	// он сломал бы легитимно-долгие core.archive/core.url.
	//
	// Парсер — config.ParseDuration (тот же, что keeper применяет при ПАРСЕ
	// destiny: validateDurationField; и что core.url/Reaper используют). Это
	// единственная convention `duration` Soul Stack: Go-формы + суффикс `<N>d`
	// (docs/keeper/config.md → «Конвенции типов»). Голый time.ParseDuration не
	// понимает `30d` — keeper принял бы такой timeout, а Soul тихо отбросил.
	taskCtx := ctx
	if to := task.GetTimeout(); to != "" {
		switch d, err := config.ParseDuration(to); {
		case err != nil:
			// Невалидная duration-строка (defensive: Keeper уже валидировал при
			// парсе destiny, «не должно случиться»). Трактуем как «не задан» + warn,
			// а не падаем на служебной ошибке.
			slog.Default().Warn("runtime: невалидный task timeout, лимит не применён",
				slog.String("task", task.GetName()),
				slog.String("module", modName),
				slog.String("timeout", to),
				slog.Any("error", err))
		case d > 0:
			var cancel context.CancelFunc
			taskCtx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		default:
			// d <= 0 (`0s`, `-1s`): трактуем как «лимита нет», а НЕ как мгновенный
			// дедлайн. WithTimeout(ctx, 0) истёк бы немедленно → ложный TIMED_OUT
			// ещё до запуска модуля.
			slog.Default().Warn("runtime: неположительный task timeout, лимит не применён",
				slog.String("task", task.GetName()),
				slog.String("module", modName),
				slog.String("timeout", to))
		}
	}

	// Soulprint-инжект (Вариант A, ADR-018(b)): in-process core-модуль,
	// реализующий util.SoulprintAware (core.pkg / core.service), получает собранный
	// факт хоста ПЕРЕД Apply — это primary-источник выбора backend-а (pkg-mgr /
	// init-система), единый с CEL `soulprint.self.os.*`. Out-of-process-плагины
	// интерфейс не реализуют → факт не получают (зарезервировано на Вариант B).
	if aware, ok := mod.(util.SoulprintAware); ok {
		aware.SetHostFacts(r.hostFacts)
	}

	stream := newInProcApplyStream(taskCtx)
	pluginReq := &pluginv1.ApplyRequest{
		State:  state,
		Params: task.GetParams(),
	}

	// Apply-вызов блокирующий; модуль шлёт ApplyEvent в stream и возвращает
	// nil или error. Финальный event — последний с changed/failed; раньше
	// идут диагностические message-ы (мы их игнорируем — TaskEvent несёт
	// финальный статус, прогресс в MVP не передаём).
	applyErr := mod.Apply(pluginReq, stream)
	stream.close()
	last := stream.lastEvent()

	// Per-task timeout истёк (taskCtx — дочерний дедлайн), а родительский ctx жив
	// (отличаем timeout от CancelApply): задача — TIMED_OUT. Проверяем ДО разбора
	// applyErr — модуль обычно возвращает ctx.Err() (DeadlineExceeded), и без
	// этой ветки он маскировался бы под module.error.
	if taskCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
		ev.Error = &keeperv1.TaskError{
			Code:    "task.timed_out",
			Module:  modName,
			Message: fmt.Sprintf("task %q timed out after %s", task.GetName(), task.GetTimeout()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), last)
		return ev, nil
	}

	// Базовый исход от модуля (changed/failed) до override flow-control-предикатами.
	// Исходную ошибку модуля держим отдельно — её может перекрыть failed_when:false
	// (ignore_errors), но в этом случае она НЕ теряется, а сохраняется в
	// register.self.ignored_error (см. ниже).
	var (
		baseChanged bool
		baseFailed  bool
		moduleErr   *keeperv1.TaskError
	)
	switch {
	case applyErr != nil:
		baseFailed = true
		moduleErr = &keeperv1.TaskError{
			Code:    "module.error",
			Module:  modName,
			Message: applyErr.Error(),
		}
	case last == nil:
		// Apply вернул nil, но не прислал ни одного события — модуль
		// неисправен или это no-op (например, Plan-only). Считаем OK без
		// changes; модули core MVP всегда шлют финальное событие, см.
		// util.SendOK / SendChanged / SendFailed.
	case last.GetFailed():
		baseFailed = true
		moduleErr = &keeperv1.TaskError{
			Code:    "module.failed",
			Module:  modName,
			Message: last.GetMessage(),
		}
	case last.GetChanged():
		baseChanged = true
	}

	// flow-control ПОСЛЕ Apply (ADR-012(d)): сначала changed_when (override
	// changed), потом failed_when (override failed). Активация — flow_context +
	// register.* (предыдущих задач) + register.self.* (СВЕЖИЙ результат: changed/
	// failed/timed_out + output-поля). selfRegister строим из БАЗОВОГО исхода
	// модуля — changed_when видит сырой changed, failed_when видит результат уже
	// с применённым changed_when (см. порядок). timed_out здесь всегда false —
	// ветка TIMED_OUT обработана выше и сюда не доходит.
	selfRegister := selfRegisterData(baseChanged, baseFailed, last)

	changed := baseChanged
	if cw := task.GetChangedWhen(); cw != "" {
		res, err := r.evalFlowPredicate(cw, task, mergeRegisterSelf(registerByName, selfRegister))
		if err != nil {
			r.logFlowControlError("changed_when", task, err)
			return flowControlErrorEvent(applyID, idx, "flowcontrol.changed_when_error", task, cw, err), nil
		}
		changed = res
		selfRegister["changed"] = changed
	}

	failed := baseFailed
	if fw := task.GetFailedWhen(); fw != "" {
		res, err := r.evalFlowPredicate(fw, task, mergeRegisterSelf(registerByName, selfRegister))
		if err != nil {
			r.logFlowControlError("failed_when", task, err)
			return flowControlErrorEvent(applyID, idx, "flowcontrol.failed_when_error", task, fw, err), nil
		}
		failed = res
		selfRegister["failed"] = failed
	}

	// Финальный статус: failed имеет приоритет над changed (FAILED — отдельный
	// терминал enum). changed_when определил changed, failed_when определил failed.
	switch {
	case failed:
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		// Источник ошибки: если упал именно модуль — его TaskError; если failed
		// искусственно поднят failed_when:true при OK-модуле — синтетическая ошибка.
		if moduleErr != nil {
			ev.Error = moduleErr
		} else {
			ev.Error = &keeperv1.TaskError{
				Code:    "flowcontrol.failed_when",
				Module:  modName,
				Message: fmt.Sprintf("failed_when %q вычислился в true", task.GetFailedWhen()),
			}
		}
	case changed:
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
	default:
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_OK
	}

	// register_data: финальный flow-control-исход (changed/failed после override)
	// + output:-поля. Берём из selfRegister — там уже учтены changed_when/failed_when.
	ev.RegisterData = registerStruct(selfRegister)

	// ignore_errors-аудит (ADR-012(d)): модуль упал, но failed_when:false
	// перекрыл провал (итог OK/CHANGED). Исходную ошибку НЕ теряем — она уезжает
	// в register.self.ignored_error (доступна последующим предикатам, уходит в
	// audit через register_data). TaskEvent.error при этом остаётся пустым —
	// контракт apply.proto: error заполнен только при FAILED/TIMED_OUT.
	if !failed && moduleErr != nil {
		ev.RegisterData.GetFields()["ignored_error"] = structpb.NewStringValue(moduleErr.GetMessage())
		// until-eval (retry-обёртка) видит register.self той же формы, что и
		// финальный register_data — кладём ignored_error и в selfRegister.
		selfRegister["ignored_error"] = moduleErr.GetMessage()
	}
	return ev, selfRegister
}

// selfRegisterData строит map register.self.* для flow-control-предикатов
// changed_when/failed_when: DSL-ядро (changed/failed/timed_out) текущей задачи +
// output:-поля из финального ApplyEvent. timed_out всегда false — TIMED_OUT
// обработан в runTask раньше и в flow-control не попадает. Это форма cel-активации
// (map[string]any), как registerByName, а не *structpb.Struct.
func selfRegisterData(changed, failed bool, last *pluginv1.ApplyEvent) map[string]any {
	self := map[string]any{
		"changed":   changed,
		"failed":    failed,
		"timed_out": false,
	}
	if last != nil && last.GetOutput() != nil {
		for k, v := range last.GetOutput().AsMap() {
			self[k] = v
		}
	}
	return self
}

// mergeRegisterSelf накладывает register.self (свежий результат текущей задачи) на
// register-индекс предыдущих задач, не мутируя исходный map: changed_when/
// failed_when читают register.<предыдущие>.* И register.self.*.
func mergeRegisterSelf(prev map[string]any, self map[string]any) map[string]any {
	merged := make(map[string]any, len(prev)+1)
	for k, v := range prev {
		merged[k] = v
	}
	merged["self"] = self
	return merged
}

// registerStruct сериализует self-register-map (changed/failed/timed_out + output)
// в *structpb.Struct для TaskEvent.register_data. skipped здесь всегда false —
// дошедшая до runTask задача не пропущена. output-значения уже structpb-формы
// (взяты из ApplyEvent.Output), пере-оборачиваем через NewValue.
func registerStruct(self map[string]any) *structpb.Struct {
	fields := make(map[string]*structpb.Value, len(self)+1)
	for k, v := range self {
		val, err := structpb.NewValue(v)
		if err != nil {
			// output-поле невыразимо в structpb (не должно случаться — оно само
			// пришло из *structpb.Struct). Пропускаем, чтобы не ронять прогон.
			continue
		}
		fields[k] = val
	}
	fields["skipped"] = structpb.NewBoolValue(false)
	return &structpb.Struct{Fields: fields}
}

// evalFlowPredicate вычисляет changed_when/failed_when sandboxed flow-control-
// движком. Активация — flow_context (input/vars/essence/incarnation/self) + register
// (предыдущие задачи + register.self свежего результата). Симметрично evalWhen.
func (r *ApplyRunner) evalFlowPredicate(expr string, task *keeperv1.RenderedTask, reg map[string]any) (bool, error) {
	return r.flowEngine.EvalPredicate(expr, flowControlVars(task.GetFlowContext(), reg))
}

// flowControlErrorEvent — единый TaskEvent для runtime-/compile-error CEL в
// changed_when/failed_when: задача FAILED (как when, templating.md §10). Симметрично
// when-ветке в Run.
func flowControlErrorEvent(applyID string, idx int32, code string, task *keeperv1.RenderedTask, expr string, err error) *keeperv1.TaskEvent {
	ev := &keeperv1.TaskEvent{
		ApplyId: applyID,
		TaskIdx: idx,
		Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error: &keeperv1.TaskError{
			Code:    code,
			Module:  task.GetModule(),
			Message: fmt.Sprintf("%s %q: %v", code, expr, err),
		},
	}
	ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
	return ev
}

// evalWhen вычисляет flow-control-предикат `when:` (ADR-012(d)) Soul-side
// sandboxed cel-go-движком. Пустой when → (true, nil) (безусловный запуск).
//
// Активация строится из RenderedTask.flow_context (литеральный per-host снапшот
// { input, vars, essence, incarnation, self }, собранный Keeper-ом на CEL-фазе) +
// register (registerByName — payload предыдущих задач по register-имени, Soul
// строит сам). soulprint биндится {self: flow_context.self} — каноническая форма
// soulprint.self.<path>; soulprint.hosts/where недоступны (sandbox-изоляция).
//
// changed_when/failed_when вычисляются отдельно — ПОСЛЕ Apply, в [ApplyRunner.runTask]
// (override changed/failed по результату); evalWhen — только gating ДО Apply.
func (r *ApplyRunner) evalWhen(task *keeperv1.RenderedTask, registerByName map[string]any) (bool, error) {
	when := task.GetWhen()
	if when == "" {
		return true, nil
	}
	return r.flowEngine.EvalPredicate(when, flowControlVars(task.GetFlowContext(), registerByName))
}

// flowControlVars строит cel.Vars из flow_context-снапшота и накопленного
// registerByName. flow_context — данные от Keeper-а (input/vars/essence/
// incarnation/self), register — результаты предыдущих задач (Soul строит).
// nil/отсутствующие секции → пустые map (штатный CEL no-such-key, не паника).
func flowControlVars(flowCtx *structpb.Struct, registerByName map[string]any) cel.Vars {
	fc := map[string]any{}
	if flowCtx != nil {
		fc = flowCtx.AsMap()
	}
	return cel.Vars{
		Input:         flowSection(fc, "input"),
		Vars:          flowSection(fc, "vars"),
		Essence:       flowSection(fc, "essence"),
		Incarnation:   flowSection(fc, "incarnation"),
		SoulprintSelf: flowSection(fc, "self"),
		Register:      registerByName,
		// AllowHosts намеренно false (zero-value): flow-control-движок и так
		// форсит изоляцию soulprint.hosts (NewFlowControl); дублируем намерение.
	}
}

// flowSection извлекает секцию верхнего уровня flow_context как map[string]any.
// Отсутствие/не-объект → пустой map (обращение к полю → штатный no-such-key).
func flowSection(fc map[string]any, key string) map[string]any {
	if sec, ok := fc[key].(map[string]any); ok {
		return sec
	}
	return map[string]any{}
}

// logFlowControlError логирует ошибку вычисления flow-control-предиката. Compile-/
// unsupported-ошибка на Soul-е = internal (Keeper обязан был отсеять невалидный
// предикат при валидации перед рендером) — warn-уровень, чтобы такое расхождение
// keeper↔soul было видно в логах/OTel. Runtime-error CEL (register.x нет) —
// штатная ошибка автора Destiny, тоже warn (задача всё равно станет FAILED).
func (r *ApplyRunner) logFlowControlError(kind string, task *keeperv1.RenderedTask, err error) {
	var ce *cel.ErrCompile
	var ue *cel.ErrUnsupported
	if errors.As(err, &ce) || errors.As(err, &ue) {
		slog.Default().Warn("runtime: flow-control predicate невалиден на Soul (Keeper пропустил — internal-расхождение)",
			slog.String("kind", kind),
			slog.String("task", task.GetName()),
			slog.String("module", task.GetModule()),
			slog.Any("error", err))
		return
	}
	slog.Default().Warn("runtime: flow-control predicate runtime-error",
		slog.String("kind", kind),
		slog.String("task", task.GetName()),
		slog.String("module", task.GetModule()),
		slog.Any("error", err))
}

// recordRegister копит register-payload завершённой/пропущенной задачи в оба
// индекса: registerByIdx (по позиции, для onchanges-gating) и registerByName (по
// register-имени, для flow-control-предикатов when:/…). Пустое name → в
// registerByName не пишем (задача без register: адресуется только своим idx).
// registerByName хранит payload как map[string]any (форма cel-активации), а не
// *structpb.Struct: cel читает Go-данные через адаптер.
func recordRegister(byIdx map[int32]*structpb.Struct, byName map[string]any, idx int32, name string, data *structpb.Struct) {
	byIdx[idx] = data
	if name != "" {
		byName[name] = data.AsMap()
	}
}

// skipOnChanges решает, пропустить ли задачу по DSL-ядру `onchanges:`
// (destiny/tasks.md §8). onchangesIdx — индексы задач-источников (резолвнутые
// Keeper-ом register-имена, proto onchanges_idx); registerByIdx — register уже
// выполненных задач прогона по индексу.
//
// Семантика: пусто onchangesIdx → false (безусловный запуск). Иначе пропуск
// (true), ЕСЛИ ни у одного источника register.changed != true. Хотя бы один
// changed → false (выполнить). Отсутствующий в registerByIdx источник трактуется
// как changed=false (источник не выполнялся — например, сам был пропущен): он не
// «спасает» от пропуска, что согласовано с skipped ≠ changed.
func skipOnChanges(onchangesIdx []int32, registerByIdx map[int32]*structpb.Struct) bool {
	if len(onchangesIdx) == 0 {
		return false
	}
	for _, srcIdx := range onchangesIdx {
		rd := registerByIdx[srcIdx]
		if rd.GetFields()["changed"].GetBoolValue() {
			return false
		}
	}
	return true
}

// skipOnFail решает, пропустить ли onfail-задачу по DSL-ядру `onfail:`
// (destiny/tasks.md §8) — rescue-зеркало skipOnChanges, триггер register.failed
// вместо register.changed. onfailIdx — индексы задач-источников (резолвнутые
// Keeper-ом register-имена, proto onfail_idx); registerByIdx — register уже
// выполненных задач прогона по индексу.
//
// Семантика: пусто onfailIdx → false (задача не-onfail, gating не применяется —
// решение об исполнении принимают when/onchanges/runFailed-ветка). Иначе пропуск
// (true), ЕСЛИ ни у одного источника register.failed != true. Хотя бы один failed
// → false (выполнить rescue). register.failed охватывает и TIMED_OUT — он пишется
// в register с failed==true (buildRegisterData), так что таймаут источника тоже
// триггерит onfail. Отсутствующий в registerByIdx источник трактуется как
// failed=false (источник не выполнялся — например, сам был пропущен): он не
// «активирует» onfail, что согласовано со skipped ≠ failed.
func skipOnFail(onfailIdx []int32, registerByIdx map[int32]*structpb.Struct) bool {
	if len(onfailIdx) == 0 {
		return false
	}
	for _, srcIdx := range onfailIdx {
		rd := registerByIdx[srcIdx]
		if rd.GetFields()["failed"].GetBoolValue() {
			return false
		}
	}
	return true
}

// skippedTaskEvent — единый TaskEvent для пропущенной задачи (gating when/
// onchanges/onfail не пройден ЛИБО задача пропущена после провала прогона).
// register_data несёт skipped=true, changed=false, failed=false — skipped-задача
// не триггерит ни onchanges, ни onfail последующих (skipped ≠ changed/failed).
func skippedTaskEvent(applyID string, idx int32) *keeperv1.TaskEvent {
	return &keeperv1.TaskEvent{
		ApplyId:      applyID,
		TaskIdx:      idx,
		Status:       keeperv1.TaskStatus_TASK_STATUS_SKIPPED,
		RegisterData: buildRegisterData(keeperv1.TaskStatus_TASK_STATUS_SKIPPED, nil),
	}
}

// buildRegisterData — собирает google.protobuf.Struct для TaskEvent.register_data
// из финального ApplyEvent. В MVP-объёме это {changed, failed, timed_out,
// skipped, output...}; полная схема — docs/destiny/tasks.md (register.<task>.*).
//
// skipped=true только у TASK_STATUS_SKIPPED (gating `onchanges:` не пропустил
// задачу, mod.Apply не вызывался). skipped-задача несёт changed=false — она НЕ
// триггерит onchanges последующих задач (skipped ≠ changed, destiny/tasks.md §8).
func buildRegisterData(status keeperv1.TaskStatus, last *pluginv1.ApplyEvent) *structpb.Struct {
	changed := status == keeperv1.TaskStatus_TASK_STATUS_CHANGED
	failed := status == keeperv1.TaskStatus_TASK_STATUS_FAILED ||
		status == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
	timedOut := status == keeperv1.TaskStatus_TASK_STATUS_TIMED_OUT
	skipped := status == keeperv1.TaskStatus_TASK_STATUS_SKIPPED

	fields := map[string]*structpb.Value{
		"changed":   structpb.NewBoolValue(changed),
		"failed":    structpb.NewBoolValue(failed),
		"timed_out": structpb.NewBoolValue(timedOut),
		"skipped":   structpb.NewBoolValue(skipped),
	}
	if last != nil && last.GetOutput() != nil {
		for k, v := range last.GetOutput().GetFields() {
			fields[k] = v
		}
	}
	return &structpb.Struct{Fields: fields}
}

// aggregateRegisterData строит register_data ТЕРМИНАЛЬНОЙ синтетической задачи
// applier-register (core.noop.run с aggregate_of, материализация Вариант B,
// orchestration.md §2.1.1) как сводный итог дочерних destiny-задач applier-а:
//
//	changed   = OR(registerByIdx[i].changed)
//	failed    = OR(registerByIdx[i].failed)
//	timed_out = OR(registerByIdx[i].timed_out)
//
// по ЛОКАЛЬНЫМ индексам aggregateOf (ремап global→local сделал Keeper, ToProtoTasks).
// Это дублирует семантику register.<applier>: внешний onchanges:[<applier>] / when:
// register.<applier>.changed резолвится по этому register_data. skipped — всегда
// false (агрегат — реальный исход группы, не пропуск самой задачи).
//
// Отсутствующий в registerByIdx источник (sentinel-индекс -1 от ToProtoTasks:
// дочерняя задача отфильтрована where: на этом хосте / уехала в другой Passage)
// читается как nil → его changed/failed/timed_out=false (нулевой вклад в OR),
// симметрично skipOnChanges/skipOnFail. Пустой aggregateOf сюда не доходит (caller
// проверяет len>0); если бы дошёл — все OR=false (no-op applier без дочерних задач).
//
// output-поля дочерних задач НЕ проецируются (это OUT-OF-SCOPE: проброс
// декларированного top-level output: destiny в register.<applier>.<поле> — отдельный
// слайс). Только DSL-ядро changed/failed/timed_out/skipped.
func aggregateRegisterData(aggregateOf []int32, registerByIdx map[int32]*structpb.Struct) *structpb.Struct {
	var changed, failed, timedOut bool
	for _, idx := range aggregateOf {
		rd := registerByIdx[idx]
		fields := rd.GetFields()
		if fields["changed"].GetBoolValue() {
			changed = true
		}
		if fields["failed"].GetBoolValue() {
			failed = true
		}
		if fields["timed_out"].GetBoolValue() {
			timedOut = true
		}
	}
	return &structpb.Struct{Fields: map[string]*structpb.Value{
		"changed":   structpb.NewBoolValue(changed),
		"failed":    structpb.NewBoolValue(failed),
		"timed_out": structpb.NewBoolValue(timedOut),
		"skipped":   structpb.NewBoolValue(false),
	}}
}

// inProcApplyStream — реализация `grpc.ServerStreamingServer[pluginv1.ApplyEvent]`
// для in-process вызова core-модулей. Эмулирует server-stream через slice;
// SetTrailer/SendHeader — no-op (core-модули их не используют).
//
// Сохраняет ВСЕ ApplyEvent-ы, но runtime смотрит только на финальный. В
// логах/debug режиме можно вывести события из stream.events; в MVP это
// просто буфер.
type inProcApplyStream struct {
	grpc.ServerStream
	ctx     context.Context
	events  []*pluginv1.ApplyEvent
	hdr     metadata.MD
	trailer metadata.MD
	closed  bool
}

func newInProcApplyStream(ctx context.Context) *inProcApplyStream {
	return &inProcApplyStream{ctx: ctx, hdr: metadata.MD{}, trailer: metadata.MD{}}
}

func (s *inProcApplyStream) Context() context.Context { return s.ctx }

func (s *inProcApplyStream) Send(ev *pluginv1.ApplyEvent) error {
	if s.closed {
		return fmt.Errorf("inproc stream: Send after close")
	}
	s.events = append(s.events, ev)
	return nil
}

func (s *inProcApplyStream) SetHeader(md metadata.MD) error {
	s.hdr = metadata.Join(s.hdr, md)
	return nil
}
func (s *inProcApplyStream) SendHeader(md metadata.MD) error {
	s.hdr = metadata.Join(s.hdr, md)
	return nil
}
func (s *inProcApplyStream) SetTrailer(md metadata.MD) { s.trailer = metadata.Join(s.trailer, md) }
func (s *inProcApplyStream) SendMsg(m any) error {
	ev, ok := m.(*pluginv1.ApplyEvent)
	if !ok {
		return fmt.Errorf("inproc stream: SendMsg got %T, want *pluginv1.ApplyEvent", m)
	}
	return s.Send(ev)
}
func (s *inProcApplyStream) RecvMsg(any) error {
	return fmt.Errorf("inproc stream: RecvMsg not supported")
}

func (s *inProcApplyStream) close() { s.closed = true }

func (s *inProcApplyStream) lastEvent() *pluginv1.ApplyEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}
