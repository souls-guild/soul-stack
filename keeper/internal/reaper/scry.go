// scry.go — Reaper-правило `scry_background` (ADR-031 Slice C):
// фоновое периодическое drift-сканирование incarnation-ов через тот же
// pipeline, что использует Slice B on-demand (`scenario.Runner.CheckDrift`).
//
// Архитектурное намерение (PM-pilot 2026-05):
//
//   - **Default OFF + opt-in**: правило отсутствует в дефолтном keeper.yml,
//     включается оператором явно (см. docs/keeper/reaper.md → scry_background).
//
//   - **Counts-only в фоне**: полный DriftReport НЕ сохраняется — Reaper
//     берёт у CheckDrift только агрегаты Summary и пишет их в новую колонку
//     `incarnation.last_drift_summary` (миграция 050). Slice B on-demand
//     возвращает полный отчёт прямо в response, как и было.
//
//   - **Validate-and-skip gate**: ПЕРЕД вызовом тяжёлого CheckDrift Reaper
//     берёт короткую PG-tx с `SELECT … FOR UPDATE` и проверяет, что incarnation
//     до сих пор `ready`/`drift`. Если статус ушёл в applying/destroying/locked
//     между iterator-выборкой и goroutine-stem-stage-ом — фон тихо skip-ает.
//
//   - **Throttle**: `max_concurrent_in_flight` (default 10) ограничивает
//     одновременные активные dry_run-прогоны (semaphore + добор счёта из PG).
//     `min_interval_per_incarnation` отсекает повторный скан одной incarnation
//     раньше срока (применяется в iterator-предикате).
//
//   - **НЕ блокирует tick**: каждый кандидат уходит в отдельную goroutine;
//     Reaper-tick стартует их батч и возвращается к loop-у, не дожидаясь
//     завершения. Метрики/завершение пишутся в самой goroutine.
package reaper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	// defaultScryBackgroundInterval — period между tick-ами правила, если в
	// конфиге не задан. 6h — продакт-выбор PM (Slice C pilot): редкий фон,
	// чтобы не создавать постоянной dry_run-нагрузки; оператор поднимает по
	// необходимости.
	defaultScryBackgroundInterval = 6 * time.Hour

	// defaultScryBackgroundMaxConcurrent — потолок одновременно идущих
	// фоновых dry_run-прогонов. 10 — компромисс: достаточно для прохода
	// крупного инвентаря за рабочий час без забивания Acolyte-пула, и мало,
	// чтобы случайный backlog не съел весь work-queue.
	defaultScryBackgroundMaxConcurrent = 10

	// defaultScryBackgroundBatchSize — размер батча incarnation-ов, который
	// одна tick-итерация забирает из iterator-а. Меньше max_concurrent_in_flight
	// + headroom, чтобы при полностью занятом пуле tick тратил мало времени
	// и быстро отдавал управление tickLoop-у.
	defaultScryBackgroundBatchSize = 20
)

// DriftChecker — узкий интерфейс scenario.Runner-а, нужный фоновому правилу.
// Сужение позволяет unit-тестам подставлять fake; production wire-up в
// daemon.setupReaper передаёт *scenario.Runner.
type DriftChecker interface {
	CheckDrift(ctx context.Context, spec scenario.CheckDriftSpec) (*scenario.DriftReport, error)
	MarkDriftStatus(ctx context.Context, name string, hasDrift bool) error
}

