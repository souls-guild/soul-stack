package scenario

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	coremodutil "github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// dispatchKeeperTasks executes this Passage's keeper-side tasks (`on: keeper`,
// docs/keeper/modules.md) LOCALLY on this keeper instance — no work-queue/Soul
// involved. Execution contract mirrors the Soul-side path (events_taskevent.go /
// events_runresult.go) but runs in-process: the module executes through the
// keeper-side core Registry, its ApplyEvents are collected by an in-proc stream
// and folded into the same run tables:
//   - apply_runs (apply_id, sid=[render.KeeperTargetSID]) — terminal success/failed;
//   - apply_task_register — task register result (read by loadRegisterByHostUpToPassage);
//   - error_summary — failure reason (RecordTaskFailure), same as Soul-side tasks.
//
// All keeper tasks of a Passage share ONE apply_runs row (apply_id, sid=keeper,
// passage): task_idx distinguishes them in apply_task_register, and the row
// goes success only once every keeper task of THIS Passage passed. First
// failing keeper task → row failed + error returned (run() aborts). A staged
// run with keeper tasks across several Passages writes N keeper rows, one per
// (apply_id, keeper, passage) (triple PK, migration 078). N=1 (or all keeper
// tasks in Passage 0) → single passage-0 row, bit-for-bit as before Slice 2.
//
// Runs per-Passage INSIDE the stage loop (run.go), on tasks re-rendered for
// ActivePassage=passage, STRICTLY BEFORE this Passage's host-dispatch: a
// keeper-task failure on Passage>0 aborts before this Passage's host fan-out
// starts, same as Passage 0. keeper→keeper register chaining: a keeper task on
// Passage N sees `register.<prev>.*` from keeper tasks of earlier Passages via
// renderIn.KeeperRegister (stage-loop carry-over, see keeperRegisterBucket).
// Ordering matters at the refresh boundary: keeper dispatch of Passage P runs
// before P+1's re-resolve reads its effect (e.g. core.soul.registered{refresh_soulprint}
// writes souls+coven).
//
// No keeper tasks for this Passage → no-op (host-only Passage, or a run with no
// keeper-side tasks at all — ordinary Soul-side path).
// sealedPaths is the run's seal ([ADR-010] §7.4, render.SealedSet.Paths) — the
// params cells whose value came from a secret source. It is threaded down to
// the task because a keeper-side PLUGIN takes its secrets AS PARAMS (NIM-757,
// decided 2026-09-01: the plugin has no Vault access of its own), so the values
// the seal marks are exactly what must not come back out through the module's
// message. nil (push/trial paths, or a run whose render sealed nothing) → no
// values to redact, behaviour bit-for-bit as before.
func (r *Runner) dispatchKeeperTasks(ctx context.Context, spec RunSpec, stateSchema config.InputSchemaMap, log *slog.Logger, passage int, tasks []*render.RenderedTask, plans []render.DispatchPlan, sealedPaths map[string]bool) error {
	keeperTasks := keeperTasksOf(tasks, plans, passage)
	if len(keeperTasks) == 0 {
		return nil
	}
	if r.keeperModules == nil {
		return ErrKeeperModulesNotConfigured
	}

	// One apply_runs row per keeper-target per Passage: inserted running before
	// the first task, terminal after all keeper tasks of the Passage ran.
	// Passage is part of the PK (apply_id, sid, passage) — N keeper rows for a
	// staged run with keeper tasks in different Passages.
	if err := applyrun.Insert(ctx, r.deps.DB, &applyrun.ApplyRun{
		ApplyID:         spec.ApplyID,
		SID:             render.KeeperTargetSID,
		IncarnationName: spec.IncarnationName,
		Scenario:        spec.ScenarioName,
		Status:          applyrun.StatusRunning,
		StartedByAID:    startedByPtr(spec.StartedByAID),
		Passage:         passage,
		Input:           spec.inputSnapshot,
		// Engine provenance (ADR-0076(l)): keeper-side tasks are rendered AND
		// executed by this instance. No soul version — no agent is involved.
		KeeperVersion: r.deps.KeeperVersion,
	}); err != nil {
		return fmt.Errorf("scenario: insert keeper apply_run (passage %d): %w", passage, err)
	}

	for _, rt := range keeperTasks {
		changed, failed, output, msg := r.applyKeeperTask(ctx, spec, stateSchema, rt, sealedPaths)
		log.Info("scenario: keeper-side task executed",
			slog.String("module", rt.Module),
			slog.Int("task_idx", rt.Index),
			slog.Bool("changed", changed),
			slog.Bool("failed", failed))

		// task.executed for EVERY keeper task — mirrors the Soul-side handler
		// (events_taskevent.go). Without it, changed keeper tasks (on: keeper) never
		// reach the changed_tasks fold (auditpg) and the task: Tiding subscription
		// (ADR-052 §k) silently misses them. Emitted BEFORE the failed-return so a
		// failing keeper task is logged too (status FAILED, not CHANGED).
		r.emitKeeperTaskExecuted(ctx, spec.ApplyID, passage, rt, changed, failed, msg, log)
		// applybus publish (ADR-068 §A2) — ALONGSIDE audit, not instead: keeper-side
		// `on: keeper` progress is visible on operator SSE just like Soul-side.
		r.publishKeeperTaskExecuted(spec.ApplyID, passage, rt, changed, failed)

		if failed {
			summary := composeKeeperFailure(rt, msg)
			r.recordKeeperFailure(ctx, spec.ApplyID, passage, rt, summary, log)
			return fmt.Errorf("scenario: keeper-side task %q (%s) failed: %s", rt.Name, rt.Module, msg)
		}

		// Bind membership (ADR-008 amendment 2026-07-17/NIM-124):
		// core.soul.registered is the bind act — it writes incarnation membership
		// (incarnation_membership) for its SIDs from the current run's incarnation.
		// Authoritative: a membership write failure fails the run (a
		// registered-but-not-a-member host would be invisible to the roster).
		//
		// The bind writes the relation and NOTHING else: it attaches no coven and
		// no trait to the host (NIM-281), so there is no label projection to run
		// afterwards — and a host that should carry one needs an explicit
		// soul.coven-assign / soul.traits-assign from the operator.
		if berr := r.bindMembershipOnRegistered(ctx, spec, rt, output); berr != nil {
			summary := composeKeeperFailure(rt, berr.Error())
			r.recordKeeperFailure(ctx, spec.ApplyID, passage, rt, summary, log)
			return fmt.Errorf("scenario: keeper-side task %q (%s) membership bind failed: %w", rt.Name, rt.Module, berr)
		}

		// register: of a keeper task accumulates under KeeperTargetSID — same path
		// as Soul-side accumulateRegister. No register: (rt.Register=="") →
		// not written (buildRegisterByHost wouldn't resolve it anyway). passage is
		// REQUIRED (Slice 2): FK apply_task_register→apply_runs is the triple
		// (apply_id, sid, passage) (migration 078) — a Passage P task's register
		// must reference the keeper apply_runs row for exactly passage P (inserted
		// above); otherwise the FK would target (apply_id, keeper, 0), which for
		// P>0 doesn't exist, and the register would be lost.
		r.accumulateKeeperRegister(ctx, spec.ApplyID, passage, rt, changed, failed, output, log)
	}

	if err := applyrun.UpdateStatus(ctx, r.deps.DB, spec.ApplyID, render.KeeperTargetSID, passage, applyrun.StatusSuccess, nil); err != nil {
		return fmt.Errorf("scenario: transitioning keeper apply_run (passage %d) to success: %w", passage, err)
	}
	return nil
}

