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

// dispatchPassage runs cross-host fan-out for ONE Passage (ADR-056 §b.2-3) and
// waits its barrier (§b.3). tasks/plans are the subset of a single Passage
// (run.go's stage-loop filtered by RenderedTask.Passage); passage is stamped
// into ApplyRequest.passage + apply_runs(apply_id, sid, passage) for
// per-Passage terminal correlation — the barrier waits only on this Passage's
// rows.
//
// Dispatch model: one apply_id per run, one sid per host → one ApplyRequest per
// host per Passage, carrying all the Passage's tasks that target it (after
// on/where/run_once resolution, from DispatchPlan). Matches the composite PK
// (apply_id, sid, passage) on apply_runs.
//
// TWO BRANCHES carry that request (NIM-880). An agent host gets it over the
// gRPC EventStream and answers asynchronously with a RunResult; a
// `transport: ssh` host gets it over an SSH session the Keeper opens itself,
// and its goroutine writes the terminal when the run on the host is done.
// Everything downstream of the send is shared: one `apply_runs` row, one
// [applysink.Sink], one barrier. hosts is the run's roster — the host's own
// registry transport is what picks the branch, see [resolveHostDispatch].
//
// `serial:` (orchestration.md §2.2.1): rolling execution in waves of hosts
// sorted by SID, size ≤width; within a wave hosts dispatch in parallel, waves
// are strictly sequential (per-wave barrier). Since each host gets one
// ApplyRequest, a wave is a subset of the run's hosts, not a repeat send to one
// host; per-task serial widths aggregate into one wave width = minimum positive
// SerialWidth across the Passage's tasks (narrowest window, fail-closed — see
// effectiveSerialWidth). 0 → all hosts in one wave.
//
// Serial waves operate within EACH Passage independently: the serial (hosts)
// and Passage (tasks) axes are orthogonal (ADR-056 §S4 amend, 2D
// serial×passage). A probe Passage without serial runs as one wave even if a
// later Passage carries serial:N. N=1 runs call this with passage=0 — the
// single wave(s) of one Passage, bit-for-bit.
//
// Fail-stop (§2.2.1): the first failed/cancelled host in a wave stops rolling —
// later waves don't start, dispatch returns an error.
//
// Barrier invariant (§7): serial never splits the state commit — dispatch
// returns only after all waves complete (or fail-stop); state commits once in
// run(), never per-wave.
func (r *Runner) dispatchPassage(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, tasks []*render.RenderedTask, plans []render.DispatchPlan, gate *crossPassageGate, hosts []*topology.HostFacts) error {
	perHost := groupByHost(tasks, plans)
	// Cross-passage requisite gating (ADR-056 R3): per-host resolution of
	// onchanges/onfail links whose source is in an earlier Passage. nil gate
	// (N=1 / Passage 0) → no-op. Applied BEFORE dispatch: a consumer whose
	// cross-passage onchanges didn't fire on a host (and has no same-passage
	// source) is dropped from that host's slice; otherwise cross-passage
	// indexes are stripped from the wire (Soul gates by same-passage only).
	perHost = gate.applyGate(perHost, passage)
	if len(perHost) == 0 {
		// No Passage task targets any host (where: filtered out all hosts on
		// every task). Not an error: nothing to apply in this Passage; for
		// N=1 the run succeeds as a no-op.
		log.Info("scenario: dispatch - no Passage task targets hosts, no-op",
			slog.Int("passage", passage))
		return nil
	}

	// Per-host transport, resolved BEFORE any send: a contradiction between the
	// task's `transport:` and the host's registry row must stop the run, not
	// surface as a connect error halfway through a wave.
	dispatchByHost, refusals := resolveHostDispatch(perHost, plans, hosts)
	if len(refusals) > 0 {
		return refusedDispatchError(refusals)
	}
	// Every push host's SshProvider, resolved before this Passage's first wave —
	// a routing miss must not surface at a host's turn, with the hosts ahead of it
	// already applied. See [Runner.resolvePushRoutes] for why the provider is a
	// per-Passage question while "is push wired at all" is settled once per run.
	pushRoutes, routeRefusals := r.resolvePushRoutes(ctx, dispatchByHost)
	if len(routeRefusals) > 0 {
		return refusedDispatchError(routeRefusals)
	}

	sids := sortedSIDs(perHost)
	waves := splitWaves(sids, effectiveSerialWidth(plans))

	// Waves are strictly sequential (§2.2.1). dispatchedTotal accumulates
	// hosts from all waves started so far — the barrier waits on exactly
	// those (a host with no apply_runs row must not make the barrier wait
	// until timeout).
	dispatchedTotal := 0
	for wi, wave := range waves {
		dispatched, derr := r.dispatchWave(ctx, spec, log, passage, perHost, wave, dispatchByHost, pushRoutes)
		dispatchedTotal += dispatched

		// Per-wave barrier: wait for terminal status of hosts from ALL waves
		// started so far in this Passage. classify scans the run's
		// apply_runs rows filtered by passage, so a failed host in the
		// current wave breaks the barrier immediately (fail-stop) — the next
		// wave doesn't start.
		if berr := r.waitBarrier(ctx, spec.ApplyID, passage, dispatchedTotal, log); berr != nil {
			return berr
		}
		if derr != nil {
			return derr
		}
		if len(waves) > 1 {
			log.Info("scenario: serial wave completed",
				slog.Int("wave", wi+1), slog.Int("waves_total", len(waves)), slog.Int("hosts", len(wave)))
		}
	}
	return nil
}

