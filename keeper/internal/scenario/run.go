package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// run is the run-goroutine body: a full scenario pass from incarnation
// resolution to committing incarnation.state. Errors are logged and (when
// they change state) move the incarnation to error_locked; run returns
// nothing to the caller (a goroutine).
func (r *Runner) run(ctx context.Context, spec RunSpec) {
	log := r.logger.With(
		slog.String("apply_id", spec.ApplyID),
		slog.String("incarnation", spec.IncarnationName),
		slog.String("scenario", spec.ScenarioName),
	)

	// In-process span for the whole scenario run. incarnation/scenario name are
	// trace attributes for filtering (can't be metric labels — cardinality,
	// ADR-024 §2.2); carry no secrets. apply_id correlates with RunResult/
	// audit. OTel disabled → tracer is no-op, Start/End are free.
	ctx, span := tracer.Start(ctx, "scenario.run",
		trace.WithAttributes(
			attribute.String("incarnation", spec.IncarnationName),
			attribute.String("scenario", spec.ScenarioName),
			attribute.String("apply_id", spec.ApplyID),
		),
	)

	// Run metric recorded once on any exit. result/duration fill in along the
	// way: a locked exit skips the histogram (duration=0, run never started);
	// ok/failed record start-to-terminal duration.
	started := time.Now()
	result := runResultFailed
	defer func() {
		var dur float64
		if result != runResultLocked {
			dur = time.Since(started).Seconds()
		}
		r.deps.Metrics.ObserveRun(result, dur)
		if result != runResultOK {
			span.SetStatus(codes.Error, result)
		}
		span.End()
	}()

	// 1. Resolve incarnation + check status, move to applying. From here any
	//    early exit must record a terminal status — lockRun already set
	//    applying, the incarnation can't be left hanging.
	inc, err := r.lockRun(ctx, spec)
	if err != nil {
		if errors.Is(err, ErrAlreadyRunning) {
			result = runResultLocked
			log.Warn("scenario: incarnation уже в статусе applying — прогон отклонён")
			return
		}
		if errors.Is(err, ErrLocked) {
			result = runResultLocked
			log.Warn("scenario: incarnation в статусе error_locked — прогон отклонён до unlock")
			return
		}
		if errors.Is(err, ErrNotRunnable) {
			result = runResultLocked
			log.Warn("scenario: статус incarnation не допускает прогон — отклонён",
				slog.Any("error", err))
			return
		}
		log.Error("scenario: подготовка прогона провалена", slog.Any("error", err))
		return
	}
	stateBefore := inc.State

	// Failure terminal status depends on the finalization mode (S-D2b):
	//   - TerminalCommitState → error_locked (normal run, state untouched);
	//   - TerminalDestroy      → destroy_failed (teardown failed; NOT
	//     error_locked — from destroy_failed the operator retries destroy or
	//     unlocks to ready).
	// state stays untouched in both cases (last known-good).
	failStatus := failureStatus(spec.TerminalMode)

	// tasks/plans are declared BEFORE abort so a failed incarnation.run_completed
	// (ADR-052 §k) sees PARTIAL state on a LATE abort (dispatch_failed/
	// register_load_failed/…: render already populated them, changed_tasks
	// carries whatever reached CHANGED). On an EARLY abort (before render) they
	// stay nil → buildChangedTasks returns empty. Populated by assignment at the
	// render point (step 5), not `:=`.
	var (
		tasks []*render.RenderedTask
		plans []render.DispatchPlan
	)

	// seal / sealed-paths ([ADR-010] §7.4): accumulates params-cell paths whose
	// CEL expression read a secret source (secret-input/vault()/transit). Render
	// (step 5) fills it in per-task; abort/lockIncarnation use sealed.Paths() for
	// seal-aware masking of observable channels (audit.MaskSecretsSealed) on top
	// of vault+regex. The pointer is shared across staged-render passages (paths
	// accumulate over the whole run). Declared BEFORE abort — abort can fire
	// before Render (sealed empty → degrades to vault+regex, bit-for-bit).
	sealed := render.NewSealedSet()

	// From here, a run failure means failStatus (state untouched). result stays
	// runResultFailed on any abort exit below; ok is set only on a successful
	// finish.
	abort := func(reason string, cause error) {
		// cause may carry a vault:secret/-ref from a render/dispatch error. Apply
		// the same masking as status_details in lockIncarnation to both observable
		// channels — OTel trace (span exception) and slog — to avoid an asymmetry
		// with the masked DB status_details.
		maskedCause := errors.New(maskErrText(cause, sealed.Paths()))
		span.RecordError(maskedCause)
		log.Error("scenario: прогон провален — incarnation заблокирована",
			slog.String("reason", reason),
			slog.String("terminal_status", string(failStatus)),
			slog.String("error", maskErrText(cause, sealed.Paths())))
		finalized := r.lockIncarnation(ctx, spec, stateBefore, failStatus, reason, cause, sealed.Paths(), log)
		// BAG-1: guarantee a terminal apply_runs row for the run. An early abort
		// (no_hosts etc.) happens BEFORE dispatch — no apply_runs row exists yet,
		// and the Voyage-awaiter polls for all rows to reach terminal, waiting
		// forever on an empty set. error_locked is an incarnation status, not
		// apply_runs; the awaiter doesn't see it. ensureTerminalApplyRun closes the
		// run with a failure terminal status (terminalizes non-terminal rows on a
		// late abort, or inserts a sentinel on an empty set) so the barrier/awaiter
		// get a terminal. Status depends on cause: operator-initiated Cancel →
		// cancelled, everything else (including timeout/Shutdown) → failed.
		// keepRunning: on operator Cancel (errCancelRequested) leave a running
		// apply on a LIVE host untouched — an honest RunResult will arrive. On
		// timeout/dead-host/ctx.Err no RunResult will arrive (host is stuck) and
		// the reaper doesn't pick up running (narrowed to claimed, ADR-027), so we
		// force-fail running rows, otherwise apply_run hangs forever (BAG-1
		// recovery).
		keepRunning := errors.Is(cause, errCancelRequested)
		r.ensureTerminalApplyRun(ctx, spec, reason, failureTerminalStatus(cause), keepRunning, log)

		// Terminal run-failure event (T4 foundation, ADR-052 §k):
		// incarnation.run_completed (status=failed) with partial/empty
		// changed_tasks, symmetric to the success branch of run(). Gate:
		//   - TerminalDestroy does NOT emit (destroy has its own terminal —
		//     destroy_completed/destroy_failed via writeDestroyFailedAudit, see
		//     lockIncarnation);
		//   - SINGLE-WINNER: emit ONLY when our lockIncarnation actually recorded
		//     the terminal (finalized). On ErrAlreadyFinalized (recovery loser) the
		//     winning instance emits the event — avoid duplicating it per run.
		// On an early abort (before render) tasks/plans=nil → changed_tasks empty;
		// on a late abort — partial (whatever reached CHANGED). status="failed" is
		// a single value (error_locked → failed, no sub-statuses). Best-effort:
		// doesn't fail failure handling (detached ctx inside emitRunCompleted).
		if spec.TerminalMode != TerminalDestroy && finalized {
			r.emitRunCompleted(ctx, spec, runCompletedStatusFailed, tasks, plans, log)
		}
	}

	// 2. Load the service artifact (one git snapshot per run) + parse
	//    scenario/<name>/main.yml.
	art, err := r.deps.Loader.Load(ctx, spec.ServiceRef)
	if err != nil {
		abort("scenario_load_failed", fmt.Errorf("scenario: load service: %w", err))
		return
	}
	scn, err := r.parseScenario(art, spec.ScenarioName, spec.FromUpgrade)
	if err != nil {
		abort("scenario_load_failed", err)
		return
	}

	// Expand include into a flat task list — BEFORE render (orchestration.md §6,
	// two-level resolve scenario-local → service-level, cycles detected by
	// resolved path). Render already handles a flat list; no include nodes
	// remain in its input after this.
	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(r.deps.Loader, art, spec.ScenarioName))
	if diag.HasErrors(idiags) {
		abort("scenario_load_failed", fmt.Errorf("scenario: раскрытие include в %s/%s: %s", spec.ScenarioName, scenarioMainFile, firstError(idiags)))
		return
	}
	scn.Tasks = expanded

	// Synthesize core.module.installed install steps from service.yml::modules[]
	// (ADR-065): AFTER ExpandIncludes (consumers in branches are visible), BEFORE
	// Stratify (a synthesized step is a roster task, stratified like its
	// consumer).
	if synthed, names := config.SynthesizeModuleInstalls(scn.Tasks, art.Manifest.Modules); len(names) > 0 {
		scn.Tasks = synthed
		log.Info("scenario: синтезированы install-шаги модулей из manifest.modules[] (ADR-065)",
			slog.Any("modules", names))
	}

	// 2.5. Provision-aware effective run-timeout (ADR-0061). Deadline moved here
	//      from Start (runner.go) — the plan is parsed, so we know if this is a
	//      provision run. A normal run keeps defaultRunTimeout (5m) — the
	//      protection against a stuck barrier stays intact. A run with a refresh
	//      emitter (provision-from-zero: cloud-create + `await_online` onboarding
	//      up to its ceiling + role deploy) legitimately runs longer: base =
	//      onboarding barrier ceiling (ResolvedMaxAwaitTimeout) + deployBudget.
	//      Without this, defaultRunTimeout would cut a provision run off at
	//      minute 5 — before joinWait (6m) and await_timeout (up to 30m).
	//      WithTimeout wraps runCtx (WithCancel from Start): the sub-context
	//      still inherits cancellation from the active-map, so Cancel/Shutdown
	//      still abort the run. defer cancel covers any return from run()
	//      (including the early aborts below).
	ctx, cancel := context.WithTimeout(ctx, r.effectiveRunTimeout(scn.Tasks))
	defer cancel()

	// 3. Resolve run hosts (roster by the incarnation's Coven label). Factored
	//    into resolveRoster because it's called AGAIN in the stage-loop at
	//    refresh boundaries (mid-run re-resolve, ADR-0061 §S3): after a
	//    successful `refresh_soulprint: true` step, newly created+onboarded
	//    hosts enter the next Passage's roster.
	hosts, err := r.resolveRoster(ctx, spec.IncarnationName)
	if err != nil {
		abort("topology_failed", err)
		return
	}
	// no_hosts gate — fail-closed by default (S1 restriction, ADR-0061): a run
	// requires connected hosts, empty roster → error_locked. TWO bypass CLASSES
	// for provision-from-zero (ADR-0061 amendments):
	//
	//   (a) all-keeper (allKeeperTasks): ALL tasks are `on: keeper` — a
	//       keeper-only scenario (core.cloud.created creates a VM FROM SCRATCH),
	//       no hosts exist at start by definition.
	//   (b) mixed with a refresh emitter (HasRefreshEmitter): the plan carries a
	//       refresh emitter (core.soul.registered with refresh_soulprint: true)
	//       → roster is re-resolved mid-run (ADR-0061 §S2/§S3). An empty
	//       starting roster is legitimate: host deploy tasks stratify into a
	//       Passage AFTER the refresh boundary and see the re-resolved live
	//       snapshot (onboarded VMs), not an empty P0. This is staged
	//       provision→role in one run.
	//
	// chicken-egg for both classes: "run needs hosts, run creates them". WITHOUT
	// either signal, an empty roster is no_hosts bit-for-bit (unextended
	// behavior). Computed over scn.Tasks AFTER ExpandIncludes — the same flat
	// top-level list Render and Stratify see.
	provisionsRoster := config.HasRefreshEmitter(scn.Tasks)
	if len(hosts) == 0 && !allKeeperTasks(scn.Tasks) && !provisionsRoster {
		abort("no_hosts", fmt.Errorf("incarnation %q не имеет connected-хостов", spec.IncarnationName))
		return
	}

	// 4. Essence (effective layer). render.Pipeline exposes it to CEL as
	//    `essence.<path>` (slice E2). Uses the first host's OS family as the
	//    representative (per-host essence is an extension).
	//
	//    keeper context (empty roster, provision-from-zero): no representative
	//    host exists, hosts[0] would panic. Resolve essence WITHOUT a per-host
	//    overlay — default layer + the incarnation's Coven overlay (root Coven
	//    label = inc.Name, ADR-008) + spec.essence override. OS-family overlay is
	//    skipped (OSFamily empty), symmetric to renderKeeperTask, which renders
	//    keeper tasks without a per-host soulprint. After onboarding the created
	//    VMs, later Passages get per-host essence the normal way (mid-run
	//    re-resolve, ADR-0061 §S3).
	essenceIn := keeperEssenceInput(art.LocalDir, inc)
	if len(hosts) > 0 {
		essenceIn = essenceInput(art.LocalDir, inc, hosts[0])
	}
	essenceMap, err := r.deps.Essence.Resolve(essenceIn)
	if err != nil {
		abort("essence_failed", err)
		return
	}

	// 4.5. Effective input: apply the scenario `input:` schema to operator-
	//      supplied values. Order: merge defaults + required →
	//      scoped-resolve `vault:`-ref input → value validation (pattern/enum
	//      checked against the ALREADY resolved value, docs/input.md
	//      §"vault_scope"). vault-ref resolve happens ONCE here (render reads
	//      input N times, already resolved). Without the merge, params not
	//      passed but carrying default: stay absent, and CEL `${ input.<def> }`
	//      fails with "no such key". Vault=nil (unit/L0) → input vault-refs
	//      aren't resolved.
	resolver := r.newInputVaultResolver(ctx, inputVaultAuditCtx{
		aid:         spec.StartedByAID,
		incarnation: spec.IncarnationName,
		scenario:    spec.ScenarioName,
	}, r.deps.InputDenyPaths)
	effectiveInput, err := config.ResolveInputValuesVault(scn.Input, spec.Input, resolver)
	if err != nil {
		abort("input_invalid", fmt.Errorf("scenario: input %s/%s: %w", spec.IncarnationName, spec.ScenarioName, err))
		return
	}

	// 5. Render: vault-resolve → CEL → on/where → []RenderedTask + []DispatchPlan.
	//    The destiny resolver is per-run: knows THIS service snapshot's
	//    destiny[] refs (art.Manifest.Destiny[]) + default_destiny_source.
	//    nil Destiny in Deps → apply:destiny is rejected by the render phase
	//    (ErrUnsupportedDSL).
	renderIn := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           inc.Name,
			Service:        inc.Service,
			ServiceVersion: inc.ServiceVersion,
		},
		Hosts: hosts,
		// State — a read-only snapshot of incarnation.state at the run's row-lock
		// (stateBefore captured under FOR UPDATE). Exposed to scenario-render CEL
		// as `incarnation.state.<path>` (ADR-009/010, Option A). ONE snapshot:
		// renderIn is reused across all staged-render passages, so it stays
		// invariant (P0 ≡ P1+ = pre-run state, does NOT accumulate across
		// passages).
		State: stateBefore,
		Ctx:   ctx, // vault() (RenderStateChanges doesn't go through Render → ctx needed explicitly)
		// Templates: reader for the service snapshot's .tmpl files, used by
		// core.file.rendered. Two-level resolve scenario-local→service-level
		// (ADR-009): reads via artifact.ReadSnapshotFile over art.LocalDir
		// (securejoin-protected).
		Templates: render.NewSnapshotTemplateReader(
			func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(art.LocalDir, rel) },
			scenarioTemplatePrefix(spec.ScenarioName),
		),
		// seal (ADR-010 §7.4): Render fills sealed with params-cell paths whose
		// expression read a secret source. Used by abort/lockIncarnation for
		// seal-aware masking. Pointer shared across passages (accumulates).
		Sealed: sealed,
	}
	if r.deps.Destiny != nil {
		renderIn.Destiny = r.deps.Destiny.resolverFor(art.Manifest)
	}

	// 4.9. Stratify the run by register dependency (staged-render, ADR-056 §b) —
	//      BEFORE the first Render: a task reading register.X in
	//      where:/apply:input:/params:/vars: lands in a Passage strictly AFTER
	//      the probe step that emits register: X. Stratify works over the same
	//      flat top-level scn.Tasks (post-ExpandIncludes) as Render. N=1 (no
	//      register dependencies) → Passage{Count:1, all passage 0} → bit-for-bit
	//      behavior. A cycle / dangling register ref → errStratify →
	//      render_failed (explicit rejection, not silent-wrong-target). Done
	//      BEFORE Render because for staged runs the first Render must know
	//      TaskPassage/ActivePassage=0 — otherwise it would eagerly render a
	//      future Passage's register-dependent where against an empty register
	//      and fail (the original drift).
	passage, perr := render.Stratify(scn.Tasks)
	if perr != nil {
		abort("render_failed", fmt.Errorf("scenario: стратификация passage %s/%s: %w", spec.IncarnationName, spec.ScenarioName, perr))
		return
	}
	staged := passage.Count > 1

	// 4.91. Roster-refresh boundaries (ADR-0061 §S2/§S3). refreshBoundaries[P] —
	//       whether roster needs a re-resolve BEFORE rendering Passage P (Passage
	//       P-1 finished a successful `refresh_soulprint: true` step → onboarded
	//       hosts entered souls+coven → the live roster snapshot changed).
	//       out[0]=false (up-front roster). Without a refresh emitter, all false
	//       → no re-resolve happens (bit-for-bit). RefreshBoundaries is a pure
	//       function over the same scn.Tasks and passage: boundaries sit before
	//       every Passage that follows a Passage with a refresh emitter.
	refreshBoundaries := config.RefreshBoundaries(scn.Tasks, passage)

	// 4.92. Within-block register dependency — KEEPER-SIDE FAIL-CLOSED safety net
	//       (ADR-056, §"Risks — silent-wrong-target"). A block: child reading the
	//       register of a SIBLING child in the SAME block isn't caught by
	//       Stratify (an intra-block edge doesn't cross the top-level task
	//       boundary — a block is atomic per Passage). The peer register becomes
	//       available Soul-side only AFTER probe, but the consumer's
	//       where/when/params resolve Keeper-side BEFORE dispatch → where would
	//       silently select hosts by a stale/external register. soul-lint must
	//       catch this offline; here it's a runtime safety net (reject, not
	//       silent-wrong-target).
	if info, bad := config.WithinBlockRegisterDependency(scn.Tasks); bad {
		abort(config.CodeWithinBlockRegisterDependency, fmt.Errorf(
			"scenario %s/%s: задача %q внутри block: читает register %q, эмитнутый соседней %q ТОГО ЖЕ блока — невозможно на render (block атомарен, peer-register доступен только Soul-side ПОСЛЕ probe, а where/when/params резолвятся Keeper-side ДО dispatch); вынесите probe на top-level (разные Passage)",
			spec.IncarnationName, spec.ScenarioName, info.ReaderName, info.RegisterName, info.EmitterName))
		return
	}

	// 4.925. Cross-passage when-gating — KEEPER-SIDE FAIL-CLOSED safety net
	//        (ADR-056:85 amend, FC-5). A task gates `when:`/`changed_when:`/
	//        `failed_when:` by a register emitted in an EARLIER Passage.
	//        flow-control is Soul-side per-task gating (ADR-012(d)), which only
	//        sees its OWN Passage's register; a cross-passage register is
	//        unavailable to it (a different ApplyRequest) → silent `no such key`
	//        → task FAILED. After the narrow-fix, flow-control itself doesn't
	//        split a Passage, but a probe may have landed in an earlier Passage
	//        for a DIFFERENT reason (another task with `where: register.X`).
	//        where: handles this (Keeper re-renders with accumulated register),
	//        when: doesn't. soul-lint catches this offline; here it's a runtime
	//        safety net (reject, not a silent no-such-key failure). Gate applies
	//        strictly to staged runs (N=1 → one Passage, cross-passage
	//        impossible).
	if info, bad := config.CrossPassageWhenGating(scn.Tasks, passage); bad {
		abort(config.CodeCrossPassageWhenGating, fmt.Errorf(
			"scenario %s/%s: задача %q гейтит %s: по register %q из другого Passage (consumer passage %d, источник passage %d) — Soul-side gating видит только свой Passage, cross-passage register недоступен → no such key; используйте where: для cross-task register-таргетинга или register.self для same-task gating (ADR-056:85)",
			spec.IncarnationName, spec.ScenarioName, info.ConsumerName, info.Kind, info.RegisterName, info.ConsumerPassage, info.SourcePassage))
		return
	}

	// 4.95. serial + staged (N>1) — 2D serial×passage IMPLEMENTED (ADR-056 §S4
	//       amend, S-2D1). The `serial_staged_unsupported` restriction is
	//       LIFTED. The serial (HOST waves) and Passage (TASK stratification)
	//       axes are orthogonal and now run together: the Passage loop below
	//       runs each Passage in order, and dispatchPassage internally splits
	//       hosts into serial waves from THIS Passage's tasks
	//       (effectiveSerialWidth on the tasksForPassage slice → per-Passage
	//       width, NOT per-run). A probe Passage without serial runs as one wave
	//       even when a later Passage carries serial:1 (no silent-wrong-width).
	//       serial+staged takes the same inline path as serial without staged,
	//       so it inherits the staged-inline crash-recovery limit (ADR-056 §S4:
	//       Acolyte-reclaim doesn't cover staged-inline) — not a new regression.

	// 4.955. Cross-passage requisite — KEEPER-SIDE GATING (ADR-056 R3). An
	//        onchanges/onfail source in an EARLIER Passage than its consumer
	//        travels in a separate ApplyRequest → single-Passage Soul gating
	//        can't see another Passage's source result (R1-remap fixes ONLY
	//        same-passage). So Keeper resolves the link per-host from
	//        accumulated CHANGED/FAILED facts of previous Passages
	//        (crosspassage.go): cross-passage onchanges is OR over CHANGED,
	//        onfail mirrors it over FAILED∪TIMED_OUT. R2-reject is LIFTED —
	//        cross-passage is supported.
	//
	//        Source of CHANGED/FAILED facts is the audit log (AuditReader).
	//        Without it, keeper can't tell whether the cross-passage source fired
	//        → fail-closed reject (symmetric to nil passageCap §S5): guessing
	//        "not changed" would silently skip a genuinely-needed consumer / not
	//        run a rescue. Gate applies strictly to staged runs (N=1 → one
	//        Passage, cross-passage impossible). The CrossPassageRequisite
	//        detector is reused to check whether a cross-passage link exists (if
	//        so, a reader is required).
	if staged && r.deps.AuditReader == nil {
		if info, bad := config.CrossPassageRequisite(scn.Tasks, passage); bad {
			abort("cross_passage_requisite_unsupported", fmt.Errorf(
				"scenario %s/%s: задача %q ссылается через %s: на register %q, чей источник в другом Passage (consumer passage %d, источник passage %d) — cross-passage gating требует журнала аудита (AuditReader), но он недоступен → отказ fail-closed (ADR-056 R3)",
				spec.IncarnationName, spec.ScenarioName, info.ConsumerName, info.Kind, info.RequisiteName, info.ConsumerPassage, info.SourcePassage))
			return
		}
	}

	// 4.96. Forward-compat staged gate (ADR-056 §S5). A staged run sends N
	//       ApplyRequests per host (one per Passage); each Passage's barrier
	//       waits for its terminal — a RunResult echoing the passage. A Soul that
	//       can't echo passage (old binary, no passage capability) would return
	//       RunResult with passage=0 for every Passage under N>1 → the barrier
	//       for Passage 1+ would wait for a terminal that never arrives → STUCK
	//       in applying. So before dispatch we verify EVERY online host in the
	//       run has announced passage capability. Any host that doesn't →
	//       fail-closed abort `soul_passage_unsupported` (not a hang, not a
	//       silent single-pass execution that would reintroduce the original
	//       drift). An N=1 run (staged==false) skips this gate — it sends a
	//       single passage=0, compatible with old Souls bit-for-bit. A nil
	//       checker (no Redis / unit) → reject staged entirely: without a
	//       presence source we can't confirm support, and sending N>1 blind
	//       carries the same hang risk.
	if staged {
		sids := make([]string, len(hosts))
		for i, h := range hosts {
			sids[i] = h.SID
		}
		if r.passageCap == nil {
			abort("soul_passage_unsupported", fmt.Errorf(
				"scenario %s/%s: staged-прогон (%d Passage) требует подтверждения passage-capability хостов, но presence-чекер недоступен (нет Redis) — отказ fail-closed (ADR-056 §S5)",
				spec.IncarnationName, spec.ScenarioName, passage.Count))
			return
		}
		lacking, lerr := r.passageCap.SoulsLackingPassage(ctx, sids)
		if lerr != nil {
			abort("soul_passage_unsupported", fmt.Errorf(
				"scenario %s/%s: проверка passage-capability хостов staged-прогона провалилась — отказ fail-closed (ADR-056 §S5): %w",
				spec.IncarnationName, spec.ScenarioName, lerr))
			return
		}
		if len(lacking) > 0 {
			abort("soul_passage_unsupported", fmt.Errorf(
				"scenario %s/%s: staged-прогон (%d Passage по register-зависимости) требует Passage-aware Soul, но хосты %v не поддерживают поле passage — обнови soul-бинарь либо убери register-зависимость (ADR-056 §S5)",
				spec.IncarnationName, spec.ScenarioName, passage.Count, lacking))
			return
		}
	}

	if staged {
		// The first Render of a staged run fully renders Passage 0; future
		// Passages get placeholders (ActivePassage=0, register still empty).
		renderIn.TaskPassage = passage.TaskPassage
		renderIn.ActivePassage = 0
	}

	tasks, plans, err = r.deps.Render.Render(ctx, renderIn)
	if err != nil {
		abort("render_failed", err)
		return
	}

	// 6. Dispatch: path branching (ADR-027, Phase 1.4.2) × staged-render (ADR-056).
	//
	// Keeper-side tasks (`on: keeper`, docs/keeper/modules.md) run LOCALLY on
	// this instance via the keeper-side core Registry — STRICTLY BEFORE their
	// Passage's host-dispatch (keeper steps go first: provision/coven-bind →
	// apply on hosts). They write their own apply_runs row
	// (sid=render.KeeperTargetSID, passage) + register, which the barrier and
	// loadRegisterByHost see alongside host rows. The first failing keeper task
	// → abort (this Passage's host-dispatch never starts). dispatchKeeperTasks
	// is now called PER-Passage (Slice 2): for the staged path, inside the
	// stage-loop on tasks RE-rendered at ActivePassage=p (a Passage>0
	// keeper task at step-5 render is a placeholder with no Params — it can
	// only be dispatched once its Passage becomes active); for the Acolyte
	// path, just Passage 0 (Acolyte excludes staged).
	//
	//   - staged (Passage.Count>1): OLD path (inline) — stage-loop below.
	//     Acolyte renders per-host at claim time ONCE (not per-Passage) — staged
	//     on Acolyte is deferred to S4 (ADR-056 §S4). So at Count>1 we go inline
	//     even with AcolyteEnabled.
	//   - serial-guard: a scenario with any `serial:` task (post-ExpandIncludes)
	//     → OLD path (inline render+SendApply+per-wave barrier), even with
	//     AcolyteEnabled. Distributed serial is Phase 3.
	//   - otherwise AcolyteEnabled → NEW path (dispatchPlanned): planned jobs for
	//     all roster hosts + Summons; render/SendApply is done by Acolyte at
	//     claim time.
	//   - otherwise → old path (direct Insert(running)+SendApply).
	//
	// Render above (steps 4.5/5) runs on BOTH paths: the run-goroutine keeps
	// tasks/renderIn for post-barrier register-load + state_changes commit
	// (KEY invariant: barrier+commit stay in the run-goroutine in Phase 1).
	// state commits strictly AFTER the LAST Passage's barrier — a single commit,
	// not per-wave or per-Passage (§7 / ADR-056 §d).
	if r.acolyteEnabled && !hasSerialTask(scn) && !staged {
		// Acolyte path — non-staged (Acolyte excludes staged): all keeper tasks
		// are in Passage 0, step-5 render (ActivePassage=0) already fully
		// rendered them. Run them BEFORE the host tasks' planned fan-out —
		// keeper-fail → abort, planned never gets written. KeeperRegister is
		// empty here (P0, no chaining): host-fallback intact.
		// NIM-37 (H1): persist the Passage 0 plan (all non-staged tasks are in
		// it) before dispatch.
		r.persistRunPlan(ctx, spec, tasks, 0, sealed.Paths(), log)
		if err := r.dispatchKeeperTasks(ctx, spec, log, 0, tasks, plans); err != nil {
			abort("keeper_dispatch_failed", err)
			return
		}
		if err := r.dispatchPlanned(ctx, spec, log, hosts, tasks); err != nil {
			abort("dispatch_failed", err)
			return
		}
	} else {
		// Passage loop (ADR-056 §c): for each Passage P in order —
		//   render(tasks in P, RegisterByHost = accumulated from Passage < P) →
		//   dispatch(tasks in P) → barrier(P) → collect register(P).
		// At P=0 we reuse the step-5 render (for staged it's already
		// ActivePassage=0; for Count==1, a normal render, bit-for-bit). P>0
		// (staged only): re-render with per-host register from Passage < P.
		// state-commit happens ONCE after the last Passage (steps 7-8), NOT
		// per-Passage (ADR-056 §d).
		passageTasks, passagePlans := tasks, plans
		for p := 0; p < passage.Count; p++ {
			if p > 0 {
				// Mid-run roster re-resolve (ADR-0061 §S3) at a refresh boundary:
				// Passage P-1 finished a successful `refresh_soulprint: true` step →
				// its barrier converged (created+onboarded hosts are written to
				// souls+coven) → re-resolve roster BEFORE rendering Passage P.
				// re-resolve semantics: a FRESH LIVE SNAPSHOT of the incarnation's
				// roster at the refresh boundary (resolveRoster → LoadIncarnationHosts
				// → filterAlive), reflecting the CURRENT online set. It grows as
				// provisioned hosts onboard (created VMs come up → become visible),
				// but this is NOT monotonic: a host that went offline by the boundary
				// (lease expired / status≠connected) is EXCLUDED from the live
				// snapshot — targeting goes to the actually-online set (no point
				// rolling a role onto an offline host). The updated renderIn.Hosts
				// feeds the re-render → soulprint.hosts and Passage P's
				// on:[incarnation.name] targeting see the current set
				// (resolveTargets/soulprint.hosts are built from in.Hosts). Roster is
				// STABLE within a Passage: re-resolve only happens at boundaries,
				// preserving per-Passage determinism (waves/run_once/assert stay
				// fixed within a Passage). Re-resolve failure → abort (not silently
				// falling back to the old roster).
				if refreshBoundaries[p] {
					grown, rerr := r.resolveRoster(ctx, spec.IncarnationName)
					if rerr != nil {
						abort("topology_failed", fmt.Errorf("scenario: re-resolve roster перед Passage %d: %w", p, rerr))
						return
					}
					prevSize := len(renderIn.Hosts)
					renderIn.Hosts = grown
					log.Info("scenario: roster пере-резолвлен на refresh-границе — live-снимок (ADR-0061 §S3)",
						slog.Int("passage", p), slog.Int("roster_size", len(grown)), slog.Int("prev_roster_size", prevSize))
				}

				// Staged run, P>0: re-render with per-host register accumulated from
				// Passage < P. Tasks in future Passages (> P) are emitted as
				// placeholders (register not ready, ADR-056 §c.1); the active
				// Passage P and earlier ones are fully resolved (where: register.*
				// now sees the real fact). loadRegisterByHostUpToPassage resolves
				// task_idx→register-name from the ALREADY-available tasks (Index is
				// stable across Passages — same plan).
				reg, lerr := r.loadRegisterByHostUpToPassage(ctx, spec.ApplyID, p, tasks)
				if lerr != nil {
					abort("register_load_failed", lerr)
					return
				}
				renderIn.RegisterByHost = reg
				// keeper→keeper register-chaining (staged-render): keeper tasks
				// accumulate register under the synthetic host KeeperTargetSID, and
				// keeperVars (render/dispatch.go) reads it from the ISOLATED
				// renderIn.KeeperRegister channel (a per-host map isn't available to
				// keeper context — a keeper task has no host). We copy the previous
				// Passages' keeper bucket from RegisterByHost into KeeperRegister so
				// the active Passage's keeper task sees `register.<prev>.*` from
				// earlier Passages' keeper tasks (e.g. core.bootstrap.delivered reads
				// register from core.cloud.created). This channel is deliberately
				// SEPARATE from the flat renderIn.Register (host-fallback guard,
				// hostRegister stays on Register): a mixed-Passage host task with an
				// empty per-host bucket must NOT read keeper-register. Consumed by
				// the per-passage keeper-dispatch (same Passage, below). nil bucket →
				// reset KeeperRegister (host-only Passage: keeper context is empty,
				// bit-for-bit).
				renderIn.KeeperRegister = keeperRegisterBucket(reg)
				renderIn.TaskPassage = passage.TaskPassage
				renderIn.ActivePassage = p
				pt, pp, rerr := r.deps.Render.Render(ctx, renderIn)
				if rerr != nil {
					abort("render_failed", rerr)
					return
				}
				passageTasks, passagePlans = pt, pp
				// Keep the resolved tasks/plans for the next Passage's register resolve
				// and the final changed_tasks aggregation (Index stable across Passages).
				tasks, plans = pt, pp
			}

			// NIM-37 (H1): persist THIS Passage's plan from its active render
			// (passageTasks: p=0 → step-5 render, p>0 → re-render at
			// ActivePassage=p). The t.Passage==p filter drops placeholders of
			// future Passages (their compacted indices would diverge from actual
			// execution). Runs before dispatch, best-effort. Idempotent +
			// non-overlapping passage slices → accumulates, doesn't overwrite.
			r.persistRunPlan(ctx, spec, passageTasks, p, sealed.Paths(), log)

			// Keeper-side tasks for THIS Passage (Slice 2): run on tasks RE-rendered
			// at ActivePassage=p (passageTasks), STRICTLY BEFORE this Passage's
			// host-dispatch. At step-5 render / p>0 re-render, a Passage p keeper
			// task carries full Params (pipeline.go's placeholder-gate doesn't mute
			// it on its OWN active Passage) — keeperTasksOf filters exactly passage
			// p's keeper tasks. keeper→keeper register-chaining: a Passage p keeper
			// task sees register from Passage<p keeper tasks via
			// renderIn.KeeperRegister (copied above). keeper FAIL → abort (return)
			// BEFORE dispatchPassage p: this Passage's host-dispatch never starts.
			// Ordering vs. refresh boundaries: keeper-dispatch p completes BEFORE
			// iteration p+1 begins, where re-resolve reads its effect
			// (core.soul.registered{refresh_soulprint} writes souls+coven). A
			// Passage with no keeper tasks is a no-op (host-only Passage). N=1 →
			// one call for passage 0, same behavior as the pre-loop call before
			// Slice 2 (bit-for-bit).
			if err := r.dispatchKeeperTasks(ctx, spec, log, p, passageTasks, passagePlans); err != nil {
				abort("keeper_dispatch_failed", err)
				return
			}

			pTasks, pPlans := tasksForPassage(passageTasks, passagePlans, p)
			// Cross-passage requisite gate for Passage p (ADR-056 R3): for p>0, load
			// CHANGED/FAILED facts from Passage < p (from the audit log) and resolve
			// per-host onchanges/onfail links whose source is an earlier Passage.
			// p==0 → gate=nil (no earlier Passage). The full plan (tasks) is used
			// for passageByIndex sources not present in the Passage-p slice.
			var gate *crossPassageGate
			if p > 0 && r.deps.AuditReader != nil {
				changed, ferr := r.deps.AuditReader.SelectChangedTaskKeys(ctx, spec.ApplyID)
				if ferr != nil {
					abort("register_load_failed", fmt.Errorf("scenario: cross-passage changed-факты: %w", ferr))
					return
				}
				failed, ferr := r.deps.AuditReader.SelectFailedTaskKeys(ctx, spec.ApplyID)
				if ferr != nil {
					abort("register_load_failed", fmt.Errorf("scenario: cross-passage failed-факты: %w", ferr))
					return
				}
				gate = newCrossPassageGate(tasks, changed, failed)
			}
			if err := r.dispatchPassage(ctx, spec, log, p, pTasks, pPlans, gate); err != nil {
				abort("dispatch_failed", err)
				return
			}
		}
	}

	// 6.5. Teardown finalization (TerminalMode=TerminalDestroy, S-D2b): the
	//      barrier passed on ALL of the incarnation's hosts → the `destroy`
	//      teardown scenario succeeded. We do NOT commit ready and do NOT touch
	//      incarnation.state (destroy doesn't edit the state graph; teardown
	//      works with hosts, not jsonb): the incarnation stays `destroying`.
	//      Steps 7-8 (register-load + state_changes commit) are the normal-run
	//      path and don't run for destroy. result=runResultOK records a
	//      successful teardown for metrics/trace.
	if spec.TerminalMode == TerminalDestroy {
		// In-process span for teardown finalization (dropping the incarnation row
		// after successful host teardown). Child of scenario.run — gives the
		// archive+DELETE tx its own duration in the trace (distinguishing "host
		// teardown" from "DB row removal"). No secrets in attributes:
		// incarnation/scenario name are already on the parent; here only
		// host/task counts (cardinality-safe numbers, not labels). OTel disabled
		// → tracer is no-op, Start/End are free.
		dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer dcancel()
		dctx, dspan := tracer.Start(dctx, "scenario.destroy_teardown",
			trace.WithAttributes(
				attribute.Int("hosts", len(hosts)),
				attribute.Int("tasks", len(tasks)),
			),
		)
		defer dspan.End()
		// S-D3: teardown passed on every host → physically drop the row. Archive
		// (incarnation_archive / state_history_archive, migration 039) +
		// single-winner DELETE WHERE status='destroying' + audit
		// destroy_completed — one tx inside DeleteAfterTeardown (the V3 cascade
		// drops live state_history / apply_runs / register; the archive is
		// written BEFORE DELETE). result=runResultOK records success for metrics
		// even if DELETE was a no-op (Deleted=false — someone already dropped
		// the row): teardown itself succeeded. Detached ctx: the original ctx
		// may have been cancelled (Shutdown/timeout), but the row still needs
		// dropping after a successful teardown.
		res, derr := incarnation.DeleteAfterTeardown(dctx, r.deps.DB, r.deps.Audit, spec.IncarnationName, destroyForce(inc), log)
		if derr != nil {
			// DELETE/archive failed after a successful host teardown: hosts are
			// gone but the row wasn't removed. This error exit does NOT move to
			// destroy_failed (failureStatus would only fire via abort before this
			// point) — leave it destroying for triage, the operator retries
			// destroy. result stays runResultFailed (recorded by defer).
			dspan.RecordError(derr)
			dspan.SetStatus(codes.Error, "teardown_delete_failed")
			span.RecordError(derr)
			log.Error("scenario: teardown успешен, но снос строки incarnation провалился — остаётся в destroying",
				slog.Any("error", derr))
			return
		}
		dspan.SetAttributes(attribute.Bool("deleted", res.Deleted))
		result = runResultOK
		log.Info("scenario: destroy завершён — teardown успешен, строка incarnation снесена",
			slog.Bool("deleted", res.Deleted), slog.Int("tasks", len(tasks)), slog.Int("hosts", len(hosts)))
		return
	}

	// 7. After the barrier — load run-task register data (accumulated from
	//    TaskEvents in apply_task_register) and resolve it into a per-host
	//    register map (sid → register-name → payload) via the task_idx→
	//    register-name mapping from tasks. Gives `sets: ${ register.<task>.
	//    <field> }` access to register (slice 2 of the full grammar,
	//    orchestration.md §7.1).
	registerByHost, err := r.loadRegisterByHost(ctx, spec.ApplyID, tasks)
	if err != nil {
		abort("register_load_failed", err)
		return
	}
	renderIn.RegisterByHost = registerByHost

	// 8. All tasks succeeded on all hosts → render state_changes (Keeper-side
	//    CEL, last-wins cross-host) and commit into incarnation.state. Render
	//    happens strictly AFTER the barrier (orchestration.md §7): values are
	//    fixed by the fact of a successful apply, not before it. RenderStateOps
	//    is an ordered list of set+add operations (the new list form); merge
	//    applies them to stateBefore in order (set-overwrite / idempotent add).
	renderedOps, err := r.deps.Render.RenderStateOps(renderIn)
	if err != nil {
		abort("state_changes_render_failed", err)
		return
	}
	stateAfter, err := mergeStateChanges(stateBefore, renderedOps, art.Manifest.StateSchema, r.deps.Render.EvalStateMatch, r.deps.Render.EvalStateOpExpr)
	if err != nil {
		// A failed operation apply (on_conflict: error / inconsistent collection /
		// match predicate failed) → error_locked, state is NOT committed
		// (stateAfter never reaches commitSuccess). orchestration.md §7: a failed
		// merge means locking.
		abort("state_changes_apply_failed", err)
		return
	}
	if err := r.commitSuccess(ctx, spec, stateBefore, stateAfter); err != nil {
		// Single-winner (ADR-027(j) W1): the incarnation was already moved out
		// of applying by another committer (recovery takeover / parallel
		// finish) — NOT a failure. We don't abort (don't overwrite someone
		// else's terminal with error_locked); the run itself succeeded on
		// hosts. result=runResultOK.
		if errors.Is(err, incarnation.ErrAlreadyFinalized) {
			result = runResultOK
			log.Info("scenario: state-commit пропущен — incarnation уже финализирована другим коммиттером",
				slog.Int("tasks", len(tasks)), slog.Int("hosts", len(hosts)))
			// The losing commit does NOT emit incarnation.run_completed (returns
			// before emitRunCompleted below): the winning instance (whose
			// commitSuccess succeeded) emits it — deliberate protection against
			// duplicate events per run.
			return
		}
		// Commit failed after a successful apply on hosts — hosts are in the
		// right state, but the DB is out of sync. error_locked for triage
		// (state untouched — UpdateStateFromRun in commitSuccess is atomic, the
		// transaction rolled back on failure).
		abort("state_commit_failed", err)
		return
	}
	result = runResultOK
	log.Info("scenario: прогон завершён успешно", slog.Int("tasks", len(tasks)), slog.Int("hosts", len(hosts)))

	// Terminal per-incarnation run-outcome event (T3, ADR-052 §k):
	// incarnation.run_completed (status=success) with per-task changed_tasks.
	// Emitted on a normal run's successful finish (TerminalDestroy finalizes
	// above via its own path — destroy_completed/destroy_failed; failures go
	// through abort). Best-effort: doesn't fail an already-successful run
	// (result=runResultOK already recorded).
	r.emitRunCompleted(ctx, spec, runCompletedStatusSuccess, tasks, plans, log)
}

