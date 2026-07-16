package pushorch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// syntheticScenarioName is the name of the scenario assembled by pushorch around
// a single apply task. Prefix `_` signals "not from user service repo"; name is
// transient (appears only in render pipeline logs).
const syntheticScenarioName = "_push"

// syntheticTaskName is the name of the single apply task in the synthetic scenario.
// Same: transient, needed only for render phase diagnostics.
const syntheticTaskName = "push.apply"

// orchestratorContextTimeout is the ceiling for async-execution prepare phase
// duration (LoadByInventory + render). Hard cap to prevent hung git-fetch / SQL
// from holding goroutine indefinitely when request-ctx is absent (HTTP handler
// already returned 202). Dispatch phase has its own per-host timeout.
const orchestratorContextTimeout = 30 * time.Minute

// SshDispatcher is a narrow interface of [push.SshDispatcher] for the orchestrator.
// per-host SendApply returns RunResult synchronously (push S1+S5, oneshot).
// `providerName` is the name of the SshProvider plugin selected by ProviderRouter
// (ADR-032 amendment 2026-05-27, P2 W-2/W-3 multi-provider routing); empty string
// or unknown name → push.ErrProviderUnknown.
type SshDispatcher interface {
	SendApply(ctx context.Context, sid string, providerName string, req *keeperv1.ApplyRequest) (*keeperv1.RunResult, error)
}

// Cleaner is a narrow interface of [push.SshDispatcher.Cleanup] for best-effort
// post-success cleanup of stale versions (`cleanup_stale_versions: true`).
// The same *push.SshDispatcher satisfies both interfaces — wire-up passes it to
// both fields. `providerName` is the same one used in the preceding SendApply
// (caller maintains per-SID decision).
type Cleaner interface {
	Cleanup(ctx context.Context, sid string, providerName string) error
}

// ProviderRouter is a narrow interface of [push.ProviderRouter] for the orchestrator.
// Dependency narrowed to one method for easy mocking in unit tests.
type ProviderRouter interface {
	RouteFor(ctx context.Context, sid string) (providerName string, source push.RouteSource, err error)
}

// ProviderMetricsObserver is a narrow interface of [push.Metrics.ObserveProviderRouted]
// (P2 W-4). nil → no-op (push without observability setups).
type ProviderMetricsObserver interface {
	ObserveProviderRouted(providerName, decisionSource string)
}

// AuditWriter is a narrow interface of shared/audit.Writer (same interface as
// keeper/internal/mcp.AuditWriter). Narrowed for unit mocks.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// InventoryResolver is a narrow interface of [topology.Resolver.LoadByInventory] for
// PushRun. Narrowing allows mocking in unit tests without raising PG+Redis.
type InventoryResolver interface {
	LoadByInventory(ctx context.Context, sids []string) ([]*topology.HostFacts, error)
}

// RenderPipeline is a narrow interface of [render.Pipeline.Render] (no dependency
// on *render.Pipeline in Deps signature). *render.Pipeline satisfies it.
type RenderPipeline interface {
	Render(ctx context.Context, in render.RenderInput) ([]*render.RenderedTask, []render.DispatchPlan, error)
}

// Deps are external dependencies of PushRun. All non-Audit fields are required;
// AuditWriter is optional (nil → audit events not written, diagnostics remain
// in logs). KID is the Keeper instance identifier for started_by_kid (Reaper
// purge_orphan_push_runs filters orphaned runs by it).
type Deps struct {
	Store         *Store
	Topology      InventoryResolver
	Render        RenderPipeline
	DestinyLoader DestinyArtifactLoader
	Template      DestinyTemplateSource
	Dispatcher    SshDispatcher
	Cleaner       Cleaner
	// Router is a 3-tier ProviderRouter (P2 W-3). Required in multi-provider setup.
	// Per-SID resolution happens before dispatch phase; resolution error
	// (ErrProviderNotRouted) maps to per-host status="error" +
	// error_code="provider_not_routed".
	Router ProviderRouter
	// ProviderMetrics is the counter for routing decisions (P2 W-4). nil → no-op.
	ProviderMetrics ProviderMetricsObserver
	Audit           AuditWriter
	Logger          *slog.Logger
	KID             string

	// Now is the current time source for tests; production wire-up passes time.Now.
	// nil → time.Now is used.
	Now func() time.Time
}

