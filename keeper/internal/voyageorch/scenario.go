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

// kind=scenario execution (ADR-043 S2, approach B1).
//
// Batch (Leg) = N incarnations. Each incarnation in Leg is a full
// scenario-run with its own cross-host barrier and per-incarnation state-commit; this
// commit is done BY scenario-runner itself (ADR-009 §7), voyageorch does NOT duplicate it —
// it only orchestrates N independent scenario-run-s and tracks them through
// `voyage_targets`.
//
// Algorithm (executeScenarioVoyage):
//  1. target_resolved → []incarnationName (JSONB array of names).
//  2. chunkIncarnations(names, batch_size) → Legs (NULL/0 → one Leg = all).
//  3. Per Leg: parallel fan-out across incarnations under semaphore-cap
//     (concurrency, parity errandrunorch.runFanOut). Per incarnation:
//     SpawnScenarioRun (= scenario-runner per-incarnation) → MarkTargetRunning →
//     Await terminal → MarkTargetTerminal.
//  4. on_failure=abort → first failed Leg stops transition to
//     next; on_failure=continue → continue to end.
//  5. Between Legs — pause inter_batch_interval (controlled rollout).
//  6. After all Legs — Finalize voyage (succeeded / partial_failed / failed)
//     + Summary under ownership-guard.

// ScenarioSpawner spawns one per-incarnation scenario-run. Isolates
// voyageorch from scenario-runner / ServiceRegistry / incarnation-CRUD (parity
// tideorch.SurgeSpawner / errandrunorch.ErrandSpawner): production wire-up (S5)
// provides adapter that resolves ServiceRef (incarnation.SelectByName →
// ServiceRegistry.Resolve) and calls scenario.Runner.Start; unit-tests use fake
// without dependencies.
//
// Contract:
//   - returns applyID of spawned run (ULID, for back-link to apply_runs
//     and subsequent [IncarnationAwaiter.Await]);
//   - async call: scenario.Runner.Start returns immediately (run lives in own
//     goroutine), terminal arrives via apply_runs; awaiting is separate phase
//     via Awaiter;
//   - err != nil — run could not even start (incarnation not found /
//     error_locked / ServiceRef does not resolve / Runner.Start rejected). Caller
//     records target as failed without applyID.
//
// cadenceID — back-link to Cadence schedule (voyages.cadence_id, ADR-046 §2):
// nil ⇒ manual Voyage; populated ⇒ Voyage spawned by schedule. Production-spawner
// puts it in RunSpec.CadenceID so terminal event of run
// (incarnation.run_completed) carries cadence_id for persistent Tiding rules with
// cadence selector (T4b).
type ScenarioSpawner interface {
	SpawnScenarioRun(ctx context.Context, voyageID, incarnationName, scenarioName string, input []byte, startedByAID string, cadenceID *string) (applyID string, err error)
}

// OrphanLockReleaser releases orphaned applying-lock of incarnation left from
// scenario-run of dead Keeper owner of this Voyage from previous attempt (recovery
// seam, ADR-027(k)). Isolates voyageorch from incarnation/applyrun-CRUD (parity
// ScenarioSpawner): production wire-up (daemon) provides adapter over
// incarnation.ReleaseApplyingOrphan; unit-tests use fake.
//
// orphanApplyID — back-link apply_id of orphaned run from voyage_targets of this
// Voyage (from previous attempt). Implementation must be FENCED single-winner:
// releases lock ONLY when incarnation is applying AND orphanApplyID belongs to it
// (incarnation.ReleaseApplyingOrphan: FOR UPDATE + apply_id-match + CAS).
//
// Return contract:
//   - released=true  — lock released (applying → ready), re-run can start;
//   - released=false, err=nil — nothing to release (not applying / orphan apply_id not
//     ours / honest finalization of previous owner already won row): caller continues
//     re-run WITHOUT release (lockRun will reject if state still not runnable);
//   - err != nil — PG CRUD failure: caller logs and does NOT spawn (fail-closed per
//     incarnation, not abort entire Voyage).
//
// nil field → orphaned lock detection disabled (unit test without recovery seam /
// dev build): runOneIncarnation goes straight to spawn as before fix.
type OrphanLockReleaser interface {
	ReleaseOrphanLock(ctx context.Context, voyageID, incarnationName string, attempt int, kid string, orphanApplyID string) (released bool, err error)
}