// resolveRoster resolves the run's host roster by the incarnation's root Coven
// label (online souls + soulprint + declared/Choir role). Factored out of
// run() step 3 because it's called AGAIN in the stage-loop at refresh
// boundaries (mid-run re-resolve, ADR-0061 §S3): the same resolve path
// produces both the up-front roster and the re-resolved roster between
// Passages. The presence source of truth is the Redis SID lease (Resolver
// phase 2), the same one the `await_online` onboarding barrier uses — so
// re-resolve sees exactly who the refresh step's barrier waited online for.
func (r *Runner) resolveRoster(ctx context.Context, incarnationName string) ([]*topology.HostFacts, error) {
	return r.deps.Topology.LoadIncarnationHosts(ctx, incarnationName)
}

// allKeeperTasks reports whether the scenario consists ENTIRELY of keeper-side
// tasks (each `on: keeper`, render.IsKeeperTask). This is the FIRST bypass
// class for the no_hosts gate in provision-from-zero (ADR-0061 amendment): an
// all-keeper create scenario (core.cloud.created creates a VM FROM SCRATCH)
// legitimately starts on an empty roster.
//
// ALL, not ANY: a mixed keeper+host run does NOT pass this predicate. But the
// SECOND bypass class covers mixed provision→role — config.HasRefreshEmitter
// (a plan with a refresh emitter, see the no_hosts gate above): the host task
// stratifies into a Passage after the refresh boundary and sees the
// re-resolved roster, not an empty P0. A mixed plan WITHOUT a refresh emitter
// still hits no_hosts (a host task on an empty P0 is correctly no_hosts). An
// empty scenario (len==0) → false: "no tasks" isn't a reason to bypass the
// gate. Computed over tasks AFTER ExpandIncludes (the flat top-level list
// Render also sees).
func allKeeperTasks(tasks []config.Task) bool {
	if len(tasks) == 0 {
		return false
	}
	for _, t := range tasks {
		if !render.IsKeeperTask(t) {
			return false
		}
	}
	return true
}