// dispatchPlanned is the NEW dispatch path (ADR-027, Phase 1.4.2): instead of
// inline render + SendApply, it writes planned assignments to ALL roster
// hosts and publishes a Summons. Render + transition to dispatched +
// SendApply happens in the Acolyte at claim time ([RenderForHost] →
// MarkDispatched → SendApply, ADR-027 amend).
//
// Variant B (no where-filter): planned is written to EVERY roster host, even
// if on:/where: filters out all its tasks — the Acolyte closes such a host as
// a no-op with terminal `no_match` (FINDING-01 variant (b); not success —
// apply_runs must not over-report success on non-targeted hosts), and the
// barrier counts it as a benign terminal. This keeps wantHosts = len(roster)
// deterministic at dispatch time without a per-host on:/where: pre-resolution
// (that's the Acolyte's job). The recipe is identical for all hosts
// (ServiceRef/ScenarioName/Input WITH vault-ref as-is — invariant A, before
// ResolveInputValuesVault/StartedByAID).
//
// After all Inserts: one best-effort PublishSummons (errors are swallowed —
// planned assignments are persistent, the Acolyte poll-fallback picks them
// up). Then waitBarrier polls apply_runs.status until all inserted hosts
// reach a terminal (KEY invariant: the barrier stays in the run-goroutine in
// Phase 1). wantHosts = number of inserted planned rows.
//
// No render for SendApply happens here — the Acolyte that claims the assignment
// renders the plan itself.
func (r *Runner) dispatchPlanned(ctx context.Context, spec RunSpec, log *slog.Logger, hosts []*topology.HostFacts) error {
	if len(hosts) == 0 {
		// Host resolution upstream (run.go step 3) already rejects an empty
		// roster (no_hosts). Empty shouldn't reach here; defensive check.
		return fmt.Errorf("scenario: dispatchPlanned: empty roster for run %s", spec.ApplyID)
	}
	// The Acolyte claims an assignment and dispatches it over the EventStream —
	// it has no push branch, and a planned row for a `transport: ssh` host would
	// be claimed and then fail `soul_not_connected` (NIM-880). The refusal that
	// an operator actually hits is upstream in run.go, BEFORE the keeper-side
	// tasks provision anything; this one is the defence-in-depth copy, so a
	// future caller that reaches dispatchPlanned another way still cannot write a
	// planned row nothing will close. Giving the work-queue its own push branch
	// is a separate slice — it needs the claim, the fencing epoch and the SSH
	// session to agree on one owner, which the in-goroutine path does not have to
	// solve.
	if pushSIDs := pushHostSIDs(hosts); len(pushSIDs) > 0 {
		return fmt.Errorf("scenario: the work-queue dispatch path (keeper.acolytes>0) cannot reach transport=ssh hosts %v - run this scenario on an instance with acolytes=0, or move those hosts off the incarnation", pushSIDs)
	}

	recipe := &applyrun.Recipe{
		ServiceRef:   spec.ServiceRef,
		ScenarioName: spec.ScenarioName,
		Input:        spec.Input, // vault-ref as-is — invariant A
		StartedByAID: startedByPtr(spec.StartedByAID),
		FromUpgrade:  spec.FromUpgrade, // upgrade run loads upgrade/<slug>/ at claim (ADR-0068)
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
			Input:           spec.inputSnapshot,
		}); err != nil {
			return fmt.Errorf("scenario: insert planned apply_run (%s): %w", h.SID, err)
		}
		dispatched++
		log.Info("scenario: planned job recorded", slog.String("sid", h.SID))
	}

	// Summons is best-effort: persisted planned assignments are picked up by
	// the Acolyte poll-fallback even if the signal is lost (ADR-027(a)).
	// Errors are only logged.
	r.publishSummons(ctx, log)

	// Acolyte path is non-staged (ADR-056 §S4): planned assignments write
	// passage 0, the barrier waits on the Passage 0 slice. Staged (N>1) is
	// rejected for Acolyte in run.go.
	return r.waitBarrier(ctx, spec.ApplyID, 0, dispatched, log)
}

