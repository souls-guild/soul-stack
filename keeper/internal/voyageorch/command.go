package voyageorch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// kind=command execution (ADR-043 S3). Absorbs the ErrandRun pattern: batched
// run of a whitelisted module (ADR-033 whitelist + ErrandReadSafe) across a set
// of HOSTS. `incarnation.state` is NOT touched - no state commit (ADR-033/043).
//
// Batch (Leg) = N HOSTS. Unlike kind=scenario (Leg = N incarnations,
// per-incarnation scenario-run with its own state commit), here a Leg is a flat
// fan-out of one Errand per SID, without barrier/state. The algorithm is
// symmetric to executeScenarioVoyage:
//   1. target_resolved (kind=command) -> []sid (JSONB array of SIDs).
//   2. chunkSIDs(sids, batch_size) -> Legs (NULL/0 -> one Leg = all).
//   3. Per Leg: parallel fan-out across hosts under semaphore-cap (concurrency).
//      For each SID: SpawnCommand (= blocking Errand dispatch until terminal,
//      reusing errand machinery) -> MarkTargetRunning(errand_id) -> MarkTargetTerminal.
//   4. on_failure=abort -> first failed Leg stops transition to the next one;
//      continue -> run to the end.
//   5. Between Legs - inter_batch_interval pause (controlled rollout).
//   6. After all Legs - Finalize voyage (succeeded / partial_failed / failed).
//
// Errand machinery reuse: errandrunorch wrapped blocking errand.Dispatcher into
// one DI call, ErrandSpawner.SpawnErrand (spawn+await are collapsed because
// Dispatch is synchronous until terminal). Here we repeat exactly that boundary
// as [CommandSpawner.SpawnCommand] (production wire-up S5 will supply an adapter
// over the same Dispatcher). The S2 "Spawner+Awaiter" shape for command
// degenerates into one interface: Errand has no async-spawn phase to await
// separately (unlike scenario-runner).

// CommandSpawner spawns one Errand for one SID, blocking until terminal.
// It isolates voyageorch from errand.Dispatcher / Outbound / ApplyBus (parity
// with errandrunorch.ErrandSpawner): production wire-up (S5) supplies an adapter
// over the existing Dispatcher.Dispatch; unit tests use a dependency-free fake.
//
// Contract (matches errandrunorch.ErrandSpawner.SpawnErrand minus Cancel -
// voyage-level cancel-all is deferred to S5):
//
//  1. SpawnCommand blocks until Errand reaches a terminal status or until ctx is
//     cancelled (caller passes fanCtx, which is cancelled on leaseLost /
//     on_failure=abort).
//  2. Returned status is a string projection of errand.Status: success / failed /
//     timed_out / cancelled / module_not_allowed. Whitelist (ADR-033) is checked
//     on the Soul side; module_not_allowed arrives as a normal terminal and
//     voyageorch does not duplicate it.
//  3. errandID is a back-link to the errands row (for voyage_targets.errand_id and
//     S5 drill). Empty string is possible if Spawner failed before Insert; then
//     caller records target as failed without a back-link.
//  4. err != nil is an internal orchestrator-call error (not a failed Errand);
//     caller treats the Errand as failed.
//
// module is voyages.module (whitelisted, NOT NULL for kind=command). input is
// voyages.input (jsonb, passed into errands.input unchanged).
// startedByAID is the AID of the initiating Archon (FK errands.started_by_aid).
type CommandSpawner interface {
	SpawnCommand(ctx context.Context, voyageID, sid, module, startedByAID string, input []byte) (errandID, status string, err error)
}

// commandResult is the runtime outcome of one host in a Leg, collected in a
// per-SID goroutine and stored under mutex. ErrandID is empty if spawn failed
// before creating the errand row (then Status=failed without a back-link).
type commandResult struct {
	SID      string
	ErrandID string
	Outcome  TargetOutcome
}