// effectiveRunTimeout returns the run-duration ceiling for a task plan
// (ADR-0061, provision-aware). Base is runTimeout (Deps.RunTimeout /
// defaultRunTimeout, 5m). If the plan carries a refresh emitter
// ([config.HasRefreshEmitter] — provision-from-zero: create VM + `await_online`
// onboarding + role deploy), the ceiling rises to the onboarding barrier's
// ceiling (ResolvedMaxAwaitTimeout) + deployBudget, but only if that's GREATER
// than base (max, not replace): an operator who raised RunTimeout above eff
// for their own needs isn't cut down. Without a refresh emitter — exactly base
// (a stuck barrier is cut off as before).
//
// ceiling comes from the hot-reload-aware maxAwaitTimeoutFn (the same
// keeper.yml::max_await_timeout snapshot the onboarding barrier in coremod
// sees) — eff stays consistent with the real `await_online` ceiling. nil fn
// (unit/L0 without config.Store) → [config.DefaultMaxAwaitTimeout] (30m): a
// provision run still gets the extended ceiling, just without the override.
func (r *Runner) effectiveRunTimeout(tasks []config.Task) time.Duration {
	base := r.runTimeout
	if !config.HasRefreshEmitter(tasks) {
		return base
	}
	ceiling := config.DefaultMaxAwaitTimeout
	if r.maxAwaitTimeoutFn != nil {
		ceiling = r.maxAwaitTimeoutFn()
	}
	if eff := ceiling + deployBudget; eff > base {
		return eff
	}
	return base
}