// IncarnationAwaiter waits for terminal status of one per-incarnation scenario-run
// by applyID. Production wire-up (S5) provides implementation over
// applyrun.SelectStatusesByApplyID (poll until terminal for all incarnation hosts,
// parity tideorch.PgApplyTerminalAwaiter); unit-tests use fake.
//
// Blocks until terminal or ctx.Done. Returns [TargetOutcome] (succeeded /
// failed / cancelled / no_match). ctx.Err on cancellation (graceful-shutdown /
// on_failure abort) — caller records target as cancelled.
type IncarnationAwaiter interface {
	Await(ctx context.Context, applyID string) (TargetOutcome, error)
}

// TargetOutcome — terminal outcome of one per-incarnation scenario-run,
// projected to [voyage.TargetStatus]. Closed-set of values matches
// CHECK voyage_targets_status_valid (terminal subset).
type TargetOutcome string

const (
	OutcomeSucceeded TargetOutcome = "succeeded"
	OutcomeFailed    TargetOutcome = "failed"
	OutcomeCancelled TargetOutcome = "cancelled"
	OutcomeNoMatch   TargetOutcome = "no_match"
)

// toTargetStatus translates TargetOutcome to voyage.TargetStatus for writing to
// voyage_targets. Unknown value → failed (fail-closed: not silently success).
func (o TargetOutcome) toTargetStatus() voyage.TargetStatus {
	switch o {
	case OutcomeSucceeded:
		return voyage.TargetStatusSucceeded
	case OutcomeCancelled:
		return voyage.TargetStatusCancelled
	case OutcomeNoMatch:
		return voyage.TargetStatusNoMatch
	default:
		return voyage.TargetStatusFailed
	}
}

// isFailure reports whether outcome is considered failure for decision-gate on_failure and
// Summary.Failed count. cancelled/no_match — NOT failure (cancelled — consequence of
// abort/shutdown, no_match — benign "incarnation out of scope", parity
// tideorch.classifyApplyOutcome).
func (o TargetOutcome) isFailure() bool { return o == OutcomeFailed }

// parseIncarnationTargets parses target_resolved (kind=scenario): JSONB array
// of incarnation names (snapshot of set from run start, ADR-043). Empty array /
// invalid JSON — error (Voyage without targets — programmer error in S5 handler,
// Insert requires non-empty target_resolved). Duplicates and empty strings
// rejected (UNIQUE PK voyage_targets and invalid target_id).
func parseIncarnationTargets(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("voyageorch: empty target_resolved")
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("voyageorch: target_resolved (kind=scenario) is not a JSON array of names: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("voyageorch: target_resolved is empty (no incarnations)")
	}
	seen := make(map[string]struct{}, len(names))
	for i, n := range names {
		if n == "" {
			return nil, fmt.Errorf("voyageorch: target_resolved[%d] has empty incarnation name", i)
		}
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("voyageorch: target_resolved contains duplicate incarnation %q", n)
		}
		seen[n] = struct{}{}
	}
	return names, nil
}

// chunkIncarnations splits flat incarnation list into sequential Legs
// of size at most batchSize. batchSize <= 0 → one Leg for all (NULL/0
// batch_size = "entire run in one Leg", ADR-043). batch_index 0-based (CHECK
// voyage_targets_batch_index_non_negative; first Leg = 0, parity to Insert
// targets). Empty input → empty result (caller finalizes succeeded).
func chunkIncarnations(names []string, batchSize int) [][]string {
	if len(names) == 0 {
		return nil
	}
	if batchSize <= 0 {
		return [][]string{append([]string(nil), names...)}
	}
	legs := make([][]string, 0, (len(names)+batchSize-1)/batchSize)
	for off := 0; off < len(names); off += batchSize {
		end := off + batchSize
		if end > len(names) {
			end = len(names)
		}
		legs = append(legs, append([]string(nil), names[off:end]...))
	}
	return legs
}

// targetResult — runtime outcome of one incarnation in Leg, collected in per-target
// goroutine and merged under mutex. ApplyID empty if spawn failed before run start
// (then Status=failed without back-link).
type targetResult struct {
	IncarnationName string
	ApplyID         string
	Outcome         TargetOutcome
}