// emitKeeperTaskExecuted writes the task.executed audit event for a keeper-side
// task — mirrors the Soul-side handler (events_taskevent.go). It's the only
// source through which the changed_tasks fold (auditpg) and the task: Tiding
// subscription (ADR-052 §k) see keeper tasks (on: keeper): without it, a
// changed keeper-target task silently drops out of run_completed.changed_tasks.
//
// Payload shape is the shared [audit.BuildTaskExecutedPayload] (same as
// Soul-side) so the fold (payload->>'sid'/'task_idx'/'status') sees both sides
// uniformly. sid = render.KeeperTargetSID, correlation_id = apply_id (matches
// the SelectChangedTaskKeys filter).
//
// Secret hygiene: register_data/output are NOT put in the payload at all (keeper
// tasks may carry vault-resolved output) — which is why the per-field redaction
// the Soul-side handler does ([ADR-0083] §8) has nothing to redact here. message
// only on failure. This is the audit path (changed_tasks/Tiding fold); SSE
// visibility of keeper-side progress is a separate channel,
// [Runner.publishKeeperTaskExecuted] (ADR-068 §A2).
//
// Audit=nil (unit build without audit) → no-op. A write error is only logged:
// losing the event degrades observability but doesn't fail the run.
func (r *Runner) emitKeeperTaskExecuted(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, changed, failed bool, message string, log *slog.Logger) {
	if r.deps.Audit == nil {
		return
	}
	in := audit.TaskExecutedInput{
		SID:     render.KeeperTargetSID,
		ApplyID: applyID,
		TaskIdx: rt.Index,
		// keeper-side tasks (`on: keeper`) execute as ONE row per (apply_id, keeper,
		// passage) — no per-host where:, local position always equals the global
		// RenderedTask.Index. plan_index == task_idx == rt.Index (plan-wide across
		// all Passages → correlation key unique between Passages).
		PlanIndex: rt.Index,
		Status:    keeperTaskStatus(changed, failed).String(),
		// This task's passage (Slice 2: keeper tasks are stratified by Passage). In
		// the payload for per-Passage triage; doesn't affect changed_tasks
		// correlation (that goes by sid/plan_index). 0 for N=1 / Passage-0 tasks.
		Passage: passage,
	}
	if failed {
		in.Error = &audit.TaskExecutedError{
			Module:  rt.Module,
			Message: message,
		}
	}
	ev := &audit.Event{
		EventType:     audit.EventTaskExecuted,
		Source:        audit.SourceKeeperInternal,
		CorrelationID: applyID,
		Payload:       audit.BuildTaskExecutedPayload(in),
	}
	if err := r.deps.Audit.Write(ctx, ev); err != nil {
		log.Warn("scenario: writing audit task.executed for the keeper task failed",
			slog.Int("task_idx", rt.Index), slog.String("module", rt.Module), slog.Any("error", err))
	}
}