// lockRun resolves the incarnation, checks its status under FOR UPDATE, and
// moves it to the run's working status. The gate is an explicit allow-list
// (fail-closed): the set of valid starting statuses depends on the
// finalization mode (S-D2b). A new status added to the enum later REJECTS a
// run by default rather than silently allowing it.
//
// TerminalCommitState (normal run): starts ONLY from ready → moves to
// applying. Rejections (specific sentinels for clear logging/result above):
//   - applying     → [ErrAlreadyRunning] (another run in progress — a pilot
//     rejects, doesn't queue);
//   - error_locked → [ErrLocked] (run rejected until an explicit unlock,
//     ADR-009; retry from error_locked is NOT allowed);
//   - everything else (destroying / migration_failed / any future status) →
//     [ErrNotRunnable].
//
// TerminalDestroy (the `destroy` teardown scenario): starts ONLY from
// destroying (S-D1 already moved it there in the Destroy transaction) and
// does NOT change status — the incarnation stays destroying for the whole
// teardown (a concurrent run/upgrade sees destroying and is rejected by its
// own gate; a failed teardown → just destroy_failed). Any other status →
// [ErrNotRunnable]: teardown only starts from an already-initiated destroy,
// not from an arbitrary state.
//
// All checks happen under one FOR UPDATE — the gate's authority is in the
// transaction, not just the HTTP handler (TOCTOU-safe).
func (r *Runner) lockRun(ctx context.Context, spec RunSpec) (*incarnation.Incarnation, error) {
	var inc *incarnation.Incarnation
	err := pgx.BeginFunc(ctx, r.deps.DB, func(tx pgx.Tx) error {
		got, serr := selectForUpdate(ctx, tx, spec.IncarnationName)
		if serr != nil {
			return serr
		}
		if spec.TerminalMode == TerminalDestroy {
			// Teardown starts strictly from destroying; status is left UNTOUCHED
			// (stays destroying for the whole run).
			if got.Status != incarnation.StatusDestroying {
				return fmt.Errorf("%w: %s", ErrNotRunnable, got.Status)
			}
			inc = got
			return nil
		}
		if spec.FromLocked {
			// rerun-last: UnlockForRerun already transitioned error_locked→applying
			// under FOR UPDATE, bypassing ready (race-free). We do NOT transition
			// status again — we must SEE applying, otherwise the start is rejected
			// (fail-closed): any other status means the reserved row slipped out
			// from under us (someone else's takeover / an inconsistent call).
			if got.Status != incarnation.StatusApplying {
				return fmt.Errorf("%w: %s (rerun expected applying)", ErrNotRunnable, got.Status)
			}
			// Write the applying epoch flag onto an already-applying row (ADR-027
			// amend (m-S1)): UnlockForRerun transitions error_locked→applying
			// WITHOUT an epoch, so without this write a rerun-last-applying row
			// would be left with a NULL epoch and wouldn't be caught by
			// reconcile_orphan_applying — an owner crash mid-rerun-last would orphan
			// it forever. lockApplyingWithEpoch WHERE name=$1 (no status guard)
			// idempotently overwrites the epoch, status stays applying. The
			// residual micro-window between the UnlockForRerun tx and this tx
			// degrades to the same NULL-epoch known gap (no worse than today).
			if uerr := lockApplyingWithEpoch(ctx, tx, spec.IncarnationName, spec.ApplyID, r.kid, 0); uerr != nil {
				return uerr
			}
			inc = got
			return nil
		}
		switch got.Status {
		case incarnation.StatusReady, incarnation.StatusDrift:
			// ready is the normal start; drift is Scry's informational status
			// (ADR-031): remediating drift is just a normal apply, which on
			// success returns the incarnation to ready via commitSuccess. Same
			// applying transition and same gate as from ready — drift does NOT
			// block.
		case incarnation.StatusApplying:
			return ErrAlreadyRunning
		case incarnation.StatusErrorLocked:
			return ErrLocked
		default:
			// destroying / migration_failed / any future status.
			return fmt.Errorf("%w: %s", ErrNotRunnable, got.Status)
		}
		// Transition to applying + write the applying epoch flag in ONE
		// UPDATE/one tx (ADR-027 amend (m-S1)): the run's apply_id + attempt
		// (echoes the initial apply_runs.attempt=0, no apply_runs row exists yet)
		// + this instance's KID (same source as the lease-holder) +
		// applying_since=NOW(). The reconcile_orphan_applying reaper rule uses
		// this epoch to tell a live run apart from an orphaned lock left by a
		// dead owner. Atomicity with status='applying' rules out an
		// applying-without-epoch window on a crash before commit.
		if uerr := lockApplyingWithEpoch(ctx, tx, spec.IncarnationName, spec.ApplyID, r.kid, 0); uerr != nil {
			return uerr
		}
		inc = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inc, nil
}

// parseScenario reads scenario/<scenarioName>/main.yml from an already
// materialized service snapshot and parses it with the normative config
// parser (error-level diagnostics → a load error).
func (r *Runner) parseScenario(art *artifact.ServiceArtifact, scenarioName string, fromUpgrade bool) (*config.ScenarioManifest, error) {
	return parseScenarioFromArtifact(r.deps.Loader, art, scenarioName, fromUpgrade)
}

// scenarioRelPath builds the scenario's main YAML rel-path in the service
// snapshot: upgrade/<name>/main.yml when fromUpgrade (ADR-0068), otherwise
// scenario/<name>/main.yml.
func scenarioRelPath(scenarioName string, fromUpgrade bool) string {
	format := scenarioMainFile
	if fromUpgrade {
		format = upgradeMainFile
	}
	return fmt.Sprintf(format, scenarioName)
}

// parseScenarioFromArtifact is the package-level form of
// [Runner.parseScenario]: reads and parses scenario/<scenarioName>/main.yml
// from a service snapshot. Factored out of the method so the Acolyte path
// ([RenderForHost]) can reuse it without a Runner. Behavior is identical —
// pure read+parse, no side effects.
func parseScenarioFromArtifact(loader *artifact.ServiceLoader, art *artifact.ServiceArtifact, scenarioName string, fromUpgrade bool) (*config.ScenarioManifest, error) {
	rel := scenarioRelPath(scenarioName, fromUpgrade)
	data, err := loader.ReadFile(art, rel)
	if err != nil {
		return nil, fmt.Errorf("scenario: read %s: %w", rel, err)
	}
	// Resolve $type at load time: the render pipeline and value validation
	// below work with a self-contained input schema (see
	// artifact.LoadScenarioManifestResolved).
	scn, _, diags, err := artifact.LoadScenarioManifestResolved(art, rel, data)
	if err != nil {
		return nil, fmt.Errorf("scenario: parse %s: %w", rel, err)
	}
	if diag.HasErrors(diags) {
		return nil, fmt.Errorf("scenario: %s невалиден: %s", rel, firstError(diags))
	}
	return scn, nil
}

// failureStatus returns the run-failure terminal status by finalization mode
// (S-D2b): normal run → error_locked; teardown (TerminalDestroy) →
// destroy_failed. destroy_failed is NOT error_locked: the recovery semantics
// differ (from destroy_failed the operator retries destroy or unlocks to
// ready, S-D2a).
func failureStatus(mode TerminalMode) incarnation.Status {
	if mode == TerminalDestroy {
		return incarnation.StatusDestroyFailed
	}
	return incarnation.StatusErrorLocked
}

// lockIncarnation moves the incarnation to failStatus (error_locked for a
// normal run / destroy_failed for teardown) with status_details (state stays
// unchanged — we keep last known-good). Write errors are only logged: the
// incarnation stays in applying, which triage will notice.
//
// Returns finalized — true ONLY when THIS instance actually recorded the
// terminal (UpdateStateFromRun succeeded). false on a single-winner loss
// (ErrAlreadyFinalized: another committer already moved the row out of
// applying — recovery takeover / parallel finish) OR on a write error. abort()
// needs this signal so a failed incarnation.run_completed is emitted by
// exactly one winning instance (no duplicate event on a recovery takeover,
// symmetric to the success branch, where the losing commit returns before
// emitRunCompleted).
func (r *Runner) lockIncarnation(ctx context.Context, spec RunSpec, stateBefore map[string]any, failStatus incarnation.Status, reason string, cause error, sealedPaths map[string]bool, log *slog.Logger) (finalized bool) {
	// Commit under a detached ctx: the original ctx may have been cancelled
	// (Cancel/Shutdown/timeout), but error_locked must be recorded regardless.
	// WithoutCancel: keeps trace baggage, doesn't inherit the teardown path's
	// cancel. 5s cap guards against PG hanging on cancellation.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	details := map[string]any{
		"reason":   reason,
		"apply_id": spec.ApplyID,
	}
	if cause != nil {
		details["error"] = cause.Error()
	}
	// status_details is read externally via GET incarnation without masking,
	// and cause.Error() may carry a resolved secret / vault-ref in transit from
	// Params (e.g. in a render/dispatch error). Mask before writing —
	// observability-only, doesn't affect the wire (ApplyRequest.Params).
	// seal-aware (ADR-010 §7.4): vault+regex layers + this run's sealed paths +
	// regex alarm (DefaultSealHooks). sealedPaths empty (abort before Render) →
	// degrades to vault+regex bit-for-bit.
	details = audit.MaskSecretsSealed(details, audit.SealOpts{
		Sealed:        sealedPaths,
		RegexFallback: audit.DefaultSealHooks.RegexFallback,
		Logger:        audit.DefaultSealHooks.Logger,
	})
	historyID := audit.NewULID()
	err := pgx.BeginFunc(wctx, r.deps.DB, func(tx pgx.Tx) error {
		return incarnation.UpdateStateFromRun(
			wctx, tx,
			spec.IncarnationName, spec.ScenarioName, spec.ApplyID,
			stateBefore, stateBefore, // state untouched on failure
			failStatus, details,
			startedByPtr(spec.StartedByAID),
			historyID,
		)
	})
	if err != nil {
		// Single-winner (ADR-027(j) W1): another committer already moved the row
		// out of applying/destroying (recovery takeover) — the terminal was
		// recorded by them, not us. This is NEITHER "stuck in applying" NOR a
		// write error: log it as a no-op and skip the destroy_failed audit (the
		// terminal isn't ours anymore).
		if errors.Is(err, incarnation.ErrAlreadyFinalized) {
			log.Info("scenario: терминальный статус провала пропущен — incarnation уже финализирована другим коммиттером",
				slog.String("terminal_status", string(failStatus)))
			return false
		}
		log.Error("scenario: запись терминального статуса провала провалена — incarnation осталась в applying",
			slog.String("terminal_status", string(failStatus)),
			slog.Any("error", err))
		return false
	}

	// A destroy-failure terminal gets an explicit audit event (S-D3):
	// destroy_failed now has its own name in the catalog, not just
	// status_details + slog. Written ONLY for teardown mode (a normal run →
	// error_locked has its own incarnation.locked event — a separate
	// subsystem). reason is already masked above (details went through
	// MaskSecrets). A failed audit write doesn't fail the call — destroy_failed
	// is already committed, we only lose the trail.
	if failStatus == incarnation.StatusDestroyFailed {
		r.writeDestroyFailedAudit(wctx, spec, reason, details, log)
	}
	return true
}

// ensureTerminalApplyRun guarantees run `spec.ApplyID` has at least one
// TERMINAL apply_runs row after an abort (BAG-1). Two cases:
//
//   - rows ALREADY exist (late abort: dispatch got as far as inserting
//     planned/claimed/dispatched, OR Passage>0 keeper-dispatch failed AFTER a
//     successful earlier Passage's host-dispatch — Slice 2: keeper-dispatch is
//     now INSIDE the stage-loop) — terminalize EVERY non-terminal row to
//     `terminal` via [applyrun.UpdateStatus] (single-winner: already-terminal
//     rows are left alone, ADR-027(j)). running rows are left ALONE: the apply
//     already reached the host, an honest terminal will arrive from it
//     (RunResult). The failed Passage's keeper row is already failed
//     (dispatchKeeperTasks wrote it BEFORE returning the error), earlier
//     Passages' host rows stay success. No sentinel inserted — real rows
//     exist;
//   - NO rows exist (early abort: no_hosts / scenario_load_failed /
//     topology_failed / essence_failed / input_invalid / render_failed, and
//     also keeper_dispatch_failed on Passage 0 — the first Passage's keeper
//     tasks run BEFORE any host-dispatch) — insert ONE sentinel row
//     [render.RunSentinelSID] with status=`terminal` and error_summary=reason.
//
// `terminal` is the failure terminal status (see [failureTerminalStatus]):
// cancelled on operator Cancel, otherwise failed.
//
// reason's source of truth is the abort reason (no_hosts etc.). Write errors
// are only logged (warn): the incarnation is already error_locked, losing a
// barrier row degrades observability, not a reason to fail the goroutine.
// Detached ctx — the original ctx may have been cancelled (Shutdown/timeout),
// as in [lockIncarnation].

// failureTerminalStatus maps an abort cause to the run's apply_runs terminal
// status. Distinguished STRICTLY by [errCancelRequested] — the sentinel for
// an operator-initiated cluster-wide Cancel (G1): only that gives cancelled.
// context.Canceled/DeadlineExceeded (RunTimeout, Shutdown-abort) go through
// ctx.Err() and do NOT match — an honest provider failure, stays failed.
func failureTerminalStatus(cause error) applyrun.Status {
	if errors.Is(cause, errCancelRequested) {
		return applyrun.StatusCancelled
	}
	return applyrun.StatusFailed
}

func (r *Runner) ensureTerminalApplyRun(ctx context.Context, spec RunSpec, reason string, terminal applyrun.Status, keepRunning bool, log *slog.Logger) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	statuses, err := applyrun.SelectStatusesByApplyID(wctx, r.deps.DB, spec.ApplyID)
	if err != nil {
		log.Warn("scenario: чтение apply_runs для терминализации прогона провалено",
			slog.String("reason", reason), slog.Any("error", err))
		return
	}

	summary := reason
	if len(statuses) == 0 {
		// Early abort: no hosts, no rows. The sentinel closes the run with a
		// terminal so the Voyage-awaiter doesn't wait forever. reason is the
		// abort-cause text (carries no secrets; render/dispatch cause doesn't
		// flow here).
		run := applyrun.ApplyRun{
			ApplyID:         spec.ApplyID,
			SID:             render.RunSentinelSID,
			IncarnationName: spec.IncarnationName,
			Scenario:        spec.ScenarioName,
			Status:          terminal,
			ErrorSummary:    &summary,
			StartedByAID:    startedByPtr(spec.StartedByAID),
		}
		if ierr := applyrun.Insert(wctx, r.deps.DB, &run); ierr != nil {
			log.Warn("scenario: вставка sentinel-строки apply_runs провалена — прогон без терминальной строки",
				slog.String("reason", reason), slog.Any("error", ierr))
		}
		return
	}

	// Late abort: terminalize real hosts' non-terminal rows to terminal
	// (cancelled on operator Cancel, otherwise failed). running rows are
	// handled differently BY CAUSE (keepRunning):
	//   - operator Cancel (keepRunning): the apply reached a LIVE host — leave
	//     it, an honest RunResult will arrive (honest reporting).
	//   - timeout/dead-host/ctx.Err (!keepRunning): no RunResult will arrive,
	//     the reaper doesn't pick up running — force-fail (BAG-1 recovery).
	for _, st := range statuses {
		switch st.Status {
		case applyrun.StatusPlanned, applyrun.StatusClaimed, applyrun.StatusDispatched:
			// non-terminal, apply hasn't reached the host yet — close to terminal
		case applyrun.StatusRunning:
			if keepRunning {
				continue // operator-Cancel: an honest RunResult will arrive
			}
			// timeout/dead-host: no RunResult will arrive — force-fail
		default:
			continue // already terminal
		}
		if uerr := applyrun.UpdateStatus(wctx, r.deps.DB, spec.ApplyID, st.SID, st.Passage, terminal, &summary); uerr != nil {
			log.Warn("scenario: терминализация строки apply_runs провалена",
				slog.String("sid", st.SID), slog.Int("passage", st.Passage), slog.String("reason", reason), slog.Any("error", uerr))
		}
	}
}