// PushRun is a multi-host orchestrator for push runs (Variant C).
//
// One instance per process; concurrent-safe (holds no mutable state, everything
// through Store + per-Apply goroutine). Apply is async: returns apply_id and spawns
// goroutine with executeAsync under its own ctx (NOT HTTP request-ctx — it will be
// cancelled after 202).
type PushRun struct {
	deps Deps
}

// NewPushRun validates dependencies and returns the orchestrator. Error return
// indicates misconfiguration by caller (wire-up).
func NewPushRun(deps Deps) (*PushRun, error) {
	if deps.Store == nil {
		return nil, errors.New("pushorch: Store is required")
	}
	if deps.Topology == nil {
		return nil, errors.New("pushorch: Topology is required")
	}
	if deps.Render == nil {
		return nil, errors.New("pushorch: Render is required")
	}
	if deps.DestinyLoader == nil {
		return nil, errors.New("pushorch: DestinyLoader is required")
	}
	if deps.Template == nil {
		return nil, errors.New("pushorch: DestinyTemplateSource is required")
	}
	if deps.Dispatcher == nil {
		return nil, errors.New("pushorch: Dispatcher is required")
	}
	if deps.Router == nil {
		return nil, errors.New("pushorch: Router is required (multi-provider routing, ADR-032 amendment 2026-05-27)")
	}
	if deps.Logger == nil {
		return nil, errors.New("pushorch: Logger is required")
	}
	if deps.KID == "" {
		return nil, errors.New("pushorch: KID is required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &PushRun{deps: deps}, nil
}

// ApplyRequest is input for PushRun.Apply (HTTP handler / MCP tool map body).
// Host-side fields (DestinyRef, SSHProvider) match `PushApplyRequest` in
// docs/keeper/operator-api.md -> Push endpoints.
type ApplyRequest struct {
	InventorySIDs []string
	DestinyRef    string // "<name>@<ref>"
	SSHProvider   string
	Input         map[string]any
	CleanupStale  bool
	StartedByAID  string
}

// Apply receives a push run, performs Insert(pending), and spawns an async goroutine
// with executeAsync. Returns apply_id (ULID) for 202 response.
//
// Validation is at the HTTP/MCP boundary (parse destiny, inventory non-empty); here
// we do defense-in-depth: ParseDestinyRef fails with sentinel → caller maps to 422.
func (r *PushRun) Apply(ctx context.Context, req ApplyRequest) (applyID string, err error) {
	if len(req.InventorySIDs) == 0 {
		return "", errors.New("pushorch: inventory must be non-empty")
	}
	name, ref, err := ParseDestinyRef(req.DestinyRef)
	if err != nil {
		return "", err
	}

	applyID = audit.NewULID()
	row := PushRunRow{
		ApplyID:       applyID,
		InventorySIDs: req.InventorySIDs,
		DestinyRef:    req.DestinyRef,
		SSHProvider:   req.SSHProvider,
		Input:         req.Input,
		CleanupStale:  req.CleanupStale,
		Status:        StatusPending,
		StartedByAID:  req.StartedByAID,
		StartedByKID:  r.deps.KID,
	}
	if err := r.deps.Store.Insert(ctx, row); err != nil {
		return "", err
	}

	// Audit event push.applied (run start) — parallel to incarnation.scenario_started:
	// written on request receipt, before executeAsync starts. Payload does not carry
	// full inventory (may be huge); numbers are enough for correlation with
	// GET /v1/push/{apply_id}.
	r.writeAudit(ctx, audit.EventPushApplied, req.StartedByAID, map[string]any{
		"apply_id":       applyID,
		"destiny":        req.DestinyRef,
		"inventory_size": len(req.InventorySIDs),
		"ssh_provider":   req.SSHProvider,
		"cleanup_stale":  req.CleanupStale,
	})

	// Goroutine runs its own bg-ctx with timeout-cap: HTTP-ctx will be cancelled
	// right after 202. orchestratorContextTimeout is the ceiling for prepare phase;
	// per-host dispatch uses the same bg-ctx without additional cancel layer
	// (SshDispatcher holds DialTimeout internally).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), orchestratorContextTimeout)
		defer cancel()
		r.executeAsync(bgCtx, applyID, name, ref, req)
	}()

	return applyID, nil
}

// GetRow reads the current state of a push run by apply_id from push_runs.
// Thin wrapper over Store.Get — left as PushRun method for symmetry with Apply
// (handler and MCP-tool work through one object).
func (r *PushRun) GetRow(ctx context.Context, applyID string) (*PushRunRow, error) {
	return r.deps.Store.Get(ctx, applyID)
}

// ListRows is a global list of push runs (`GET /v1/push-runs`, UI-4). Thin wrapper
// over Store.SelectAll, symmetric to GetRow: handler and MCP-tool go through the
// orchestrator object, not Store directly.
func (r *PushRun) ListRows(ctx context.Context, filter ListFilter, offset, limit int) ([]*PushRunRow, int, error) {
	return r.deps.Store.SelectAll(ctx, filter, offset, limit)
}

// executeAsync is the main execution flow of a run. Steps:
//
//  1. MarkRunning;
//  2. LoadByInventory (filter terminal/onboarding + lease-presence);
//  3. assemble synthetic ScenarioManifest + pushDestinyResolver, run through
//     render.Pipeline.Render (destinyIsolated by design — register/state/essence/
//     soulprint.hosts are unavailable);
//  4. ToProtoTasks + ApplyRequest for each targeted SID;
//  5. per-host SendApply via SshDispatcher (concurrent, see fanOut);
//  6. assemble summary {hosts: [{sid, status, error?}], total, success_count,
//     fail_count} + terminal state (success/partial_failed/failed);
//  7. cleanup_stale_versions=true → best-effort Cleanup per-host.
func (r *PushRun) executeAsync(ctx context.Context, applyID, name, ref string, req ApplyRequest) {
	log := r.deps.Logger.With(slog.String("apply_id", applyID), slog.String("destiny", req.DestinyRef))

	if err := r.deps.Store.MarkRunning(ctx, applyID); err != nil {
		log.Error("pushorch: mark running failed — run did not start", slog.Any("error", err))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "mark_running_failed: " + err.Error(),
		}, req.StartedByAID, req)
		return
	}

	hosts, err := r.deps.Topology.LoadByInventory(ctx, req.InventorySIDs)
	if err != nil {
		log.Error("pushorch: inventory load failed", slog.Any("error", err))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "inventory_load_failed: " + err.Error(),
		}, req.StartedByAID, req)
		return
	}
	if len(hosts) == 0 {
		log.Warn("pushorch: no live hosts in inventory — run cancelled",
			slog.Int("requested", len(req.InventorySIDs)))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error":     "no_live_hosts",
			"requested": len(req.InventorySIDs),
		}, req.StartedByAID, req)
		return
	}

	resolver := newPushDestinyResolver(r.deps.DestinyLoader, r.deps.Template, name, ref)
	manifest := &config.ScenarioManifest{
		Name: syntheticScenarioName,
		Tasks: []config.Task{
			{
				Name: syntheticTaskName,
				Apply: &config.ApplyTask{
					Destiny: name,
					Input:   req.Input,
				},
			},
		},
	}
	renderIn := render.RenderInput{
		Scenario: manifest,
		Input:    req.Input,
		Hosts:    hosts,
		Destiny:  resolver,
		// Essence/Register/RegisterByHost are empty: push run is not tied to
		// an incarnation, scenario-scope is unavailable (same logic as destiny phase
		// of scenario-runner: render-pipeline itself guarantees destiny isolation).
		Incarnation: render.IncarnationMeta{Name: syntheticScenarioName},
	}

	tasks, plans, rerr := r.deps.Render.Render(ctx, renderIn)
	if rerr != nil {
		log.Error("pushorch: render failed", slog.Any("error", rerr))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "render_failed: " + rerr.Error(),
		}, req.StartedByAID, req)
		return
	}
	if len(tasks) == 0 {
		// Destiny without tasks — formally correct artifact, but push loses its purpose.
		log.Warn("pushorch: destiny rendered to empty plan — nothing to dispatch")
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "empty_plan",
		}, req.StartedByAID, req)
		return
	}

	// Push run targeting: union across all plans (in pilot — usually one plan per
	// apply task). If multiple tasks in destiny target different subsets — take union
	// (push semantics: "run over inventory", not per-task orchestration). plan.TargetSIDs
	// already sorted (resolveTargets).
	target := unionTargetSIDs(plans)
	if len(target) == 0 {
		log.Warn("pushorch: no host remained after where-filter — run skipped")
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "no_targets_after_where",
		}, req.StartedByAID, req)
		return
	}

	protoTasks := render.ToProtoTasks(tasks)

	// P2 W-3 routing phase. Runs BEFORE fanOut: per-SID routing miss must not open
	// SSH session and must not consume plugin env-payload.
	// α-compat (PM-decision): non-empty req.SSHProvider → per-job preset applied to
	// ALL SIDs, overrides router. Otherwise router.RouteFor per-SID; error → per-host
	// status="error" + error_code="provider_not_routed".
	sidProvider, routingResults := r.resolveProviders(ctx, target, req, log)

	// hostResults are collected by target; for SIDs where routing failed, there's
	// already an entry in routingResults (we exclude them from dispatch list).
	dispatchTargets := make([]string, 0, len(target))
	for _, sid := range target {
		if _, failed := routingResults[sid]; failed {
			continue
		}
		dispatchTargets = append(dispatchTargets, sid)
	}

	hostResults := r.fanOut(ctx, applyID, dispatchTargets, sidProvider, protoTasks, log)
	// Merge: routing failures (no dispatch) + dispatch results.
	if len(routingResults) > 0 {
		for sid, hr := range routingResults {
			_ = sid
			hostResults = append(hostResults, hr)
		}
		// Deterministic per-SID order in summary.hosts — re-sort.
		sortHostResults(hostResults)
	}

	status, summary := summarize(hostResults)
	r.finalize(ctx, applyID, status, summary, req.StartedByAID, req)

	// Best-effort cleanup of stale versions on hosts (cleanup_stale_versions).
	// Runs AFTER finalization so run terminate status is not blocked by
	// SSH roundtrips of cleanup. All errors go to logs, not summary.
	if req.CleanupStale && r.deps.Cleaner != nil {
		go r.cleanupHosts(dispatchTargets, sidProvider, log)
	}
}

