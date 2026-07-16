// Package voyageorch orchestrates a pool of VoyageWorkers executing Voyage runs
// (ADR-043, S1). Each worker runs a claim-loop: atomically claims one pending
// Voyage via [voyage.ClaimNext], spawns a renewal goroutine via
// [voyage.RenewLease], executes the run, and finalizes via [voyage.Finalize]
// under CAS-guard of ownership. Pattern mirrors [tideorch]/[errandrunorch]:
// claim → renew → execute → finalize-with-ownership. Execution branches by kind:
//   - kind=scenario (S2): real batched scenario run over N incarnations per
//     batch (Leg); each incarnation gets its own scenario-run + state-commit.
//   - kind=command (S3): NOOP stub (S1 foundation); finalize succeeds.
// Config-gated OFF by default (see daemon.setupVoyageWorker); production
// wire-up of Spawner/Awaiter for kind=scenario is S5.
// Failover-resilience: on instance death, Reaper returns stale claim to
// pending; another Keeper picks it up via ClaimNext.
// TODO(post-S1): claim+lease helpers duplicate tide/errandrun; extract to
// shared `claimlease/` deferred (architect decision 2026-05-27).
package voyageorch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// VoyageWorker runs a claim+execute loop for Voyage runs. One Worker per goroutine;
// daemon spawns multiple (cfg.Voyage.Workers).
// Lifecycle: Run(ctx) loops until ctx cancellation; graceful-shutdown:
// cancel ctx → current executeVoyage finishes (S1: immediate NOOP-finalize);
// renewLoop exits on ctx.Done.
type VoyageWorker struct {
	KID           string
	Pool          voyage.ExecQueryRower
	LeaseTTL      time.Duration
	RenewInterval time.Duration
	PollInterval  time.Duration
	Logger        *slog.Logger

	// ScenarioSpawner / ScenarioAwaiter DI for kind=scenario execution (S2):
	// spawn per-incarnation scenario-run + await its terminal (parity
	// tideorch.SurgeSpawner/TerminalAwaiter). nil → claimed scenario-Voyage
	// finalizes failed (fail-closed; production wire-up S5). kind=command (S3)
	// does not use.
	ScenarioSpawner ScenarioSpawner
	ScenarioAwaiter IncarnationAwaiter

	// OrphanReleaser recovery-patch (ADR-027(k)): before respawning per-
	// incarnation scenario-run of reclaimed Voyage, releases stale applying-lock
	// left by dead prior owner (FENCED single-winner). nil → detection off (pre-fix
	// behavior; unit-test without recovery). kind=scenario (S2) only.
	OrphanReleaser OrphanLockReleaser

	// CommandSpawner DI for kind=command execution (S3): blocking Errand spawn
	// per SID (reuse errand machinery, parity errandrunorch.ErrandSpawner).
	// nil → claimed command-Voyage finalizes failed (fail-closed; production
	// wire-up S5). kind=scenario (S2) does not use.
	CommandSpawner CommandSpawner

	// Audit writer for finalize-audit family (ADR-043, A3): per-Leg
	// (scenario_run.leg_*) + terminal (scenario_run/command_run.{completed|
	// partial_failed|failed}) + lease_lost + voyage.reclaimed (written by Reaper
	// separately). nil-safe: dev without audit works; emit only if Audit != nil;
	// validate() does not require.
	Audit audit.Writer
}

// validate checks required fields. Called at Run start; error is
// programmatic (caller setupVoyageWorker should have provided all deps).
func (w *VoyageWorker) validate() error {
	if w.KID == "" {
		return errors.New("voyageorch: KID is required")
	}
	if w.Pool == nil {
		return errors.New("voyageorch: Pool is required")
	}
	if w.LeaseTTL <= 0 {
		return errors.New("voyageorch: LeaseTTL must be > 0")
	}
	if w.RenewInterval <= 0 {
		return errors.New("voyageorch: RenewInterval must be > 0")
	}
	if w.PollInterval <= 0 {
		return errors.New("voyageorch: PollInterval must be > 0")
	}
	if w.Logger == nil {
		return errors.New("voyageorch: Logger is required")
	}
	return nil
}