// writeDestroyFailedAudit writes an incarnation.destroy_failed audit event
// after moving the incarnation to destroy_failed (teardown failure, S-D3).
// source=keeper_internal (write path is the scenario-runner, archon_aid
// column NULL), correlation_id = apply_id. reason comes from the already-
// masked status_details (cause may have carried a vault-ref in transit).
// Nil Audit → no trail written.
func (r *Runner) writeDestroyFailedAudit(ctx context.Context, spec RunSpec, reason string, details map[string]any, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	payload := map[string]any{
		"name":     spec.IncarnationName,
		"apply_id": spec.ApplyID,
		"reason":   reason,
	}
	// The masked reason text (if any) comes from status_details, not the raw
	// cause: details already went through MaskSecrets above.
	if errText, ok := details["error"].(string); ok && errText != "" {
		payload["error"] = errText
	}
	ev := &audit.Event{
		EventType:     audit.EventIncarnationDestroyFailed,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: spec.ApplyID,
		Payload:       payload,
	}
	if err := r.deps.Audit.Write(ctx, ev); err != nil && log != nil {
		log.Warn("scenario: запись audit incarnation.destroy_failed провалена",
			slog.String("incarnation", spec.IncarnationName), slog.Any("error", err))
	}
}

// destroyForce extracts the force intent from the incarnation's
// status_details (S-D1 put `force` there when transitioning to destroying).
// A missing key / non-bool → false (conservative default: treat as
// teardown-destroy). The value only feeds the destroy_completed audit payload
// — actual teardown behavior already happened above.
func destroyForce(inc *incarnation.Incarnation) bool {
	if inc == nil || inc.StatusDetails == nil {
		return false
	}
	f, _ := inc.StatusDetails["force"].(bool)
	return f
}