// resolveProviders resolves provider name for each SID in inventory.
//
// α-compat (PM-decision P2 W-3): if req.SSHProvider is non-empty — preset is
// applied to ALL SIDs, ProviderRouter is NOT called. Source in audit summary
// is marked as "soul" (per-job override is semantically equivalent to per-SID
// explicit for all targets).
//
// Without preset: for each SID call router.RouteFor. ErrProviderNotRouted →
// hostResult with status="error" + errText="provider_not_routed" placed in
// routingResults[sid]. Real PG error → same, errText contains underlying message
// (transient — operator retries).
//
// Return:
//   - sidProvider: map[sid]provider, populated for SUCCESSFULLY resolved SIDs
//     (including α-compat preset);
//   - routingResults: map[sid]hostResult for SIDs where routing failed
//     (caller adds them to final hosts[] without dispatch).
func (r *PushRun) resolveProviders(ctx context.Context, target []string, req ApplyRequest, log *slog.Logger) (map[string]string, map[string]hostResult) {
	sidProvider := make(map[string]string, len(target))
	routingResults := make(map[string]hostResult)

	if req.SSHProvider != "" {
		// α-compat: per-job preset, single provider for all SIDs.
		for _, sid := range target {
			sidProvider[sid] = req.SSHProvider
			observeRouted(r.deps.ProviderMetrics, req.SSHProvider, push.SourceSoul.String())
		}
		log.Info("pushorch: α-compat ssh_provider preset applied to all SIDs",
			slog.String("provider", req.SSHProvider),
			slog.Int("count", len(target)))
		return sidProvider, routingResults
	}

	for _, sid := range target {
		providerName, source, rerr := r.deps.Router.RouteFor(ctx, sid)
		if rerr != nil {
			errCode := "provider_not_routed"
			if !errors.Is(rerr, push.ErrProviderNotRouted) {
				errCode = "provider_route_failed"
			}
			log.Warn("pushorch: routing failed",
				slog.String("sid", sid),
				slog.String("error_code", errCode),
				slog.Any("error", rerr))
			routingResults[sid] = hostResult{
				sid:     sid,
				status:  "error",
				errText: errCode + ": " + rerr.Error(),
			}
			continue
		}
		sidProvider[sid] = providerName
		observeRouted(r.deps.ProviderMetrics, providerName, source.String())
	}
	return sidProvider, routingResults
}