// summarize aggregates host outcomes into [voyage.Summary] (Total = full run
// scope, passed separately because results can be shorter on abort). It also
// returns anyFailure for final-status selection. This is the shared aggregator
// for both command executor control-flow frames (barrier loop executeCommandVoyage
// and window runSlidingWindow); the frames themselves differ and are NOT merged,
// only this pure reduction is shared.
func summarize(results []commandResult, total int) (*voyage.Summary, bool) {
	summary := &voyage.Summary{Total: total}
	var anyFailure bool
	for _, res := range results {
		switch res.Outcome {
		case OutcomeSucceeded, OutcomeNoMatch:
			summary.Succeeded++
		case OutcomeCancelled:
			summary.Cancelled++
		default:
			summary.Failed++
		}
		if res.Outcome == OutcomeNoMatch {
			summary.NoMatch++
		}
		if res.Outcome.isFailure() {
			anyFailure = true
		}
	}
	return summary, anyFailure
}

// commandStatusToOutcome maps string errand status to [TargetOutcome].
// success -> succeeded; cancelled -> cancelled; everything else (failed / timed_out /
// module_not_allowed / unknown) -> failed (fail-closed, parity with
// errandrunorch.isFailureStatus + buildSummary).
func commandStatusToOutcome(status string) TargetOutcome {
	switch status {
	case "success":
		return OutcomeSucceeded
	case "cancelled":
		return OutcomeCancelled
	default:
		return OutcomeFailed
	}
}

// parseSIDTargets parses target_resolved (kind=command): JSONB array of SIDs
// (snapshot of the host set at run start, ADR-043). Empty array / invalid JSON /
// empty SID / duplicate is an error (parity with parseIncarnationTargets: Insert
// requires non-empty valid target_resolved, duplicate breaks UNIQUE PK
// voyage_targets).
func parseSIDTargets(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("voyageorch: empty target_resolved")
	}
	var sids []string
	if err := json.Unmarshal(raw, &sids); err != nil {
		return nil, fmt.Errorf("voyageorch: target_resolved (kind=command) is not a JSON array of SIDs: %w", err)
	}
	if len(sids) == 0 {
		return nil, fmt.Errorf("voyageorch: target_resolved is empty (no hosts)")
	}
	seen := make(map[string]struct{}, len(sids))
	for i, s := range sids {
		if s == "" {
			return nil, fmt.Errorf("voyageorch: target_resolved[%d] has empty SID", i)
		}
		if _, dup := seen[s]; dup {
			return nil, fmt.Errorf("voyageorch: target_resolved contains duplicate SID %q", s)
		}
		seen[s] = struct{}{}
	}
	return sids, nil
}

// chunkSIDs cuts the flat host list into sequential Legs of at most batchSize.
// batchSize <= 0 -> one Leg for everything (NULL/0 batch_size = "whole run in one
// Leg", ADR-043). Semantics are identical to chunkIncarnations (kind=scenario):
// command variant is kept separate to avoid editing S2 code.
func chunkSIDs(sids []string, batchSize int) [][]string {
	if len(sids) == 0 {
		return nil
	}
	if batchSize <= 0 {
		return [][]string{append([]string(nil), sids...)}
	}
	legs := make([][]string, 0, (len(sids)+batchSize-1)/batchSize)
	for off := 0; off < len(sids); off += batchSize {
		end := off + batchSize
		if end > len(sids) {
			end = len(sids)
		}
		legs = append(legs, append([]string(nil), sids[off:end]...))
	}
	return legs
}

