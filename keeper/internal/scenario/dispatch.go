package scenario

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// dispatch выполняет cross-host fan-out прогона и ждёт RunResult всех хостов
// (cross-host barrier, orchestration.md §7).
//
// Pilot-модель (PM-decision): один `apply_id` на прогон, разный `sid` на
// каждый хост → один `ApplyRequest` на хост, несущий ВСЕ задачи, таргетящие
// этот хост (после резолва on/where/run_once, из DispatchPlan). Это согласовано
// с composite PK `(apply_id, sid)` таблицы apply_runs и моделью «один RunResult
// на (apply_id, sid)» в events_runresult.go.
//
// `serial:` (orchestration.md §2.2.1) — rolling-исполнение по ВОЛНАМ хостов:
// хосты прогона, отсортированные по SID, бьются на последовательные волны
// размера ≤width; внутри волны хосты диспатчатся параллельно (Insert+SendApply
// всем), волны строго последовательны (per-wave barrier между ними). При «один
// ApplyRequest на хост» (composite PK) volna — это подмножество хостов прогона,
// а не повторная отправка одному хосту; per-task serial разных задач
// агрегируется в одну ширину волны прогона = минимальная положительная
// SerialWidth среди задач (самое узкое окно — fail-closed-консервативно, см.
// effectiveSerialWidth). 0 → все хосты в одной волне (текущее поведение).
//
// Fail-stop (§2.2.1): первый failed/cancelled хост в волне останавливает
// rolling — последующие волны НЕ стартуют, dispatch возвращает ошибку.
//
// Barrier (§7, инвариант): serial НЕ дробит state-commit. dispatch возвращается
// только после завершения ВСЕХ волн (или fail-stop); state коммитится один раз
// в run() ПОСЛЕ возврата dispatch, никогда по-волново.
func (r *Runner) dispatch(ctx context.Context, spec RunSpec, log *slog.Logger, tasks []*render.RenderedTask, plans []render.DispatchPlan) error {
	// passage 0: N=1-прогон (без staged-render) — единственный Passage, поведение
	// БИТ-В-БИТ как до ADR-056 (одна строка apply_runs на хост, barrier по passage 0).
	// gate=nil: один Passage → cross-passage requisite невозможен.
	return r.dispatchPassage(ctx, spec, log, 0, tasks, plans, nil)
}

// dispatchPassage выполняет cross-host fan-out ОДНОГО Passage (ADR-056 §в.2-3) и
// ждёт его barrier (§в.3). tasks/plans — подмножество ОДНОГО Passage (run.go
// stage-loop отфильтровал по RenderedTask.Passage); passage клеймится в
// ApplyRequest.passage + apply_runs(apply_id, sid, passage) для per-Passage
// корреляции терминала. barrier ждёт терминалы строк именно этого Passage.
//
// serial-логика (волны хостов) работает внутри КАЖДОГО Passage: оси serial (хосты)
// и Passage (задачи) ортогональны (ADR-056 §S4 amend, 2D serial×passage). Ширина
// волны выводится из plans ЭТОГО Passage (effectiveSerialWidth на tasksForPassage-
// срезе — per-Passage width, НЕ per-RUN): probe-Passage без serial едет одной волной,
// даже когда последующий Passage несёт serial:N. N=1-прогон вызывается с passage=0 —
// единственная волна(ы) одного Passage, БИТ-В-БИТ.
func (r *Runner) dispatchPassage(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, tasks []*render.RenderedTask, plans []render.DispatchPlan, gate *crossPassageGate) error {
	noLogByIndex := noLogIndex(tasks)
	perHost := groupByHost(tasks, plans)
	// Cross-passage requisite-gating (ADR-056 R3): per-host резолв onchanges/onfail-
	// связей, чей источник в более раннем Passage. nil-gate (N=1 / Passage 0) → no-op.
	// Применяется ДО dispatch: consumer, чей cross-passage onchanges не сработал на
	// хосте (и без same-passage источника), исключается из среза этого хоста; иначе
	// cross-passage idx убираются с wire (Soul гейтит по same-passage / безусловно).
	perHost = gate.applyGate(perHost, passage)
	if len(perHost) == 0 {
		// Ни одна задача Passage не таргетит ни одного хоста (where: отфильтровал
		// всех на каждой задаче). Не ошибка: нечего применять в этом Passage, идём
		// дальше (для N=1 — прогон успешен no-op-ом).
		log.Info("scenario: dispatch — ни одна задача Passage не таргетит хосты, no-op",
			slog.Int("passage", passage))
		return nil
	}

	sids := sortedSIDs(perHost)
	waves := splitWaves(sids, effectiveSerialWidth(plans))

	// Волны строго последовательны (§2.2.1). dispatchedTotal копит хосты всех
	// уже стартованных волн — финальный barrier ждёт именно их (хост без
	// строки apply_runs не должен заставлять barrier ждать до timeout-а).
	dispatchedTotal := 0
	for wi, wave := range waves {
		dispatched, derr := r.dispatchWave(ctx, spec, log, passage, perHost, wave)
		dispatchedTotal += dispatched

		// Per-wave barrier: ждём терминала хостов ВСЕХ стартованных волн ЭТОГО
		// Passage. classify сканирует apply_runs-строки прогона и фильтрует по
		// passage, поэтому failed-хост текущей волны ломает barrier сразу
		// (fail-stop): следующая волна не стартует.
		if berr := r.waitBarrier(ctx, spec.ApplyID, passage, dispatchedTotal, noLogByIndex, log); berr != nil {
			return berr
		}
		if derr != nil {
			return derr
		}
		if len(waves) > 1 {
			log.Info("scenario: волна serial завершена",
				slog.Int("wave", wi+1), slog.Int("waves_total", len(waves)), slog.Int("hosts", len(wave)))
		}
	}
	return nil
}

