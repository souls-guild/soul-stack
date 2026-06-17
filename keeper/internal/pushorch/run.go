package pushorch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// syntheticScenarioName — имя scenario, скомпонованного pushorch вокруг
// единственной apply-задачи. Префикс `_` сигналит «не из пользовательского
// service-репо»; имя транзитное (попадает только в логи render-pipeline).
const syntheticScenarioName = "_push"

// syntheticTaskName — имя единственной apply-задачи в синтетическом scenario.
// То же: транзитное, нужно только для диагностики render-фаз.
const syntheticTaskName = "push.apply"

// orchestratorContextTimeout — потолок длительности async-execution prepare-фазы
// (LoadByInventory + render). Жёсткий cap, чтобы зависший git-fetch / SQL не
// держал goroutine бесконечно при отсутствии request-ctx (HTTP-handler уже
// вернул 202). dispatch-фаза имеет собственный per-host timeout.
const orchestratorContextTimeout = 30 * time.Minute

// SshDispatcher — узкая поверхность [push.SshDispatcher] для orchestrator-а.
// per-host SendApply возвращает RunResult синхронно (push S1+S5, oneshot).
// `providerName` — имя SshProvider-плагина, выбранное ProviderRouter-ом
// (ADR-032 amendment 2026-05-27, P2 W-2/W-3 multi-provider routing); пустая
// строка / неизвестное имя → push.ErrProviderUnknown.
type SshDispatcher interface {
	SendApply(ctx context.Context, sid string, providerName string, req *keeperv1.ApplyRequest) (*keeperv1.RunResult, error)
}

// Cleaner — узкая поверхность [push.SshDispatcher.Cleanup] для best-effort
// post-success-чистки устаревших версий (`cleanup_stale_versions: true`).
// Тот же *push.SshDispatcher удовлетворяет обоим интерфейсам — wire-up
// передаёт его в оба поля. `providerName` — то же, что использовалось в
// предшествующем SendApply (caller хранит per-SID решение).
type Cleaner interface {
	Cleanup(ctx context.Context, sid string, providerName string) error
}

// ProviderRouter — узкая поверхность [push.ProviderRouter] для orchestrator-а.
// Зависимость сужена до одного метода ради лёгкого fake-а в unit-тестах.
type ProviderRouter interface {
	RouteFor(ctx context.Context, sid string) (providerName string, source push.RouteSource, err error)
}

// ProviderMetricsObserver — узкая поверхность [push.Metrics.ObserveProviderRouted]
// (P2 W-4). nil — no-op (push-без-observability стенды).
type ProviderMetricsObserver interface {
	ObserveProviderRouted(providerName, decisionSource string)
}

// AuditWriter — узкая поверхность shared/audit.Writer (тот же интерфейс, что
// keeper/internal/mcp.AuditWriter). Сужение для unit-mock-ов.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// InventoryResolver — узкая поверхность [topology.Resolver.LoadByInventory] для
// PushRun-а. Сужение позволяет fake в unit-тестах без подъёма PG+Redis.
type InventoryResolver interface {
	LoadByInventory(ctx context.Context, sids []string) ([]*topology.HostFacts, error)
}

// RenderPipeline — узкая поверхность [render.Pipeline.Render] (без зависимости
// от *render.Pipeline в подписи Deps). *render.Pipeline удовлетворяет ему.
type RenderPipeline interface {
	Render(ctx context.Context, in render.RenderInput) ([]*render.RenderedTask, []render.DispatchPlan, error)
}

// Deps — внешние зависимости PushRun-а. Все non-Audit поля обязательны;
// AuditWriter опционален (nil → audit-events не пишутся, диагностика остаётся
// в логах). KID — идентификатор Keeper-инстанса для started_by_kid (Reaper
// purge_orphan_push_runs фильтрует осиротевшие прогоны по нему).
type Deps struct {
	Store         *Store
	Topology      InventoryResolver
	Render        RenderPipeline
	DestinyLoader DestinyArtifactLoader
	Template      DestinyTemplateSource
	Dispatcher    SshDispatcher
	Cleaner       Cleaner
	// Router — 3-tier ProviderRouter (P2 W-3). Обязателен в multi-provider
	// раскладке. Резолв per-SID идёт до dispatch-фазы; ошибка резолва
	// (ErrProviderNotRouted) маппится в per-host status="error" +
	// error_code="provider_not_routed".
	Router ProviderRouter
	// ProviderMetrics — счётчик routing-decisions (P2 W-4). nil → no-op.
	ProviderMetrics ProviderMetricsObserver
	Audit           AuditWriter
	Logger          *slog.Logger
	KID             string

	// Now — источник текущего времени для тестов; production-wire-up передаёт
	// time.Now. nil → используется time.Now.
	Now func() time.Time
}