// publishSummons sends one best-effort Summons signal for planned assignments
// (ADR-027(a)). nil publisher (Summons disabled / unit test) → no-op. Errors
// are only logged: planned assignments are persistent and the Acolyte
// poll-fallback will pick them up — publishing only speeds up wakeup.
func (r *Runner) publishSummons(ctx context.Context, log *slog.Logger) {
	if r.deps.Summons == nil {
		return
	}
	if err := r.deps.Summons.PublishSummons(ctx); err != nil {
		log.Warn("scenario: publish Summons failed - poll-fallback will pick it up", slog.Any("error", err))
	}
}

// hasSerialTask reports whether any scenario task carries `serial:`
// (serial-guard, ADR-027 Phase 1.4.2): such a run takes the OLD path even
// with AcolyteEnabled (distributed serial is Phase 3). Checked on the PARSED
// scenario (after ExpandIncludes), without render. Task.Serial is opaque any
// (int>=1 | "<N>%"); serial: counts as set for any non-nil value, empty
// string means "not set" (the config validator rejects empty serial, but
// fail-closed: an empty value alone isn't reason to take the new path).
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

// serialPresent reports whether a task's `serial:` is set. Task.Serial is
// any: nil → unset; empty string → unset (see hasSerialTask); anything else
// (int / non-empty percent string) → set.
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

