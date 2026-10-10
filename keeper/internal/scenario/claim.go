package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/applysink"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ClaimDeps — Acolyte claim-callback dependencies (ADR-027, Phase 1.4.3).
// Carries the render pipeline (via [Deps], reused [RenderForHost]) + gRPC
// Outbound + Ward-capture params. Assembled at wire-up (1.4.4) and passed to
// [NewClaimRunner]; the pool calls [ClaimRunner.Claim] on every poll tick /
// Summons wake (acolyte.Pool.SetClaim).
type ClaimDeps struct {
	// Deps — same dependencies as Runner: Loader/Topology/ServiceVars/Render/
	// Vault/Audit/Destiny/DB + Outbound (SID-lease routing for SendApply is
	// already inside). InputDenyPaths/Logger also come from here.
	Deps Deps

	// KID — Acolyte-owner identifier (claim_by_kid, fencing epoch).
	KID string

	// Lease — Ward-capture TTL (claim_expires_at = NOW()+Lease). Recovery scan
	// (ADR-027 amend GATE-1) reclaims expired ones.
	Lease time.Duration

	// Batch — max planned jobs claimed in one tick (LIMIT of the claim query).
	// Workers across instances share the queue via FOR UPDATE SKIP LOCKED —
	// Batch only caps one tick's appetite.
	Batch int
}

// ClaimRunner executes one Acolyte claim→render→apply cycle. Stateless across
// runs (everything from PG + recipe), safe for concurrent calls from multiple
// workers: the only shared operation is [applyrun.ClaimNext] under
// FOR UPDATE SKIP LOCKED, guaranteeing one Acolyte per row.
type ClaimRunner struct {
	deps   ClaimDeps
	logger *slog.Logger
}

// NewClaimRunner assembles a ClaimRunner. Panics on nil required dependencies
// / invalid params — a wire-up programmer error (1.4.4), not a runtime
// condition.
func NewClaimRunner(deps ClaimDeps) *ClaimRunner {
	if deps.Deps.Loader == nil || deps.Deps.Topology == nil || deps.Deps.ServiceVars == nil ||
		deps.Deps.Render == nil || deps.Deps.Outbound == nil || deps.Deps.DB == nil {
		panic("scenario: NewClaimRunner: required dependency is nil")
	}
	if deps.KID == "" {
		panic("scenario: NewClaimRunner: empty KID")
	}
	if deps.Lease <= 0 || deps.Batch <= 0 {
		panic("scenario: NewClaimRunner: non-positive Lease/Batch")
	}
	return &ClaimRunner{deps: deps, logger: depsLogger(deps.Deps)}
}

// Claim — one claim-callback pass (acolyte.ClaimFunc shape): atomically claims
// a batch of planned jobs and for each does render→MarkDispatched→SendApply.
// The returned error is only a ClaimNext failure itself (PG unavailable): the
// pool logs it and keeps running. Render/send errors for individual jobs do
// NOT propagate up — they move the row to failed (the run-goroutine barrier
// counts it), otherwise it would hang until runTimeout.
func (c *ClaimRunner) Claim(ctx context.Context) error {
	// KeeperVersion goes into the claim itself (ADR-0076(l)): this instance is the
	// one that will render the claimed jobs, and stamping on claim covers every
	// outcome of that render — dispatched, no_match, or failed.
	claimed, err := applyrun.ClaimNext(ctx, c.deps.Deps.DB, c.deps.KID, c.deps.Lease, c.deps.Batch, c.deps.Deps.KeeperVersion)
	if err != nil {
		return fmt.Errorf("scenario: claim next: %w", err)
	}
	for _, run := range claimed {
		c.execute(ctx, run)
	}
	return nil
}