// PushRun — multi-host orchestrator push-прогона (Variant C).
//
// Один экземпляр на процесс; concurrent-safe (собственного изменяемого
// состояния не держит, всё через Store + per-Apply goroutine). Apply async:
// возвращает apply_id и спавнит goroutine с executeAsync под собственным ctx
// (НЕ HTTP-request-ctx — он отменится после 202).
type PushRun struct {
	deps Deps
}

// NewPushRun валидирует зависимости и возвращает orchestrator. Возврат ошибки —
// программная неконфигурация caller-а (wire-up).
func NewPushRun(deps Deps) (*PushRun, error) {
	if deps.Store == nil {
		return nil, errors.New("pushorch: Store обязателен")
	}
	if deps.Topology == nil {
		return nil, errors.New("pushorch: Topology обязателен")
	}
	if deps.Render == nil {
		return nil, errors.New("pushorch: Render обязателен")
	}
	if deps.DestinyLoader == nil {
		return nil, errors.New("pushorch: DestinyLoader обязателен")
	}
	if deps.Template == nil {
		return nil, errors.New("pushorch: DestinyTemplateSource обязателен")
	}
	if deps.Dispatcher == nil {
		return nil, errors.New("pushorch: Dispatcher обязателен")
	}
	if deps.Router == nil {
		return nil, errors.New("pushorch: Router обязателен (multi-provider routing, ADR-032 amendment 2026-05-27)")
	}
	if deps.Logger == nil {
		return nil, errors.New("pushorch: Logger обязателен")
	}
	if deps.KID == "" {
		return nil, errors.New("pushorch: KID обязателен")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &PushRun{deps: deps}, nil
}

// ApplyRequest — вход для PushRun.Apply (HTTP-handler / MCP-tool маппят body).
// Поля host-side (DestinyRef, SSHProvider) — как в `PushApplyRequest`
// docs/keeper/operator-api.md → Push endpoints.
type ApplyRequest struct {
	InventorySIDs []string
	DestinyRef    string // "<name>@<ref>"
	SSHProvider   string
	Input         map[string]any
	CleanupStale  bool
	StartedByAID  string
}

// Apply принимает push-прогон, делает Insert(pending) и спавнит async-goroutine
// с executeAsync. Возвращает apply_id (ULID) для 202 ответа.
//
// Валидация — на HTTP/MCP-границе (parse destiny, inventory non-empty); здесь
// делаем defense-in-depth: ParseDestinyRef падает sentinel-ом → caller
// мапит в 422.
func (r *PushRun) Apply(ctx context.Context, req ApplyRequest) (applyID string, err error) {
	if len(req.InventorySIDs) == 0 {
		return "", errors.New("pushorch: inventory must be non-empty")
	}
	name, ref, err := ParseDestinyRef(req.DestinyRef)
	if err != nil {
		return "", err
	}

	applyID = audit.NewULID()
	row := PushRunRow{
		ApplyID:       applyID,
		InventorySIDs: req.InventorySIDs,
		DestinyRef:    req.DestinyRef,
		SSHProvider:   req.SSHProvider,
		Input:         req.Input,
		CleanupStale:  req.CleanupStale,
		Status:        StatusPending,
		StartedByAID:  req.StartedByAID,
		StartedByKID:  r.deps.KID,
	}
	if err := r.deps.Store.Insert(ctx, row); err != nil {
		return "", err
	}

	// Audit-event push.applied (старт прогона) — параллель с
	// incarnation.scenario_started: пишется при приёме запроса, до начала
	// executeAsync. payload не несёт inventory целиком (может быть огромным);
	// чисел достаточно для корреляции с GET /v1/push/{apply_id}.
	r.writeAudit(ctx, audit.EventPushApplied, req.StartedByAID, map[string]any{
		"apply_id":       applyID,
		"destiny":        req.DestinyRef,
		"inventory_size": len(req.InventorySIDs),
		"ssh_provider":   req.SSHProvider,
		"cleanup_stale":  req.CleanupStale,
	})

	// goroutine ведёт собственный bg-ctx с timeout-cap: HTTP-ctx отменится сразу
	// после 202. orchestratorContextTimeout — потолок prepare-фазы; per-host
	// dispatch использует тот же bg-ctx без дополнительного слоя cancel-а
	// (SshDispatcher изнутри держит DialTimeout).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), orchestratorContextTimeout)
		defer cancel()
		r.executeAsync(bgCtx, applyID, name, ref, req)
	}()

	return applyID, nil
}