// commitSuccess records a successful run: state_changes are committed into
// incarnation.state, status → ready, a snapshot goes to state_history. One PG
// transaction (FOR UPDATE inside UpdateStateFromRun).
func (r *Runner) commitSuccess(ctx context.Context, spec RunSpec, stateBefore, stateAfter map[string]any) error {
	historyID := audit.NewULID()
	return pgx.BeginFunc(ctx, r.deps.DB, func(tx pgx.Tx) error {
		return incarnation.UpdateStateFromRun(
			ctx, tx,
			spec.IncarnationName, spec.ScenarioName, spec.ApplyID,
			stateBefore, stateAfter,
			incarnation.StatusReady, nil,
			startedByPtr(spec.StartedByAID),
			historyID,
		)
	})
}

// run_completed status values for the incarnation.run_completed payload
// (ADR-052 §k). One event for any normal-run terminal; the outcome goes into
// payload.status (task.executed/run.completed pattern — filter by field, not
// by event_type sprawl). error_locked collapses into "failed" (no
// sub-statuses).
const (
	runCompletedStatusSuccess = "success"
	runCompletedStatusFailed  = "failed"
)

// emitRunCompleted writes the incarnation.run_completed audit event at a
// normal run's terminal (T3/T4 foundation, ADR-052 §k): the per-incarnation
// scenario-run outcome with a changed_tasks array and status ∈ {success,
// failed}. source=keeper_internal (write path is the scenario-runner,
// archon_aid column NULL), correlation_id = apply_id. One event per
// incarnation-run, NOT per-host.
//
// Called from TWO places: run()'s success branch (after commitSuccess) with
// status=success, AND abort() (after terminalizing a failure, only when our
// lockIncarnation actually recorded the terminal) with status=failed.
// TerminalDestroy never reaches either — destroy has its own terminal
// (destroy_completed/_failed).
//
// changed_tasks is built from the audit log AGGREGATE (task.executed+CHANGED,
// AuditReader) — read-only over address fields (sid, task_idx); task metadata
// comes from in-memory tasks (secret hygiene). Folded by register∪id address
// (loop iterations of one address → one record, union of unique sids). On
// failure, tasks/plans may be nil (early abort before render) →
// buildChangedTasks(nil,…) returns nil (see TestBuildChangedTasks_EmptyInputs),
// or partial (late abort) → changed_tasks carries whatever reached CHANGED
// before failing. Nil Audit/AuditReader degrade: nil AuditReader → event
// written without changed_tasks (just the terminal fact); nil Audit → not
// written at all.
//
// cadence_id goes into the payload ONLY when spec.CadenceID != nil (a
// scheduled Voyage's child run, T4b) — a manual run carries no such key
// (conservative, like the drift payload), so a standing Tiding rule with a
// cadence selector catches exactly scheduled-run results.
//
// voyage_id goes into the payload ONLY when spec.VoyageID != nil (a run
// through a Voyage, ADR-052 amend §k) — direct paths (create/rerun/destroy)
// bypass Voyage and carry no such key (symmetric to cadence_id). Needed by
// the Voyage detail page's visibility fetch of per-incarnation run events:
// the event carries correlation_id=apply_id, and the voyage page filters by
// voyage_id in the payload.
//
// Detached ctx: the original ctx may be near timeout/cancelled
// (timeout/Cancel/Shutdown on failure), but the run's terminal must still be
// recorded. All errors are only logged (warn) — the run already reached
// terminal, losing the event just degrades observability.
func (r *Runner) emitRunCompleted(ctx context.Context, spec RunSpec, status string, tasks []*render.RenderedTask, plans []render.DispatchPlan, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var changed []ChangedTask
	if r.deps.AuditReader != nil {
		keys, err := r.deps.AuditReader.SelectChangedTaskKeys(wctx, spec.ApplyID)
		if err != nil {
			// Reading the aggregate failed — write the event without changed_tasks,
			// without losing the terminal fact itself. Best-effort aggregation.
			log.Warn("scenario: чтение changed-агрегата для incarnation.run_completed провалено — событие без changed_tasks",
				slog.Any("error", err))
		} else {
			changed = buildChangedTasks(tasks, plans, keys)
		}
	}

	payload := map[string]any{
		"incarnation":   spec.IncarnationName,
		"scenario":      spec.ScenarioName,
		"apply_id":      spec.ApplyID,
		"status":        status,
		"changed_tasks": changedTasksPayload(changed),
	}
	if spec.CadenceID != nil {
		payload["cadence_id"] = *spec.CadenceID
	}
	if spec.VoyageID != nil {
		payload["voyage_id"] = *spec.VoyageID
	}

	ev := &audit.Event{
		EventType:     audit.EventIncarnationRunCompleted,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: spec.ApplyID,
		Payload:       payload,
	}
	if err := r.deps.Audit.Write(wctx, ev); err != nil {
		log.Warn("scenario: запись audit incarnation.run_completed провалена",
			slog.String("incarnation", spec.IncarnationName), slog.Any("error", err))
	}
}