// execute carries one claimed job through: render for its SID → filter this
// host's tasks → (no-op no_match if on:/where: filtered everything out —
// FINDING-01 option (b)) → MarkDispatched (claimed→dispatched) STRICTLY BEFORE
// SendApply. Any render/SendApply error → failed (masked summary), so the
// run-goroutine barrier doesn't hang until runTimeout. Marking BEFORE send is
// the invariant against double apply (ADR-027 amend S3): once dispatched,
// recovery-reclaim doesn't touch the row.
//
// Invariant A (ADR-027): resolved input/vars/rendered params live only on
// the RenderForHost stack (in RAM); only the masked form reaches PG/logs/status.
func (c *ClaimRunner) execute(ctx context.Context, run *applyrun.ApplyRun) {
	log := c.logger.With(
		slog.String("apply_id", run.ApplyID),
		slog.String("sid", run.SID),
		slog.Int("passage", run.Passage),
		slog.Int("attempt", run.Attempt),
	)

	if run.Recipe == nil {
		// A planned job without a recipe can't be rendered by Acolyte (a
		// dispatch programmer error — InsertPlanned requires non-nil recipe).
		// Close as failed so the barrier doesn't hang.
		c.markFailed(ctx, run, "recipe_missing", log)
		return
	}

	// hostTasks are the tasks this host is sent; settled is what a linear run's step
	// decided on the Keeper for this host before sending anything (settleStep).
	var (
		hostTasks []*render.RenderedTask
		settled   *settledHost
	)
	tasks, plans, err := RenderForHost(ctx, c.deps.Deps, run.Recipe, run.IncarnationName, run.ApplyID, run.SID, run.Passage)
	if err == nil {
		if run.Recipe.Step != nil {
			hostTasks, settled, err = c.settleStep(ctx, run, tasks, plans)
		} else {
			hostTasks = groupByHost(tasks, plans)[run.SID]
		}
	}
	if err != nil {
		// Drain interruption (claimCtx canceled after grace expired,
		// graceful-drain ADR-027 amend GATE-1): do NOT mark the job failed —
		// leave Ward as-is (claimed), lease expires → recovery scan picks it up
		// (ADR-027(i)). Marking failed here would force-consume the job and
		// burn an attempt without applying.
		if c.aborted(ctx) {
			log.Info("scenario: claim - render interrupted by drain, Ward left for recovery")
			return
		}
		if errors.Is(err, errHostDestroyed) {
			// NIM-56: host status destroyed (removed by cloud-destroy cascade) → benign no_match, not a failure.
			if uerr := c.closeWard(ctx, run, applyrun.StatusNoMatch, nil); uerr != nil {
				if errors.Is(uerr, applyrun.ErrApplyRunAlreadyTerminal) || errors.Is(uerr, applyrun.ErrWardLost) {
					log.Info("scenario: claim benign no_match (destroy cascade) - not written", slog.Any("reason", uerr))
					return
				}
				log.Error("scenario: claim benign no_match (destroy cascade) not recorded", slog.Any("error", uerr))
				return
			}
			log.Info("scenario: claim - host removed by destroy cascade, benign no_match")
			return
		}
		// err may carry a vault:secret/-ref in transit — mask before writing to
		// status (operator-facing, unmasked-out) and to the log. The Acolyte path
		// doesn't track a seal set (per-host render at claim, a separate slice) →
		// nil sealed paths: degrades to the vault+regex layers (ADR-010 §7.4),
		// bit-for-bit.
		c.markFailed(ctx, run, maskErrText(err, nil), log)
		return
	}

	// Tasks targeting exactly this SID (after on:/where: in RenderForHost). A
	// linear run's step row never arrives here empty: settleStep refuses a host
	// whose render differs from the run's, and the run queues only hosts it sends to.
	if len(hostTasks) == 0 {
		// on:/where: filtered out all tasks on this host: the host isn't targeted
		// for this run. FINDING-01 option (b) — terminal `no_match`, NOT `success`:
		// apply_runs no longer over-reports "success where nothing was applied"
		// (the Acolyte path writes planned for EVERY roster host BEFORE per-host
		// on:/where: resolution). UpdateWardStatus sets finished_at. The barrier
		// counts no_match as a benign terminal (like success) → the run proceeds
		// to ready, not error_locked.
		if err := c.closeWard(ctx, run, applyrun.StatusNoMatch, nil); err != nil {
			if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) || errors.Is(err, applyrun.ErrWardLost) {
				log.Info("scenario: claim no-op no_match - not written", slog.Any("reason", err))
				return
			}
			log.Error("scenario: claim no-op no_match not recorded", slog.Any("error", err))
			return
		}
		log.Info("scenario: claim - no task targets the host, no-op no_match")
		return
	}

	// Cancel window for planned/claimed (ADR-027 cutover, minor fix): a
	// cluster-wide Cancel flag could have been set between claim and SendApply
	// (RequestCancel now also hits planned/claimed). Fresh read BEFORE sending:
	// if Cancel was requested, apply does NOT go to Soul — the job moves to
	// terminal cancelled (the run-goroutine barrier counts it as non-success and
	// cancels the run). Canceling BEFORE SendApply is safe — nothing was sent to
	// the host. A PG read error is treated fail-open (don't cancel): rare, and
	// skipping the cancel is safer than falsely canceling an already-valid
	// apply — a repeat Cancel/recovery will finish it off.
	if cancelled, cerr := applyrun.SelectCancelRequested(ctx, c.deps.Deps.DB, run.ApplyID, run.SID, run.Passage); cerr != nil {
		log.Warn("scenario: claim - reading cancel_requested failed, continuing apply", slog.Any("error", cerr))
	} else if cancelled {
		if err := c.closeWard(ctx, run, applyrun.StatusCancelled, nil); err != nil {
			if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) || errors.Is(err, applyrun.ErrWardLost) {
				log.Info("scenario: claim cancelled - not written", slog.Any("reason", err))
				return
			}
			log.Error("scenario: claim - cancelled status not recorded after Cancel in the claim window", slog.Any("error", err))
			return
		}
		log.Info("scenario: claim - Cancel requested before SendApply, apply not sent, job cancelled")
		return
	}

	// Drain interruption BEFORE marking dispatched: if claim-ctx was canceled by
	// drain, do NOT mark dispatched and do NOT send, leave Ward as-is (claimed),
	// lease expires → recovery scan picks it up (ADR-027(i)). Nothing was sent —
	// no double apply.
	if c.aborted(ctx) {
		log.Info("scenario: claim - drain before the dispatched mark, Ward left for recovery")
		return
	}

	// claimed → dispatched STRICTLY BEFORE SendApply (deliver-once intent
	// marker, ADR-027 amend S3). This is the core of the anti-double-apply
	// invariant: once a row is dispatched, recovery-reclaim doesn't touch it
	// (reclaim is scoped to status='claimed', S4). If MarkDispatched fails
	// (PG error), we do NOT send apply — Ward stays claimed (recovery
	// re-claims it); nothing was sent — no double apply. Races, repeats and a
	// Ward lost to a re-claim are cut off by MarkDispatched's filter on
	// status='claimed' and this claim's attempt.
	//
	// The version this host announced rides along (ADR-0076(l)): this is the last
	// point before handoff at which the target is known, and it is still the same
	// connection the capability gate judged.
	soulVersion := readAnnouncedSoulVersion(ctx, c.deps.Deps.SoulVersion, run.SID, log)
	if err := applyrun.MarkDispatched(ctx, c.deps.Deps.DB, run.ApplyID, run.SID, run.Passage, run.Attempt, soulVersion); err != nil {
		if errors.Is(err, applyrun.ErrWardLost) {
			log.Info("scenario: Ward re-claimed at a later attempt, SendApply not called", slog.Any("reason", err))
			return
		}
		log.Error("scenario: claimed -> dispatched not recorded, SendApply not called (Ward left claimed for recovery)", slog.Any("error", err))
		return
	}
	// Records of the tasks the step does not send, after the mark: the mark is
	// the fence, so a claim that lost its Ward does not write them twice.
	if settled != nil {
		writeKeeperRecords(ctx, c.sinkDeps(log), run.ApplyID, run.Passage, run.SID, numberedAfter(settled.records, len(hostTasks)))
	}

	// ApplyRequest carries attempt = run.Attempt (fencing epoch, ADR-027(g)):
	// ClaimNext raised it on Ward capture above every attempt this host reached
	// in the run, so a re-claim of an expired job — or the next step after a
	// re-claimed one — arrives with a higher attempt and the Soul-guard rejects
	// only the stale duplicate. SendApply additionally routes by SID-lease
	// (apply only through the stream owner — the first layer of protection
	// against double execution). Passage is echoed back in TaskEvent/RunResult,
	// which is how they find this step's row.
	req := &keeperv1.ApplyRequest{
		ApplyId: run.ApplyID,
		// ToProtoTasksForHost(run.SID): THIS host's per-host render_context for
		// self-variant core.file.rendered (open Q #25, render_context.self).
		// Acolyte renders the full roster (RenderForHost) — RenderContextBySID
		// is filled with the same per-host variants as in the run-goroutine path.
		Tasks:   render.ToProtoTasksForHost(hostTasks, run.SID),
		Attempt: int32(run.Attempt),
		Passage: int32(run.Passage),
	}
	if err := c.deps.Deps.Outbound.SendApply(ctx, run.SID, req); err != nil {
		// SendApply returned an error: delivery is NOT CONFIRMED (a network
		// failure could have hit AFTER transit — the host may have received the
		// job). Terminal to failed (UpdateWardStatus allows dispatched→failed) —
		// safe even if delivery actually happened: attempt fencing +
		// single-winner cut off the duplicate, and reclaim doesn't touch a
		// terminal. Drain interruption lands here too (closing with a terminal
		// on a canceled ctx is safer than leaving a dangling dispatched that
		// recovery won't pick up anymore). req carries resolved Params — do NOT
		// echo them into status; safe reason string only.
		c.markFailed(ctx, run, "send_apply_failed", log)
		return
	}

	// KNOWN GAP closed: the old gap "row stayed claimed after SendApply,
	// recovery re-claims it → DOUBLE SendApply" is no longer possible — the
	// (claimed→dispatched) mark now happens STRICTLY BEFORE send, reclaim only
	// takes claimed.
	//
	// New known SAFE gap W-a (ADR-027 amend): MarkDispatched succeeded but
	// Keeper crashed BEFORE SendApply — row is dispatched, nothing was sent to
	// the host, RunResult never arrives → job hangs. This is NOT a double
	// apply. Closed by Soul-reconcile (post-MVP, Variant A): on reconnect Soul
	// reports its in-flight apply_ids, Keeper reconciles stuck dispatched rows.
	log.Info("scenario: ApplyRequest sent (claim, dispatched)", slog.Int("tasks", len(hostTasks)))
}