// dispatchPlanned — НОВЫЙ путь dispatch-а (ADR-027, Phase 1.4.2): вместо
// inline-рендера и SendApply пишет planned-задания на ВСЕ roster-хосты прогона
// и публикует Summons. Render + перевод в dispatched + SendApply делает Acolyte
// при claim ([RenderForHost] → MarkDispatched → SendApply, ADR-027 amend).
//
// Вариант Б (БЕЗ where-фильтра): planned пишется на КАЖДЫЙ roster-хост, даже
// если on:/where: отфильтрует на нём все задачи — такой хост Acolyte закроет
// no-op-ом в терминал `no_match` (FINDING-01 вариант (б); НЕ success — apply_runs
// не over-reports «успех» на нецелевых хостах), барьер его засчитает как
// benign-терминал. Это держит wantHosts = len(roster) детерминированным на
// dispatch-е без предварительного per-host резолва on:/where: (его делает
// Acolyte). Рецепт у всех хостов идентичен (ServiceRef/ScenarioName/Input С
// vault-ref КАК ЕСТЬ — инвариант A, ДО ResolveInputValuesVault/StartedByAID).
//
// После всех Insert — один best-effort PublishSummons (ошибку глотаем:
// planned-задание персистентно, poll-fallback Acolyte подхватит). Затем
// waitBarrier поллит apply_runs.status до терминала всех вставленных хостов
// (KEY-инвариант: barrier остаётся в run-goroutine в Phase 1). wantHosts = число
// вставленных planned-строк.
//
// tasks нужны лишь для noLogIndex (подавление stderr no_log-задачи в barrier-
// причине) — render для SendApply здесь НЕ делается.
func (r *Runner) dispatchPlanned(ctx context.Context, spec RunSpec, log *slog.Logger, hosts []*topology.HostFacts, tasks []*render.RenderedTask) error {
	if len(hosts) == 0 {
		// Резолв хостов выше (run.go шаг 3) уже отверг пустой roster (no_hosts).
		// Сюда пустой не доходит; защита от программной ошибки.
		return fmt.Errorf("scenario: dispatchPlanned: пустой roster прогона %s", spec.ApplyID)
	}

	recipe := &applyrun.Recipe{
		ServiceRef:   spec.ServiceRef,
		ScenarioName: spec.ScenarioName,
		Input:        spec.Input, // vault-ref КАК ЕСТЬ — инвариант A
		StartedByAID: startedByPtr(spec.StartedByAID),
	}

	dispatched := 0
	for _, h := range hosts {
		if err := applyrun.InsertPlanned(ctx, r.deps.DB, &applyrun.ApplyRun{
			ApplyID:         spec.ApplyID,
			SID:             h.SID,
			IncarnationName: spec.IncarnationName,
			Scenario:        spec.ScenarioName,
			StartedByAID:    startedByPtr(spec.StartedByAID),
			Recipe:          recipe,
		}); err != nil {
			return fmt.Errorf("scenario: insert planned apply_run (%s): %w", h.SID, err)
		}
		dispatched++
		log.Info("scenario: planned-задание записано", slog.String("sid", h.SID))
	}

	// Summons — best-effort: persisted planned-задания подхватит poll-fallback
	// Acolyte даже при потере сигнала (ADR-027(a)). Ошибку только логируем.
	r.publishSummons(ctx, log)

	noLogByIndex := noLogIndex(tasks)
	// Acolyte-путь — non-staged (ADR-056 §S4): planned-задания пишутся passage 0,
	// barrier ждёт срез Passage 0. Staged (N>1) на Acolyte отвергается в run.go.
	return r.waitBarrier(ctx, spec.ApplyID, 0, dispatched, noLogByIndex, log)
}