// executeScenarioVoyage — S2 executor for kind=scenario: batch run of scenario
// over N incarnations (B1). Called from [VoyageWorker.executeVoyage] after
// claim; renewal-goroutine already holds lease (leaseLost closed on loss).
//
// Returns final [voyage.Status] + [*voyage.Summary] + error_code for
// Finalize/finalize-audit. error_code non-empty only for fail-closed paths before
// incarnation start (spawner_not_configured / empty_scenario_name /
// target_resolve_failed); empty for happy-path and "all incarnations failed". On
// lease loss / ctx.Done mid-run returns ("", nil, "") — caller does NOT
// finalize (Reaper-reclaim returns Voyage to pending, other Keeper picks it up).
func (w *VoyageWorker) executeScenarioVoyage(ctx context.Context, run *voyage.Voyage, leaseLost <-chan struct{}) (voyage.Status, *voyage.Summary, string) {
	if w.ScenarioSpawner == nil || w.ScenarioAwaiter == nil {
		// Production wire-up (S5) must provide both; their absence with
		// claimed scenario-Voyage — programmer error in setup. Fail-closed:
		// finalize failed, not silently succeeded.
		w.Logger.Error("voyageorch: scenario execution requested but Spawner/Awaiter not configured",
			slog.String("voyage_id", run.VoyageID),
		)
		return voyage.StatusFailed, &voyage.Summary{Total: run.TotalBatches}, "spawner_not_configured"
	}
	if run.ScenarioName == nil || *run.ScenarioName == "" {
		w.Logger.Error("voyageorch: kind=scenario without scenario_name", slog.String("voyage_id", run.VoyageID))
		return voyage.StatusFailed, &voyage.Summary{}, "empty_scenario_name"
	}

	names, err := parseIncarnationTargets(run.TargetResolved)
	if err != nil {
		w.Logger.Error("voyageorch: parse target_resolved failed",
			slog.String("voyage_id", run.VoyageID), slog.Any("error", err))
		return voyage.StatusFailed, &voyage.Summary{}, "target_resolve_failed"
	}

	concurrency := 1
	if run.Concurrency != nil && *run.Concurrency > 0 {
		concurrency = *run.Concurrency
	}

	// failThreshold — generalized abort-gate (ADR-043 amendment §3): threshold of
	// absolute number of failures → stop. abort ≡ 1 (backcompat: first failure →
	// stop); continue/nil without fail_threshold ≡ 0 (no threshold); explicit N → N.
	// 0 = "no threshold".
	failThreshold := voyage.ResolveFailThreshold(run.FailThreshold, run.OnFailure)

	// batch_mode=window → sliding window across INCARNATIONS (ADR-043 amendment §1,
	// S-W2): shared pool of concurrency workers from single incarnation queue, without
	// Legs/barriers BETWEEN incarnations. §7-invariant: window splits only between
	// incarnations — INSIDE incarnation scenario-runner preserves cross-host barrier
	// + per-incarnation state-commit (unit of window = whole scenario-run of one
	// incarnation). barrier (NULL) → existing chunk+runLeg path below.
	if voyage.ResolveBatchMode(run.BatchMode) == voyage.BatchModeWindow {
		return w.runScenarioSlidingWindow(ctx, run, names, concurrency, failThreshold, leaseLost)
	}

	batchSize := 0
	if run.BatchSize != nil {
		batchSize = *run.BatchSize
	}
	legs := chunkIncarnations(names, batchSize)

	var (
		summary    = &voyage.Summary{Total: len(names)}
		anyFailure bool
	)

	for legIdx, leg := range legs {
		// Early detection of lease loss / cancellation — before spawning new Leg
		// (otherwise MarkTarget*/Finalize will hit ownership-guard).
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost between Legs — other Keeper will pick up Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			w.emitLeaseLost(run, "leg")
			return "", nil, ""
		case <-ctx.Done():
			w.Logger.Info("voyageorch: scenario-loop interrupted by ctx.Done",
				slog.String("voyage_id", run.VoyageID), slog.Any("reason", ctx.Err()))
			return "", nil, ""
		default:
		}

		// Pause before each Leg except first (inter_batch_interval —
		// controlled rollout, ADR-043). Interrupted by ctx.Done / leaseLost.
		if legIdx > 0 && run.InterBatchInterval != nil && *run.InterBatchInterval > 0 {
			if !w.interBatchPause(ctx, *run.InterBatchInterval, leaseLost) {
				w.Logger.Warn("voyageorch: interrupted on inter_batch_interval — not finalizing",
					slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
				return "", nil, ""
			}
		}

		w.emitLegStarted(run, legIdx, len(leg))

		results := w.runLeg(ctx, run, legIdx, leg, concurrency, leaseLost)

		// Lease lost MID-Leg: runLeg stopped spawn-loop and interrupted
		// in-flight Await-s via cancelled fanCtx, remaining incarnations marked
		// cancelled. NOT finalizing (Reaper-reclaim returns Voyage to pending,
		// other Keeper picks it up), same as lease loss between Legs.
		select {
		case <-leaseLost:
			w.Logger.Warn("voyageorch: lease lost mid-Leg — other Keeper will pick up Voyage",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx))
			w.emitLeaseLost(run, "leg")
			return "", nil, ""
		default:
		}

		var legAgg legOutcome
		for _, res := range results {
			legAgg.total++
			switch res.Outcome {
			case OutcomeSucceeded, OutcomeNoMatch:
				summary.Succeeded++
				legAgg.succeeded++
			case OutcomeCancelled:
				summary.Cancelled++
				legAgg.cancelled++
			default:
				summary.Failed++
				legAgg.failed++
			}
			if res.Outcome == OutcomeNoMatch {
				summary.NoMatch++
			}
			if res.Outcome.isFailure() {
				anyFailure = true
			}
		}

		w.emitLegCompleted(run, legIdx, legAgg)

		// Batch progress for UI: Leg legIdx COMPLETED → current_batch_index =
		// legIdx+1 (best-effort, ownership-guarded). Error/0-rows doesn't fail run
		// (truth about progress — in voyage_targets), only warn.
		w.advanceBatchProgress(ctx, run, legIdx+1)

		// Generalized abort-gate (ADR-043 amendment §3): reached failure count threshold
		// → stop transition to next Leg. threshold=0 → no threshold
		// (continue). summary.Failed — cumulative failures across all Legs.
		if failThreshold > 0 && summary.Failed >= failThreshold {
			w.Logger.Info("voyageorch: fail_threshold reached — remaining Legs skipped",
				slog.String("voyage_id", run.VoyageID), slog.Int("batch_index", legIdx),
				slog.Int("failed", summary.Failed), slog.Int("fail_threshold", failThreshold))
			break
		}
	}

	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// runLeg executes one Leg: parallel fan-out across incarnations under