// GetRow читает текущее состояние push-прогона по apply_id из push_runs.
// Тонкая обёртка над Store.Get — оставлена методом PushRun для симметрии
// с Apply (handler и MCP-tool работают через один объект).
func (r *PushRun) GetRow(ctx context.Context, applyID string) (*PushRunRow, error) {
	return r.deps.Store.Get(ctx, applyID)
}

// ListRows — глобальный list push-прогонов (`GET /v1/push-runs`, UI-4). Тонкая
// обёртка над Store.SelectAll, симметрично GetRow: handler и MCP-tool ходят в
// orchestrator-объект, не в Store напрямую.
func (r *PushRun) ListRows(ctx context.Context, filter ListFilter, offset, limit int) ([]*PushRunRow, int, error) {
	return r.deps.Store.SelectAll(ctx, filter, offset, limit)
}

// executeAsync — основной поток прогона. Шаги:
//
//  1. MarkRunning;
//  2. LoadByInventory (filter terminal/онбординг + lease-presence);
//  3. собрать синтетический ScenarioManifest + pushDestinyResolver, прогнать
//     через render.Pipeline.Render (destinyIsolated по конструкции изоляции
//     destiny — register/state/essence/soulprint.hosts ей недоступны);
//  4. ToProtoTasks + ApplyRequest на каждый таргетированный SID;
//  5. per-host SendApply через SshDispatcher (concurrent, см. fanOut);
//  6. собрать summary {hosts: [{sid, status, error?}], total, success_count,
//     fail_count} + терминал (success/partial_failed/failed);
//  7. cleanup_stale_versions=true → best-effort Cleanup per-host.
func (r *PushRun) executeAsync(ctx context.Context, applyID, name, ref string, req ApplyRequest) {
	log := r.deps.Logger.With(slog.String("apply_id", applyID), slog.String("destiny", req.DestinyRef))

	if err := r.deps.Store.MarkRunning(ctx, applyID); err != nil {
		log.Error("pushorch: mark running failed — прогон не стартовал", slog.Any("error", err))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "mark_running_failed: " + err.Error(),
		}, req.StartedByAID, req)
		return
	}

	hosts, err := r.deps.Topology.LoadByInventory(ctx, req.InventorySIDs)
	if err != nil {
		log.Error("pushorch: inventory load failed", slog.Any("error", err))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "inventory_load_failed: " + err.Error(),
		}, req.StartedByAID, req)
		return
	}
	if len(hosts) == 0 {
		log.Warn("pushorch: no live hosts in inventory — прогон отменён",
			slog.Int("requested", len(req.InventorySIDs)))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error":     "no_live_hosts",
			"requested": len(req.InventorySIDs),
		}, req.StartedByAID, req)
		return
	}

	resolver := newPushDestinyResolver(r.deps.DestinyLoader, r.deps.Template, name, ref)
	manifest := &config.ScenarioManifest{
		Name: syntheticScenarioName,
		Tasks: []config.Task{
			{
				Name: syntheticTaskName,
				Apply: &config.ApplyTask{
					Destiny: name,
					Input:   req.Input,
				},
			},
		},
	}
	renderIn := render.RenderInput{
		Scenario: manifest,
		Input:    req.Input,
		Hosts:    hosts,
		Destiny:  resolver,
		// Essence/Register/RegisterByHost — пусты: push-прогон не привязан к
		// incarnation, scenario-scope недоступен (та же логика, что destiny-фаза
		// scenario-runner-а: render-pipeline сам гарантирует изоляцию destiny).
		Incarnation: render.IncarnationMeta{Name: syntheticScenarioName},
	}

	tasks, plans, rerr := r.deps.Render.Render(ctx, renderIn)
	if rerr != nil {
		log.Error("pushorch: render failed", slog.Any("error", rerr))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "render_failed: " + rerr.Error(),
		}, req.StartedByAID, req)
		return
	}
	if len(tasks) == 0 {
		// destiny без задач — формально корректный артефакт, но push смысл теряет.
		log.Warn("pushorch: destiny отрендерилась в пустой план — нечего диспатчить")
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "empty_plan",
		}, req.StartedByAID, req)
		return
	}

	// Таргетинг push-прогона: union по всем планам (in pilot — обычно один план
	// на одну apply-задачу). Если несколько task-ов в destiny таргетят разные
	// подмножества — берём union (push семантика: «прогон по инвентарю», не per-task
	// orchestration). plan.TargetSIDs уже sorted (resolveTargets).
	target := unionTargetSIDs(plans)
	if len(target) == 0 {
		log.Warn("pushorch: после where-фильтра ни один хост не остался — прогон пропущен")
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "no_targets_after_where",
		}, req.StartedByAID, req)
		return
	}

	protoTasks := render.ToProtoTasks(tasks)

	// P2 W-3 routing-фаза. Идёт ДО fanOut: routing-промах per-SID не должен
	// открывать SSH-сессию и не должен расходовать env-payload плагина.
	// α-compat (PM-decision): req.SSHProvider непустой → per-job preset
	// применяется КО ВСЕМ SID-ам, перебивает router. Иначе router.RouteFor
	// per-SID; ошибка → per-host status="error" + error_code="provider_not_routed".
	sidProvider, routingResults := r.resolveProviders(ctx, target, req, log)

	// hostResults собираются по target; для SID-ов, на которых routing провалился,
	// уже стоит entry в routingResults (мы их вычеркнем из dispatch-списка).
	dispatchTargets := make([]string, 0, len(target))
	for _, sid := range target {
		if _, failed := routingResults[sid]; failed {
			continue
		}
		dispatchTargets = append(dispatchTargets, sid)
	}

	hostResults := r.fanOut(ctx, applyID, dispatchTargets, sidProvider, protoTasks, log)
	// Слияние: routing-fail-ы (без dispatch) + dispatch-результаты.
	if len(routingResults) > 0 {
		for sid, hr := range routingResults {
			_ = sid
			hostResults = append(hostResults, hr)
		}
		// Детерминированный порядок per-SID в summary.hosts — повторно сортируем.
		sortHostResults(hostResults)
	}

	status, summary := summarize(hostResults)
	r.finalize(ctx, applyID, status, summary, req.StartedByAID, req)

	// Best-effort cleanup устаревших версий на хостах (cleanup_stale_versions).
	// Запускается ПОСЛЕ финализации, чтобы terminate-статус прогона не блокировался
	// SSH-roundtrip-ами cleanup-а. Все ошибки идут в логи, не в summary.
	if req.CleanupStale && r.deps.Cleaner != nil {
		go r.cleanupHosts(dispatchTargets, sidProvider, log)
	}
}