// Run loops claim-loop until ctx cancellation. Returns on ctx.Done with nil —
// normal graceful-shutdown. On invalid config (validate fail) returns error
// for caller (programmatic setup error).
func (w *VoyageWorker) Run(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}

	w.Logger.Info("voyageorch: worker started",
		slog.String("kid", w.KID),
		slog.Duration("lease_ttl", w.LeaseTTL),
		slog.Duration("renew_interval", w.RenewInterval),
		slog.Duration("poll_interval", w.PollInterval),
	)

	for {
		select {
		case <-ctx.Done():
			w.Logger.Info("voyageorch: worker stopped",
				slog.String("kid", w.KID),
				slog.Any("reason", ctx.Err()),
			)
			return nil
		default:
		}

		run, err := voyage.ClaimNext(ctx, w.Pool, w.KID, w.LeaseTTL)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			w.Logger.Error("voyageorch: ClaimNext failed",
				slog.String("kid", w.KID),
				slog.Any("error", err),
			)
			if !w.sleep(ctx, w.PollInterval) {
				return nil
			}
			continue
		}
		if run == nil {
			if !w.sleep(ctx, w.PollInterval) {
				return nil
			}
			continue
		}

		w.executeVoyage(ctx, run)
	}
}

// sleep ждёт duration или ctx.Done. Возвращает false, если вышли по ctx.
func (w *VoyageWorker) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// renewLoop CAS-renews lease every RenewInterval. On [voyage.ErrLeaseLost]
// closes leaseLost channel — executeVoyage sees lease lost and skips finalize
// (another Keeper picks it up). Parity errandrunorch.
func (w *VoyageWorker) renewLoop(ctx context.Context, runID string, leaseLost chan<- struct{}) {
	ticker := time.NewTicker(w.RenewInterval)
	defer ticker.Stop()

	var closed bool
	closeOnce := func() {
		if !closed {
			close(leaseLost)
			closed = true
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := voyage.RenewLease(ctx, w.Pool, runID, w.KID, w.LeaseTTL)
			if err == nil {
				continue
			}
			if errors.Is(err, voyage.ErrLeaseLost) {
				w.Logger.Warn("voyageorch: lease lost — another Keeper claimed Voyage",
					slog.String("voyage_id", runID),
					slog.String("kid", w.KID),
				)
				closeOnce()
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			w.Logger.Warn("voyageorch: RenewLease failed (will retry on next tick)",
				slog.String("voyage_id", runID),
				slog.String("kid", w.KID),
				slog.Any("error", err),
			)
		}
	}
}

// executeVoyage executes one Voyage claim. Branches by kind: scenario (S2,
// real batched run) / command (S3, NOOP-stub). Finalizes under ownership-guard;
// if lease lost mid-run (executeScenarioVoyage returns ""), skips finalize
// (another Keeper picks it up).
// renewCtx derived from ctx — renewal stops on ctx.Done. Defer wrapper
// guarantees order: cancelRenew() first (renewLoop sees ctx.Done and exits),
// then renewWG.Wait(). Bare `defer renewWG.Wait()` without cancel deadlocks.
func (w *VoyageWorker) executeVoyage(ctx context.Context, run *voyage.Voyage) {
	renewCtx, cancelRenew := context.WithCancel(ctx)
	var renewWG sync.WaitGroup
	leaseLost := make(chan struct{})
	renewWG.Add(1)
	go func() {
		defer renewWG.Done()
		w.renewLoop(renewCtx, run.VoyageID, leaseLost)
	}()
	defer func() {
		cancelRenew()
		renewWG.Wait()
	}()

	w.Logger.Info("voyageorch: voyage claimed",
		slog.String("kid", w.KID),
		slog.String("voyage_id", run.VoyageID),
		slog.String("kind", string(run.Kind)),
		slog.Int("total_batches", run.TotalBatches),
		slog.Int("attempt", run.Attempt),
	)

	// Branch by kind: scenario (S2) real batched run over N incarnations;
	// command (S3) real batched run over N hosts (absorbs ErrandRun, ADR-043).
	// Both executeXxxVoyage return ("", nil) on lease lost / ctx.Done mid-run —
	// skip finalize (Reaper-reclaim returns to pending; another Keeper picks up).
	var (
		finalStatus voyage.Status
		summary     *voyage.Summary
		// errCode machine code for fail-closed-failure reason (pre-start):
		// spawner_not_configured / empty_scenario_name / empty_module /
		// target_resolve_failed. Empty for happy-path or "all units failed"
		// (failure is actual outcome, not setup error). Written to terminal
		// failed-event payload (see emitFinalized).
		errCode string
	)
	switch run.Kind {
	case voyage.KindScenario:
		finalStatus, summary, errCode = w.executeScenarioVoyage(ctx, run, leaseLost)
	case voyage.KindCommand:
		finalStatus, summary, errCode = w.executeCommandVoyage(ctx, run, leaseLost)
	default:
		w.Logger.Error("voyageorch: unknown voyage kind",
			slog.String("voyage_id", run.VoyageID), slog.String("kind", string(run.Kind)))
		finalStatus = voyage.StatusFailed
		summary = &voyage.Summary{Total: run.TotalBatches}
		errCode = "unknown_kind"
	}
	if finalStatus == "" {
		// lease lost / interruption mid-run — skip finalize.
		return
	}

	// Before finalize, check if lease lost (renewLoop closed channel) —
	// another Keeper now owns Voyage, cannot finalize.
	select {
	case <-leaseLost:
		w.Logger.Warn("voyageorch: lease lost before finalize — skipping",
			slog.String("voyage_id", run.VoyageID),
			slog.String("kid", w.KID),
		)
		w.emitLeaseLost(run, "finalize")
		return
	default:
	}

	err := voyage.Finalize(ctx, w.Pool, run.VoyageID, w.KID, finalStatus, summary)
	if err != nil {
		if errors.Is(err, voyage.ErrLeaseLost) {
			w.Logger.Warn("voyageorch: finalize — lease lost, new owner finalizes",
				slog.String("voyage_id", run.VoyageID),
				slog.String("kid", w.KID),
			)
			w.emitLeaseLost(run, "finalize")
			return
		}
		w.Logger.Error("voyageorch: finalize failed",
			slog.String("voyage_id", run.VoyageID),
			slog.String("kid", w.KID),
			slog.Any("error", err),
		)
		return
	}

	w.Logger.Info("voyageorch: voyage finalized",
		slog.String("voyage_id", run.VoyageID),
		slog.String("kid", w.KID),
		slog.String("status", string(finalStatus)),
	)

	w.emitFinalized(run, finalStatus, summary, errCode)
}

// emitFinalized writes terminal finalize-event by kind+status (ADR-043, A3).
// source=keeper_internal, archon_aid="" (NULL), correlation_id=voyage_id.
// nil-safe: emit only if Audit != nil. scenario → scenario_run.*,
// command → command_run.*; payload form varies (scenario carries
// total_batches+summary, command carries total+succeeded from Summary, parity
// errand_run.*). errCode written only to failed-event of fail-closed paths.
func (w *VoyageWorker) emitFinalized(run *voyage.Voyage, status voyage.Status, summary *voyage.Summary, errCode string) {
	if w.Audit == nil {
		return
	}
	if summary == nil {
		summary = &voyage.Summary{}
	}

	var eventType audit.EventType
	payload := map[string]any{
		"voyage_id": run.VoyageID,
		"kind":      string(run.Kind),
	}
	// cadence_id on Voyage terminal (ADR-052 §l amend): Voyage spawned by
	// schedule (run.CadenceID != nil, claim selects cadence_id via
	// voyage.ClaimNext) carries cadence_id in terminal payload so cadence
	// selector Tiding catches ONE aggregated notification. nil-guarded symmetric
	// to scenario/run.go emitRunCompleted: manual Voyage (CadenceID nil) omits
	// field → cadence-selector won't match.
	if run.CadenceID != nil {
		payload["cadence_id"] = *run.CadenceID
	}

	if run.Kind == voyage.KindCommand {
		payload["total"] = summary.Total
		payload["succeeded"] = summary.Succeeded
		switch status {
		case voyage.StatusSucceeded:
			eventType = audit.EventCommandRunCompleted
		case voyage.StatusPartialFailed:
			eventType = audit.EventCommandRunPartialFailed
			payload["failed"] = summary.Failed
			payload["cancelled"] = summary.Cancelled
			payload["on_failure"] = derefOnFailure(run.OnFailure)
		default: // StatusFailed
			eventType = audit.EventCommandRunFailed
			if errCode != "" {
				payload["error_code"] = errCode
			}
		}
	} else {
		payload["total_batches"] = run.TotalBatches
		payload["summary"] = summaryPayload(summary)
		switch status {
		case voyage.StatusSucceeded:
			eventType = audit.EventScenarioRunCompleted
		case voyage.StatusPartialFailed:
			eventType = audit.EventScenarioRunPartialFailed
			payload["on_failure"] = derefOnFailure(run.OnFailure)
		default: // StatusFailed
			eventType = audit.EventScenarioRunFailed
			if errCode != "" {
				payload["error_code"] = errCode
			}
		}
	}

	w.writeAudit(eventType, run.VoyageID, payload)
}

// emitLeaseLost writes scenario_run.lease_lost (ADR-043, A3): VoyageWorker
// lost lease mid-run or before finalize. kind=scenario only (parity
// tide.lease_lost); command family has no lease_lost (parity errand_run.*) —
// run silently picked up by another Keeper. phase ∈ leg/finalize.
func (w *VoyageWorker) emitLeaseLost(run *voyage.Voyage, phase string) {
	if w.Audit == nil || run.Kind != voyage.KindScenario {
		return
	}
	w.writeAudit(audit.EventScenarioRunLeaseLost, run.VoyageID, map[string]any{
		"voyage_id":    run.VoyageID,
		"kind":         string(run.Kind),
		"kid_who_lost": w.KID,
		"phase":        phase,
	})
}

// emitLegStarted writes scenario_run.leg_started before Leg fan-out for
// kind=scenario (ADR-043, A3, parity tide.surge_started). command family
// has no leg-events (flat fan-out).
func (w *VoyageWorker) emitLegStarted(run *voyage.Voyage, legIndex, incarnationsInLeg int) {
	if w.Audit == nil {
		return
	}
	w.writeAudit(audit.EventScenarioRunLegStarted, run.VoyageID, map[string]any{
		"voyage_id":           run.VoyageID,
		"kind":                string(run.Kind),
		"leg_index":           legIndex,
		"incarnations_in_leg": incarnationsInLeg,
	})
}

// emitLegCompleted writes scenario_run.leg_completed after all incarnations
// in Leg terminal + Summary-delta aggregation (ADR-043, A3, parity
// tide.surge_completed). terminal is Leg terminal status from outcomes.
func (w *VoyageWorker) emitLegCompleted(run *voyage.Voyage, legIndex int, leg legOutcome) {
	if w.Audit == nil {
		return
	}
	w.writeAudit(audit.EventScenarioRunLegCompleted, run.VoyageID, map[string]any{
		"voyage_id": run.VoyageID,
		"kind":      string(run.Kind),
		"leg_index": legIndex,
		"terminal":  leg.terminal(),
		"total":     leg.total,
		"succeeded": leg.succeeded,
		"failed":    leg.failed,
		"cancelled": leg.cancelled,
	})
}

// advanceBatchProgress advances Voyage current_batch_index to completed Leg
// count (completedBatches) — UI indicator "Batch N/total". Barrier-only:
// window path (runSlidingWindow/runScenarioSlidingWindow) does not call
// (no batches, total_batches=1, progress by targets).
// Best-effort (voyage.UpdateBatchProgress, ownership-guarded): error/0-rows
// (lease lost, Reaper-reclaim) logged as warn, does not fail run — source of
// truth is voyage_targets, progress is UI hint only.
func (w *VoyageWorker) advanceBatchProgress(ctx context.Context, run *voyage.Voyage, completedBatches int) {
	if err := voyage.UpdateBatchProgress(ctx, w.Pool, run.VoyageID, w.KID, run.Attempt, completedBatches); err != nil {
		w.Logger.Warn("voyageorch: failed to update current_batch_index (best-effort)",
			slog.String("voyage_id", run.VoyageID), slog.Int("completed_batches", completedBatches),
			slog.Any("error", err))
	}
}

// writeAudit generic best-effort emit of keeper_internal event for run.
// Background ctx: emit outside apply-ctx so write succeeds even if original
// ctx canceled (graceful-shutdown). PG error logged as warn — finalize
// already committed to DB, audit-trail is secondary.
func (w *VoyageWorker) writeAudit(eventType audit.EventType, voyageID string, payload map[string]any) {
	ev := &audit.Event{
		EventType:     eventType,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: voyageID,
		Payload:       payload,
	}
	if err := w.Audit.Write(context.Background(), ev); err != nil {
		w.Logger.Warn("voyageorch: finalize audit write failed",
			slog.String("voyage_id", voyageID),
			slog.String("event_type", string(eventType)),
			slog.Any("error", err))
	}
}

// summaryPayload projects [voyage.Summary] to audit-payload form (no_match
// omitempty parity voyageSummaryDTO).
func summaryPayload(s *voyage.Summary) map[string]any {
	out := map[string]any{
		"total":     s.Total,
		"succeeded": s.Succeeded,
		"failed":    s.Failed,
		"cancelled": s.Cancelled,
	}
	if s.NoMatch > 0 {
		out["no_match"] = s.NoMatch
	}
	return out
}

// derefOnFailure returns string form of on_failure (empty if nil).
func derefOnFailure(p *voyage.OnFailure) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// legOutcome aggregates outcomes of one Leg for scenario_run.leg_completed.
type legOutcome struct {
	total     int
	succeeded int
	failed    int
	cancelled int
}

// terminal classifies Leg terminal (parity SurgeRecord.Terminal): failed
// (failure, no success) / partial (failure + success) / cancelled (cancelled
// only, no failure/success) / success.
func (l legOutcome) terminal() string {
	if l.failed > 0 {
		if l.succeeded > 0 {
			return "partial"
		}
		return "failed"
	}
	if l.cancelled > 0 && l.succeeded == 0 {
		return "cancelled"
	}
	return "success"
}