// fanOut runs per-host SendApply in parallel (one goroutine per host), collects
// hostResults in deterministic order (by SID). Concurrency is unlimited (push
// inventory is usually small; large-scale rolling is a separate slice via
// render.DispatchPlan.SerialWidth, not used in pilot).
//
// sidProvider is map sid → provider name (P2 W-3 multi-provider routing).
// SID without entry in map is invariant violation (resolveProviders already
// filtered such), defensive guard inside.
func (r *PushRun) fanOut(ctx context.Context, applyID string, sids []string, sidProvider map[string]string, tasks []*keeperv1.RenderedTask, log *slog.Logger) []hostResult {
	results := make([]hostResult, len(sids))
	var wg sync.WaitGroup
	for i, sid := range sids {
		wg.Add(1)
		providerName := sidProvider[sid]
		go func(idx int, sid string, providerName string) {
			defer wg.Done()
			req := &keeperv1.ApplyRequest{
				ApplyId: applyID,
				Tasks:   tasks,
			}
			rr, err := r.deps.Dispatcher.SendApply(ctx, sid, providerName, req)
			results[idx] = buildHostResult(sid, providerName, rr, err)
			if err != nil {
				log.Warn("pushorch: SendApply failed",
					slog.String("sid", sid),
					slog.String("ssh_provider", providerName),
					slog.Any("error", err))
			} else {
				log.Info("pushorch: per-host run completed",
					slog.String("sid", sid),
					slog.String("ssh_provider", providerName),
					slog.String("status", rr.GetStatus().String()))
			}
		}(i, sid, providerName)
	}
	wg.Wait()
	return results
}