// ServiceRefResolver — узкая поверхность реестра сервисов (`scenario.ServiceRegistry`),
// нужная фоновому правилу: резолв `service` → `artifact.ServiceRef` для
// `scenario.CheckDriftSpec`. Возвращает копию ref-а; геттер синхронный (см.
// serviceregistry.Holder.Resolve).
type ServiceRefResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// ScryDeps — внешние зависимости фонового drift-правила. Передаются через
// поля reaper.Deps (см. Deps.Scry); nil-ScryDeps → правило `scry_background`
// в dispatch-е пропускается с одним warn-ом за uptime (защита от опечатки
// в keeper.yml).
type ScryDeps struct {
	// Pool — конкретный pgx-pool. queryRower-абстракции недостаточно: gate
	// требует BeginTx (SELECT FOR UPDATE + Commit/Rollback). nil → правило не
	// функционирует.
	Pool TxBeginnerExecQuerier

	// DriftChecker — реализатор Slice B pipeline (`scenario.Runner`). nil → no-op.
	DriftChecker DriftChecker

	// Services — реестр сервисов для резолва ServiceRef по incarnation.service.
	Services ServiceRefResolver

	// Audit — writer audit-event-а `incarnation.drift_checked` (source=background).
	// nil → audit не пишется (warn в лог caller-а wire-up-а).
	Audit audit.Writer
}

// TxBeginnerExecQuerier — узкое подмножество [*pgxpool.Pool], нужное Scry-rule.
// Объединяет [incarnation.TxBeginner] (для FOR UPDATE-tx) и
// [incarnation.ExecQueryRower] (для iterator/update-вызовов). Реальный
// *pgxpool.Pool удовлетворяет автоматически.
type TxBeginnerExecQuerier interface {
	incarnation.TxBeginner
	incarnation.ExecQueryRower
}

// runScryBackground — handler правила `scry_background` (вызывается из
// reaper.dispatch). Iterator → батч → per-incarnation goroutine с
// throttle-check + validate-tx + CheckDrift + update + audit.
//
// throttle: семафор `max_concurrent_in_flight` минус число уже идущих
// dry_run-прогонов в PG. Семафор-канал создаётся на каждый tick (lifetime —
// один tick + завершение всех его goroutine-ов).
//
// Reaper-tick синхронно ждёт завершения всех запущенных goroutine-ов до
// возврата управления tickLoop-у. Это даёт корректные метрики
// (ObserveRule с числом действительно завершённых) и предотвращает накопление
// «висящих» goroutine-ов при ctx.Cancel. Семантика «фон не блокируется»
// сохраняется на уровне правила (next tick придёт только через interval), но
// один tick — не пожизненный.
func (r *Runner) runScryBackground(ctx context.Context, ruleName string, rule config.ReaperRule, batchSize int, dryRun bool, scry *ScryDeps) {
	if scry == nil || scry.Pool == nil || scry.DriftChecker == nil || scry.Services == nil {
		r.deps.Logger.Warn("reaper: scry_background — отсутствуют ScryDeps, правило пропущено",
			slog.String("rule", ruleName),
		)
		return
	}

	maxConcurrent := defaultScryBackgroundMaxConcurrent
	if rule.MaxConcurrentInFlight != nil {
		maxConcurrent = *rule.MaxConcurrentInFlight
	}
	if maxConcurrent <= 0 {
		r.deps.Logger.Info("reaper: scry_background — max_concurrent_in_flight<=0, тик пропущен (правило заглушено без enabled=false)",
			slog.String("rule", ruleName),
			slog.Int("max_concurrent_in_flight", maxConcurrent),
		)
		return
	}

	minInterval, err := parseRuleDuration(rule.MinIntervalPerIncarnation, 0)
	if err != nil {
		r.deps.Logger.Warn("reaper: scry_background — невалидный min_interval_per_incarnation, использую 0 (без throttle)",
			slog.String("rule", ruleName),
			slog.String("raw", rule.MinIntervalPerIncarnation),
			slog.Any("error", err),
		)
		minInterval = 0
	}

	iterBatch := batchSize
	if iterBatch <= 0 {
		iterBatch = defaultScryBackgroundBatchSize
	}
	// Не запрашиваем у iterator-а больше, чем поместится в пул: lишние строки
	// просто будут отброшены семафором с warn-ом «throttled». Cap-аем по
	// max_concurrent_in_flight (плюс маленький headroom не нужен — sem.Acquire
	// блокирует, не дропает, см. ниже).
	if iterBatch > maxConcurrent {
		iterBatch = maxConcurrent
	}

	if dryRun {
		r.deps.Logger.Info("reaper: dry_run, skipping",
			slog.String("rule", ruleName),
			slog.Int("max_concurrent_in_flight", maxConcurrent),
			slog.Duration("min_interval_per_incarnation", minInterval),
			slog.Int("batch_size", iterBatch),
		)
		return
	}

	start := time.Now()

	// Учитываем уже идущие в кластере dry_run-прогоны (cross-keeper-throttle):
	// сем-cap = max_concurrent_in_flight - active. <=0 → этот tick молча
	// пропускается.
	active, err := incarnation.CountActiveDryRuns(ctx, scry.Pool)
	if err != nil {
		r.deps.Logger.Warn("reaper: scry_background — не получилось посчитать активные dry_run, пропуск тика",
			slog.String("rule", ruleName), slog.Any("error", err))
		r.deps.Metrics.ObserveRule(ruleName, 0, err, time.Since(start))
		return
	}
	headroom := maxConcurrent - active
	if headroom <= 0 {
		r.deps.Logger.Info("reaper: scry_background — пул занят, тик пропущен",
			slog.String("rule", ruleName),
			slog.Int("active", active),
			slog.Int("max_concurrent_in_flight", maxConcurrent),
		)
		r.deps.Metrics.ObserveRule(ruleName, 0, nil, time.Since(start))
		return
	}

	candidates, err := incarnation.SelectScryCandidates(ctx, scry.Pool, minInterval, iterBatch)
	if err != nil {
		r.deps.Logger.Warn("reaper: scry_background — iterator упал",
			slog.String("rule", ruleName), slog.Any("error", err))
		r.deps.Metrics.ObserveRule(ruleName, 0, err, time.Since(start))
		return
	}
	if len(candidates) == 0 {
		r.deps.Metrics.ObserveRule(ruleName, 0, nil, time.Since(start))
		return
	}

	// Семафор-канал + WaitGroup, чтобы tick дождался завершения всех своих
	// goroutine-ов (см. docstring выше).
	sem := make(chan struct{}, headroom)
	done := make(chan struct{})
	launched := 0
	for _, c := range candidates {
		select {
		case <-ctx.Done():
			break
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			break
		}
		launched++
		go func(cand incarnation.ScryCandidate) {
			defer func() { <-sem; done <- struct{}{} }()
			r.runScryOne(ctx, cand, scry)
		}(c)
	}

	for i := 0; i < launched; i++ {
		<-done
	}
	r.deps.Metrics.ObserveRule(ruleName, int64(launched), nil, time.Since(start))
	r.deps.Logger.Info("reaper: scry_background tick завершён",
		slog.String("rule", ruleName),
		slog.Int("launched", launched),
		slog.Int("candidates", len(candidates)),
		slog.Int("headroom", headroom),
	)
}