// settleStep is this host's part of a linear run's step, settled as the run
// goroutine settles it (dispatchPassage): the step's slice, the flow control the
// render decided and the cross-step requisite gate. It returns the tasks the host
// is sent and the records of those it is not, and refuses the host unless it
// arrives at the tasks the run decided to send it (applyrun.RecipeStep.Tasks): the
// run decides, and a claim that would send anything else sends nothing.
func (c *ClaimRunner) settleStep(ctx context.Context, run *applyrun.ApplyRun, tasks []*render.RenderedTask, plans []render.DispatchPlan) ([]*render.RenderedTask, *settledHost, error) {
	stepTasks, stepPlans := tasksForPassage(tasks, plans, run.Passage)
	var (
		kept    []*render.RenderedTask
		settled *settledHost
	)
	if mine := groupByHost(stepTasks, stepPlans)[run.SID]; len(mine) > 0 {
		gate, err := c.stepGate(ctx, run, tasks, plans, mine)
		if err != nil {
			return nil, nil, err
		}
		out, byHost := settleHosts(map[string][]*render.RenderedTask{run.SID: mine}, gate, run.Passage)
		kept, settled = out[run.SID], byHost[run.SID]
	}
	decided := run.Recipe.Step.Tasks
	if got := planIndices(kept); (settled != nil && settled.failed) || len(got) == 0 || !slices.Equal(got, decided) {
		return nil, nil, fmt.Errorf("scenario: step %d of %s renders tasks %v at claim where the run decided %v - its roster, a fact or a destiny on a branch moved between the two renders; nothing was sent",
			run.Passage, run.SID, got, decided)
	}
	return kept, settled, nil
}