// publishSummons шлёт один best-effort Summons-сигнал planned-заданий
// (ADR-027(a)). nil-publisher (Summons выключен / unit-тест) → no-op.
// Ошибка только логируется: planned-задания персистентны, poll-fallback Acolyte
// их подхватит — публикация лишь УСКОРЯЕТ пробуждение.
func (r *Runner) publishSummons(ctx context.Context, log *slog.Logger) {
	if r.deps.Summons == nil {
		return
	}
	if err := r.deps.Summons.PublishSummons(ctx); err != nil {
		log.Warn("scenario: publish Summons провален — poll-fallback подхватит", slog.Any("error", err))
	}
}

// hasSerialTask сообщает, несёт ли хоть одна задача scenario `serial:`
// (serial-guard, ADR-027 Phase 1.4.2): такой прогон идёт СТАРЫМ путём даже при
// AcolyteEnabled (распределённый serial — Phase 3). Проверка по РАСПАРСЕННОМУ
// scenario (после ExpandIncludes), БЕЗ render. Task.Serial — opaque any (int>=1
// | "<N>%"), serial: считается заданным при любом non-nil значении; пустая
// строка трактуется как «не задан» (config-валидатор пустой serial не пропускает,
// но fail-closed: пустое значение — не повод гнать в новый путь).
func hasSerialTask(scn *config.ScenarioManifest) bool {
	if scn == nil {
		return false
	}
	for i := range scn.Tasks {
		if serialPresent(scn.Tasks[i].Serial) {
			return true
		}
	}
	return false
}

// serialPresent — задано ли значение `serial:` задачи. Task.Serial — any:
// nil → не задан; пустая строка → не задан (см. hasSerialTask); прочее
// (int / непустая строка-процент) → задан.
func serialPresent(serial any) bool {
	switch v := serial.(type) {
	case nil:
		return false
	case string:
		return v != ""
	default:
		return true
	}
}