// persistRunPlan saves the run's ACTIVE Passage's host-invariant task plan
// (apply_run_plan, NIM-37) for the /tasks read endpoint: one row per
// plan_index with name/module/no_log/passage + masked params (S1b). Called
// PER-PASSAGE from its active render; the t.Passage != activePassage filter
// drops future Passages' placeholders (staged-render, ADR-056 §c.1), whose
// compacted indices would diverge from actual execution (H1). Idempotent
// (insertRunPlanSQL = ON CONFLICT DO UPDATE), passage slices don't overlap by
// plan_index. name/module/no_log are NOT secret (task address/type), no
// masking needed; params CAN carry a secret → maskRunPlanParams (seal-aware
// masking + transport filter) on the write path. sealedPaths is the run's
// sealed paths (sealed.Paths()) for the same seal-aware masker as
// status_details/error_summary.
//
// Best-effort: r.deps.DB=nil (unit without PG) or a write error are only
// logged — losing the plan degrades /tasks observability, not run
// correctness (symmetric to accumulateRegister/emitRunCompleted).
func (r *Runner) persistRunPlan(ctx context.Context, spec RunSpec, tasks []*render.RenderedTask, activePassage int, sealedPaths map[string]bool, log *slog.Logger) {
	if r.deps.DB == nil || len(tasks) == 0 {
		return
	}
	plan := make([]applyrun.RunPlanTask, 0, len(tasks))
	for _, t := range tasks {
		if t == nil || t.Passage != activePassage {
			continue
		}
		plan = append(plan, applyrun.RunPlanTask{
			ApplyID:   spec.ApplyID,
			PlanIndex: t.Index,
			Name:      t.Name,
			Module:    t.Module,
			NoLog:     t.NoLog,
			Passage:   t.Passage,
			Params:    maskRunPlanParams(t, sealedPaths),
		})
	}
	if err := applyrun.InsertRunPlan(ctx, r.deps.DB, spec.ApplyID, plan); err != nil {
		log.Warn("scenario: персист плана задач прогона (apply_run_plan) провален — /tasks без плана",
			slog.String("apply_id", spec.ApplyID), slog.Any("error", err))
	}
}