// resolveProviders резолвит provider-имя для каждого SID из inventory.
//
// α-compat (PM-decision P2 W-3): если req.SSHProvider непустое — preset
// применяется КО ВСЕМ SID-ам, ProviderRouter НЕ вызывается. Источник в audit-
// summary помечается как "soul" (per-job override семантически эквивалентен
// per-SID explicit для всех таргетов).
//
// Без preset-а: для каждого SID зовём router.RouteFor. ErrProviderNotRouted →
// в routingResults[sid] кладётся hostResult с status="error" +
// errText="provider_not_routed". Real PG-ошибка → то же, errText содержит
// underlying-сообщение (transient — оператор retry).
//
// Возврат:
//   - sidProvider — map[sid]provider, заполнена для УСПЕШНО зарезолвенных SID-ов
//     (включая α-compat preset);
//   - routingResults — map[sid]hostResult для SID-ов, у которых routing
//     провалился (caller добавит их в final hosts[] без dispatch).
func (r *PushRun) resolveProviders(ctx context.Context, target []string, req ApplyRequest, log *slog.Logger) (map[string]string, map[string]hostResult) {
	sidProvider := make(map[string]string, len(target))
	routingResults := make(map[string]hostResult)

	if req.SSHProvider != "" {
		// α-compat: per-job preset, единый provider на все SID-ы.
		for _, sid := range target {
			sidProvider[sid] = req.SSHProvider
			observeRouted(r.deps.ProviderMetrics, req.SSHProvider, push.SourceSoul.String())
		}
		log.Info("pushorch: α-compat ssh_provider preset применён ко всем SID-ам",
			slog.String("provider", req.SSHProvider),
			slog.Int("count", len(target)))
		return sidProvider, routingResults
	}

	for _, sid := range target {
		providerName, source, rerr := r.deps.Router.RouteFor(ctx, sid)
		if rerr != nil {
			errCode := "provider_not_routed"
			if !errors.Is(rerr, push.ErrProviderNotRouted) {
				errCode = "provider_route_failed"
			}
			log.Warn("pushorch: routing failed",
				slog.String("sid", sid),
				slog.String("error_code", errCode),
				slog.Any("error", rerr))
			routingResults[sid] = hostResult{
				sid:     sid,
				status:  "error",
				errText: errCode + ": " + rerr.Error(),
			}
			continue
		}
		sidProvider[sid] = providerName
		observeRouted(r.deps.ProviderMetrics, providerName, source.String())
	}
	return sidProvider, routingResults
}