// dispatchWave стартует одну волну: Insert(running) + SendApply каждому хосту
// волны (внутри волны — параллельно по семантике §2.2.1; в pilot отправка
// последовательная, но без барьера между хостами одной волны — barrier стоит
// между волнами). Возвращает число успешно отправленных хостов и первую ошибку
// Insert/Send (если была).
//
// При send-фейле хост помечается failed сразу — RunResult от него не придёт,
// иначе per-wave barrier завис бы до timeout-а; failed-строка ломает barrier
// штатно (fail-stop).
func (r *Runner) dispatchWave(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, perHost map[string][]*render.RenderedTask, wave []string) (int, error) {
	dispatched := 0
	for _, sid := range wave {
		// attempt НЕ проставляется (остаётся 0) ОСОЗНАННО: это старый inline-путь
		// без Acolyte-claim и recovery-скана (ADR-027 Phase 0/serial-guard), Ward
		// не клеймится и не переклеймливается, поэтому fencing-epoch здесь
		// вырождается. attempt=0 на проводе = «старый Keeper без fencing»
		// (apply.proto field 4), Soul-guard такой ApplyRequest не отвергает. Это
		// не баг — у inline-пути нет источника stale-дубля, который fencing
		// отсекает.
		req := &keeperv1.ApplyRequest{
			ApplyId: spec.ApplyID,
			// ToProtoTasksForHost(sid): для self-вариативной core.file.rendered
			// подставляет per-host render_context ЭТОГО хоста (а не первого по SID)
			// — частичное закрытие open Q №25 (render_context.self).
			Tasks: render.ToProtoTasksForHost(perHost[sid], sid),
			// passage (ADR-056): Soul эхает его в TaskEvent/RunResult — Keeper
			// коррелирует терминал и копит register per-(apply_id, sid, passage).
			Passage: int32(passage),
		}
		if err := applyrun.Insert(ctx, r.deps.DB, &applyrun.ApplyRun{
			ApplyID:         spec.ApplyID,
			SID:             sid,
			IncarnationName: spec.IncarnationName,
			Scenario:        spec.ScenarioName,
			Status:          applyrun.StatusRunning,
			StartedByAID:    startedByPtr(spec.StartedByAID),
			Passage:         passage,
		}); err != nil {
			return dispatched, fmt.Errorf("scenario: insert apply_run (%s): %w", sid, err)
		}
		// Multi-keeper-guard (footgun acolytes=0): этот старый путь держит
		// владение прогоном in-memory в run-goroutine ЭТОГО инстанса. Если стрим
		// целевого Soul-а держит ДРУГОЙ Keeper-инстанс, RunResult уйдёт туда, а
		// здешний barrier зависнет до runTimeout → incarnation останется в
		// applying. Точечный WARN ровно перед SendApply ловит это в точке выстрела.
		r.warnCrossKeeperDispatch(ctx, sid, log)
		if err := r.deps.Outbound.SendApply(ctx, sid, req); err != nil {
			// error_summary читается наружу через barrier/status_details (GET
			// incarnation, без маскинга). err от SendApply несёт req с сырыми
			// (зарезолвленными) Params и может эхнуть секрет в transport/marshal-
			// сообщении — в наблюдаемый канал кладём только safe-причину без
			// payload-эха. Полный err уходит лишь в обёрнутую ошибку выше (она
			// проходит через MaskSecrets в lockIncarnation перед записью).
			summary := "send_apply_failed"
			_ = applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, sid, passage, applyrun.StatusFailed, &summary)
			return dispatched, fmt.Errorf("scenario: send apply (%s): %w", sid, err)
		}
		dispatched++
		log.Info("scenario: ApplyRequest отправлен", slog.String("sid", sid), slog.Int("tasks", len(perHost[sid])))
	}
	return dispatched, nil
}

// warnCrossKeeperDispatch печатает громкий WARN, если этот (старый) путь
// dispatch-а отправляет ApplyRequest Soul-у, чей EventStream держит ДРУГОЙ
// Keeper-инстанс. Это single-keeper-only footgun дефолта `acolytes: 0`:
// владение прогоном живёт in-memory в run-goroutine ЭТОГО инстанса, а RunResult
// от Soul-а придёт владельцу его стрима (другой инстанс) — здешний barrier не
// увидит завершения и зависнет до runTimeout, оставив incarnation в applying.
// С `acolytes>0` (work-queue ADR-027: claim+dispatch через Redis-Summons +
// наблюдение терминала через общий PG) проблемы нет — туда guard не зовётся.
//
// Guard выключен (no-op) при:
//   - нет LeaseOwner-чекера (nil Redis / unit-тест без координации);
//   - этот инстанс работает в work-queue-режиме (acolyteEnabled=true): сюда мы
//     попадаем только serial-guard-ом, у которого нет cross-keeper-зависания
//     (barrier тот же, но владелец стрима роли не играет — ниже §);
//   - lease целевого Soul-а держим МЫ САМИ (kid совпадает) либо lease-ключа нет
//     (Soul ни у кого на стриме — отдельная проблема, не наш footgun);
//   - ошибка чтения lease (best-effort: не блокируем dispatch, не шумим).
//
// § serial-guard при acolytes>0: даже там этот старый путь держит barrier в
// run-goroutine локального инстанса, поэтому cross-keeper-зависание возможно
// идентично. Чтобы не молчать в этом частном случае, guard проверяет владельца
// независимо от acolyteEnabled — он дёшев (один GET) и срабатывает только на
// реально опасной конфигурации (чужой владелец стрима).
func (r *Runner) warnCrossKeeperDispatch(ctx context.Context, sid string, log *slog.Logger) {
	if r.leaseOwner == nil {
		return
	}
	owner, ok, err := r.leaseOwner.SoulLeaseOwner(ctx, sid)
	if err != nil || !ok || owner == "" || owner == r.kid {
		return
	}
	log.Warn("scenario: footgun multi-keeper + acolytes=0 — Soul на стриме другого Keeper-инстанса; прогон может зависнуть в applying (для HA-кластера выставьте keeper.acolytes>0, ADR-027)",
		slog.String("sid", sid),
		slog.String("stream_owner_kid", owner),
		slog.String("self_kid", r.kid))
}

