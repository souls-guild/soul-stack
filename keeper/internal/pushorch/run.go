package pushorch

import (
	"context"
	"errors"
	"fmt"
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
// [push.Route] carries the SshProvider plugin name selected by ProviderRouter
// (ADR-032 amendment 2026-05-27, P2 W-2/W-3 multi-provider routing; empty string
// or unknown name → push.ErrProviderUnknown) together with the task's
// `transport:` override (NIM-870).
//
// The [push.EventHandler] this orchestrator passes is nil, deliberately: a bare
// `POST /v1/push/apply` run writes `push_runs` and mints no `apply_runs` row, so
// there is no `apply_task_register` FK target to hang a `register:` off, no
// scenario around it to consume one and no barrier to release (NIM-880). The
// scenario dispatcher's push branch passes a handler for exactly the opposite
// reasons.
type SshDispatcher interface {
	SendApply(ctx context.Context, sid string, route push.Route, req *keeperv1.ApplyRequest, onEvent push.EventHandler) (*keeperv1.RunResult, error)
}

// Cleaner is a narrow interface of [push.SshDispatcher.Cleanup] for best-effort
// post-success cleanup of stale versions (`cleanup_stale_versions: true`).
// The same *push.SshDispatcher satisfies both interfaces — wire-up passes it to
// both fields. The [push.Route] is the SAME one the preceding SendApply ran
// under, override included: cleanup opens a second session to the same host and
// must land on the same account and port (caller maintains the per-SID decision).
type Cleaner interface {
	Cleanup(ctx context.Context, sid string, route push.Route) error
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

	// Transport is the task-level `transport:` in its raw DSL form — the scalar
	// or the one-key mapping [config.TransportSpecOf] decodes (NIM-870). It is
	// put on the synthetic task below and read back off the RENDERED PLAN, not
	// from here: that is the path a scenario task will take when the scenario
	// dispatcher grows a push branch, and having one reader means the precedence
	// cannot come out differently on the two routes.
	Transport any
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
	if err := validateTransport(req.Transport); err != nil {
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

// GetRowScoped is [PushRun.GetRow] narrowed to the caller's host boundary
// (NIM-842) — the operator read, where GetRow is the orchestration one.
func (r *PushRun) GetRowScoped(ctx context.Context, applyID string, hostScope func(startIdx int) (string, []any, int)) (*PushRunRow, error) {
	return r.deps.Store.GetScoped(ctx, applyID, hostScope)
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
//     render.Pipeline.Render (destinyIsolated by design — register/state/
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
				Name:      syntheticTaskName,
				Transport: req.Transport,
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
		// ServiceVars/Register/RegisterByHost are empty: push run is not tied to
		// an incarnation, scenario-scope is unavailable (same logic as destiny phase
		// of scenario-runner: render-pipeline itself guarantees destiny isolation).
		Incarnation: render.IncarnationMeta{ID: syntheticScenarioName},
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

	// The task's `transport:` (NIM-870), read off the rendered plan. A param that
	// did not decode fails the RUN rather than falling back to the registry: the
	// key exists in order to beat the registry, so answering from the registry
	// after failing to read it would be the silent wrong answer this key's whole
	// audit requirement is about.
	transportName, override, oerr := transportOverrideOf(plans)
	if oerr != nil {
		log.Error("pushorch: transport override unusable", slog.Any("error", oerr))
		r.finalize(ctx, applyID, StatusFailed, map[string]any{
			"error": "transport_invalid: " + oerr.Error(),
		}, req.StartedByAID, req)
		return
	}

	// P2 W-3 routing phase. Runs BEFORE fanOut: per-SID routing miss must not open
	// SSH session and must not consume plugin env-payload.
	// α-compat (PM-decision): non-empty req.SSHProvider → per-job preset applied to
	// ALL SIDs, overrides router. Otherwise router.RouteFor per-SID; error → per-host
	// status="error" + error_code="provider_not_routed".
	sidRoute, sidSource, routingResults := r.resolveProviders(ctx, target, req, transportName, override, log)

	// hostResults are collected by target; for SIDs where routing failed, there's
	// already an entry in routingResults (we exclude them from dispatch list).
	dispatchTargets := make([]string, 0, len(target))
	for _, sid := range target {
		if _, failed := routingResults[sid]; failed {
			continue
		}
		dispatchTargets = append(dispatchTargets, sid)
	}

	hostResults := r.fanOut(ctx, applyID, dispatchTargets, sidRoute, sidSource, transportName, protoTasks, log)
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
		go r.cleanupHosts(dispatchTargets, sidRoute, log)
	}
}

// resolveProviders resolves the route for each SID in inventory, in four
// levels, highest first:
//
//	Level 0:    the task's `transport: { ssh: { ssh_provider: … } }` (NIM-870)
//	α-compat:   req.SSHProvider, the per-job preset (PM-decision P2 W-3)
//	Levels 1-3: ProviderRouter (per-SID → per-coven → cluster)
//
// A hit at level 0 or at the α-compat preset applies to ALL SIDs and the
// ProviderRouter is NOT called; the source recorded in the run summary is
// "task" and "soul" respectively (a per-job preset is semantically the per-SID
// explicit answer for every target).
//
// Without either: for each SID call router.RouteFor. ErrProviderNotRouted →
// hostResult with status="error" + errText="provider_not_routed" placed in
// routingResults[sid]. Real PG error → same, errText contains underlying message
// (transient — operator retries).
//
// The task's user/port override rides EVERY route regardless of which level
// picked the provider: choosing a provider and overriding the connection fields
// are independent axes, and a task that named only a user must not lose it
// because the cluster default answered for the provider.
//
// Return:
//   - sidRoute: map[sid]push.Route for SUCCESSFULLY resolved SIDs;
//   - sidSource: map[sid]push.RouteSource, the level that picked the provider;
//   - routingResults: map[sid]hostResult for SIDs where routing failed
//     (caller adds them to final hosts[] without dispatch).
func (r *PushRun) resolveProviders(ctx context.Context, target []string, req ApplyRequest, transportName string, override push.TransportOverride, log *slog.Logger) (map[string]push.Route, map[string]push.RouteSource, map[string]hostResult) {
	sidRoute := make(map[string]push.Route, len(target))
	sidSource := make(map[string]push.RouteSource, len(target))
	routingResults := make(map[string]hostResult)

	// Level 0 — the task's own `transport: { ssh: { ssh_provider: … } }`. It
	// stands above the α-compat preset and above all three router levels, by the
	// owner's decision that the task beats the registry (NIM-870). The router is
	// not called at all: its cheapest level is a PG read per SID, and there is
	// nothing left for it to decide.
	if override.Provider != "" {
		for _, sid := range target {
			sidRoute[sid] = push.Route{Provider: override.Provider, Override: override}
			sidSource[sid] = push.SourceTask
			observeRouted(r.deps.ProviderMetrics, override.Provider, push.SourceTask.String())
		}
		log.Info("pushorch: transport: on the task selected the provider for all SIDs",
			slog.String("provider", override.Provider),
			slog.Int("count", len(target)))
		return sidRoute, sidSource, routingResults
	}

	if req.SSHProvider != "" {
		// α-compat: per-job preset, single provider for all SIDs.
		for _, sid := range target {
			sidRoute[sid] = push.Route{Provider: req.SSHProvider, Override: override}
			sidSource[sid] = push.SourceSoul
			observeRouted(r.deps.ProviderMetrics, req.SSHProvider, push.SourceSoul.String())
		}
		log.Info("pushorch: α-compat ssh_provider preset applied to all SIDs",
			slog.String("provider", req.SSHProvider),
			slog.Int("count", len(target)))
		return sidRoute, sidSource, routingResults
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
			// The transport fields ride a routing failure too: the host never
			// dialled, but the summary still has to say what the task asked for —
			// otherwise the one entry an incident starts from reads as "went by
			// the registry", which is exactly the wrong place to look.
			routingResults[sid] = hostResult{
				sid:           sid,
				status:        "error",
				errText:       errCode + ": " + rerr.Error(),
				transportName: transportName,
				override:      override,
			}
			continue
		}
		sidRoute[sid] = push.Route{Provider: providerName, Override: override}
		sidSource[sid] = source
		observeRouted(r.deps.ProviderMetrics, providerName, source.String())
	}
	return sidRoute, sidSource, routingResults
}

// fanOut runs per-host SendApply in parallel (one goroutine per host), collects
// hostResults in deterministic order (by SID). Concurrency is unlimited (push
// inventory is usually small; large-scale rolling is a separate slice via
// render.DispatchPlan.SerialWidth, not used in pilot).
//
// ★ What is kept here is a RunResult and nothing else — no per-task register.
// The nil [push.EventHandler] is the decision, not an omission (NIM-880): this
// run records its outcome in `push_runs` and writes no `apply_runs` row, so
// `apply_task_register` has no FK target, and there is no scenario around it to
// read a `register.<name>` back or a barrier to release — which is why the
// synthetic scenario above carries one `apply:` task and no `register:`. A
// scenario task dispatched over push is the other case entirely and fills its
// register exactly as a streamed one does; see [scenario.Runner.dispatchPushHost].
//
// sidRoute is map sid → route (P2 W-3 multi-provider routing + the NIM-870
// task override); sidSource is the level that picked the provider, carried into
// the summary. A SID without an entry is an invariant violation (resolveProviders
// already filtered such), defensive guard inside.
func (r *PushRun) fanOut(ctx context.Context, applyID string, sids []string, sidRoute map[string]push.Route, sidSource map[string]push.RouteSource, transportName string, tasks []*keeperv1.RenderedTask, log *slog.Logger) []hostResult {
	results := make([]hostResult, len(sids))
	var wg sync.WaitGroup
	for i, sid := range sids {
		wg.Add(1)
		route := sidRoute[sid]
		source := sidSource[sid]
		go func(idx int, sid string, route push.Route, source push.RouteSource) {
			defer wg.Done()
			req := &keeperv1.ApplyRequest{
				ApplyId: applyID,
				Tasks:   tasks,
			}
			rr, err := r.deps.Dispatcher.SendApply(ctx, sid, route, req, nil)
			results[idx] = buildHostResult(sid, route, source, transportName, rr, err)
			if err != nil {
				log.Warn("pushorch: SendApply failed",
					slog.String("sid", sid),
					slog.String("ssh_provider", route.Provider),
					slog.Any("error", err))
			} else {
				log.Info("pushorch: per-host run completed",
					slog.String("sid", sid),
					slog.String("ssh_provider", route.Provider),
					slog.String("status", rr.GetStatus().String()))
			}
		}(i, sid, route, source)
	}
	wg.Wait()
	return results
}

// cleanupHosts runs per-host Cleanup; best-effort, errors → log-warn, do not
// affect run status. Uses its own bg-ctx with the same cap-timeout as executeAsync.
//
// sidRoute is map sid → route, populated by resolveProviders. SID without
// entry (failed routing) does not reach here (cleanupHosts receives only
// dispatchTargets).
func (r *PushRun) cleanupHosts(sids []string, sidRoute map[string]push.Route, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), orchestratorContextTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, sid := range sids {
		wg.Add(1)
		route := sidRoute[sid]
		go func(sid string, route push.Route) {
			defer wg.Done()
			if err := r.deps.Cleaner.Cleanup(ctx, sid, route); err != nil {
				log.Warn("pushorch: post-success cleanup failed",
					slog.String("sid", sid),
					slog.String("ssh_provider", route.Provider),
					slog.Any("error", err))
				return
			}
			log.Info("pushorch: post-success cleanup OK",
				slog.String("sid", sid),
				slog.String("ssh_provider", route.Provider))
		}(sid, route)
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

	// The transport decision this host was dispatched under (NIM-870), recorded
	// so an incident review can see WHICH source answered rather than infer it
	// from a registry the run may not have read. routeSource is the level that
	// picked the provider; transportName is what the task NAMED (empty = it wrote
	// no key) and override is what it overrode — the two are separate because
	// `transport: ssh` names a transport and overrides nothing, and a summary
	// that showed only the override would make that run indistinguishable from
	// one that never wrote the key.
	routeSource   push.RouteSource
	transportName string
	override      push.TransportOverride
}

// buildHostResult classifies SendApply return:
//   - err != nil → ok=false, status="error" (delivery/connect did not reach RunResult);
//   - rr.Status == SUCCESS → ok=true;
//   - rr.Status other → ok=false, status is enum string.
//
// The route is always remembered (even on the error path) so the summary shows
// which SshProvider the fail occurred on, and under whose say.
func buildHostResult(sid string, route push.Route, source push.RouteSource, transportName string, rr *keeperv1.RunResult, err error) hostResult {
	base := hostResult{sid: sid, provider: route.Provider, routeSource: source, transportName: transportName, override: route.Override}
	if err != nil {
		base.status, base.errText = "error", err.Error()
		return base
	}
	st := rr.GetStatus()
	if st == keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		base.ok, base.status = true, "success"
		return base
	}
	base.status = runStatusLabel(st)
	base.errText = "run_status=" + runStatusLabel(st)
	return base
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
//	  "hosts":         [ {sid, status, error?, ssh_provider?, route_source?,
//	                      transport?, ssh_user?, ssh_port?}, … ],
//	  "total":         <int>,
//	  "success_count": <int>,
//	  "fail_count":    <int>
//	}
//
// ★ The transport fields are the visibility half of NIM-870. The task's
// `transport:` beats `souls.ssh_target` and the cluster config, which makes a
// THIRD source of truth for provider/user/port; the price of that decision is
// that a run must say which source actually answered, or an incident review
// reads a registry the run never went to.
//
//   - `transport` is present exactly when the TASK named one — the scalar
//     `transport: ssh` included, which names a transport and overrides nothing;
//   - `route_source` is the level that picked the provider (`task`/`soul`/
//     `coven`/`cluster`);
//   - `ssh_user`/`ssh_port` appear only when the TASK set them. Their absence
//     means the resolver answered, and only the resolver knows whether that was
//     the `souls.ssh_target` row or its own default — a label here would have to
//     guess, and guessing in the field whose whole job is provenance is worse
//     than leaving it out.
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
			entry["route_source"] = h.routeSource.String()
		}
		if h.transportName != "" {
			// Present exactly when the TASK named a transport — including the
			// scalar form, which names one and overrides nothing.
			entry["transport"] = h.transportName
		}
		if h.override.User != "" {
			entry["ssh_user"] = h.override.User
		}
		if h.override.Port != 0 {
			entry["ssh_port"] = h.override.Port
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

// ErrInvalidTransport — the `transport:` in the request body is not a form this
// build can carry. A sentinel so the HTTP handler and the MCP tool both answer
// 422 instead of accepting a run that would fail asynchronously two seconds
// later with nothing for the caller to correlate it to.
var ErrInvalidTransport = errors.New("pushorch: invalid transport")

// validateTransport rejects, at the API boundary, every `transport:` shape a
// push run cannot execute. It is the same decode the rendered plan goes through
// afterwards ([transportOverrideOf]) — one decoder, called twice — so a body
// this accepts cannot be refused later for its shape.
//
// Three refusals:
//   - a form that does not decode (two keys, an unregistered name, a param
//     block that is not a mapping);
//   - `agent`, which this endpoint cannot carry: `POST /v1/push/apply` IS the
//     ssh transport, and naming the other one is a contradiction rather than a
//     preference. The same refusal a `transport: agent` scenario task gets
//     inside a push run;
//   - a param that does not decode (an unknown key, a non-integer port) or a
//     port outside 1..65535 — the range the DSL validator enforces offline and
//     the MCP tool schema declares, so this door has to enforce it too or the
//     schema would assert a check that exists nowhere.
//
// A nil transport is the ordinary case and passes: the registry decides.
func validateTransport(transport any) error {
	if transport == nil {
		return nil
	}
	name, params, ok := config.TransportSpecOf(transport)
	if !ok {
		return fmt.Errorf("%w: `transport` must be a transport name or a one-key mapping of one (registered: %v), got %T",
			ErrInvalidTransport, config.TransportNames(), transport)
	}
	override, usable, err := push.TransportOverrideFrom(name, params)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidTransport, err)
	}
	if !usable {
		return fmt.Errorf("%w: transport %q cannot carry a push run (this endpoint is the ssh transport)", ErrInvalidTransport, name)
	}
	if _, present := params[config.TransportParamPort]; present && (override.Port < 1 || override.Port > 65535) {
		return fmt.Errorf("%w: transport.ssh.%s must be in 1..65535, got %d", ErrInvalidTransport, config.TransportParamPort, override.Port)
	}
	return nil
}