// fanOut запускает per-host SendApply параллельно (по хосту = одна goroutine),
// собирает hostResult-ы в детерминированном порядке (по SID). Конкурентность —
// без ограничения (push-инвентарь обычно мал; large-scale rolling — отдельный
// slice через render.DispatchPlan.SerialWidth, в pilot не используется).
//
// sidProvider — карта sid → provider-имя (P2 W-3 multi-provider routing).
// SID без записи в карте — invariant violation (resolveProviders уже
// отфильтровал такие), defensive guard внутри.
func (r *PushRun) fanOut(ctx context.Context, applyID string, sids []string, sidProvider map[string]string, tasks []*keeperv1.RenderedTask, log *slog.Logger) []hostResult {
	results := make([]hostResult, len(sids))
	var wg sync.WaitGroup
	for i, sid := range sids {
		wg.Add(1)
		providerName := sidProvider[sid]
		go func(idx int, sid string, providerName string) {
			defer wg.Done()
			req := &keeperv1.ApplyRequest{
				ApplyId: applyID,
				Tasks:   tasks,
			}
			rr, err := r.deps.Dispatcher.SendApply(ctx, sid, providerName, req)
			results[idx] = buildHostResult(sid, providerName, rr, err)
			if err != nil {
				log.Warn("pushorch: SendApply failed",
					slog.String("sid", sid),
					slog.String("ssh_provider", providerName),
					slog.Any("error", err))
			} else {
				log.Info("pushorch: per-host прогон завершён",
					slog.String("sid", sid),
					slog.String("ssh_provider", providerName),
					slog.String("status", rr.GetStatus().String()))
			}
		}(i, sid, providerName)
	}
	wg.Wait()
	return results
}

// cleanupHosts проходит per-host Cleanup; best-effort, ошибки → лог-warn, не
// влияют на статус прогона. Используется собственный bg-ctx с тем же
// cap-таймаутом, что и executeAsync.
//
// sidProvider — карта sid → provider, заполненная resolveProviders. SID без
// записи (failed routing) сюда не попадает (cleanupHosts получает только
// dispatchTargets).
func (r *PushRun) cleanupHosts(sids []string, sidProvider map[string]string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), orchestratorContextTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, sid := range sids {
		wg.Add(1)
		providerName := sidProvider[sid]
		go func(sid string, providerName string) {
			defer wg.Done()
			if err := r.deps.Cleaner.Cleanup(ctx, sid, providerName); err != nil {
				log.Warn("pushorch: post-success cleanup failed",
					slog.String("sid", sid),
					slog.String("ssh_provider", providerName),
					slog.Any("error", err))
				return
			}
			log.Info("pushorch: post-success cleanup OK",
				slog.String("sid", sid),
				slog.String("ssh_provider", providerName))
		}(sid, providerName)
	}
	wg.Wait()
}