// dispatchWave starts one wave: Insert(running) + send for each host in the
// wave (parallel within a wave per §2.2.1 semantics; the stream branch sends
// sequentially but without a barrier between hosts of the same wave — the
// barrier sits between waves). Returns the number of hosts that now have a
// running `apply_runs` row the barrier will wait on, and the first Insert/send
// error, if any.
//
// On a stream send failure the host is marked failed immediately — no RunResult
// will arrive for it, otherwise the per-wave barrier would hang until timeout;
// the failed row breaks the barrier normally (fail-stop).
//
// A push host (NIM-880) counts as dispatched the moment its goroutine starts:
// its send IS the run, so there is no send outcome to inspect here, and every
// exit of [Runner.dispatchPushHost] writes a terminal the barrier observes —
// which is the same contract the stream branch has with an arriving RunResult.
func (r *Runner) dispatchWave(ctx context.Context, spec RunSpec, log *slog.Logger, passage int, perHost map[string][]*render.RenderedTask, wave []string, dispatchByHost map[string]hostDispatch, pushRoutes map[string]pushRoute) (int, error) {
	dispatched := 0
	for _, sid := range wave {
		// attempt is INTENTIONALLY left at 0: this is the old inline path
		// without Acolyte-claim or a recovery scan (ADR-027 Phase 0 /
		// serial-guard) — no Ward claim/reclaim, so the fencing epoch is
		// moot here. attempt=0 on the wire means "old Keeper, no fencing"
		// (apply.proto field 4); the Soul guard doesn't reject such a
		// request. Not a bug — the inline path has no stale-duplicate source
		// for fencing to guard against.
		req := &keeperv1.ApplyRequest{
			ApplyId: spec.ApplyID,
			// ToProtoTasksForHost(sid): for self-variant core.file.rendered,
			// substitutes THIS host's per-host render_context (not the first
			// by SID) — partially closes open Q #25 (render_context.self).
			Tasks: render.ToProtoTasksForHost(perHost[sid], sid),
			// passage (ADR-056): Soul echoes it in TaskEvent/RunResult —
			// Keeper correlates the terminal and accumulates register per
			// (apply_id, sid, passage).
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
			Input:           spec.inputSnapshot,
			// Engine provenance (ADR-0076(l)). This path renders inline in the run
			// goroutine, so this instance IS the renderer; the soul version is the
			// one this host announced on the connection the apply is about to
			// travel — the same announcement its capability set came from.
			//
			// A push host announced nothing and never will (NIM-880): it holds no
			// heartbeat entry, so asking costs a Redis round-trip per host per
			// Passage to be told "not recorded", and a transient Redis failure
			// would log a Warn about a lookup that is meaningless on this branch.
			// The field stays empty, which is the honest answer — what runs there
			// is the binary this Keeper delivers, named by a keeper.yml key.
			KeeperVersion: r.deps.KeeperVersion,
			SoulVersion:   r.dispatchSoulVersion(ctx, sid, dispatchByHost[sid], log),
		}); err != nil {
			return dispatched, fmt.Errorf("scenario: insert apply_run (%s): %w", sid, err)
		}

		// Push branch (NIM-880): the send IS the whole run, so it goes to its own
		// goroutine and the barrier — already waiting on the row inserted above —
		// observes the terminal that goroutine writes. Failing to START it is the
		// one case handled inline: without a goroutine nothing writes a terminal,
		// and the barrier would wait out the run timeout.
		if hd := dispatchByHost[sid]; hd.isPush() {
			if err := r.startPushHost(ctx, spec, log, passage, sid, hd, pushRoutes[sid], req, len(perHost[sid])); err != nil {
				return dispatched, err
			}
			dispatched++
			continue
		}

		// Multi-keeper guard (footgun with acolytes=0): this old path keeps
		// run ownership in-memory in THIS instance's run-goroutine. If the
		// target Soul's stream is held by ANOTHER Keeper instance, the
		// RunResult goes there and this barrier hangs until runTimeout →
		// incarnation stays in applying. The WARN right before SendApply
		// catches this at the point of dispatch.
		r.warnCrossKeeperDispatch(ctx, sid, log)
		if err := r.deps.Outbound.SendApply(ctx, sid, req); err != nil {
			// error_summary is exposed via barrier/status_details (GET
			// incarnation, no masking). The SendApply err carries req with
			// raw (resolved) Params and could echo a secret in a
			// transport/marshal message — the observable channel gets only a
			// safe reason, no payload echo. The full err only flows into the
			// wrapped error above (which passes through MaskSecrets in
			// lockIncarnation before being written).
			summary := "send_apply_failed"
			_ = applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, sid, passage, applyrun.StatusFailed, &summary)
			return dispatched, fmt.Errorf("scenario: send apply (%s): %w", sid, err)
		}
		dispatched++
		log.Info("scenario: ApplyRequest sent", slog.String("sid", sid), slog.Int("tasks", len(perHost[sid])))
	}
	return dispatched, nil
}

