// Package errandrunner — Soul-side исполнитель Errand-запросов (ADR-033 §6).
//
// Errand — pull-ad-hoc exec одиночного модуля на конкретном Soul через уже
// доверенный mTLS EventStream-канал. От apply-цикла отличается контрактом:
//
//   - НЕ мутирует incarnation.state (`state_changes` игнорируются);
//   - один [keeperv1.ErrandRequest] → один [keeperv1.ErrandResult], без
//     промежуточных TaskEvent / RunResult;
//   - whitelist модулей: жёсткий список `core.cmd.shell` / `core.exec.run` +
//     marker-интерфейс [sdkmodule.ErrandReadSafe] (см. [IsAllowed]);
//   - stdout/stderr — captured из финального ApplyEvent.Output, cap 64 KiB на
//     канал + secret-masking (defense-in-depth — Keeper-side делает то же при
//     приёме результата, см. keeper/internal/errand/mask.go).
//
// Runner вызывает SoulModule.Apply того же [Registry], что и applyrunner
// (shared core + plugin), но синтетическим ApplyRequest{state, params} без
// flow-control/retry/onfail-обвязки apply-цикла: Errand — одиночный вызов.
//
// Concurrency: одновременных Run-вызовов несколько — разрешено (Errand-ы не
// сериализуются на Soul, в отличие от apply, ADR-012(a)). Runner stateless,
// Registry / Logger / Metrics — read-only после конструктора.
package errandrunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
)

// Registry — узкий интерфейс над runtime.Registry (`Lookup` по голому имени
// модуля без state-суффикса). Объявлен здесь, чтобы пакет не зависел от
// `soul/internal/runtime` (избегаем циклов: runtime импортирует sdk/module,
// errandrunner тоже, общая точка — sdk/module).
type Registry interface {
	Lookup(name string) (sdkmodule.SoulModule, bool)
}