// waitBarrier поллит apply_runs.status до тех пор, пока все wantHosts хостов
// прогона не достигнут терминального статуса, либо до отмены ctx.
//
// noLogByIndex — множество индексов задач прогона с `no_log: true`; нужно
// barrier-у, чтобы подавить stderr упавшей no_log-задачи в operator-facing
// причине (BUG-3, см. failureReason).
//
// Возврат:
//   - nil — все хосты success.
//   - ошибка — хотя бы один failed/cancelled, либо ctx отменён (timeout/Cancel/
//     Shutdown). Любой не-success терминал ломает прогон (fail-closed, §7).
func (r *Runner) waitBarrier(ctx context.Context, applyID string, passage, wantHosts int, noLogByIndex map[int]bool, log *slog.Logger) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, applyID)
		if err != nil {
			return fmt.Errorf("scenario: barrier poll: %w", err)
		}

		// Cluster-wide Cancel (G1): флаг cancel_requested мог поставить ЛЮБОЙ
		// Keeper-инстанс (в т.ч. не тот, где живёт эта run-goroutine). Проверяем
		// до classify: запрошенная отмена прерывает прогон тем же путём, что и
		// локальный ctx-Cancel (run() → abort → error_locked), но переживает
		// cross-Keeper-роутинг. Локальный Cancel остаётся быстрым путём через
		// <-ctx.Done() ниже.
		if cancelRequested(statuses) {
			log.Info("scenario: barrier — получен cluster-wide Cancel (cancel_requested), прогон отменяется")
			return fmt.Errorf("scenario: barrier прерван: %w", errCancelRequested)
		}

		done, failed := classify(statuses, passage, wantHosts, noLogByIndex)
		if failed != nil {
			return failed
		}
		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("scenario: barrier прерван: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// cancelRequested сообщает, выставлен ли cluster-wide Cancel-флаг (G1) хотя бы
// на одной строке прогона. RequestCancel пишет флаг по apply_id (на все
// running-строки), но проекция несёт его per-host — barrier-у достаточно
// увидеть true на любой строке.
func cancelRequested(statuses []applyrun.HostStatus) bool {
	for i := range statuses {
		if statuses[i].CancelRequested {
			return true
		}
	}
	return false
}

// classify оценивает срез статусов хостов прогона:
//   - failed != nil — есть хотя бы один failed/cancelled/orphaned (fail-closed).
//   - done == true  — все wantHosts достигли терминала и все benign (success
//     либо no_match — FINDING-01 вариант (б): нецелевой хост, на котором on:/
//     where: отфильтровал все задачи).
//   - (false, nil)  — ещё running или строк меньше wantHosts (не все Insert-ы
//     успели / poll опередил).
//
// Причина падения берётся из apply_runs.error_summary (заполнен per-task-ом:
// `task <idx> <module>: <message>`, BUG-3); для no_log-задачи stderr
// подавляется ([failureReason]). NULL error_summary (dispatch-level фейл без
// TaskEvent-а) → подставляется сам статус хоста.
func classify(statuses []applyrun.HostStatus, passage, wantHosts int, noLogByIndex map[int]bool) (done bool, failed error) {
	terminal := 0
	for _, hs := range statuses {
		// Staged-render (ADR-056): barrier ЭТОГО Passage считает терминалы строк
		// только своего среза. Строки предыдущих Passage (уже success) и keeper-
		// target-а (passage 0, исполнен ДО host-fan-out-а) сюда не относятся —
		// иначе terminal раздулся бы и barrier объявил бы done преждевременно.
		// N=1-прогон: единственный Passage 0 → фильтр пропускает все host-строки,
		// БИТ-В-БИТ как до staged-render.
		if hs.Passage != passage {
			continue
		}
		// keeper-target (`on: keeper`) — НЕ хост: его apply_runs-строку пишет
		// dispatchKeeperTasks ДО host-barrier-а, а wantHosts считает только
		// реальные хосты. Без этого исключения keeper-success раздувал бы
		// terminal на единицу и barrier объявлял бы done на один хост раньше
		// (silent success при падении последнего хоста). keeper-failed сюда не
		// доходит — dispatchKeeperTasks abort-ит прогон ДО host-fan-out-а, так
		// что фильтрация failed-ветку не ослабляет.
		if hs.SID == render.KeeperTargetSID {
			continue
		}
		switch hs.Status {
		case applyrun.StatusSuccess, applyrun.StatusNoMatch:
			// no_match (FINDING-01 вариант (б)) — терминальный НЕ-провал (benign,
			// как success): хост нецелевой для прогона (on:/where: отфильтровал
			// все задачи), Acolyte закрыл его no_match без ApplyRequest. Засчитываем
			// в terminal, чтобы барьер досчитал прогон и НЕ висел до runTimeout.
			// Прогон, где целевые success + не-целевые no_match → done без failed →
			// incarnation идёт в ready (commitSuccess), не error_locked.
			terminal++
		case applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusOrphaned:
			// orphaned (Soul-reconcile, ADR-027(g)) — терминальный не-успех:
			// барьер засчитывает его как фейл хоста (incarnation → error_locked
			// через commitRunState), как failed/cancelled. RunResult по нему
			// никогда не придёт — без этой ветки барьер висел бы до runTimeout.
			return false, fmt.Errorf("scenario: хост %s завершился со статусом %s (%s)",
				hs.SID, hs.Status, failureReason(hs, noLogByIndex))
		}
	}
	return terminal >= wantHosts, nil
}

// failureReason формирует operator-facing причину падения хоста из строки
// apply_runs (BUG-3). Источники по приоритету:
//
//   - error_summary (per-task `task <idx> <module>: <message>`) — основной;
//   - сам статус хоста (`failed`/`cancelled`) — если summary нет (dispatch-
//     level фейл без TaskEvent-а).
//
// no_log: если упавшая задача объявлена `no_log: true`, её stderr мог нести
// пароль — message заменяется нейтральным `(no_log task failed)` с сохранением
// `task <idx>`-префикса для триажа. MaskSecrets уже отработал на write-path-е
// (recordTaskFailure); тут — полное подавление тела сообщения no_log-задачи.
//
// Адрес упавшей задачи в noLogByIndex — по ГЛОБАЛЬНОМУ plan_index (ADR-056 §S1
// fix Variant B): noLogByIndex построен по RenderedTask.Index (глобальный
// сквозной индекс), failed_plan_index несёт тот же глобальный индекс (эхо
// TaskEvent.plan_index). Локальный task_idx под staged/per-host-where указывал
// бы на соседнюю задачу — и тогда либо НЕ подавил бы stderr реальной no_log-
// задачи (утечка пароля), либо подавил бы причину обычной задачи. Резолв строго
// глобальный; для N=1 plan_index==task_idx, поведение БИТ-В-БИТ.
func failureReason(hs applyrun.HostStatus, noLogByIndex map[int]bool) string {
	if hs.ErrorSummary == nil {
		return string(hs.Status)
	}
	if idx, ok := failedPlanIndex(hs); ok && noLogByIndex[idx] {
		return fmt.Sprintf("task %d: (no_log task failed)", idx)
	}
	return *hs.ErrorSummary
}

// noLogIndex строит множество индексов задач прогона с `no_log: true`.
// Используется barrier-ом для подавления stderr упавшей no_log-задачи в
// operator-facing причине ([failureReason], BUG-3).
func noLogIndex(tasks []*render.RenderedTask) map[int]bool {
	out := make(map[int]bool)
	for _, t := range tasks {
		if t.NoLog {
			out[t.Index] = true
		}
	}
	return out
}

// tasksForPassage отбирает RenderedTask-и и их DispatchPlan-ы, принадлежащие
// Passage p (staged-render, ADR-056): ApplyRequest одного Passage несёт только
// его задачи, barrier ждёт только его терминалы. keeper-side задачи (Keeper-plan)
// исключаются — они исполнены ДО host-fan-out-а (dispatchKeeperTasks, passage 0).
// Plan-ы фильтруются по тому же набору task-индексов (groupByHost уже игнорирует
// plan без задачи в наборе, но явная фильтрация делает serial-ширину Passage-точной).
func tasksForPassage(tasks []*render.RenderedTask, plans []render.DispatchPlan, p int) ([]*render.RenderedTask, []render.DispatchPlan) {
	idxInPassage := make(map[int]bool, len(tasks))
	outTasks := make([]*render.RenderedTask, 0, len(tasks))
	for _, t := range tasks {
		if t.Passage == p {
			outTasks = append(outTasks, t)
			idxInPassage[t.Index] = true
		}
	}
	outPlans := make([]render.DispatchPlan, 0, len(plans))
	for _, pl := range plans {
		if pl.Keeper {
			continue
		}
		if idxInPassage[pl.TaskIndex] {
			outPlans = append(outPlans, pl)
		}
	}
	return outTasks, outPlans
}

// groupByHost строит SID → []RenderedTask по DispatchPlan-ам. Каждая задача
// попадает в список тех хостов, на которые она таргетится (TargetSIDs).
// Порядок задач внутри хоста — по Index (= порядок scenario.tasks[]).
func groupByHost(tasks []*render.RenderedTask, plans []render.DispatchPlan) map[string][]*render.RenderedTask {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}

	perHost := make(map[string][]*render.RenderedTask)
	for _, plan := range plans {
		// keeper-side задачи (`on: keeper`) исполнены локально до host-fan-out-а
		// (run.go::dispatchKeeperTasks) — в Soul-side группировку не попадают, иначе
		// их synthetic-target (render.KeeperTargetSID) ушёл бы SendApply-ем как
		// будто Soul.
		if plan.Keeper {
			continue
		}
		task := byIndex[plan.TaskIndex]
		if task == nil {
			continue
		}
		for _, sid := range plan.TargetSIDs {
			perHost[sid] = append(perHost[sid], task)
		}
	}
	return perHost
}