// publishKeeperTaskExecuted relays keeper-side task.executed onto the SSE
// channel via applybus (ADR-068 §A2) — MIRRORS Soul-side [publishTaskExecuted]
// (grpc/events_taskevent.go): same payload shape (snake_case keys, same field
// set), sid = [render.KeeperTargetSID]. Without this, `on: keeper` steps
// (cloud/vault/registered) are silent in the live run view.
//
// ★SECRET HYGIENE (same as Soul-side): SSE does NOT carry output/register_data/
// message (keeper tasks may carry vault-resolved output); on failed — only
// error{code,module}, no message. The final [audit.MaskSecrets] on the SSE
// write path is a second barrier. task_idx/passage are int32 (type parity with
// Soul-side proto getters).
//
// ApplyBus=nil (single-Keeper dev / unit without SSE) → no-op, same as Soul-side.
func (r *Runner) publishKeeperTaskExecuted(applyID string, passage int, rt *render.RenderedTask, changed, failed bool) {
	if r.deps.ApplyBus == nil {
		return
	}
	payload := map[string]any{
		"apply_id":    applyID,
		"kind":        string(applybus.KindTaskExecuted),
		"sid":         render.KeeperTargetSID,
		"task_idx":    int32(rt.Index),
		"task_status": keeperTaskStatus(changed, failed).String(),
		"passage":     int32(passage),
	}
	if failed {
		// keeper-side carries no structured error.code (module returned a
		// message/gRPC error); code="" is honest, keys code+module mirror
		// Soul-side. message (stderr) is NOT put in SSE — floor for all failed
		// tasks (BUG-3, same as Soul-side).
		payload["error"] = map[string]any{
			"code":   "",
			"module": rt.Module,
		}
	}
	r.deps.ApplyBus.Publish(applybus.Event{
		ApplyID: applyID,
		Kind:    applybus.KindTaskExecuted,
		Payload: payload,
	})
}

// keeperTaskStatus maps a keeper task's outcome (changed/failed) to the
// keeperv1 enum so the task.executed payload carries the same status string as
// Soul-side (Status().String()). The changed fold filters on the literal
// "TASK_STATUS_CHANGED" (auditpg). keeper-side doesn't distinguish timed_out
// (the module either returns a failed event or a gRPC error) — failed suffices.
func keeperTaskStatus(changed, failed bool) keeperv1.TaskStatus {
	switch {
	case failed:
		return keeperv1.TaskStatus_TASK_STATUS_FAILED
	case changed:
		return keeperv1.TaskStatus_TASK_STATUS_CHANGED
	default:
		return keeperv1.TaskStatus_TASK_STATUS_OK
	}
}