// cleanupHosts runs per-host Cleanup; best-effort, errors → log-warn, do not
// affect run status. Uses its own bg-ctx with the same cap-timeout as executeAsync.
//
// sidProvider is map sid → provider, populated by resolveProviders. SID without
// entry (failed routing) does not reach here (cleanupHosts receives only
// dispatchTargets).
func (r *PushRun) cleanupHosts(sids []string, sidProvider map[string]string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), orchestratorContextTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, sid := range sids {
		wg.Add(1)
		providerName := sidProvider[sid]
		go func(sid string, providerName string) {
			defer wg.Done()
			if err := r.deps.Cleaner.Cleanup(ctx, sid, providerName); err != nil {
				log.Warn("pushorch: post-success cleanup failed",
					slog.String("sid", sid),
					slog.String("ssh_provider", providerName),
					slog.Any("error", err))
				return
			}
			log.Info("pushorch: post-success cleanup OK",
				slog.String("sid", sid),
				slog.String("ssh_provider", providerName))
		}(sid, providerName)
	}
	wg.Wait()
}

// finalize writes terminal state to push_runs + audit event. If MarkTerminal fails
// — log it, don't write audit (event with wrong state is worse than its absence).
// After orphaning, Reaper will catch the record via purge_orphan_push_runs.
func (r *PushRun) finalize(ctx context.Context, applyID string, status PushRunStatus, summary map[string]any, startedByAID string, req ApplyRequest) {
	if err := r.deps.Store.MarkTerminal(ctx, applyID, status, summary); err != nil {
		r.deps.Logger.Error("pushorch: mark terminal failed — record remains running (Reaper will pick it up)",
			slog.String("apply_id", applyID),
			slog.String("status", string(status)),
			slog.Any("error", err))
		return
	}

	eventType := audit.EventPushFailed
	switch status {
	case StatusSuccess:
		eventType = audit.EventPushCompleted
	case StatusPartialFailed:
		eventType = audit.EventPushPartialFailed
	case StatusFailed:
		eventType = audit.EventPushFailed
	case StatusCancelled:
		// Cancelled — written by Reaper, not orchestrator (this path is unreachable
		// from executeAsync). Defensive fallback.
		eventType = audit.EventPushFailed
	}
	r.writeAudit(ctx, eventType, startedByAID, terminalAuditPayload(applyID, req, status, summary))
}

