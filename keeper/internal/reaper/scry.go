// scry.go implements the Reaper rule `scry_background` (ADR-031 Slice C):
// periodic background drift scanning for incarnations through the same pipeline
// used by Slice B on-demand (`scenario.Runner.CheckDrift`).
//
// Architecture intent (PM pilot 2026-05):
//
//   - **Default OFF + opt-in**: the rule is absent from the default keeper.yml
//     and is enabled explicitly by the operator; see docs/keeper/reaper.md ->
//     scry_background.
//
//   - **Counts-only in background**: the full DriftReport is NOT stored. Reaper
//     takes only Summary aggregates from CheckDrift and writes them to the new
//     `incarnation.last_drift_summary` column (migration 050). Slice B on-demand
//     still returns the full report directly in the response.
//
//   - **Validate-and-skip gate**: BEFORE calling the heavy CheckDrift, Reaper
//     takes a short PG tx with `SELECT ... FOR UPDATE` and checks that the
//     incarnation is still `ready`/`drift`. If status moved to
//     applying/destroying/locked between iterator selection and goroutine stem
//     stage, background quietly skips it.
//
//   - **Throttle**: `max_concurrent_in_flight` (default 10) limits concurrent
//     active dry_run runs (semaphore plus active count from PG).
//     `min_interval_per_incarnation` filters repeated scans of the same
//     incarnation before the deadline, applied in the iterator predicate.
//
//   - **Does NOT block tick**: each candidate goes to a separate goroutine; the
//     Reaper tick starts the batch and returns to the loop without waiting for
//     completion. Metrics/completion are written in the goroutine itself.
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
	// defaultScryBackgroundInterval is the period between rule ticks when config
	// does not set it. 6h is the PM product choice for the Slice C pilot: sparse
	// background work avoids constant dry_run load, and the operator can raise it
	// if needed.
	defaultScryBackgroundInterval = 6 * time.Hour

	// defaultScryBackgroundMaxConcurrent is the cap for concurrently running
	// background dry_run runs. 10 is a compromise: enough to scan a large
	// inventory within a work hour without saturating the Acolyte pool, and small
	// enough that an accidental backlog does not consume the whole work queue.
	defaultScryBackgroundMaxConcurrent = 10

	// defaultScryBackgroundBatchSize is the incarnation batch size fetched from
	// the iterator by one tick iteration. It is smaller than
	// max_concurrent_in_flight plus headroom so a tick spends little time when
	// the pool is full and quickly returns control to tickLoop.
	defaultScryBackgroundBatchSize = 20
)

// DriftChecker is the narrow scenario.Runner interface needed by the background
// rule. Narrowing lets unit tests substitute a fake; production wire-up in
// daemon.setupReaper passes *scenario.Runner.
type DriftChecker interface {
	CheckDrift(ctx context.Context, spec scenario.CheckDriftSpec) (*scenario.DriftReport, error)
	MarkDriftStatus(ctx context.Context, name string, hasDrift bool) error
}

// ServiceRefResolver is the narrow service registry surface
// (`scenario.ServiceRegistry`) needed by the background rule: resolve `service`
// -> `artifact.ServiceRef` for `scenario.CheckDriftSpec`. It returns a copy of
// the ref; the getter is synchronous, see
// serviceregistry.Holder.Resolve).
type ServiceRefResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// ScryDeps are external dependencies for the background drift rule. They are
// passed through reaper.Deps fields, see Deps.Scry. nil ScryDeps makes
// `scry_background` skipped in dispatch with one warn per uptime, guarding
// against keeper.yml typos.
type ScryDeps struct {
	// Pool is the concrete pgx pool. queryRower abstraction is not enough
	// because gate requires BeginTx (SELECT FOR UPDATE + Commit/Rollback). nil
	// means the rule does not function.
	Pool TxBeginnerExecQuerier

	// DriftChecker implements the Slice B pipeline (`scenario.Runner`). nil -> no-op.
	DriftChecker DriftChecker

	// Services is the service registry for resolving ServiceRef by incarnation.service.
	Services ServiceRefResolver

	// Audit writes `incarnation.drift_checked` audit events (source=background).
	// nil -> audit is not written, with a warn in the wire-up caller log.
	Audit audit.Writer
}