// executeCommandVoyage is the S3 kind=command executor: batched run of a
// whitelisted module across N hosts. Called from [VoyageWorker.executeVoyage]
// after claim; renewal goroutine already holds the lease (leaseLost closes on
// loss).
//
// Returns final [voyage.Status] + [*voyage.Summary] + error_code for
// Finalize/finalize-audit. error_code is non-empty only for fail-closed paths
// before hosts start (spawner_not_configured / empty_module / target_resolve_failed);
// for happy path and "all hosts failed" it is empty. On lease loss / ctx.Done in
// the middle of the run, returns ("", nil, "") - caller does NOT finalize
// (Reaper-reclaim returns Voyage to pending, another Keeper picks it up). Symmetric
// to executeScenarioVoyage; command leg events and lease_lost are NOT emitted (parity
// errand_run.*).
func (w *VoyageWorker) executeCommandVoyage(ctx context.Context, run *voyage.Voyage, leaseLost <-chan struct{}) (voyage.Status, *voyage.Summary, string) {
	if w.CommandSpawner == nil {
		// Production wire-up (S5) must provide Spawner; absence on a claimed
		// command-Voyage is a setup programming error. Fail closed.
		w.Logger.Error("voyageorch: command execution requested but CommandSpawner not configured",
			slog.String("voyage_id", run.VoyageID),
		)
		return voyage.StatusFailed, &voyage.Summary{Total: run.TotalBatches}, "spawner_not_configured"
	}
	if run.Module == nil || *run.Module == "" {
		w.Logger.Error("voyageorch: kind=command without module", slog.String("voyage_id", run.VoyageID))
		return voyage.StatusFailed, &voyage.Summary{}, "empty_module"
	}

	sids, err := parseSIDTargets(run.TargetResolved)
	if err != nil {
		w.Logger.Error("voyageorch: parse target_resolved failed",
			slog.String("voyage_id", run.VoyageID), slog.Any("error", err))
		return voyage.StatusFailed, &voyage.Summary{}, "target_resolve_failed"
	}

	concurrency := 1
	if run.Concurrency != nil && *run.Concurrency > 0 {
		concurrency = *run.Concurrency
	}

	// failThreshold is a generalized abort-gate (ADR-043 amendment section 3):
	// absolute failure count threshold that stops the run. abort == 1
	// (backcompat: first failure -> stop); continue/nil without fail_threshold == 0
	// (no threshold - run to the end); explicit fail_threshold N -> N.
	// 0 means "no threshold".
	failThreshold := voyage.ResolveFailThreshold(run.FailThreshold, run.OnFailure)

	// batch_mode=window -> sliding window across hosts (ADR-043 amendment section 1):
	// one shared pool of concurrency workers from a single SID queue, without
	// Legs or barriers. barrier (NULL) -> existing chunk+runCommandLeg path below.
	if voyage.ResolveBatchMode(run.BatchMode) == voyage.BatchModeWindow {
		return w.runSlidingWindow(ctx, run, sids, concurrency, failThreshold, leaseLost)
	}

	batchSize := 0
	if run.BatchSize != nil {
		batchSize = *run.BatchSize
	}
	legs := chunkSIDs(sids, batchSize)

	var (
		summary    = &voyage.Summary{Total: len(sids)}
		anyFailure bool
	)

	for legIdx, leg := range legs {
		// Early detection of lease loss / cancellation before spawning a new Leg.
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost between Legs - another Keeper will pick up Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			return "", nil, ""
		case <-ctx.Done():
			w.Logger.Info("voyageorch: command-loop interrupted by ctx.Done",
				slog.String("voyage_id", run.VoyageID), slog.Any("reason", ctx.Err()))
			return "", nil, ""
		default:
		}

		// Pause before every Leg except the first (inter_batch_interval).
		if legIdx > 0 && run.InterBatchInterval != nil && *run.InterBatchInterval > 0 {
			if !w.interBatchPause(ctx, *run.InterBatchInterval, leaseLost) {
				w.Logger.Warn("voyageorch: interrupted during inter_batch_interval - not finalizing",
					slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
				return "", nil, ""
			}
		}

		results, fenceLost := w.runCommandLeg(ctx, run, legIdx, leg, concurrency, leaseLost)

		// Lease loss IN THE MIDDLE of a Leg: runCommandLeg stopped spawn-loop and
		// interrupted in-flight through cancelled fanCtx, remaining hosts are marked
		// cancelled. Do NOT finalize (Reaper-reclaim). Two detectors: renewLoop
		// leaseLost channel (on tick) AND fenceLost - fencing CAS before dispatch
		// caught the loss before renew tick (S-med-2). Reclaiming Keeper owns Voyage,
		// so this worker must not finalize.
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost in the middle of a Leg - another Keeper will pick up Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			return "", nil, ""
		default:
		}
		if fenceLost {
			w.Logger.Warn("voyageorch: ownership-fence lost in the middle of a Leg - another Keeper will pick up Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			return "", nil, ""
		}

		legSummary, legFailure := summarize(results, 0)
		summary.Succeeded += legSummary.Succeeded
		summary.Failed += legSummary.Failed
		summary.Cancelled += legSummary.Cancelled
		summary.NoMatch += legSummary.NoMatch
		anyFailure = anyFailure || legFailure

		// Batch progress for UI: Leg legIdx COMPLETED -> current_batch_index =
		// legIdx+1 (best-effort, ownership-guarded; symmetry with scenario path).
		w.advanceBatchProgress(ctx, run, legIdx+1)

		// Generalized abort-gate (ADR-043 amendment section 3): failure count
		// threshold reached -> stop moving to the next Leg. threshold=0 -> no
		// threshold (continue). summary.Failed is cumulative across all Legs.
		if failThreshold > 0 && summary.Failed >= failThreshold {
			w.Logger.Info("voyageorch: fail_threshold reached - remaining Legs skipped",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx),
				slog.Int("failed", summary.Failed), slog.Int("fail_threshold", failThreshold))
			break
		}
	}

	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// runSlidingWindow is the S-W1 batch_mode=window executor for kind=command