// applyKeeperTask calls a keeper-side module in-process and folds its
// ApplyEvent stream into a final result (changed/failed/output/message),
// mirroring Soul-side runTask (selfRegisterData). Module address splits into
// (base, state) via the same config.SplitModuleAddr as Soul-side plantask/
// applyrunner: the registry indexes modules by base (`core.cloud`), state
// (`created`) goes into ApplyRequest.state. A malformed address or a module
// found in neither registry → failed (like Soul on an unknown module). Apply
// returning a gRPC error (not a failed event) → failed with the error text.
//
// TWO registries, asked in this order (NIM-758): the built-in keeper-side core
// modules first, then the PLUGIN modules that declared `side: keeper`
// ([ADR-0087], [Runner.lookupKeeperPlugin]). Core first is the same precedence
// the Soul side uses for its composite registry — a plugin registered under a
// core address cannot shadow the module that really runs.
//
// This wrapper exists for the masking (see [Runner.maskKeeperTaskMessage]),
// which has to be the LAST thing that happens to the message and must not be
// something a later return path can slip past. The work is in
// [Runner.runKeeperTask].
//
// The run's incarnation travels on the module context (coremod/util runctx):
// ApplyRequest carries no run context, and a keeper-side module needs the owner
// to tell its own leftovers from another incarnation's hosts (NIM-170). The
// service name and the service's `state_schema` ride the same channel, for a
// module whose Vault path is DERIVED from the run's owner rather than authored
// ([ADR-0083] §4). stateSchema is the loaded artifact's map, shared not copied —
// every reader treats it as immutable.
func (r *Runner) applyKeeperTask(ctx context.Context, spec RunSpec, stateSchema config.InputSchemaMap, rt *render.RenderedTask, sealedPaths map[string]bool) (changed, failed bool, output map[string]any, message string) {
	changed, failed, output, message = r.runKeeperTask(ctx, spec, stateSchema, rt)
	return changed, failed, output, maskKeeperTaskMessage(rt, sealedPaths, message)
}

// lookupKeeperPlugin resolves a keeper-side task address the core registry does
// not know against the plugins this Keeper discovered and allow-listed. The
// error it returns IS the operator-facing failure message, so its three shapes
// are the whole contract:
//
//   - no plugin registry, or an address nothing declares → `unknown keeper-side
//     module %q`, byte-identical to what this path answered before keeper-side
//     plugins existed. NIM-688 asks for exactly that: an unknown address must
//     stay a fast, readable refusal and not become a plugin-spawn timeout;
//   - a registered module that declares the Soul side → a refusal naming the
//     side. This is the case the acceptance is about. Such a task must NOT
//     quietly go to a host — nothing would run it there either, and the author
//     would be reading "unknown module" about a module that exists;
//   - a module declaring `side: keeper` → the executable handle.
//
// The registry only ever hands back a keeper-side module, so the second case
// cannot be reached by getting the order of these branches wrong.
func (r *Runner) lookupKeeperPlugin(base, addr string) (module.SoulModule, error) {
	if r.keeperPlugins == nil {
		return nil, fmt.Errorf("unknown keeper-side module %q", addr)
	}
	if mod, ok := r.keeperPlugins.LookupKeeperSide(base); ok {
		return mod, nil
	}
	if side, known := r.keeperPlugins.DeclaredSide(base); known {
		return nil, fmt.Errorf("plugin module %q declares side=%s and does not execute on the keeper: a keeper-side step needs `side: keeper` in the module's schema document", base, side)
	}
	return nil, fmt.Errorf("unknown keeper-side module %q", addr)
}