// dispatchSoulVersion is [Runner.announcedSoulVersion] with the push branch
// carved out — see the call site for why a push host has no announcement to
// read.
func (r *Runner) dispatchSoulVersion(ctx context.Context, sid string, hd hostDispatch, log *slog.Logger) string {
	if hd.isPush() {
		return ""
	}
	return r.announcedSoulVersion(ctx, sid, log)
}

// warnCrossKeeperDispatch logs a loud WARN if this (old) dispatch path sends
// an ApplyRequest to a Soul whose EventStream is held by ANOTHER Keeper
// instance. Single-keeper-only footgun of the `acolytes: 0` default: run
// ownership lives in-memory in this instance's run-goroutine, but the
// RunResult goes to the stream owner (another instance) — this barrier never
// sees completion and hangs until runTimeout, leaving the incarnation in
// applying. Not an issue with `acolytes>0` (work-queue, ADR-027: claim+
// dispatch via Redis Summons + terminal observation via shared PG) — the
// guard isn't called there.
//
// Guard is a no-op when:
//   - no LeaseOwner checker (nil Redis / unit test without coordination);
//   - this instance runs in work-queue mode (acolyteEnabled=true): reached
//     only via serial-guard, which has no cross-keeper hang (same barrier,
//     but stream ownership doesn't matter there — see § below);
//   - we own the target Soul's lease ourselves (kid matches), or there's no
//     lease key (Soul isn't on anyone's stream — a separate issue, not this
//     footgun);
//   - lease read error (best-effort: don't block dispatch, don't be noisy).
//
// § serial-guard with acolytes>0: even there, the old path holds the barrier
// in the local instance's run-goroutine, so the same cross-keeper hang is
// possible. To not stay silent in this case, the guard checks ownership
// regardless of acolyteEnabled — it's cheap (one GET) and only fires on a
// genuinely dangerous configuration (foreign stream owner).
func (r *Runner) warnCrossKeeperDispatch(ctx context.Context, sid string, log *slog.Logger) {
	if r.leaseOwner == nil {
		return
	}
	owner, ok, err := r.leaseOwner.SoulLeaseOwner(ctx, sid)
	if err != nil || !ok || owner == "" || owner == r.kid {
		return
	}
	log.Warn("scenario: footgun multi-keeper + acolytes=0 - Soul is streamed to a different Keeper instance; the run may hang in applying (for an HA cluster set keeper.acolytes>0, ADR-027)",
		slog.String("sid", sid),
		slog.String("stream_owner_kid", owner),
		slog.String("self_kid", r.kid))
}