// finalize пишет терминал в push_runs + audit-event. Если MarkTerminal сам
// упал — логируем, audit не пишем (event с ложным состоянием хуже его
// отсутствия). После орфанизации Reaper догонит запись через purge_orphan_push_runs.
func (r *PushRun) finalize(ctx context.Context, applyID string, status PushRunStatus, summary map[string]any, startedByAID string, req ApplyRequest) {
	if err := r.deps.Store.MarkTerminal(ctx, applyID, status, summary); err != nil {
		r.deps.Logger.Error("pushorch: mark terminal failed — запись осталась running (Reaper подберёт)",
			slog.String("apply_id", applyID),
			slog.String("status", string(status)),
			slog.Any("error", err))
		return
	}

	eventType := audit.EventPushFailed
	switch status {
	case StatusSuccess:
		eventType = audit.EventPushCompleted
	case StatusPartialFailed:
		eventType = audit.EventPushPartialFailed
	case StatusFailed:
		eventType = audit.EventPushFailed
	case StatusCancelled:
		// Cancelled — пишется Reaper-ом, не orchestrator-ом (этот путь
		// недостижим из executeAsync). Защитный fallback.
		eventType = audit.EventPushFailed
	}
	r.writeAudit(ctx, eventType, startedByAID, terminalAuditPayload(applyID, req, status, summary))
}

// writeAudit пишет audit-event best-effort: ошибку логирует, прогон не
// прерывает (audit не критичен для функциональности push-а).
func (r *PushRun) writeAudit(ctx context.Context, eventType audit.EventType, aid string, payload map[string]any) {
	if r.deps.Audit == nil {
		return
	}
	src := audit.SourceAPI // push.apply вызывается только из API/MCP — source детерминирован.
	ev := &audit.Event{
		EventType: eventType,
		Source:    src,
		ArchonAID: aid,
		Payload:   payload,
	}
	if err := r.deps.Audit.Write(ctx, ev); err != nil {
		r.deps.Logger.Warn("pushorch: audit write failed",
			slog.String("event_type", string(eventType)),
			slog.Any("error", err))
	}
}

// terminalAuditPayload собирает payload финального audit-event-а. Прозрачно
// несёт сводные числа per-host исходов из summary (success_count/fail_count) +
// destiny-ref + размер инвентаря. inventory целиком НЕ кладётся (может быть
// большой); подробности — в push_runs.summary через GET /v1/push/{apply_id}.
func terminalAuditPayload(applyID string, req ApplyRequest, status PushRunStatus, summary map[string]any) map[string]any {
	p := map[string]any{
		"apply_id":       applyID,
		"destiny":        req.DestinyRef,
		"inventory_size": len(req.InventorySIDs),
		"status":         string(status),
	}
	if v, ok := summary["success_count"]; ok {
		p["success_count"] = v
	}
	if v, ok := summary["fail_count"]; ok {
		p["fail_count"] = v
	}
	if v, ok := summary["total"]; ok {
		p["total"] = v
	}
	return p
}

// hostResult — итог одного per-host SendApply: status либо «error» (доставка
// провалена), либо RunStatus (Soul вернул RunResult, status в protobuf-enum).
//
// `provider` — имя SshProvider, который реально использовался для этого SID
// (Multi-provider routing, P2 W-3). Записывается в push_runs.summary.hosts[]
// для audit-trail (architect-decision: routing-decision сохраняется в
// summary, без отдельного per-routing event).
type hostResult struct {
	sid      string
	provider string
	ok       bool   // true ⇔ SendApply вернул nil-error И RunStatus==SUCCESS
	status   string // строковая форма для summary (`success`/`failed`/`cancelled`/`error_locked`/`error`)
	errText  string // ненулевое только при ok=false; SendApply error, либо причина не-SUCCESS статуса
}

// buildHostResult классифицирует SendApply-возврат:
//   - err != nil → ok=false, status="error" (доставка/connect не дошла до RunResult);
//   - rr.Status == SUCCESS → ok=true;
//   - rr.Status иной → ok=false, status — строка enum-а.
//
// provider запоминается всегда (даже на error-пути), чтобы summary показал,
// на каком SshProvider произошёл fail.
func buildHostResult(sid string, providerName string, rr *keeperv1.RunResult, err error) hostResult {
	if err != nil {
		return hostResult{sid: sid, provider: providerName, status: "error", errText: err.Error()}
	}
	st := rr.GetStatus()
	if st == keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		return hostResult{sid: sid, provider: providerName, ok: true, status: "success"}
	}
	return hostResult{
		sid:      sid,
		provider: providerName,
		status:   runStatusLabel(st),
		errText:  "run_status=" + runStatusLabel(st),
	}
}