// Runner — Soul-side исполнитель одного Errand. Иммутабелен после конструктора,
// кроме active-map (errand_id → cancel-fn активной Run-горутины, slice E5).
type Runner struct {
	modules Registry
	logger  *slog.Logger
	metrics *Metrics

	// active — map активных Run-горутин для cancel-flow (ADR-033 slice E5).
	// Заполняется в начале Run, очищается defer-ом перед return. Параллельные
	// Run-ы на одном errand_id невозможны (errand_id = ULID, гарантирована
	// уникальность Keeper-ом); конкурентные Run разных errand_id допустимы
	// (Errand-ы не сериализуются, ADR-033). mu — короткий read/write,
	// отдельный RWMutex избыточен.
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// New собирает Runner. logger=nil → slog.Default(); metrics=nil → no-op
// (nil-safe методы [Metrics]); modules обязателен (panic при nil — программная
// ошибка wire-up-а, не runtime-данные).
func New(modules Registry, logger *slog.Logger, metrics *Metrics) *Runner {
	if modules == nil {
		panic("errandrunner: modules registry is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		modules: modules,
		logger:  logger,
		metrics: metrics,
		active:  make(map[string]context.CancelFunc),
	}
}

// Cancel — slice E5: отменить активную Run-горутину по errand_id. Возвращает
// true, если Run был зарегистрирован и cancel вызван; false — если Run уже
// завершился или не существует (race с собственным терминалом — безопасный
// no-op). После cancel-а Run-горутина увидит ctx.Err() (Canceled) и
// возвращает ErrandResult{status: CANCELLED}.
//
// Паттерн идентичен [ApplyRunner.Cancel] — best-effort signal, без блокировки
// на завершение Run-горутины.
func (r *Runner) Cancel(errandID string) bool {
	r.mu.Lock()
	cancel, ok := r.active[errandID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// registerActive регистрирует cancel-fn в active-map. Возвращает unregister-fn
// (defer-friendly), которая удаляет запись из map. Если errand_id уже в map
// (дубль ErrandRequest от Keeper-а — невозможно по протоколу, но defensive) —
// предыдущая cancel-fn перезатирается; старая Run-горутина уже завершила
// регистрацию, race разрешится через короткий мьютекс.
func (r *Runner) registerActive(errandID string, cancel context.CancelFunc) func() {
	r.mu.Lock()
	r.active[errandID] = cancel
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.active, errandID)
		r.mu.Unlock()
	}
}

// Run исполняет один Errand. Возвращает терминальный [keeperv1.ErrandResult] —
// caller (eventstream-dispatcher) шлёт его обратно Keeper-у одним сообщением.
//
// Контракт ADR-033:
//  1. Whitelist-check ДО любых действий (defense-in-depth, Keeper тоже).
//  2. Резолв модуля через тот же Registry, что и applyrunner.
//  3. dry_run=true → mod.Plan(...) ТОЛЬКО для [sdkmodule.PlanReadSafe];
//     иначе FAILED с `errand_dry_run_unsupported`.
//  4. dry_run=false → mod.Apply(synthetic ApplyRequest).
//  5. Output cap 64 KiB на канал stdout/stderr + masking через [MaskSecrets].
//  6. state_changes игнорируются (Errand их не пишет).
//
// Если ctx истёк по timeout_seconds — статус TIMED_OUT (а не FAILED).
func (r *Runner) Run(ctx context.Context, req *keeperv1.ErrandRequest) *keeperv1.ErrandResult {
	started := time.Now()
	if req == nil {
		// defensive: dispatcher проверяет nil до Run, но Run — публичный API.
		return &keeperv1.ErrandResult{
			Status:       keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			ErrorMessage: "errand: request is nil",
		}
	}
	errandID := req.GetErrandId()

	// Address split: `core.cmd.shell` → (`core.cmd`, `shell`). Без state-суффикса
	// модуль не вызывается (whitelist опирается на полное имя). Registry
	// принимает только `<namespace>.<name>` (см. coremod.Registry).
	modName, state, ok := splitModuleAddr(req.GetModule())
	if !ok || state == "" {
		return r.terminal(errandID, started,
			keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			fmt.Sprintf("errand: bad module address %q (expect <namespace>.<name>.<state>)", req.GetModule()),
		)
	}

	mod, found := r.modules.Lookup(modName)
	if !found {
		// Несуществующий модуль трактуем как MODULE_NOT_ALLOWED: с точки зрения
		// Errand-контура «недопущенный» (whitelist подразумевает существование).
		// Аудит-маппинг тот же (errand.failed), статус-код различает причину.
		r.logger.Warn("errand: модуль не найден в Registry",
			slog.String("errand_id", errandID),
			slog.String("module", req.GetModule()))
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED, started)
		return r.terminalNoMetric(errandID, started,
			keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED,
			fmt.Sprintf("errand_module_not_allowed: %s", req.GetModule()),
		)
	}

	if ok, reason := IsAllowed(req.GetModule(), mod); !ok {
		// Whitelist-отказ — security-событие (попытка вызвать не-read-safe модуль
		// через Errand). Warn-уровень: keeper validate тоже отвергает такие
		// запросы, до Soul они доходят только при ранней-pre-validation сборке
		// клиента или баге в keeper-side, что важно видеть в логах.
		r.logger.Warn("errand: whitelist reject",
			slog.String("errand_id", errandID),
			slog.String("module", req.GetModule()),
			slog.String("reason", reason))
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED, started)
		return r.terminalNoMetric(errandID, started,
			keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED,
			reason,
		)
	}

	// Dry-run валидация: только модули с [sdkmodule.PlanReadSafe]. У ADR-031 Plan
	// — pure-read; для verb-модулей (cmd.shell/exec.run) Plan не PlanReadSafe,
	// поэтому dry_run по shell/exec вернёт `errand_dry_run_unsupported`. Это
	// конструктивное ограничение, а не баг (см. doc-комментарий core.cmd).
	if req.GetDryRun() {
		if _, planReadSafe := mod.(sdkmodule.PlanReadSafe); !planReadSafe {
			r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_FAILED, started)
			return r.terminalNoMetric(errandID, started,
				keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
				"errand_dry_run_unsupported",
			)
		}
	}

	// Timeout: applies к sub-ctx; истечение → TIMED_OUT (отличаем от FAILED).
	// 0 → без своего таймаута (родительский ctx уже несёт ServerCap dispatch-а).
	// Slice E5: cancel-flow — обёртка через WithCancel независимо от timeout-а,
	// чтобы Cancel(errandID) мог отменить Run даже при timeout_seconds=0.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if to := req.GetTimeoutSeconds(); to > 0 {
		var timeoutCancel context.CancelFunc
		runCtx, timeoutCancel = context.WithTimeout(runCtx, time.Duration(to)*time.Second)
		defer timeoutCancel()
	}

	// Slice E5: регистрируем cancel-fn в active-map, чтобы external Cancel
	// (CancelErrand от Keeper-а) мог отменить Run без обращения к runCtx.
	unregister := r.registerActive(errandID, cancelRun)
	defer unregister()

	collector := newOutputCollector(runCtx, OutputCapBytes)
	pluginReq := &pluginv1.ApplyRequest{
		State:  state,
		Params: req.GetInput(),
	}

	var modErr error
	if req.GetDryRun() {
		// PlanEvent от Plan-stream-а собирает отдельный collector (другой тип
		// stream), но для Errand финал важен только в смысле «не упал» — Plan
		// для read-safe модулей не пишет stdout/stderr/output (ADR-031: Plan
		// шлёт только final PlanEvent.changed).
		planCollector := newPlanCollector(runCtx)
		modErr = mod.Plan(&pluginv1.PlanRequest{State: state, Params: req.GetInput()}, planCollector)
	} else {
		modErr = mod.Apply(pluginReq, collector)
	}
	durationMs := time.Since(started).Milliseconds()

	// Финальный статус — по таймауту > отмене > ошибке модуля > событию модуля.
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT,
			"errand_timeout_exceeded")
	case errors.Is(runCtx.Err(), context.Canceled):
		// runCtx.Err()=Canceled означает либо внешний Cancel(errandID) (slice E5
		// — оператор отменил через DELETE /v1/errands/{id}), либо отмена parent-
		// ctx (shutdown демона). Семантически и то и другое — CANCELLED: Errand
		// не дошёл до естественного терминала, оператор/процесс остановил его.
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED, "errand cancelled by operator")
	case modErr != nil:
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_FAILED, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			maskedMessage(modErr.Error()))
	}

	last := collector.lastEvent()
	if last != nil && last.GetFailed() {
		r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_FAILED, started)
		return r.buildResult(errandID, collector, durationMs,
			keeperv1.ErrandStatus_ERRAND_STATUS_FAILED,
			maskedMessage(last.GetMessage()))
	}

	r.recordTerminal(req.GetModule(), keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS, started)
	return r.buildResult(errandID, collector, durationMs,
		keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS, "")
}