// TxBeginnerExecQuerier is the narrow [*pgxpool.Pool] subset needed by the Scry
// rule. It combines [incarnation.TxBeginner] for FOR UPDATE tx and
// [incarnation.ExecQueryRower] for iterator/update calls. A real *pgxpool.Pool
// satisfies it automatically.
type TxBeginnerExecQuerier interface {
	incarnation.TxBeginner
	incarnation.ExecQueryRower
}

// runScryBackground handles the `scry_background` rule, called from
// reaper.dispatch. Iterator -> batch -> per-incarnation goroutine with
// throttle-check + validate-tx + CheckDrift + update + audit.
//
// throttle: semaphore `max_concurrent_in_flight` minus the number of already
// running dry_run runs in PG. The semaphore channel is created per tick, with
// lifetime of one tick plus completion of all its goroutines.
//
// Reaper tick synchronously waits for all launched goroutines before returning
// control to tickLoop. This gives correct metrics (ObserveRule with the number
// actually completed) and prevents accumulating dangling goroutines on
// ctx.Cancel. The "background does not block" semantics remain at the rule
// level because the next tick comes only after interval, but one tick is not
// unbounded.
func (r *Runner) runScryBackground(ctx context.Context, ruleName string, rule config.ReaperRule, batchSize int, dryRun bool, scry *ScryDeps) {
	if scry == nil || scry.Pool == nil || scry.DriftChecker == nil || scry.Services == nil {
		r.deps.Logger.Warn("reaper: scry_background: missing ScryDeps, rule skipped",
			slog.String("rule", ruleName),
		)
		return
	}

	maxConcurrent := defaultScryBackgroundMaxConcurrent
	if rule.MaxConcurrentInFlight != nil {
		maxConcurrent = *rule.MaxConcurrentInFlight
	}
	if maxConcurrent <= 0 {
		r.deps.Logger.Info("reaper: scry_background: max_concurrent_in_flight<=0, tick skipped (rule muted without enabled=false)",
			slog.String("rule", ruleName),
			slog.Int("max_concurrent_in_flight", maxConcurrent),
		)
		return
	}

	minInterval, err := parseRuleDuration(rule.MinIntervalPerIncarnation, 0)
	if err != nil {
		r.deps.Logger.Warn("reaper: scry_background: invalid min_interval_per_incarnation, using 0 (no throttle)",
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
	// Do not ask the iterator for more than fits in the pool. Extra rows would
	// simply be dropped by the semaphore with a "throttled" warn. Cap by
	// max_concurrent_in_flight; a small headroom is unnecessary because
	// sem.Acquire blocks instead of dropping, see below.
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

	// Account for dry_run runs already active in the cluster (cross-Keeper
	// throttle): sem cap = max_concurrent_in_flight - active. <=0 means this
	// tick is skipped quietly.
	active, err := incarnation.CountActiveDryRuns(ctx, scry.Pool)
	if err != nil {
		r.deps.Logger.Warn("reaper: scry_background: failed to count active dry_run, skipping tick",
			slog.String("rule", ruleName), slog.Any("error", err))
		r.deps.Metrics.ObserveRule(ruleName, 0, err, time.Since(start))
		return
	}
	headroom := maxConcurrent - active
	if headroom <= 0 {
		r.deps.Logger.Info("reaper: scry_background: pool full, tick skipped",
			slog.String("rule", ruleName),
			slog.Int("active", active),
			slog.Int("max_concurrent_in_flight", maxConcurrent),
		)
		r.deps.Metrics.ObserveRule(ruleName, 0, nil, time.Since(start))
		return
	}

	candidates, err := incarnation.SelectScryCandidates(ctx, scry.Pool, minInterval, iterBatch)
	if err != nil {
		r.deps.Logger.Warn("reaper: scry_background: iterator failed",
			slog.String("rule", ruleName), slog.Any("error", err))
		r.deps.Metrics.ObserveRule(ruleName, 0, err, time.Since(start))
		return
	}
	if len(candidates) == 0 {
		r.deps.Metrics.ObserveRule(ruleName, 0, nil, time.Since(start))
		return
	}

	// Semaphore channel plus WaitGroup-like completion so the tick waits for all
	// its goroutines; see docstring above.
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
	r.deps.Logger.Info("reaper: scry_background tick completed",
		slog.String("rule", ruleName),
		slog.Int("launched", launched),
		slog.Int("candidates", len(candidates)),
		slog.Int("headroom", headroom),
	)
}

// runScryOne processes one incarnation in a batch: validate-tx -> CheckDrift ->
// UpdateDriftScanResult + MarkDriftStatus + audit. It is protected from
// CheckDrift panic at the tick level: CheckDrift error is logged and causes an
// update no-op, preserving previous last_drift_* values.
func (r *Runner) runScryOne(ctx context.Context, cand incarnation.ScryCandidate, scry *ScryDeps) {
	log := r.deps.Logger.With(
		slog.String("rule", "scry_background"),
		slog.String("incarnation", cand.Name),
	)

	// Validate-and-go: short PG tx with SELECT FOR UPDATE to recheck status. If
	// incarnation moved out of ready/drift between iterator selection and now,
	// background skips it without audit or metric increment.
	if ok, err := scryCheckStatus(ctx, scry.Pool, cand.Name); err != nil {
		log.Warn("scry: validate tx failed, scan skipped", slog.Any("error", err))
		return
	} else if !ok {
		log.Debug("scry: incarnation is not ready/drift, scan skipped")
		return
	}

	serviceRef, ok := scry.Services.Resolve(cand.Service)
	if !ok {
		log.Warn("scry: service is not registered, scan skipped",
			slog.String("service", cand.Service))
		return
	}

	applyID := audit.NewULID()
	report, err := scry.DriftChecker.CheckDrift(ctx, scenario.CheckDriftSpec{
		ApplyID:         applyID,
		IncarnationName: cand.Name,
		ServiceRef:      serviceRef,
		// InputOverride: nil means background does not override converge
		// parameters, only auto-from-state (Slice B resolveDriftInput logic).
		// StartedByAID: "" means background has no Archon identity.
	})
	if err != nil {
		// ErrConvergeMissing is a common expected case for services without drift
		// support. Log it quieter.
		if errors.Is(err, scenario.ErrConvergeMissing) {
			log.Info("scry: converge is absent, drift is not supported by this service",
				slog.String("service", cand.Service))
			return
		}
		log.Warn("scry: CheckDrift failed, scan not recorded",
			slog.String("apply_id", applyID),
			slog.Any("error", err))
		return
	}

	// First summary (information layer), then MarkDriftStatus (status), then
	// audit. Each step is best-effort: one error does not cancel the others.
	summary := incarnation.DriftScanSummary{
		HostsDrifted:     report.Summary.HostsDrifted,
		HostsClean:       report.Summary.HostsClean,
		HostsUnsupported: report.Summary.HostsUnsupported,
		HostsFailed:      report.Summary.HostsFailed,
		TotalHosts:       len(report.Hosts),
		ScannedAt:        report.CheckedAt,
	}
	if err := incarnation.UpdateDriftScanResult(ctx, scry.Pool, cand.Name, summary); err != nil {
		log.Warn("scry: last_drift_summary write failed", slog.Any("error", err))
	}

	hasDrift := report.Summary.HostsDrifted > 0 || report.Summary.HostsFailed > 0
	if err := scry.DriftChecker.MarkDriftStatus(ctx, cand.Name, hasDrift); err != nil {
		log.Warn("scry: MarkDriftStatus failed", slog.Any("error", err))
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

	log.Info("scry: incarnation scanned",
		slog.String("apply_id", applyID),
		slog.Int("hosts_total", summary.TotalHosts),
		slog.Int("hosts_drifted", summary.HostsDrifted),
		slog.Int("hosts_failed", summary.HostsFailed),
		slog.Bool("has_drift", hasDrift),
	)
}

// scryCheckStatus is a short PG tx that protects background from starting
// dry_run against an incarnation whose status just moved out of ready/drift
// because an operator started run/destroy/upgrade between iterator selection and
// goroutine stage. It returns true when the incarnation is still ready/drift at
// check time.
//
// SELECT ... FOR UPDATE serializes against any concurrent scenario.lockRun,
// which also takes incarnation FOR UPDATE. Tx commits immediately (gate-only)
// so the lock is not held for the whole Scry run.
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