// waitBarrier polls apply_runs.status until all wantHosts of the run reach a
// terminal status, or ctx is cancelled.
//
// Returns:
//   - nil — all hosts succeeded.
//   - error — at least one failed/cancelled, or ctx cancelled (timeout/
//     Cancel/Shutdown). Any non-success terminal fails the run (fail-closed,
//     §7).
func (r *Runner) waitBarrier(ctx context.Context, applyID string, passage, wantHosts int, log *slog.Logger) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, applyID)
		if err != nil {
			return fmt.Errorf("scenario: barrier poll: %w", err)
		}

		// Cluster-wide Cancel (G1): the cancel_requested flag may be set by
		// ANY Keeper instance (not necessarily the one running this
		// run-goroutine). Checked before classify: a requested cancel aborts
		// the run the same way as a local ctx Cancel (run() → abort →
		// error_locked), but survives cross-Keeper routing. Local Cancel
		// remains the fast path via <-ctx.Done() below.
		if cancelRequested(statuses) {
			log.Info("scenario: barrier - received cluster-wide Cancel (cancel_requested), run is being cancelled")
			return fmt.Errorf("scenario: barrier interrupted: %w", errCancelRequested)
		}

		done, failed := classify(statuses, passage, wantHosts)
		if failed != nil {
			return failed
		}
		if done {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("scenario: barrier interrupted: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// cancelRequested reports whether the cluster-wide Cancel flag (G1) is set on
// any row of the run. RequestCancel writes the flag by apply_id (on all
// running rows), but the projection carries it per host — the barrier only
// needs to see true on any row.
func cancelRequested(statuses []applyrun.HostStatus) bool {
	for i := range statuses {
		if statuses[i].CancelRequested {
			return true
		}
	}
	return false
}

// classify evaluates a run's host status slice:
//   - failed != nil — at least one failed/cancelled/orphaned (fail-closed).
//   - done == true  — all wantHosts reached a terminal and all are benign
//     (success or no_match — FINDING-01 variant (b): non-targeted host where
//     on:/where: filtered out all tasks).
//   - (false, nil)  — still running, or fewer rows than wantHosts (not all
//     Inserts landed yet / poll ran ahead).
//
// Failure reason comes from apply_runs.error_summary (filled per-task:
// `task <idx> <module>: <message>`, BUG-3). NULL error_summary (dispatch-level
// failure without a TaskEvent) → falls back to the host status itself.
func classify(statuses []applyrun.HostStatus, passage, wantHosts int) (done bool, failed error) {
	terminal := 0
	for _, hs := range statuses {
		// Staged-render (ADR-056): this Passage's barrier only counts
		// terminals of its own slice's rows. Rows from earlier Passages
		// (already success) and keeper-target rows of OTHER Passages don't
		// count — otherwise terminal would inflate and the barrier would
		// declare done prematurely. N=1 run: single Passage 0 → filter
		// passes all host rows, bit-for-bit as before staged-render.
		if hs.Passage != passage {
			continue
		}
		// keeper-target (`on: keeper`) is NOT a host terminal, even for its
		// own Passage (Slice 2: keeper tasks are stratified by Passage, its
		// row carries the real passage, not always 0). Its apply_runs row is
		// written by dispatchKeeperTasks for THIS Passage strictly BEFORE
		// this Passage's host-dispatch, and wantHosts only counts real
		// hosts. Without this exclusion, keeper-success would inflate
		// terminal by one → the barrier would declare done one host early
		// (silent success if the last host fails). keeper-FAILED never
		// reaches here: dispatchKeeperTasks for this Passage aborts the run
		// BEFORE host fan-out (return err → run.go abort), host-dispatch
		// never starts, and this Passage's barrier is never called — so
		// skipping keeper-target doesn't weaken the failed branch on any
		// Passage.
		if hs.SID == render.KeeperTargetSID {
			continue
		}
		switch hs.Status {
		case applyrun.StatusSuccess, applyrun.StatusNoMatch:
			// no_match (FINDING-01 variant (b)) is a terminal NON-failure
			// (benign, like success): host wasn't targeted (on:/where:
			// filtered out all tasks), Acolyte closed it as no_match without
			// an ApplyRequest. Counted toward terminal so the barrier
			// completes the run instead of hanging until runTimeout. A run
			// where targeted hosts succeed and non-targeted hosts get
			// no_match → done without failed → incarnation goes to ready
			// (commitSuccess), not error_locked.
			terminal++
		case applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusOrphaned:
			// orphaned (Soul reconcile, ADR-027(g)) is a terminal
			// non-success: the barrier counts it as a host failure
			// (incarnation → error_locked via commitRunState), same as
			// failed/cancelled. No RunResult will ever arrive for it —
			// without this branch the barrier would hang until runTimeout.
			return false, fmt.Errorf("scenario: host %s finished with status %s (%s)",
				hs.SID, hs.Status, failureReason(hs))
		}
	}
	return terminal >= wantHosts, nil
}

// failureReason builds the operator-facing reason for a host failure from an
// apply_runs row (BUG-3). Sources, in priority order:
//
//   - error_summary (per-task `task <idx> <module>: <message>`) — primary;
//   - the host status itself (`failed`/`cancelled`) — if there's no summary
//     (dispatch-level failure without a TaskEvent).
//
// The summary is passed through as written. The per-task `no_log:` that used to
// replace a marked task's body with neutral text is gone ([ADR-0083] §8) — its
// masking was already done on the write path (MaskSecrets in recordTaskFailure),
// and the extra suppression only cost the operator the reason the run failed,
// on exactly the tasks where a failure is most expensive to misdiagnose.
func failureReason(hs applyrun.HostStatus) string {
	if hs.ErrorSummary == nil {
		return string(hs.Status)
	}
	return *hs.ErrorSummary
}

// failedPlanIndex picks the GLOBAL plan_index of a host's failed task:
// failed_plan_index (migration 081) takes priority; falls back to the local
// task_idx (identical to global for N=1) when absent (old Soul without echoed
// plan_index, or a pre-081 run). (false, _) means no failed task was recorded.
func failedPlanIndex(hs applyrun.HostStatus) (int, bool) {
	if hs.FailedPlanIndex != nil {
		return *hs.FailedPlanIndex, true
	}
	if hs.TaskIdx != nil {
		return *hs.TaskIdx, true
	}
	return 0, false
}

// tasksForPassage selects the RenderedTasks and DispatchPlans belonging to
// Passage p (staged-render, ADR-056): one Passage's ApplyRequest carries only
// its own tasks, and the barrier waits only on its terminals. keeper-side
// tasks (Keeper-plan) are excluded — they run BEFORE host fan-out
// (dispatchKeeperTasks, passage 0). Plans are filtered by the same
// task-index set (groupByHost already ignores plans without a task in the
// set, but explicit filtering keeps the serial width Passage-accurate).
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

// groupByHost builds SID → []RenderedTask from DispatchPlans. Each task lands
// in the list of hosts it targets (TargetSIDs). Task order within a host
// follows Index (= scenario.tasks[] order).
func groupByHost(tasks []*render.RenderedTask, plans []render.DispatchPlan) map[string][]*render.RenderedTask {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}

	perHost := make(map[string][]*render.RenderedTask)
	for _, plan := range plans {
		// keeper-side tasks (`on: keeper`) run locally before host fan-out
		// (run.go::dispatchKeeperTasks) — excluded from Soul-side grouping,
		// otherwise their synthetic target (render.KeeperTargetSID) would go
		// out via SendApply as if it were a Soul.
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

// sortedSIDs returns the run's hosts sorted by SID (dispatch determinism,
// orchestration.md: lexicographic by SID).
func sortedSIDs(perHost map[string][]*render.RenderedTask) []string {
	out := make([]string, 0, len(perHost))
	for sid := range perHost {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out
}

// effectiveSerialWidth derives wave width from the per-task SerialWidth of
// the given plans (orchestration.md §2.2.1). serial: is a per-task axis, but
// the dispatch model — one ApplyRequest per host carrying all its tasks
// (composite PK apply_id,sid) — can't run different tasks in different waves
// within one request. Aggregation: wave width = the MINIMUM positive
// SerialWidth among the slice's tasks (narrowest window — fail-closed
// conservative: the narrower the wave, the fewer hosts at risk on failure).
// 0 (no task in the slice carries serial:) → all hosts in one wave (no-serial
// behavior).
//
// The slice is the tasks of ONE Passage (dispatchPassage passes
// tasksForPassage-filtered plans): ADR-056 §S4 amend made min-width
// per-Passage, not per-run. A probe Passage without serial → width 0 (one
// wave), even if another Passage carries serial:N — its narrow window
// doesn't leak into a different Passage (fixes the silent-wrong-width
// issue). For N=1 (single Passage), per-Passage and per-run coincide
// bit-for-bit.
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

// splitWaves splits an already SID-sorted host list into sequential waves of
// size ≤width (orchestration.md §2.2.1). width<=0 or width>=len(sids) → one
// wave with all hosts (serial unset / wider than the target set).
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

// The render.RenderedTask → keeperv1.RenderedTask (wire form) converter
// lives in render.ToProtoTasks (keeper/internal/render/prototask.go) —
// shared by dispatch and trial-L2 so adding a field to RenderedTask doesn't
// cause the wire form to drift across copies.