// semaphore-cap (concurrency). Per-incarnation: spawn → MarkTargetRunning →
// Await → MarkTargetTerminal. Returns slice of outcomes (for Summary aggregation +
// decision-gate).
//
// leaseLost (parity errandrunorch.runFanOut): renewal-goroutine can lose
// lease MID-long serial Leg (batch_size=NULL → one Leg=all N
// incarnations, concurrency=1). Then spawn must stop — Voyage could have been
// reclaimed by other Keeper (runaway-spawn + duplicate spawns). So
// derived fanCtx cancelled by goroutine-watcher on leaseLost close:
//   - spawn-loop stops (acquire-select catches fanCtx.Done);
//   - remaining incarnations marked cancelled (reporting "did not make it");
//   - in-flight Await-s interrupted via cancelled fanCtx.
//
// We do NOT finalize on lease loss — caller decides that
// (executeScenarioVoyage checks leaseLost after runLeg, Reaper-reclaim).
func (w *VoyageWorker) runLeg(ctx context.Context, run *voyage.Voyage, batchIndex int, leg []string, concurrency int, leaseLost <-chan struct{}) []targetResult {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// goroutine-watcher for leaseLost: on signal cancels fanCtx to
	// stop spawn-loop before start of remaining incarnations. On happy-path
	// (wg.Wait → defer cancelFan) exits via fanCtx.Done.
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
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]targetResult, 0, len(leg))
	)

	// markRemainingCancelled — remaining incarnation does not start (fanCtx cancelled:
	// lease lost / ctx.Done). Mark cancelled (reporting "did not make it");
	// MarkTargetTerminal under parent ctx so write succeeds even on
	// graceful-shutdown cancellation of fanCtx.
	markRemainingCancelled := func(name string) {
		mu.Lock()
		results = append(results, targetResult{IncarnationName: name, Outcome: OutcomeCancelled})
		mu.Unlock()
		_ = w.markTargetCancelled(ctx, run.VoyageID, name)
	}

	for _, name := range leg {
		// Priority pre-check for cancellation: when semaphore freed by completed
		// incarnation and fanCtx cancelled simultaneously, `select` below might pick
		// acquire branch randomly (Go semantics of ready cases) and spawn extra
		// incarnation after lease loss. Explicit check makes stop deterministic.
		select {
		case <-fanCtx.Done():
			markRemainingCancelled(name)
			continue
		default:
		}

		select {
		case sem <- struct{}{}:
		case <-fanCtx.Done():
			markRemainingCancelled(name)
			continue
		}

		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()

			res := w.runOneIncarnation(fanCtx, run, name)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(name)
	}

	wg.Wait()
	cancelFan() // happy-path: wake watcher via fanCtx.Done.
	<-watcherDone
	_ = batchIndex // batch_index already fixed in voyage_targets on Insert (S5); here only track status.
	return results
}