// buildResult собирает ErrandResult из выходов модуля: stdout/stderr/exit_code
// извлекаются из ApplyEvent.Output (формат core.cmd/exec), остальные поля
// Output остаются как структурный output (для будущих read-safe модулей).
// Маскировка + cap идут здесь — на одном выходе, без дублирования.
func (r *Runner) buildResult(errandID string, c *outputCollector, durationMs int64, status keeperv1.ErrandStatus, errMsg string) *keeperv1.ErrandResult {
	stdout, stderr, exitCode, structured := c.extractFinal()

	stdoutMasked, stdoutTrunc := MaskAndCapBytes(stdout)
	stderrMasked, stderrTrunc := MaskAndCapBytes(stderr)

	res := &keeperv1.ErrandResult{
		ErrandId:        errandID,
		Status:          status,
		ExitCode:        exitCode,
		Stdout:          stdoutMasked,
		Stderr:          stderrMasked,
		StdoutTruncated: stdoutTrunc,
		StderrTruncated: stderrTrunc,
		DurationMs:      durationMs,
		ErrorMessage:    errMsg,
		Output:          structured,
	}
	if (status == keeperv1.ErrandStatus_ERRAND_STATUS_FAILED ||
		status == keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT) && stdoutTrunc {
		// Эхо: оба флага видны на keeper-side mask+cap (defense-in-depth).
	}
	return res
}