// stepGate is the cross-step requisite gate the run goroutine builds for the step
// (run.go), over the CHANGED/FAILED facts of the steps before it. Without an audit
// reader the run goroutine refuses a scenario that needs one before its first
// step; an instance claiming its row without one refuses the host instead of
// guessing that a source did not fire.
func (c *ClaimRunner) stepGate(ctx context.Context, run *applyrun.ApplyRun, tasks []*render.RenderedTask, plans []render.DispatchPlan, mine []*render.RenderedTask) (*crossPassageGate, error) {
	if run.Passage == 0 {
		return nil, nil
	}
	reader := c.deps.Deps.AuditReader
	if reader == nil {
		probe := newCrossPassageGate(tasks, plans, nil, nil)
		for _, t := range mine {
			onchanges, _ := probe.splitRequisite(t.OnChangesIdx, run.Passage)
			onfail, _ := probe.splitRequisite(t.OnFailIdx, run.Passage)
			if len(onchanges) > 0 || len(onfail) > 0 {
				return nil, fmt.Errorf("scenario: task %q gates on an earlier step's result, and this instance has no audit log to read it from", t.Name)
			}
		}
		return nil, nil
	}
	changed, err := reader.SelectChangedTaskKeys(ctx, run.ApplyID)
	if err != nil {
		return nil, fmt.Errorf("scenario: cross-step changed facts: %w", err)
	}
	failed, err := reader.SelectFailedTaskKeys(ctx, run.ApplyID)
	if err != nil {
		return nil, fmt.Errorf("scenario: cross-step failed facts: %w", err)
	}
	return newCrossPassageGate(tasks, plans, changed, failed), nil
}