// runScenarioSlidingWindow — S-W2 executor for batch_mode=window of kind=scenario
// (ADR-043 amendment §1). Full sliding window across INCARNATIONS: pool of
// `concurrency` workers pull incarnation names from ONE shared run queue;
// worker returns → picks next. Always maintains ≤ concurrency active
// scenario-run-s. No Legs / barriers BETWEEN incarnations — run flat
// (batch_index=0 for all targets fixed on Insert, ADR-043 amendment §7).
//
// §7-invariant (CRITICAL): window splits only BETWEEN incarnations. INSIDE
// incarnation scenario-runner preserves cross-host barrier + per-incarnation
// state-commit — unit of window = whole scenario-run of one incarnation, which
// SpawnScenarioRun starts and Await waits for terminal. scenario-runner untouched:
// voyageorch only orchestrates N independent scenario-run-s.
//
// Lease-fencing per incarnation (parity command runOneCommand, S-med-2):
// before spawning EACH incarnation [runOneIncarnationFenced] does
// VerifyOwnership — we still own Voyage with my claim-epoch (run.Attempt).
// On lease loss (Reaper-reclaim) do NOT spawn scenario-run (else duplicate), host
// marked cancelled, fenceLost raised and cancelFan stops
// window. barrier-path of scenario (runLeg/runOneIncarnation) untouched.
//
// failThreshold / inter_unit_interval / window cancellation / queue drain — parity
// command.runSlidingWindow. require_alive for scenario NOT applied (unit =
// incarnation, presence-filter meaningful for hosts; field stored, not applied).
//
// Return symmetric to [executeScenarioVoyage]: ("", nil, "") on lease loss /
// ctx.Done mid-window (caller does not finalize — Reaper-reclaim); else
// final status + summary.
func (w *VoyageWorker) runScenarioSlidingWindow(
	ctx context.Context, run *voyage.Voyage, names []string, concurrency int,
	failThreshold int, leaseLost <-chan struct{},
) (voyage.Status, *voyage.Summary, string) {
	fanCtx, cancelFan := context.WithCancel(ctx)
	defer cancelFan()

	// watcher: lease loss (renew-tick) cancels fanCtx → workers stop pulling
	// queue and in-flight Await-s interrupted. On happy-path exits via fanCtx.Done.
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-leaseLost:
			cancelFan()
		case <-fanCtx.Done():
		}
	}()

	// queue — single incarnation queue for window; buffer for all to fill without
	// blocking, then workers pull from it (returned → next).
	queue := make(chan string, len(names))
	for _, name := range names {
		queue <- name
	}
	close(queue)

	var (
		mu        sync.Mutex
		results   = make([]targetResult, 0, len(names))
		wg        sync.WaitGroup
		fenceLost atomic.Bool
		failCount atomic.Int64 // cumulative window failures (generalized abort-gate §3).
	)

	appendResult := func(res targetResult) {
		mu.Lock()
		results = append(results, res)
		mu.Unlock()
	}

	// interUnit — per-unit pause throttle (ADR-043 amendment §4). 0 → no pause.
	var interUnit time.Duration
	if run.InterUnitInterval != nil && *run.InterUnitInterval > 0 {
		interUnit = *run.InterUnitInterval
	}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// Priority pre-check for cancellation: when queue-recv and fanCtx.Done
				// both ready simultaneously Go would pick branch randomly and spawn
				// extra scenario-run after lease loss / abort. Explicit check makes
				// stop deterministic (parity runLeg).
				select {
				case <-fanCtx.Done():
					return
				default:
				}

				var (
					name string
					ok   bool
				)
				select {
				case <-fanCtx.Done():
					return
				case name, ok = <-queue:
					if !ok {
						return // queue exhausted
					}
				}

				// inter_unit_interval (§4): per-unit throttle before unit spawn.
				// Interrupted by fanCtx (lease lost / abort / ctx.Done) — then incarnation
				// already off queue, mark cancelled (did not make it to start).
				if interUnit > 0 {
					t := time.NewTimer(interUnit)
					select {
					case <-t.C:
					case <-fanCtx.Done():
						t.Stop()
						appendResult(w.cancelIncarnation(ctx, run.VoyageID, name))
						return
					}
				}

				res := w.runOneIncarnationFenced(fanCtx, run, name, cancelFan, &fenceLost)
				appendResult(res)

				// Generalized abort-gate (§3): failure threshold reached → stop
				// SPAWNing new from queue (cancelFan stops pull by all
				// workers); current active finish their scenario-run.
				// threshold=0 → no threshold. fenceLost-path already pulled cancelFan
				// inside runOneIncarnationFenced — here duplicated safely.
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
	cancelFan() // happy-path: wake watcher via fanCtx.Done.
	<-watcherDone

	// Lease loss mid-window → NOT finalize (Reaper-reclaim). Two detectors,
	// like in barrier-path: renewLoop-channel leaseLost AND fenceLost (VerifyOwnership-CAS
	// caught reclaim before renew-tick). Before marking cancelled rest of queue:
	// on reclaim owner changed, we do not write to its voyage_targets.
	select {
	case <-leaseLost:
		w.Logger.Warn("voyageorch: lease lost mid-scenario-window — other Keeper will pick up Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	default:
	}
	if fenceLost.Load() {
		w.Logger.Warn("voyageorch: ownership-fence lost mid-scenario-window — other Keeper will pick up Voyage",
			slog.String("voyage_id", run.VoyageID))
		return "", nil, ""
	}

	// abort could stop window before queue exhaustion (cancelFan stopped pulling):
	// incarnations remaining in queue buffer nobody pulled. Mark cancelled —
	// like barrier-path (markRemainingCancelled), so Total = succeeded+failed+
	// cancelled matches in both modes. Workers already done (wg.Wait), drain of
	// closed-buffer safe; on happy-path queue empty — loop no-op.
	for name := range queue {
		results = append(results, w.cancelIncarnation(ctx, run.VoyageID, name))
	}

	summary, anyFailure := summarizeIncarnations(results, len(names))
	return scenarioFinalStatus(summary, anyFailure), summary, ""
}