// runScryOne — обработка одного incarnation в батче: validate-tx → CheckDrift
// → UpdateDriftScanResult + MarkDriftStatus + audit. Защищена от panic-а в
// CheckDrift на уровне tick-а: error из CheckDrift логируется и приводит к
// no-op-у update (не затирает прежний last_drift_*).
func (r *Runner) runScryOne(ctx context.Context, cand incarnation.ScryCandidate, scry *ScryDeps) {
	log := r.deps.Logger.With(
		slog.String("rule", "scry_background"),
		slog.String("incarnation", cand.Name),
	)

	// Validate-and-go: короткая PG-tx с SELECT FOR UPDATE для recheck-а статуса.
	// Если incarnation ушла из ready/drift между iterator-выборкой и сейчас —
	// фон skip-ает без аудита и метрик-инкремента.
	if ok, err := scryCheckStatus(ctx, scry.Pool, cand.Name); err != nil {
		log.Warn("scry: validate tx упала, скан пропущен", slog.Any("error", err))
		return
	} else if !ok {
		log.Debug("scry: incarnation не в ready/drift, скан пропущен")
		return
	}

	serviceRef, ok := scry.Services.Resolve(cand.Service)
	if !ok {
		log.Warn("scry: service не зарегистрирован, скан пропущен",
			slog.String("service", cand.Service))
		return
	}

	applyID := audit.NewULID()
	report, err := scry.DriftChecker.CheckDrift(ctx, scenario.CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: cand.Name,
		ServiceRef:      serviceRef,
		// InputOverride: nil — фон не переопределяет converge-параметры,
		// только auto-from-state (Slice B логика resolveDriftInput).
		// StartedByAID: "" — фон без identity Архонта.
	})
	if err != nil {
		// ErrConvergeMissing — частый «штатный» случай для сервисов без
		// drift-поддержки. Логируем тише.
		if errors.Is(err, scenario.ErrConvergeMissing) {
			log.Info("scry: converge отсутствует, drift не поддержан этим сервисом",
				slog.String("service", cand.Service))
			return
		}
		log.Warn("scry: CheckDrift упал, скан не зафиксирован",
			slog.String("apply_id", applyID),
			slog.Any("error", err))
		return
	}

	// Сначала summary (информационный слой), потом MarkDriftStatus (статус),
	// потом audit. На каждом шаге best-effort: ошибка одного не отменяет
	// остальные.
	summary := incarnation.DriftScanSummary{
		HostsDrifted:     report.Summary.HostsDrifted,
		HostsClean:       report.Summary.HostsClean,
		HostsUnsupported: report.Summary.HostsUnsupported,
		HostsFailed:      report.Summary.HostsFailed,
		TotalHosts:       len(report.Hosts),
		ScannedAt:        report.CheckedAt,
	}
	if err := incarnation.UpdateDriftScanResult(ctx, scry.Pool, cand.Name, summary); err != nil {
		log.Warn("scry: запись last_drift_summary упала", slog.Any("error", err))
	}

	hasDrift := report.Summary.HostsDrifted > 0 || report.Summary.HostsFailed > 0
	if err := scry.DriftChecker.MarkDriftStatus(ctx, cand.Name, hasDrift); err != nil {
		log.Warn("scry: MarkDriftStatus упал", slog.Any("error", err))
	}

	if scry.Audit != nil {
		_ = scry.Audit.Write(ctx, &audit.Event{
			EventType:     audit.EventIncarnationDriftChecked,
			Source:        audit.SourceBackground,
			CorrelationID: applyID,
			Payload: map[string]any{
				"name":     cand.Name,
				"scenario": scenario.ConvergeScenarioName,
				"apply_id": applyID,
				"drift_summary": map[string]any{
					"hosts_drifted":     summary.HostsDrifted,
					"hosts_clean":       summary.HostsClean,
					"hosts_unsupported": summary.HostsUnsupported,
					"hosts_failed":      summary.HostsFailed,
					"total_hosts":       summary.TotalHosts,
				},
			},
		})
	}

	log.Info("scry: incarnation просканирована",
		slog.String("apply_id", applyID),
		slog.Int("hosts_total", summary.TotalHosts),
		slog.Int("hosts_drifted", summary.HostsDrifted),
		slog.Int("hosts_failed", summary.HostsFailed),
		slog.Bool("has_drift", hasDrift),
	)
}

// scryCheckStatus — короткая PG-tx, защищающая фон от запуска dry_run-а против
// incarnation, статус которой только что ушёл из ready/drift (operator стартанул
// run/destroy/upgrade между iterator-выборкой и goroutine-stage-ом).
// Возвращает true, если incarnation ещё в ready/drift на момент проверки.
//
// SELECT … FOR UPDATE сериализует против любого конкурентного scenario.lockRun
// (тот тоже берёт incarnation FOR UPDATE). Tx немедленно коммитится (gate-only),
// чтобы не держать блокировку на всё время Scry-прогона.
func scryCheckStatus(ctx context.Context, pool incarnation.TxBeginner, name string) (bool, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const sql = `SELECT status FROM incarnation WHERE name = $1 FOR UPDATE`
	var statusStr string
	if err := tx.QueryRow(ctx, sql, name).Scan(&statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("select: %w", err)
	}
	if incarnation.Status(statusStr) != incarnation.StatusReady &&
		incarnation.Status(statusStr) != incarnation.StatusDrift {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}