// runKeeperTask is [Runner.applyKeeperTask] without the masking of its result
// message — see that function for the contract. Split out so masking has one
// place to happen rather than one per return.
func (r *Runner) runKeeperTask(ctx context.Context, spec RunSpec, stateSchema config.InputSchemaMap, rt *render.RenderedTask) (changed, failed bool, output map[string]any, message string) {
	base, state, ok := config.SplitModuleAddr(rt.Module)
	if !ok {
		return false, true, nil, fmt.Sprintf("invalid keeper-side module address %q (want <namespace>.<module>.<state>)", rt.Module)
	}
	mod, ok := r.keeperModules.Lookup(base)
	if !ok {
		var perr error
		if mod, perr = r.lookupKeeperPlugin(base, rt.Module); perr != nil {
			return false, true, nil, perr.Error()
		}
	}

	// The §7 fence, post-render half ([ADR-0083]): `core.vault.*` reaches Vault
	// through the module rather than through vault(), so the CEL guard never sees
	// it, and the load-time scan saw `${ vars.p }` instead of a path. Here the
	// interpolation is gone.
	if fdiags := config.ScanRenderedVaultParams(rt.Module, spec.ServiceRef.Name, rt.Params.AsMap()); len(fdiags) > 0 {
		return false, true, nil, fmt.Sprintf("%s: %s", fdiags[0].Code, fdiags[0].Message)
	}

	req := &pluginv1.ApplyRequest{
		State:  state,
		Params: rt.Params,
	}
	// Run scope for the module: incarnation (whose leftovers are whose), plus
	// service and `state_schema` for a module that DERIVES a Vault path from
	// (service, incarnation, state field, key) ([ADR-0083] §1).
	modCtx := coremodutil.WithIncarnation(ctx, spec.IncarnationName)
	modCtx = coremodutil.WithService(modCtx, spec.ServiceRef.Name)
	modCtx = coremodutil.WithStateSchema(modCtx, stateSchema)
	// The run's own identity, for a module that WRITES `incarnation.state` at the
	// step ([ADR-0084]): the `state_history` row it commits has to name the run
	// that caused the change, and there is no end-of-run commit left to name it.
	modCtx = coremodutil.WithRunScope(modCtx, coremodutil.RunScope{
		Scenario:     spec.ScenarioName,
		ApplyID:      spec.ApplyID,
		StartedByAID: spec.StartedByAID,
	})
	// The merge-time CEL evaluators, for a capture verb whose predicate runs per
	// element (`add` dedup, `modify`/`remove` matching) and therefore cannot be
	// folded render-side. Bound to the run's service so they stay inside the §7
	// vault fence, exactly as the retired `state_changes` merge had them.
	matchEval, opEval := r.deps.Render.StateOpEvaluators(ctx, spec.ServiceRef.Name)
	modCtx = coremodutil.WithStateOpEvaluators(modCtx, coremodutil.StateOpEvaluators{
		Match: matchEval,
		Op:    opEval,
	})
	sink := newKeeperApplyStream(modCtx)
	if err := mod.Apply(req, sink); err != nil {
		return false, true, nil, err.Error()
	}

	last := sink.last()
	if last == nil {
		// No final event from the module — treated as a contract error (Soul-side
		// also considers a missing final event anomalous).
		return false, true, nil, "keeper-side module produced no final event"
	}
	if last.GetFailed() {
		return false, true, nil, last.GetMessage()
	}
	var out map[string]any
	if o := last.GetOutput(); o != nil {
		out = o.AsMap()
	}
	return last.GetChanged(), false, out, last.GetMessage()
}