// summarizeIncarnations aggregates incarnation outcomes to [voyage.Summary] (Total =
// full scope of run, passed separately — results may be shorter on
// abort before drain). Also returns anyFailure for final status selection.
// Used only by window-framework; barrier-path aggregates inline (legAgg).
func summarizeIncarnations(results []targetResult, total int) (*voyage.Summary, bool) {
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

// cancelIncarnation marks non-started incarnation cancelled (abort stopped
// pull from queue of window / inter_unit interrupted by cancellation): writes terminal to
// voyage_targets (best-effort, under parent ctx) and returns [targetResult].
// Parity command.cancelRemaining; wrapper over markTargetCancelled to return
// targetResult in single call.
func (w *VoyageWorker) cancelIncarnation(ctx context.Context, voyageID, name string) targetResult {
	_ = w.markTargetCancelled(ctx, voyageID, name)
	return targetResult{IncarnationName: name, Outcome: OutcomeCancelled}
}

// runOneIncarnationFenced — window variant of [runOneIncarnation] with lease-fencing
// before spawn (parity command.runOneCommand, S-med-2). barrier-path uses
// runOneIncarnation WITHOUT fencing (there lease loss caught by acquire-select in runLeg on
// fanCtx); window-worker pulls queue directly, so fencing needed per-unit.
//
// VerifyOwnership: worker still owns Voyage with my claim-epoch
// (run.Attempt). If lease lost (Reaper-reclaim, attempt++) — do NOT spawn scenario-run
// (else duplicate execution), incarnation cancelled, fenceLost=true, and
// cancelFan stops window. Transient PG error (not ErrLeaseLost) —
// unconfirmed reclaim: fail-closed per incarnation (no spawn, failed),
// without abort of whole window.
func (w *VoyageWorker) runOneIncarnationFenced(ctx context.Context, run *voyage.Voyage, name string, cancelFan context.CancelFunc, fenceLost *atomic.Bool) targetResult {
	if err := voyage.VerifyOwnership(ctx, w.Pool, run.VoyageID, w.KID, run.Attempt); err != nil {
		if errors.Is(err, voyage.ErrLeaseLost) {
			w.Logger.Warn("voyageorch: ownership-fence lost before spawn scenario-run — not sent",
				slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
				slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
			fenceLost.Store(true)
			cancelFan()
			w.trackTerminal(ctx, run.VoyageID, name, OutcomeCancelled)
			return targetResult{IncarnationName: name, Outcome: OutcomeCancelled}
		}
		w.Logger.Warn("voyageorch: ownership-fence check failed (transient) — scenario-run not sent",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", err))
		w.trackTerminal(ctx, run.VoyageID, name, OutcomeFailed)
		return targetResult{IncarnationName: name, Outcome: OutcomeFailed}
	}
	return w.runOneIncarnation(ctx, run, name)
}

// runOneIncarnation spawns per-incarnation scenario-run and waits for its terminal,
// updating voyage_targets. Tracking errors (MarkTarget*) logged but do not fail
// incarnation run — authority of outcome = Await (actual status in apply_runs).
func (w *VoyageWorker) runOneIncarnation(ctx context.Context, run *voyage.Voyage, name string) targetResult {
	// Recovery seam (ADR-027(k)): if incarnation has MY orphaned
	// applying-lock from previous attempt of this Voyage (scenario-run of dead
	// previous owner did not reach state-commit), release it FENCED before
	// re-run — else lockRun rejects spawn ("incarnation already applying") and Voyage
	// hangs forever. CRUD failure in reconciliation → fail-closed per incarnation (no
	// spawn, failed), without abort of entire Voyage.
	if rerr := w.reconcileOrphanLock(ctx, run, name); rerr != nil {
		w.Logger.Warn("voyageorch: reconciliation of orphaned applying-lock failed — scenario-run not sent",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", rerr))
		w.trackTerminal(ctx, run.VoyageID, name, OutcomeFailed)
		return targetResult{IncarnationName: name, Outcome: OutcomeFailed}
	}

	applyID, err := w.ScenarioSpawner.SpawnScenarioRun(ctx, run.VoyageID, name, *run.ScenarioName, run.Input, run.StartedByAID, run.CadenceID)
	if err != nil {
		w.Logger.Warn("voyageorch: spawn scenario-run failed",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", err))
		w.trackTerminal(ctx, run.VoyageID, name, OutcomeFailed)
		return targetResult{IncarnationName: name, Outcome: OutcomeFailed}
	}

	if merr := voyage.MarkTargetRunning(ctx, w.Pool, run.VoyageID, voyage.TargetKindIncarnation, name, applyID, run.Attempt); merr != nil {
		w.Logger.Warn("voyageorch: MarkTargetRunning failed (best-effort)",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name), slog.Any("error", merr))
	}

	outcome, awerr := w.ScenarioAwaiter.Await(ctx, applyID)
	if awerr != nil {
		// ctx.Done / await error: scenario-run will reach terminal via
		// apply_runs anyway, but we did not wait. cancelled on ctx cancellation (abort/
		// shutdown), else failed (not silently success).
		if errors.Is(awerr, context.Canceled) || errors.Is(awerr, context.DeadlineExceeded) {
			outcome = OutcomeCancelled
		} else {
			w.Logger.Warn("voyageorch: await incarnation terminal failed",
				slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
				slog.String("apply_id", applyID), slog.Any("error", awerr))
			outcome = OutcomeFailed
		}
	}

	w.trackTerminal(ctx, run.VoyageID, name, outcome)
	return targetResult{IncarnationName: name, ApplyID: applyID, Outcome: outcome}
}

// reconcileOrphanLock — recovery seam ADR-027(k): detect + FENCED release of
// orphaned applying-lock of incarnation before re-spawn of scenario-run
// of reclaimed Voyage. Returns error ONLY on PG CRUD failure (caller →
// fail-closed per incarnation); "nothing to release" / "released" — both nil.
//
// THREE FENCING conditions (all required before release, else double apply on
// live):
//
//  1. DETECT + apply_id-match: voyage_targets of this voyage_id contains row of
//     incarnation with recorded back-link apply_id and NON-terminal status
//     (running/awaiting). This apply_id — orphan of previous attempt BY CONSTRUCTION:
//     MarkTargetRunning writes back-link AFTER spawn under CAS `v.attempt=$attempt`,
//     and we NOW BEFORE spawn of our attempt — so recorded apply_id is not ours,
//     it from previous pass (attempt < run.Attempt). Final binding apply_id
//     ↔ incarnation checked by incarnation.ReleaseApplyingOrphan (apply_runs-EXISTS).
//  2. reclaimed-attempt + self-ownership: VerifyOwnership(voyage_id, KID,
//     run.Attempt) — we current owner of Voyage with claim-epoch run.Attempt. If
//     we ourselves reclaimed (ErrLeaseLost) — NOT touch lock (actual
//     new owner will release it); transient PG error — fail-closed. The fact itself that we
//     at this point as OWNER run.Attempt and back-link already from foreign
//     apply_id — confirms re-claim (previous owner lost Voyage, its
//     RunResult fenced at apply_runs.attempt level, ADR-027(g)).
//  3. single-winner CAS: release atomic via incarnation.ReleaseApplyingOrphan
//     (FOR UPDATE + guard status='applying'). If honest RunResult of previous
//     owner already finalized incarnation — release no-op (released=false).
func (w *VoyageWorker) reconcileOrphanLock(ctx context.Context, run *voyage.Voyage, name string) error {
	if w.OrphanReleaser == nil {
		return nil // detection disabled (unit / dev without recovery seam)
	}

	// DETECT: back-link orphan apply_id from voyage_targets of this Voyage. scope small
	// (N incarnations of one Voyage) — full SelectTargets + filter cheaper than narrow
	// selector. No row / no apply_id / target already terminal → nothing to release.
	targets, err := voyage.SelectTargets(ctx, w.Pool, run.VoyageID)
	if err != nil {
		return fmt.Errorf("voyageorch: select targets for orphan-detect: %w", err)
	}
	orphanApplyID := ""
	for i := range targets {
		t := &targets[i]
		if t.TargetKind != voyage.TargetKindIncarnation || t.TargetID != name {
			continue
		}
		// Only running/awaiting with recorded back-link — orphan candidate.
		// Terminal target (succeeded/failed/cancelled/no_match) already reached —
		// incarnation not hanging. ApplyID==nil — spawn of previous attempt did not reach
		// MarkTargetRunning, lock not set by this Voyage → not our orphan.
		if t.ApplyID != nil && *t.ApplyID != "" &&
			(t.Status == voyage.TargetStatusRunning || t.Status == voyage.TargetStatusAwaiting) {
			orphanApplyID = *t.ApplyID
		}
		break
	}
	if orphanApplyID == "" {
		return nil // no back-link from previous attempt — nothing to release
	}

	// FENCING-2/3 + single-winner release. ReleaseOrphanLock first
	// VerifyOwnership (we owner run.Attempt; ErrLeaseLost → released=false,
	// err=nil — we reclaimed, lock not touched), then atomic CAS release.
	released, rerr := w.OrphanReleaser.ReleaseOrphanLock(ctx, run.VoyageID, name, run.Attempt, w.KID, orphanApplyID)
	if rerr != nil {
		return rerr
	}
	if released {
		w.Logger.Info("voyageorch: orphaned applying-lock released before re-run (recovery seam ADR-027(k))",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
			slog.String("orphan_apply_id", orphanApplyID),
			slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
	} else {
		w.Logger.Info("voyageorch: orphaned applying-lock NOT released (fenced no-op: not applying / orphan apply_id not ours / we reclaimed)",
			slog.String("voyage_id", run.VoyageID), slog.String("incarnation", name),
			slog.String("orphan_apply_id", orphanApplyID),
			slog.String("kid", w.KID), slog.Int("attempt", run.Attempt))
	}
	return nil
}

// trackTerminal best-effort records target terminal in voyage_targets. PG
// error logged as warn — outcome already fixed in targetResult (authority for finalize).
func (w *VoyageWorker) trackTerminal(ctx context.Context, voyageID, name string, outcome TargetOutcome) {
	if err := voyage.MarkTargetTerminal(ctx, w.Pool, voyageID, voyage.TargetKindIncarnation, name, outcome.toTargetStatus()); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetTerminal failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("incarnation", name),
			slog.String("outcome", string(outcome)), slog.Any("error", err))
	}
}

// markTargetCancelled best-effort marks non-started target cancelled
// (ctx.Done during Leg distribution). PG error logged as warn.
func (w *VoyageWorker) markTargetCancelled(ctx context.Context, voyageID, name string) error {
	if err := voyage.MarkTargetTerminal(ctx, w.Pool, voyageID, voyage.TargetKindIncarnation, name, voyage.TargetStatusCancelled); err != nil {
		w.Logger.Warn("voyageorch: MarkTargetTerminal(cancelled) failed (best-effort)",
			slog.String("voyage_id", voyageID), slog.String("incarnation", name), slog.Any("error", err))
		return err
	}
	return nil
}

// interBatchPause waits duration between Legs. Returns false on interruption
// (ctx.Done / leaseLost) — caller then does not finalize.
func (w *VoyageWorker) interBatchPause(ctx context.Context, d time.Duration, leaseLost <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-leaseLost:
		return false
	}
}

// scenarioFinalStatus selects terminal status of Voyage by Summary:
//   - all success (or no_match)         → succeeded;
//   - at least one fail but success too → partial_failed;
//   - all failed (nobody success)       → failed.
//
// cancelled-only (abort before any success) treated as failed —
// failure occurred that triggered abort.
func scenarioFinalStatus(s *voyage.Summary, anyFailure bool) voyage.Status {
	if !anyFailure && s.Cancelled == 0 {
		return voyage.StatusSucceeded
	}
	if s.Succeeded == 0 {
		return voyage.StatusFailed
	}
	return voyage.StatusPartialFailed
}