// (ADR-043 amendment section 1). Full sliding window across hosts: a pool of
// `concurrency` workers pulls SIDs from ONE shared run queue; when a worker
// returns, it takes the next SID. It constantly keeps <= concurrency active.
// There are no Legs / barriers / chunk loop - the run is flat (batch_index=0 is
// fixed for all targets on Insert, ADR-043 amendment section 7).
//
// Reuses barrier machinery unchanged:
//   - [runOneCommand] - per-unit logic (VerifyOwnership fencing before dispatch,
//     SpawnCommand, MarkTargetRunning/Terminal, fail-closed mapping);
//   - fanCtx + leaseLost watcher - window cancellation on lease loss / ctx.Done
//     (workers stop pulling the queue, in-flight commands are interrupted by cancelled fanCtx);
//   - fenceLost - VerifyOwnership CAS caught reclaim before renew tick;
//   - [summarize] - shared outcome aggregation into [voyage.Summary];
//   - [VoyageWorker.cancelRemaining] - mark unspawned host cancelled
//     (parity with barrier-markRemainingCancelled), called on abort for the queue remainder.
//
// failThreshold (ADR-043 amendment section 3, generalized abort-gate): absolute
// failure count threshold that stops SPAWNING new items from the queue (cancelFan
// stops polling, current active items finish); remaining queued hosts are marked
// cancelled (parity with barrier: "remaining Legs skipped" -> unspawned cancelled,
// Total = succeeded+failed+cancelled in both modes).
// threshold=0 -> no threshold (window drains the queue, continue);
// threshold=1 -> first failure -> stop (backcompat abort); N>1 - tolerance.
//
// inter_unit_interval (ADR-043 amendment section 4): per-unit pause before
// spawning each next window unit - soft throttle for the sliding window (parity
// with inter_batch_interval between Legs in barrier). Interrupted by fanCtx.
//
// Return is symmetric to [executeCommandVoyage]: ("", nil, "") on lease loss /
// ctx.Done in the middle of the window (caller does not finalize - Reaper-reclaim);
// otherwise final status + summary.
func (w *VoyageWorker) runSlidingWindow(
	ctx context.Context, run *voyage.Voyage, sids []string, concurrency int,
	failThreshold int, leaseLost <-chan struct{},
) (voyage.Status, *voyage.Summary, string) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// Watcher: lease loss (renew tick) cancels fanCtx -> workers stop pulling the
	// queue and in-flight commands are interrupted. On happy path exits via fanCtx.Done.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-leaseLost:
			cancelFan()
		case <-fanCtx.Done():
		}
	}()

	// queue is the shared host queue for the window; buffer covers everything so it
	// can be filled without blocking, then workers pull from it (returned -> next).
	queue := make(chan string, len(sids))
	for _, sid := range sids {
		queue <- sid
	}
	close(queue)

	var (
		mu        sync.Mutex
		results   = make([]commandResult, 0, len(sids))
		wg        sync.WaitGroup
		fenceLost atomic.Bool
		failCount atomic.Int64 // cumulative window failures (generalized abort-gate section 3).
	)

	appendResult := func(res commandResult) {
		mu.Lock()
		results = append(results, res)
		mu.Unlock()
	}

	// interUnit is the per-unit throttle pause (ADR-043 amendment section 4). 0 -> no pause.
	var interUnit time.Duration
	if run.InterUnitInterval != nil && *run.InterUnitInterval > 0 {
		interUnit = *run.InterUnitInterval
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// Priority cancellation pre-check: if queue-recv and fanCtx.Done are
				// both ready, Go would choose randomly and might spawn an extra Errand
				// after lease loss / abort. Explicit check makes stopping deterministic
				// (parity with runCommandLeg).
				select {
				case <-fanCtx.Done():
					return
				default:
				}

				var (
					sid string
					ok  bool
				)
				select {
				case <-fanCtx.Done():
					return
				case sid, ok = <-queue:
					if !ok {
						return // queue drained
					}
				}

				// inter_unit_interval (section 4): per-unit throttle before spawning a unit.
				// Interrupted by fanCtx (lease lost / abort / ctx.Done); then exit without
				// spawning (the unit stays in the queue and is drained as cancelled).
				if interUnit > 0 {
					t := time.NewTimer(interUnit)
					select {
					case <-t.C:
					case <-fanCtx.Done():
						t.Stop()
						// Host was already taken from the queue - mark cancelled (did not start).
						appendResult(w.cancelRemaining(ctx, run.VoyageID, sid))
						return
					}
				}

				res := w.runOneCommand(fanCtx, run, sid, cancelFan, &fenceLost)
				appendResult(res)

				// Generalized abort-gate (section 3): failure threshold reached -> stop
				// SPAWNING new items from the queue (cancelFan stops polling for all
				// workers); current active ones finish runOneCommand.
				// threshold=0 -> no threshold. fenceLost path already called cancelFan
				// inside runOneCommand; doing it here too is safe (idempotent).
				if res.Outcome.isFailure() {
					n := failCount.Add(1)
					if failThreshold > 0 && n >= int64(failThreshold) {
						cancelFan()
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	cancelFan() // happy path: wake watcher through fanCtx.Done.
	<-watcherDone

	// Lease loss in the middle of the window -> do NOT finalize (Reaper-reclaim).
	// Two detectors, as in barrier path: renewLoop leaseLost channel AND fenceLost
	// (VerifyOwnership CAS caught reclaim before renew tick). This happens before
	// marking the queue remainder cancelled: on reclaim the owner changed, so we do
	// not write to its voyage_targets.
	select {
	case <-leaseLost:
		w.Logger.Warn("voyageorch: lease lost in the middle of window - another Keeper will pick up Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	default:
	}
	if fenceLost.Load() {
		w.Logger.Warn("voyageorch: ownership-fence lost in the middle of window - another Keeper will pick up Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	}

	// abort could stop the window before the queue is drained (cancelFan stopped
	// polling): hosts left in the queue buffer were never pulled. Mark them
	// cancelled, same as barrier path (runCommandLeg.markRemainingCancelled), so
	// Total = succeeded+failed+cancelled matches in both modes (same drill UI).
	// Workers have already finished (wg.Wait), draining the closed buffer is safe;
	// on happy path the queue is empty, so the loop is a no-op.
	for sid := range queue {
		results = append(results, w.cancelRemaining(ctx, run.VoyageID, sid))
	}

	summary, anyFailure := summarize(results, len(sids))
	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// runCommandLeg executes one Leg: parallel fan-out across hosts under
// semaphore-cap (concurrency). Per-host: SpawnCommand -> MarkTargetRunning ->
// MarkTargetTerminal. Returns outcome slice.
//
// leaseLost (AS in S2 runLeg fix): renewal goroutine can lose lease IN THE MIDDLE
// of a long serial Leg (batch_size=NULL -> one Leg = all N hosts, concurrency=1).
// Spawning must not continue: Voyage may already have been reclaimed by another
// Keeper (runaway spawn + duplicate Errands). Derived fanCtx is cancelled by a
// watcher goroutine when leaseLost closes:
//   - spawn-loop stops (acquire-select catches fanCtx.Done);
//   - remaining hosts are marked cancelled (reported as "did not start in time");
//   - in-flight SpawnCommand calls are interrupted via cancelled fanCtx.
//
// Do NOT finalize on lease loss; caller decides that (executeCommandVoyage checks
// leaseLost / fenceLost after runCommandLeg, Reaper-reclaim). Returns outcomes +
// fenceLost: true if ownership-fence (VerifyOwnership before dispatch, S-med-2)
// caught lease loss before renewLoop tick; caller then also does not finalize.
func (w *VoyageWorker) runCommandLeg(ctx context.Context, run *voyage.Voyage, batchIndex int, leg []string, concurrency int, leaseLost <-chan struct{}) ([]commandResult, bool) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// leaseLost watcher goroutine: on signal cancels fanCtx to stop spawn-loop before
	// remaining hosts start. On happy path (wg.Wait -> defer cancelFan) exits through
	// fanCtx.Done.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-leaseLost:
			cancelFan()
		case <-fanCtx.Done():
		}
	}()

	sem := make(chan struct{}, concurrency)
	var (
		mu        sync.Mutex
		results   = make([]commandResult, 0, len(leg))
		wg        sync.WaitGroup
		fenceLost atomic.Bool // set when fencing CAS before dispatch caught lease loss.
	)

	// markRemainingCancelled: remaining host will not start (fanCtx cancelled:
	// lease lost / ctx.Done). Mark cancelled; MarkTargetTerminal uses parent ctx so
	// the write can complete even when fanCtx is cancelled by graceful shutdown.
	markRemainingCancelled := func(sid string) {
		mu.Lock()
		results = append(results, w.cancelRemaining(ctx, run.VoyageID, sid))
		mu.Unlock()
	}

	for _, sid := range leg {
		// Priority cancellation pre-check (AS in S2 runLeg): if sem-acquire and
		// fanCtx.Done are both ready, Go would choose randomly and might spawn an
		// extra Errand after lease loss. Explicit check makes stopping deterministic.
		select {
		case <-fanCtx.Done():
			markRemainingCancelled(sid)
			continue
		default:
		}

		select {
		case sem <- struct{}{}:
		case <-fanCtx.Done():
			markRemainingCancelled(sid)
			continue
		}

		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			defer func() { <-sem }()

			res := w.runOneCommand(fanCtx, run, sid, cancelFan, &fenceLost)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(sid)
	}

	wg.Wait()
	cancelFan() // happy path: wake watcher through fanCtx.Done.
	<-watcherDone
	_ = batchIndex // batch_index is already fixed in voyage_targets at Insert (S5).
	return results, fenceLost.Load()
}

// runOneCommand spawns Errand for one host (blocking until terminal) and updates
// voyage_targets. Tracking errors (MarkTarget*) are logged but do not fail the host
// outcome: authoritative source is the status returned by SpawnCommand.
//
// Lease-fencing (S-med-2): dispatchErrand has no ownership guard of its own (it
// sends Errand to the Soul side; parity top-level Finalize-CAS is absent at
// leg-spawn level). Therefore BEFORE spawning we check VerifyOwnership: this worker
// still owns Voyage with my claim epoch (run.Attempt). If lease is lost
// (Reaper-reclaim -> another Keeper picked up Voyage, attempt++), do NOT send Errand
// (otherwise duplicate child Errand execution), mark host cancelled, raise fenceLost,
// and stop the rest of the Leg through cancelFan (integrating with existing fanCtx
// mechanism). Transient PG verification error (not ErrLeaseLost) is not confirmed
// reclaim: fail-closed for one host (do not send Errand, failed), without aborting
// the entire Leg.
func (w *VoyageWorker) runOneCommand(ctx context.Context, run *voyage.Voyage, sid string, cancelFan context.CancelFunc, fenceLost *atomic.Bool) commandResult {
	if err := voyage.VerifyOwnership(ctx, w.Pool, run.VoyageID, w.KID, run.Attempt); err != nil {
		if errors.Is(err, voyage.ErrLeaseLost) {
			// Lease lost in the middle of a Leg - do NOT send Errand (fencing).
			w.Logger.Warn("voyageorch: ownership-fence lost before dispatch - Errand not sent",
				slog.String("voyage_id", run.VoyageID), slog.String("sid", sid),
				slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
			fenceLost.Store(true)
			cancelFan()
			w.trackCommandTerminal(ctx, run.VoyageID, sid, OutcomeCancelled)
			return commandResult{SID: sid, Outcome: OutcomeCancelled}
		}
		// Transient PG error: cannot confirm ownership - do not send Errand,
		// fail closed for this host (without aborting the whole Leg).
		w.Logger.Warn("voyageorch: ownership-fence check failed (transient) - Errand not sent",
			slog.String("voyage_id", run.VoyageID), slog.String("sid", sid), slog.Any("error", err))
		w.trackCommandTerminal(ctx, run.VoyageID, sid, OutcomeFailed)
		return commandResult{SID: sid, Outcome: OutcomeFailed}
	}

	errandID, status, err := w.CommandSpawner.SpawnCommand(ctx, run.VoyageID, sid, *run.Module, run.StartedByAID, run.Input)
	if err != nil {
		// Internal orchestrator-call error (not a failed Errand). On ctx cancellation
		// (leaseLost / abort) use cancelled, otherwise failed (not silent success).
		var outcome TargetOutcome
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = OutcomeCancelled
		} else {
			w.Logger.Warn("voyageorch: spawn command failed",
				slog.String("voyage_id", run.VoyageID), slog.String("sid", sid), slog.Any("error", err))
			outcome = OutcomeFailed
		}
		// errandID can be non-empty (Errand created, but dispatch returned an error);
		// set back-link if present.
		if errandID != "" {
			w.markCommandRunning(ctx, run.VoyageID, sid, errandID, run.Attempt)
		}
		w.trackCommandTerminal(ctx, run.VoyageID, sid, outcome)
		return commandResult{SID: sid, ErrandID: errandID, Outcome: outcome}
	}

	if errandID != "" {
		w.markCommandRunning(ctx, run.VoyageID, sid, errandID, run.Attempt)
	}

	outcome := commandStatusToOutcome(status)
	w.trackCommandTerminal(ctx, run.VoyageID, sid, outcome)
	return commandResult{SID: sid, ErrandID: errandID, Outcome: outcome}
}

// markCommandRunning best-effort sets back-link errand_id (awaiting->running) under
// attempt-fenced CAS. PG error is logged as warn; outcome is already fixed by caller.
func (w *VoyageWorker) markCommandRunning(ctx context.Context, voyageID, sid, errandID string, attempt int) {
	if err := voyage.MarkTargetRunning(ctx, w.Pool, voyageID, voyage.TargetKindSID, sid, errandID, attempt); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetRunning failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("sid", sid), slog.Any("error", err))
	}
}

// cancelRemaining marks an unspawned host cancelled: writes terminal state to
// voyage_targets (best-effort) and returns the corresponding [commandResult].
// Shared "remaining host did not start in time" mechanism for both frames:
// barrier-Leg (fanCtx cancelled in the middle of a serial Leg) and window (abort
// stopped polling from the queue). MarkTargetTerminal uses parent ctx so the write
// can complete even when fanCtx is cancelled (graceful shutdown / abort).
func (w *VoyageWorker) cancelRemaining(ctx context.Context, voyageID, sid string) commandResult {
	w.trackCommandTerminal(ctx, voyageID, sid, OutcomeCancelled)
	return commandResult{SID: sid, Outcome: OutcomeCancelled}
}

// trackCommandTerminal best-effort records host terminal state in voyage_targets.
// PG error is logged as warn; outcome is already in commandResult (finalize authority).
func (w *VoyageWorker) trackCommandTerminal(ctx context.Context, voyageID, sid string, outcome TargetOutcome) {
	if err := voyage.MarkTargetTerminal(ctx, w.Pool, voyageID, voyage.TargetKindSID, sid, outcome.toTargetStatus()); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetTerminal failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("sid", sid),
			slog.String("outcome", string(outcome)), slog.Any("error", err))
	}
}