// sortedSIDs возвращает отсортированный по SID список хостов (детерминизм
// dispatch-а, orchestration.md: лексикографически по SID).
func sortedSIDs(perHost map[string][]*render.RenderedTask) []string {
	out := make([]string, 0, len(perHost))
	for sid := range perHost {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}

// effectiveSerialWidth выводит ширину волны из per-task SerialWidth ПЕРЕДАННЫХ
// планов (orchestration.md §2.2.1). serial: — per-task ось, но dispatch-модель
// «один ApplyRequest на хост со всеми его задачами» (composite PK apply_id,sid)
// не может катить разные задачи разными волнами в рамках одного запроса.
// Агрегация: ширина волны = МИНИМАЛЬНАЯ положительная SerialWidth среди задач
// среза (самое узкое окно — строго fail-closed-консервативно: чем уже волна, тем
// меньше хостов под риском при падении). 0 (ни одна задача среза не несёт serial:)
// → все хосты в одной волне (поведение без serial).
//
// Срез — это задачи ОДНОГО Passage (dispatchPassage передаёт tasksForPassage-
// фильтрованные plans): ADR-056 §S4 amend min-width стал per-Passage, НЕ per-RUN.
// Probe-Passage без serial → width 0 (одна волна), даже если другой Passage несёт
// serial:N — его узкое окно НЕ просачивается в чужой Passage (silent-wrong-width
// устранён). При N=1 (один Passage) per-Passage и per-RUN совпадают бит-в-бит.
func effectiveSerialWidth(plans []render.DispatchPlan) int {
	width := 0
	for _, p := range plans {
		if p.SerialWidth <= 0 {
			continue
		}
		if width == 0 || p.SerialWidth < width {
			width = p.SerialWidth
		}
	}
	return width
}

// splitWaves бьёт уже отсортированный по SID список хостов на последовательные
// волны размера ≤width (orchestration.md §2.2.1). width<=0 либо width>=len(sids)
// → одна волна со всеми хостами (serial не задан / шире таргета).
func splitWaves(sids []string, width int) [][]string {
	if width <= 0 || width >= len(sids) {
		return [][]string{sids}
	}
	waves := make([][]string, 0, (len(sids)+width-1)/width)
	for i := 0; i < len(sids); i += width {
		end := i + width
		if end > len(sids) {
			end = len(sids)
		}
		waves = append(waves, sids[i:end])
	}
	return waves
}

// Конвертер render.RenderedTask → keeperv1.RenderedTask (wire-форма) вынесен в
// единый render.ToProtoTasks (keeper/internal/render/prototask.go) —
// переиспользуется dispatch-ом и trial-L2, чтобы при добавлении поля в
// RenderedTask wire-форма не разъехалась копиями.