// paramTemplateContent / paramRenderContext are transport keys in
// core.file.rendered's params (mirrors render/template.go): template_content
// is the file content Keeper read for Soul-side render, render_context is
// the assembled per-host text/template root. Neither is operator-facing task
// "input" → both are stripped from params before persisting to
// apply_run_plan (NIM-37 S1b).
const (
	paramTemplateContent = "template_content"
	paramRenderContext   = "render_context"
)

// maskRunPlanParams prepares a task's params for persisting to
// apply_run_plan (NIM-37 S1b): protobuf Struct → map, strips transport keys
// (template_content/render_context), runs the rest through seal-aware masking
// (the same audit.MaskSecretsSealed as status_details/error_summary:
// run sealed paths + vault-ref + regex-last-resort with an alarm) — a SECOND
// barrier on top of already-rendered params. A no_log task / no Params /
// empty remainder → nil (jsonb NULL, symmetric to suppressing register_data
// for no_log). A marshal error → nil (best-effort: params observability
// degrades, plan persist doesn't fail).
func maskRunPlanParams(t *render.RenderedTask, sealedPaths map[string]bool) []byte {
	if t == nil || t.NoLog || t.Params == nil {
		return nil
	}
	m := t.Params.AsMap()
	delete(m, paramTemplateContent)
	delete(m, paramRenderContext)
	if len(m) == 0 {
		return nil
	}
	masked := audit.MaskSecretsSealed(m, audit.SealOpts{
		Sealed:        sealedPaths,
		RegexFallback: audit.DefaultSealHooks.RegexFallback,
		Logger:        audit.DefaultSealHooks.Logger,
	})
	b, err := json.Marshal(masked)
	if err != nil {
		return nil
	}
	return b
}

// changedTasksPayload converts []ChangedTask to the event's JSON payload form
// (snake_case keys). A separate function so the wire payload shape doesn't
// smear across the emission code. Carries ONLY metadata + counts (secret
// hygiene, T3): no register/params values. Empty/nil input → empty slice
// (not nil), so the JSONB payload carries `"changed_tasks": []` rather than a
// missing key.
func changedTasksPayload(changed []ChangedTask) []map[string]any {
	out := make([]map[string]any, 0, len(changed))
	for _, c := range changed {
		out = append(out, map[string]any{
			"idx":           c.Idx,
			"name":          c.Name,
			"register":      c.Register,
			"id":            c.ID,
			"module":        c.Module,
			"changed_hosts": c.ChangedHosts,
			"total_hosts":   c.TotalHosts,
		})
	}
	return out
}

// startedByPtr turns StartedByAID into a *string (empty string → nil, so the
// started_by_aid FK is written NULL for runs without an Archon identity).
func startedByPtr(aid string) *string {
	if aid == "" {
		return nil
	}
	return &aid
}

// maskErrText returns error text run through seal-aware masking: a
// render/dispatch error may carry a vault:secret/-ref (a pointer to a secret
// location). The same filter used for status_details is applied to the slog
// channel too (the log file is an observable channel). sealedPaths is the
// run's sealed-cell paths ([ADR-010] §7.4): here, under the single `error`
// key, they're almost always a no-op (free-text error carries a path/
// expression, not a value at a sealed path), but masking still goes through
// the same MaskSecretsSealed for symmetry with status_details and so the
// regex alarm (DefaultSealHooks) catches sensitive-by-name text. nil → empty
// string.
func maskErrText(err error, sealedPaths map[string]bool) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecretsSealed(map[string]any{"error": err.Error()}, audit.SealOpts{
		Sealed:        sealedPaths,
		RegexFallback: audit.DefaultSealHooks.RegexFallback,
		Logger:        audit.DefaultSealHooks.Logger,
	})
	if s, ok := masked["error"].(string); ok {
		return s
	}
	return err.Error()
}

// firstError returns the first error-level diagnostic's message (for a short
// scenario-parse error report).
func firstError(diags []diag.Diagnostic) string {
	for i := range diags {
		if diags[i].Level == diag.LevelError {
			return diags[i].Message
		}
	}
	return "unknown validation error"
}