// writeAudit writes audit event best-effort: logs errors, does not interrupt run
// (audit is not critical for push functionality).
func (r *PushRun) writeAudit(ctx context.Context, eventType audit.EventType, aid string, payload map[string]any) {
	if r.deps.Audit == nil {
		return
	}
	src := audit.SourceAPI // push.apply is called only from API/MCP — source is deterministic.
	ev := &audit.Event{
		EventType: eventType,
		Source:    src,
		ArchonAID: aid,
		Payload:   payload,
	}
	if err := r.deps.Audit.Write(ctx, ev); err != nil {
		r.deps.Logger.Warn("pushorch: audit write failed",
			slog.String("event_type", string(eventType)),
			slog.Any("error", err))
	}
}

// terminalAuditPayload assembles the final audit event payload. Transparently
// carries aggregate numbers of per-host outcomes from summary (success_count/fail_count) +
// destiny-ref + inventory size. Full inventory is NOT included (may be large);
// details are in push_runs.summary via GET /v1/push/{apply_id}.
func terminalAuditPayload(applyID string, req ApplyRequest, status PushRunStatus, summary map[string]any) map[string]any {
	p := map[string]any{
		"apply_id":       applyID,
		"destiny":        req.DestinyRef,
		"inventory_size": len(req.InventorySIDs),
		"status":         string(status),
	}
	if v, ok := summary["success_count"]; ok {
		p["success_count"] = v
	}
	if v, ok := summary["fail_count"]; ok {
		p["fail_count"] = v
	}
	if v, ok := summary["total"]; ok {
		p["total"] = v
	}
	return p
}

// hostResult is the outcome of one per-host SendApply: status is either "error"
// (delivery failed) or RunStatus (Soul returned RunResult, status in protobuf enum).
//
// `provider` is the name of the SshProvider actually used for this SID
// (Multi-provider routing, P2 W-3). Written to push_runs.summary.hosts[] for
// audit trail (architect decision: routing decision is saved in summary,
// no separate per-routing event).
type hostResult struct {
	sid      string
	provider string
	ok       bool   // true iff SendApply returned nil error and RunStatus==SUCCESS
	status   string // string form for summary (`success`/`failed`/`cancelled`/`error_locked`/`error`)
	errText  string // non-empty only when ok=false; SendApply error or the non-SUCCESS status reason
}

// buildHostResult classifies SendApply return:
//   - err != nil → ok=false, status="error" (delivery/connect did not reach RunResult);
//   - rr.Status == SUCCESS → ok=true;
//   - rr.Status other → ok=false, status is enum string.
//
// Provider is always remembered (even on error path) so summary shows
// which SshProvider the fail occurred on.
func buildHostResult(sid string, providerName string, rr *keeperv1.RunResult, err error) hostResult {
	if err != nil {
		return hostResult{sid: sid, provider: providerName, status: "error", errText: err.Error()}
	}
	st := rr.GetStatus()
	if st == keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		return hostResult{sid: sid, provider: providerName, ok: true, status: "success"}
	}
	return hostResult{
		sid:      sid,
		provider: providerName,
		status:   runStatusLabel(st),
		errText:  "run_status=" + runStatusLabel(st),
	}
}