// transportOverrideOf reads the task's `transport:` off the rendered plans
// (NIM-870). A push run renders ONE synthetic apply task, so every plan of the
// expansion carries the same decision and the first non-empty one is the answer.
//
// Two refusals, both because this key exists in order to beat the registry and a
// key that quietly stops beating it is worse than no key:
//   - a transport the push flow cannot serve (`agent`, whose transport is the
//     pull stream) is an error, not a silent fall-through to SSH;
//   - two plans disagreeing is an error rather than first-wins — today it cannot
//     happen, and the day it can, a run that picked one silently is a run nobody
//     can explain.
func transportOverrideOf(plans []render.DispatchPlan) (string, push.TransportOverride, error) {
	var (
		out   push.TransportOverride
		named string
	)
	for _, p := range plans {
		if p.TransportName == "" {
			continue
		}
		if named != "" && p.TransportName != named {
			return "", push.TransportOverride{}, fmt.Errorf("pushorch: plans disagree on transport: %q and %q", named, p.TransportName)
		}
		o, ok, err := push.TransportOverrideFrom(p.TransportName, p.TransportParams)
		if err != nil {
			return "", push.TransportOverride{}, err
		}
		if !ok {
			return "", push.TransportOverride{}, fmt.Errorf("pushorch: transport %q cannot carry a push run (push is the ssh transport)", p.TransportName)
		}
		named, out = p.TransportName, o
	}
	return named, out, nil
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