// terminal — терминал без выхода модуля (early-fail на whitelist/dry_run/etc).
// Метрика инкрементируется внутри.
func (r *Runner) terminal(errandID string, started time.Time, status keeperv1.ErrandStatus, msg string) *keeperv1.ErrandResult {
	r.recordTerminal("", status, started)
	return r.terminalNoMetric(errandID, started, status, msg)
}

// terminalNoMetric — терминал без метрик (caller уже её инкрементировал).
// Разделён, чтобы early-reject (MODULE_NOT_ALLOWED, dry_run_unsupported)
// записывал метрику с реальным module-label-ом, а не пустым.
func (r *Runner) terminalNoMetric(errandID string, started time.Time, status keeperv1.ErrandStatus, msg string) *keeperv1.ErrandResult {
	return &keeperv1.ErrandResult{
		ErrandId:     errandID,
		Status:       status,
		DurationMs:   time.Since(started).Milliseconds(),
		ErrorMessage: msg,
	}
}

func (r *Runner) recordTerminal(module string, status keeperv1.ErrandStatus, started time.Time) {
	r.metrics.ObserveErrand(status)
	r.metrics.ObserveDuration(module, time.Since(started).Seconds())
}

// maskedMessage — маска для error-message (модуль может вернуть текст с
// vault-ref-ом или sensitive-ключом). Использует тот же mask-словарь, что
// stdout/stderr через [MaskAndCapBytes], без cap-а: error_message — короткий.
func maskedMessage(s string) string {
	if s == "" {
		return ""
	}
	masked, _ := MaskAndCapBytes(s)
	return masked
}

// splitModuleAddr — `<namespace>.<name>.<state>` → (`<namespace>.<name>`, state).
// Симметрично runtime.splitModuleAddr (тот пакета не экспортирует — дублируем
// 12 строк, чем тянуть зависимость на applyrunner). Для Errand state ОБЯЗАТЕЛЕН
// (whitelist `core.cmd.shell` — полная форма): пустой state → ok=false.
func splitModuleAddr(s string) (name, state string, ok bool) {
	if s == "" {
		return "", "", false
	}
	idx := -1
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			idx = i
			break
		}
	}
	if idx <= 0 || idx == len(s)-1 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// planCollector — capture-only сервер PlanEvent для dry_run-ветки. ApplyEvent
// и PlanEvent — разные типы, общий outputCollector не подходит. Plan для
// read-safe модулей шлёт только final PlanEvent.changed (ADR-031), output не
// заполняется.
type planCollector struct {
	grpc.ServerStream
	ctx    context.Context
	events []*pluginv1.PlanEvent
}

func newPlanCollector(ctx context.Context) *planCollector {
	return &planCollector{ctx: ctx}
}

func (c *planCollector) Context() context.Context     { return c.ctx }
func (c *planCollector) SetHeader(metadata.MD) error  { return nil }
func (c *planCollector) SendHeader(metadata.MD) error { return nil }
func (c *planCollector) SetTrailer(metadata.MD)       {}
func (c *planCollector) SendMsg(any) error            { return nil }
func (c *planCollector) RecvMsg(any) error {
	return errors.New("plan collector: RecvMsg not supported")
}
func (c *planCollector) Send(ev *pluginv1.PlanEvent) error {
	c.events = append(c.events, ev)
	return nil
}