// runStatusLabel is short kebab-case label of RunStatus for summary (no `RUN_STATUS_`
// prefix, lowercase). Symmetric to status field in summary of audit events.
func runStatusLabel(st keeperv1.RunStatus) string {
	switch st {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		return "success"
	case keeperv1.RunStatus_RUN_STATUS_FAILED:
		return "failed"
	case keeperv1.RunStatus_RUN_STATUS_CANCELLED:
		return "cancelled"
	case keeperv1.RunStatus_RUN_STATUS_ERROR_LOCKED:
		return "error_locked"
	default:
		return "unknown"
	}
}

// summarize classifies the aggregated run outcome:
//   - all ok          → success;
//   - all not-ok      → failed;
//   - mixed outcome   → partial_failed.
//
// Summary form (jsonb in push_runs.summary):
//
//	{
//	  "hosts":         [ {sid, status, error?}, … ],
//	  "total":         <int>,
//	  "success_count": <int>,
//	  "fail_count":    <int>
//	}
//
// Hosts order is by fanOut positions (= sids; already sorted via union).
func summarize(results []hostResult) (PushRunStatus, map[string]any) {
	hostsArr := make([]map[string]any, 0, len(results))
	success := 0
	for _, h := range results {
		entry := map[string]any{
			"sid":    h.sid,
			"status": h.status,
		}
		if h.provider != "" {
			// P2 W-3: routing decision is saved in push_runs.summary.hosts[sid]
			// (architect decision: no separate per-routing event in audit_log).
			entry["ssh_provider"] = h.provider
		}
		if h.errText != "" {
			entry["error"] = h.errText
		}
		hostsArr = append(hostsArr, entry)
		if h.ok {
			success++
		}
	}
	total := len(results)
	fail := total - success
	summary := map[string]any{
		"hosts":         hostsArr,
		"total":         total,
		"success_count": success,
		"fail_count":    fail,
	}

	switch {
	case success == total:
		return StatusSuccess, summary
	case success == 0:
		return StatusFailed, summary
	default:
		return StatusPartialFailed, summary
	}
}

// unionTargetSIDs builds a sorted unique list of SIDs from all plans (union by tasks).
// In pilot scenario with one apply task, plan is usually one → plan.TargetSIDs passed
// through directly; for a pair of tasks with different where: get union, which is
// what push semantics needs "run over inventory".
func unionTargetSIDs(plans []render.DispatchPlan) []string {
	if len(plans) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	for _, p := range plans {
		for _, sid := range p.TargetSIDs {
			seen[sid] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for sid := range seen {
		out = append(out, sid)
	}
	// Determinism of per-host dispatch: sort by SID. Lexicographically (via
	// stdlib sort — sort.Strings) gives the same layout as LoadByInventory.
	sortStrings(out)
	return out
}

// sortStrings is a simplified wrapper around sort.Strings so run.go does not need
// to import "sort" in the main path.
func sortStrings(s []string) {
	// inlined insertion-sort: on push inventories of <=100 elements this is
	// faster than sort.Strings (no interface overhead), no allocations.
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1] > s[j] {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// sortHostResults ensures deterministic order of hosts[] in summary (by SID).
// After merging routing failures with dispatch results, order is broken;
// inline insertion-sort on short selection (<=100 SID).
func sortHostResults(s []hostResult) {
	for i := 1; i < len(s); i++ {
		j := i
		for j > 0 && s[j-1].sid > s[j].sid {
			s[j-1], s[j] = s[j], s[j-1]
			j--
		}
	}
}

// observeRouted is a nil-safe wrapper around ProviderMetricsObserver. Free function
// (cannot add method to interface) so pushorch does not repeat nil-check on each
// resolveProviders call.
func observeRouted(o ProviderMetricsObserver, providerName, decisionSource string) {
	if o == nil {
		return
	}
	o.ObserveProviderRouted(providerName, decisionSource)
}