// sinkDeps is what a record the claim writes on the Keeper's behalf goes through:
// the sink a Soul's events go through.
func (c *ClaimRunner) sinkDeps(log *slog.Logger) applysink.Deps {
	return applysink.Deps{
		DB:     c.deps.Deps.DB,
		Audit:  c.deps.Deps.Audit,
		Bus:    c.deps.Deps.ApplyBus,
		Source: audit.SourceKeeperInternal,
		Logger: log,
	}
}

// aborted reports whether the claim's ctx was canceled — the pool's claimCtx
// canceled by Shutdown after the drain grace expires (Acolyte pool
// graceful-drain, ADR-027 amend GATE-1). On such an interruption BEFORE the
// dispatched mark (render branch / pre-mark drain-check), the claimed job is
// NOT moved to a terminal: its Ward stays claimed, attempt/lease untouched,
// lease expires → recovery scan returns the job to the queue (ADR-027(i)).
// Distinguishes drain-abort from a domain render error, which normally leads
// to failed.
func (c *ClaimRunner) aborted(ctx context.Context) bool {
	return ctx.Err() != nil
}

// closeWard writes this claim's terminal for its own step row, fenced by the
// claim's attempt: an Acolyte that lost the Ward to a reclaim must not close
// a row a newer claim owns ([applyrun.ErrWardLost]).
func (c *ClaimRunner) closeWard(ctx context.Context, run *applyrun.ApplyRun, status applyrun.Status, summary *string) error {
	if err := applyrun.UpdateWardStatus(ctx, c.deps.Deps.DB, run.ApplyID, run.SID, run.Passage, run.Attempt, status, summary); err != nil {
		return err
	}
	c.deps.Deps.ApplyWake.Wake(run.ApplyID)
	return nil
}

// markFailed moves a claimed job to failed with an already-safe summary (no
// exposed secret). summary is read externally via barrier/status_details
// without masking — caller must pass a scrubbed/neutral string.
func (c *ClaimRunner) markFailed(ctx context.Context, run *applyrun.ApplyRun, summary string, log *slog.Logger) {
	if err := c.closeWard(ctx, run, applyrun.StatusFailed, &summary); err != nil {
		if errors.Is(err, applyrun.ErrApplyRunAlreadyTerminal) || errors.Is(err, applyrun.ErrWardLost) {
			log.Info("scenario: claim failed - not written",
				slog.String("summary", summary), slog.Any("reason", err))
			return
		}
		log.Error("scenario: claim failed status not recorded - the barrier may hang until timeout",
			slog.String("summary", summary), slog.Any("error", err))
		return
	}
	log.Warn("scenario: claim - job failed", slog.String("summary", summary))
}