// maskKeeperTaskMessage redacts, from a keeper-side task's result message,
// every value the task's own params carry at a SEALED path — the run's
// render-time record of which cells came from a secret source ([ADR-010] §7.4).
//
// ★ Why this exists at all. Until keeper-side plugins, every module on this path
// was ours and reached Vault itself, so a params cell was not where its secrets
// were. A plugin has no Vault access and is GIVEN its credentials as params
// (NIM-757, user decision 2026-09-01) — and then writes the message. A module
// that echoes what it was handed ("connect to host=… token=… failed") turns the
// one channel it controls into a leak, and that message is observable four ways:
// the audit task.executed payload, `apply_runs.error_summary`, the run's abort
// reason, and the operator's view of each. The write-path maskers cannot save
// it: [audit.MaskSecrets] masks a value whose KEY looks sensitive or which IS a
// `vault:` ref, and a resolved credential quoted mid-sentence is neither;
// [audit.MaskSecretsSealed] masks a payload cell AT a sealed path, and a message
// is one string, not the params tree. Knowing the values is the only thing that
// works, and the seal is the platform's record of which they are — the same
// mechanism `RenderedTask.SecretOutput` points at for the params it does not
// cover.
//
// Applied at one point, to every message the task can produce and to core
// modules as well as plugins: masking that only runs on the failure path is
// masking that runs on the path where nobody was looking. (In practice only a
// FAILED task's message reaches a channel — the audit payload carries it inside
// `error` — but the mask is not what should be deciding that.)
//
// Four bounds, stated rather than implied:
//
//   - it redacts values the RENDER knew to be secret. A credential a module
//     invents at runtime, or one written as a plaintext literal in the scenario,
//     is not distinguishable from any other text — the same bound the console and
//     errand surfaces state (ADR-0074 amendment 2026-07-27);
//   - STRING leaves only. A number or a bool at a sealed path is left alone:
//     masking `true` out of a diagnostic would destroy the diagnostic and protect
//     nothing that could be recognised in text anyway. Composites ARE covered —
//     see [render.SealedValues], which masks a sealed subtree whole;
//   - the secret has to have been SEALED at render, and the seal does not follow
//     a `vars:` hop. `cel.SealSources.SealedVars` is never populated by anything
//     in this tree, so `params: {pw: "${ vars.db_pw }"}` is unsealed even when
//     `vars.db_pw` is itself `${ vault(…) }` — and routing a secret through vars
//     is the commonest idiom in `examples/`. A predating hole in the seal, not one
//     this path opened, but it bounds this masking exactly as much;
//   - it masks the MESSAGE and nothing else. The module's `output` is written to
//     `apply_task_register` verbatim by [Runner.accumulateKeeperRegister] — on
//     purpose, since that is the value the next task reads through
//     `register.<name>.<field>`, and redacting it would break the chain rather
//     than protect it. `secret: true` on an output field does NOT change that
//     write: keeper-side, what the declaration buys is that
//     [render.secretOutputRegisters] seals the register NAME, so a later cell
//     reading it is masked where IT becomes observable. A plugin that echoes a
//     credential into `output` still lands it in the register row in plaintext;
//   - a very short sealed value over-masks — see [render.SealedValues], which
//     also carries the non-secret siblings of a sealed subtree. Safe direction,
//     paid in diagnostics.
func maskKeeperTaskMessage(rt *render.RenderedTask, sealedPaths map[string]bool, message string) string {
	if message == "" || len(sealedPaths) == 0 || rt.Params == nil {
		return message
	}
	secrets := render.SealedValues(rt.Params.AsMap(), sealedPaths)
	if len(secrets) == 0 {
		return message
	}
	// Longest first: a short secret that is a substring of a longer one must not
	// consume the prefix of the longer and leave its tail in the clear.
	ordered := append([]string(nil), secrets...)
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	for _, secret := range ordered {
		message = strings.ReplaceAll(message, secret, audit.MaskedValue)
	}
	return message
}

// accumulateKeeperRegister writes a keeper task's register result into
// apply_task_register under KeeperTargetSID — same path as Soul-side
// accumulateRegister (events_taskevent.go). Payload is {changed, failed,
// timed_out, skipped} + the module's output fields (mirrors selfRegisterData
// in applyrunner.go). A task without register: → no-op (buildRegisterByHost
// has no name to resolve it under anyway). Errors are only logged
// (best-effort, like Soul-side accumulateRegister).
//
// The module's declared-secret output fields ([ADR-0083] §8) are stored HERE
// unredacted, deliberately and symmetrically with the Soul-side register: this
// is the value the NEXT task reads through `register.<name>.<field>`, and
// redacting it would break the chain rather than protect it. Redaction belongs
// to the observable copies (audit/SSE), which this is not.
//
// passage (Slice 2): FK apply_task_register→apply_runs is the triple
// (apply_id, sid, passage) (migration 078). A Passage P keeper task's register
// MUST reference the keeper apply_runs row for exactly passage P (inserted in
// dispatchKeeperTasks before the loop over this Passage's keeper tasks).
// Otherwise the FK targets (apply_id, keeper, 0), which for P>0 doesn't exist,
// losing the register and breaking the keeper→keeper Passage P→P+1 chain.
func (r *Runner) accumulateKeeperRegister(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, changed, failed bool, output map[string]any, log *slog.Logger) {
	if rt.Register == "" {
		return
	}
	data := map[string]any{
		"changed":   changed,
		"failed":    failed,
		"timed_out": false,
		"skipped":   false,
	}
	for k, v := range output {
		data[k] = v
	}
	if err := applyrun.UpsertTaskRegister(ctx, r.deps.DB, &applyrun.TaskRegister{
		ApplyID: applyID,
		SID:     render.KeeperTargetSID,
		// keeper task executes locally with the global rt.Index — both the
		// correlation key (PlanIndex) and the informational TaskIdx: keeper-side has
		// no per-Passage ApplyRequest slice, so local==global index (ADR-056 §S1 fix
		// Variant B). buildRegisterByHost resolves the name by PlanIndex.
		PlanIndex:    rt.Index,
		TaskIdx:      rt.Index,
		RegisterData: data,
		// passage of the keeper task — part of the FK to apply_runs(apply_id, sid,
		// passage) + the accumulation filter loadRegisterByHostUpToPassage (register Passage<P).
		Passage: passage,
	}); err != nil {
		log.Warn("scenario: accumulating register for the keeper task failed",
			slog.Int("passage", passage), slog.Int("task_idx", rt.Index), slog.Any("error", err))
	}
}