// runStatusLabel — короткий kebab-case label RunStatus для summary (без префикса
// `RUN_STATUS_`, lowercase). Симметрично status-полю summary в audit-event-ах.
func runStatusLabel(st keeperv1.RunStatus) string {
	switch st {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		return "success"
	case keeperv1.RunStatus_RUN_STATUS_FAILED:
		return "failed"
	case keeperv1.RunStatus_RUN_STATUS_CANCELLED:
		return "cancelled"
	case keeperv1.RunStatus_RUN_STATUS_ERROR_LOCKED:
		return "error_locked"
	default:
		return "unknown"
	}
}

// summarize классифицирует agregated-исход прогона:
//   - все ok        → success;
//   - все не-ok     → failed;
//   - смешанный исход → partial_failed.
//
// Summary-форма (jsonb в push_runs.summary):
//
//	{
//	  "hosts":         [ {sid, status, error?}, … ],
//	  "total":         <int>,
//	  "success_count": <int>,
//	  "fail_count":    <int>
//	}
//
// Порядок hosts — по позициям в fanOut (= sids; уже sorted via union).
func summarize(results []hostResult) (PushRunStatus, map[string]any) {
	hostsArr := make([]map[string]any, 0, len(results))
	success := 0
	for _, h := range results {
		entry := map[string]any{
			"sid":    h.sid,
			"status": h.status,
		}
		if h.provider != "" {
			// P2 W-3: routing-decision сохраняется в push_runs.summary.hosts[sid]
			// (architect-decision: без отдельного per-routing event-а в audit_log).
			entry["ssh_provider"] = h.provider
		}
		if h.errText != "" {
			entry["error"] = h.errText
		}
		hostsArr = append(hostsArr, entry)
		if h.ok {
			success++
		}
	}
	total := len(results)
	fail := total - success
	summary := map[string]any{
		"hosts":         hostsArr,
		"total":         total,
		"success_count": success,
		"fail_count":    fail,
	}

	switch {
	case success == total:
		return StatusSuccess, summary
	case success == 0:
		return StatusFailed, summary
	default:
		return StatusPartialFailed, summary
	}
}

// unionTargetSIDs строит отсортированный uniq-список SID-ов из всех планов
// (объединение по задачам). В pilot scenario из одной apply-задачи план обычно
// один → план.TargetSIDs прямо проходит; для пары задач с разными where:
// получаем union, что и нужно push-семантике «прогон по инвентарю».
func unionTargetSIDs(plans []render.DispatchPlan) []string {
	if len(plans) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, p := range plans {
		for _, sid := range p.TargetSIDs {
			seen[sid] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for sid := range seen {
		out = append(out, sid)
	}
	// Детерминизм per-host dispatch-а: sort по SID. Лексикографически (через
	// готовый sort из stdlib — sort.Strings) даёт ту же раскладку, что
	// LoadByInventory.
	sortStrings(out)
	return out
}

// sortStrings — упрощённая обёртка вокруг sort.Strings, чтобы run.go не тянул
// import "sort" в основном пути.
func sortStrings(s []string) {
	// inlined insertion-sort: на push-инвентарях <=100 элементов это
	// быстрее sort.Strings (нет overhead-а интерфейса), а аллокаций НЕТ.
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// sortHostResults — детерминированный порядок hosts[] в summary (по SID).
// После слияния routing-failures с dispatch-результатами порядок ломается;
// inline insertion-sort на короткой выборке (<=100 SID).
func sortHostResults(s []hostResult) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1].sid > s[j].sid {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// observeRouted — nil-safe wrapper над ProviderMetricsObserver. Free-функция
// (на interface нельзя навесить метод), чтобы pushorch не повторял nil-проверку
// на каждый вызов resolveProviders.
func observeRouted(o ProviderMetricsObserver, providerName, decisionSource string) {
	if o == nil {
		return
	}
	o.ObserveProviderRouted(providerName, decisionSource)
}