// registeredModuleBase is the base address of the keeper-side core module
// core.soul.registered (coremod/soul.Name). Kept as a local constant: scenario
// does NOT import coremod/soul (it pulls PG-store/presence deps) — the bind
// boundary is recognized by task address, not module type.
const (
	registeredModuleBase  = "core.soul"
	registeredModuleState = "registered"
)

// recordKeeperFailure records a keeper-task failure (RecordTaskFailure +
// apply_run → failed) for the triple (apply_id, keeper, passage). rt.Index is
// the global plan-wide index; for keeper-target local==global (no per-host
// where:), so task_idx and plan_index both equal rt.Index. Write errors are
// only logged (losing the reason degrades triage but the run still aborts).
func (r *Runner) recordKeeperFailure(ctx context.Context, applyID string, passage int, rt *render.RenderedTask, summary string, log *slog.Logger) {
	if rerr := applyrun.RecordTaskFailure(ctx, r.deps.DB, applyID, render.KeeperTargetSID, passage, rt.Index, rt.Index, summary); rerr != nil {
		log.Warn("scenario: recording the keeper task failure reason failed",
			slog.Int("passage", passage), slog.Int("task_idx", rt.Index), slog.Any("error", rerr))
	}
	if uerr := applyrun.UpdateStatus(ctx, r.deps.DB, applyID, render.KeeperTargetSID, passage, applyrun.StatusFailed, &summary); uerr != nil {
		log.Warn("scenario: transitioning keeper apply_run to failed failed",
			slog.Int("passage", passage), slog.Any("error", uerr))
	}
}

// bindMembershipOnRegistered writes incarnation membership for a successful
// core.soul.registered task (ADR-008 amendment 2026-07-17/NIM-124: membership is
// a first-class relation, no longer `incarnation.name ∈ souls.coven[]`). The
// current run's incarnation (spec.IncarnationName) is the unambiguous target —
// cross-incarnation binding is forbidden by the grammar. Idempotent (ON CONFLICT
// DO NOTHING). Gated on the registered module specifically; other keeper tasks
// (cloud/vault) don't bind hosts.
//
// Empty incarnation name (direct keeper test without an incarnation) / nil DB /
// no SIDs in the module output → no-op, nil. Any DB error is returned so the
// caller fails the run (the module already created the souls rows, so the FK
// sid → souls is satisfied; an error here is a real DB fault, not a missing row).
func (r *Runner) bindMembershipOnRegistered(ctx context.Context, spec RunSpec, rt *render.RenderedTask, output map[string]any) error {
	base, state, ok := config.SplitModuleAddr(rt.Module)
	if !ok || base != registeredModuleBase || state != registeredModuleState {
		return nil
	}
	if spec.IncarnationName == "" || r.deps.DB == nil {
		return nil
	}
	sids := sidsFromRegisteredOutput(output)
	if len(sids) == 0 {
		return nil
	}
	return incarnation.AddMembers(ctx, r.deps.DB, spec.IncarnationName, sids, startedByPtr(spec.StartedByAID))
}

// sidsFromRegisteredOutput extracts the registered SIDs from a
// core.soul.registered module output: `sid` is a string (single) or a list of
// strings (batch), matching buildOutput in coremod/soul. Empty/other → nil.
func sidsFromRegisteredOutput(output map[string]any) []string {
	raw, ok := output["sid"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// keeperRegisterBucket extracts the flat keeper-task register bucket from the
// run's per-host register table: entries under the synthetic host
// KeeperTargetSID ("keeper"), where accumulateKeeperRegister writes keeper-side
// task register results. Returns register-name → payload (same shape as flat
// RenderInput.Register), or nil if the keeper bucket is empty/absent.
//
// Purpose (staged render, keeper→keeper register chaining): the stage loop in
// run.go carries the result into the ISOLATED renderIn.KeeperRegister before
// per-passage render of the active Passage's keeper tasks. keeperVars
// (render/dispatch.go) reads KeeperRegister specifically — so a keeper task on
// Passage N sees `register.<prev>.*` from keeper tasks of earlier Passages,
// while the host fallback (hostRegister) does NOT get keeper-register (the
// channel is separate from flat Register). registerByHost is whatever
// loadRegisterByHostUpToPassage returned (register for Passage < active), so
// the bucket only carries ALREADY completed keeper tasks (forward-only).
func keeperRegisterBucket(registerByHost map[string]map[string]any) map[string]any {
	bucket := registerByHost[render.KeeperTargetSID]
	if len(bucket) == 0 {
		return nil
	}
	return bucket
}

// keeperTasksOf selects RenderedTasks whose DispatchPlan is marked Keeper=true
// (render.IsKeeperTask) AND whose RenderedTask.Passage == passage, in Index
// order (= scenario.tasks[] order).
//
// Passage filter (staged render, ADR-056, Slice 2): keeper tasks are
// stratified by register dependency like host tasks (core.bootstrap.delivered
// reads register.provision.* → Passage strictly AFTER core.cloud.created).
// dispatchKeeperTasks is called per-Passage on tasks re-rendered for
// ActivePassage=p; only this Passage's keeper tasks are selected here. A
// future Passage's (>p) keeper task on this render is a placeholder without
// Params (pipeline.go placeholder gate) AND without Keeper=true in the plan,
// so the Keeper filter alone would already exclude it; the Passage filter is a
// second boundary (on a fresh render of Passage p, its staged keeper task
// carries Keeper=true). N=1 run: single Passage 0, all keeper tasks have
// passage==0 → selected as before this epic (bit-for-bit).
func keeperTasksOf(tasks []*render.RenderedTask, plans []render.DispatchPlan, passage int) []*render.RenderedTask {
	byIndex := make(map[int]*render.RenderedTask, len(tasks))
	for _, t := range tasks {
		byIndex[t.Index] = t
	}
	out := make([]*render.RenderedTask, 0)
	for _, p := range plans {
		if !p.Keeper {
			continue
		}
		t := byIndex[p.TaskIndex]
		if t == nil || t.Passage != passage {
			continue
		}
		out = append(out, t)
	}
	return out
}

// composeKeeperFailure builds the operator-facing failure reason for a keeper
// task, for apply_runs.error_summary (format `task <idx> <module>: <message>`,
// mirrors composeTaskErrorSummary on Soul-side). The message is masked below,
// not suppressed: the per-task `no_log:` that used to blank it is gone
// ([ADR-0083] §8).
func composeKeeperFailure(rt *render.RenderedTask, message string) string {
	head := fmt.Sprintf("task %d %s", rt.Index, rt.Module)
	if message == "" {
		return head
	}
	// This run's sealed paths aren't passed to a keeper-task summary (narrow
	// point building the task's error text) → nil: vault+regex layers +
	// regex-alarm (ADR-010 §7.4) still apply. A resolved keeper-task message is
	// the module's message, not a sealed-path value; vault-ref/sensitive-by-name
	// are covered by those layers regardless.
	return head + ": " + maskErrText(fmt.Errorf("%s", message), nil)
}

// keeperApplyStream is an in-proc grpc.ServerStreamingServer[ApplyEvent]
// implementation for calling a keeper-side core module locally (mirrors
// inProcApplyStream in the Soul-side runtime). Buffers ApplyEvents; the caller
// looks at the last one.
type keeperApplyStream struct {
	grpc.ServerStream
	ctx    context.Context
	events []*pluginv1.ApplyEvent
}

func newKeeperApplyStream(ctx context.Context) *keeperApplyStream {
	return &keeperApplyStream{ctx: ctx}
}

func (s *keeperApplyStream) Context() context.Context { return s.ctx }

func (s *keeperApplyStream) Send(ev *pluginv1.ApplyEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *keeperApplyStream) SetHeader(metadata.MD) error  { return nil }
func (s *keeperApplyStream) SendHeader(metadata.MD) error { return nil }
func (s *keeperApplyStream) SetTrailer(metadata.MD)       {}

func (s *keeperApplyStream) SendMsg(m any) error {
	ev, ok := m.(*pluginv1.ApplyEvent)
	if !ok {
		return fmt.Errorf("keeper apply stream: SendMsg got %T, want *pluginv1.ApplyEvent", m)
	}
	return s.Send(ev)
}

func (s *keeperApplyStream) RecvMsg(any) error {
	return fmt.Errorf("keeper apply stream: RecvMsg not supported")
}

func (s *keeperApplyStream) last() *pluginv1.ApplyEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}
