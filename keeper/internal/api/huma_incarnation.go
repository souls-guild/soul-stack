package api

// Registration and spec-dump of the INCARNATION domain on huma full-typed (batch-2g, ADR-054
// §Pattern). A MIXED domain by audit class:
//
//   - MIDDLEWARE-AUDIT (create / run / unlock / upgrade): mounted via
//     newHumaIncarnationAPI(evt) (huma-audit-middleware variant B); the register func
//     sets the payload from *Typed reply.AuditPayload via SetHumaAuditPayload.
//   - SELF-AUDIT (rerun-last / check-drift / destroy / update-hosts): mounted via
//     newHumaCadenceAPI (no audit wiring); audit is written BY the handler ITSELF inside
//     *Typed (h.auditW.Write). Confusing the class = an S6 regression.
//   - READ (get / list / history): newHumaCadenceAPI, no audit written.
//
// ALL incarnation ops carry the FULL path /{name}[/...] relative to the group
// /v1/incarnations (chi.Route("/{name}") was REMOVED from router.go — otherwise the /{name}
// node sibling-shadows → 405). Coexists with the choir-mount (batch-2f) on the same group.

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// newHumaIncarnationAPI builds a huma.API over the chi group /v1/incarnations with the huma
// audit middleware (variant B) for the given event type. Parallel to newHumaRoleAPI:
// incarnation MIDDLEWARE-audit routes (create/run/unlock/upgrade) write audit OUTSIDE
// *Typed (via middleware) — SELF-audit routes write inside (newHumaCadenceAPI).
func newHumaIncarnationAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// --- MIDDLEWARE-AUDIT ---

// registerHumaIncarnationCreate mounts POST /v1/incarnations (MIDDLEWARE-AUDIT
// incarnation.created variant B). incH nil → no-op.
func registerHumaIncarnationCreate(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incCreateOperation(), func(ctx context.Context, in *incCreateInput) (*incCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		reply, err := incH.CreateTyped(ctx, claims, handlers.IncarnationCreateRequestInput{
			Name:           in.Body.Name,
			Service:        in.Body.Service,
			Covens:         in.Body.Covens,
			Input:          in.Body.Input,
			Traits:         in.Body.Traits,
			CreateScenario: in.Body.CreateScenario,
		})
		if err != nil {
			return nil, incProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, reply.AuditPayload)
		return &incCreateOutput{Status: http.StatusAccepted, Body: newIncarnationCreateReply(reply.Body)}, nil
	})
}

// registerHumaIncarnationRun mounts POST /v1/incarnations/{name}/scenarios/{scenario}
// (MIDDLEWARE-AUDIT incarnation.scenario_started). incH nil → no-op. Toll middleware
// (503 on degraded) — chi wiring of the group (huma inherits it).
func registerHumaIncarnationRun(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRunOperation(), func(ctx context.Context, in *incRunInput) (*incRunOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		var input map[string]any
		if in.Body != nil {
			input = in.Body.Input
		}
		reply, err := incH.RunTyped(ctx, claims, in.Name, in.Scenario, input)
		if err != nil {
			return nil, incProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, reply.AuditPayload)
		return &incRunOutput{Status: http.StatusAccepted, Body: newIncarnationRunReply(reply.Body)}, nil
	})
}

// registerHumaIncarnationUnlock mounts POST /v1/incarnations/{name}/unlock
// (MIDDLEWARE-AUDIT incarnation.unlocked). incH nil → no-op.
func registerHumaIncarnationUnlock(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incUnlockOperation(), func(ctx context.Context, in *incUnlockInput) (*incUnlockOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		reply, err := incH.UnlockTyped(ctx, claims, in.Name, in.Body.Reason)
		if err != nil {
			return nil, incProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, reply.AuditPayload)
		return &incUnlockOutput{Status: http.StatusOK, Body: newIncarnationUnlockReply(reply.Body)}, nil
	})
}

// registerHumaIncarnationUpgrade mounts POST /v1/incarnations/{name}/upgrade
// (MIDDLEWARE-AUDIT incarnation.upgrade_started). incH nil → no-op.
func registerHumaIncarnationUpgrade(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incUpgradeOperation(), func(ctx context.Context, in *incUpgradeInput) (*incUpgradeOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		reply, err := incH.UpgradeTyped(ctx, claims, in.Name, in.Body.ToVersion)
		if err != nil {
			return nil, incProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, reply.AuditPayload)
		return &incUpgradeOutput{Status: http.StatusAccepted, Body: newIncarnationUpgradeReply(reply.Body)}, nil
	})
}

// --- SELF-AUDIT ---

// registerHumaIncarnationRerunLast mounts POST /v1/incarnations/{name}/rerun-last
// (SELF-AUDIT incarnation.rerun_last — written BY the handler itself inside RerunLastTyped,
// the audit middleware is not wired). incH nil → no-op.
func registerHumaIncarnationRerunLast(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRerunOperation(), func(ctx context.Context, in *incRerunInput) (*incRerunOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		body, err := incH.RerunLastTyped(ctx, claims, in.Name, in.Body.Reason)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incRerunOutput{Status: http.StatusAccepted, Body: newIncarnationRerunLastReply(body)}, nil
	})
}

// registerHumaIncarnationCheckDrift mounts POST /v1/incarnations/{name}/check-drift
// (SELF-AUDIT incarnation.drift_checked — written BY the handler itself inside CheckDriftTyped).
// incH nil → no-op.
func registerHumaIncarnationCheckDrift(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incCheckDriftOperation(), func(ctx context.Context, in *incCheckDriftInput) (*incCheckDriftOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		var override map[string]any
		if in.Body != nil {
			override = in.Body.Input
		}
		report, err := incH.CheckDriftTyped(ctx, claims, in.Name, override)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incCheckDriftOutput{Body: report}, nil
	})
}

// registerHumaIncarnationDestroy mounts DELETE /v1/incarnations/{name} (SELF-AUDIT
// incarnation.destroy_started — written by the service layer incarnation.Destroy; the audit
// middleware is not wired). incH nil → no-op. allow_destroy — a required boolean query (huma bind:
// missing/non-boolean → 400).
func registerHumaIncarnationDestroy(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incDestroyOperation(), func(ctx context.Context, in *incDestroyInput) (*incDestroyOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		body, err := incH.DestroyTyped(ctx, claims, in.Name, in.AllowDestroy)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incDestroyOutput{Status: http.StatusAccepted, Body: newIncarnationDestroyReply(body)}, nil
	})
}

// registerHumaIncarnationUpdateHosts mounts PATCH /v1/incarnations/{name}/hosts
// (SELF-AUDIT incarnation.hosts_updated — written BY the handler itself inside UpdateHostsTyped).
// incH nil → no-op.
func registerHumaIncarnationUpdateHosts(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incUpdateHostsOperation(), func(ctx context.Context, in *incUpdateHostsInput) (*incUpdateHostsOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		items := make([]handlers.IncarnationSpecHostInput, len(in.Body.Hosts))
		for i, h := range in.Body.Hosts {
			role := ""
			if h.Role != nil {
				role = *h.Role
			}
			items[i] = handlers.IncarnationSpecHostInput{SID: h.SID, Role: role}
		}
		body, err := incH.UpdateHostsTyped(ctx, claims, in.Name, in.Body.Mode, items)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incUpdateHostsOutput{Body: newIncarnationGetReply(body)}, nil
	})
}

// registerHumaIncarnationSetTraits mounts PUT /v1/incarnations/{name}/traits
// (SELF-AUDIT incarnation.traits_changed — written BY the handler itself inside SetTraitsTyped).
// incH nil → no-op.
func registerHumaIncarnationSetTraits(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incSetTraitsOperation(), func(ctx context.Context, in *incSetTraitsInput) (*incSetTraitsOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		body, err := incH.SetTraitsTyped(ctx, claims, in.Name, in.Body.Traits)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incSetTraitsOutput{Body: newIncarnationGetReply(body)}, nil
	})
}

// --- READ ---

// registerHumaIncarnationGet mounts GET /v1/incarnations/{name} (READ, no audit).
// The scope predicate (ADR-047) is built from claims (out of scope → 404). incH nil → no-op.
func registerHumaIncarnationGet(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incGetOperation(), func(ctx context.Context, in *incGetInput) (*incGetOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		body, err := incH.GetTyped(ctx, in.Name, incH.GetInScopeFor(claims, "get"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incGetOutput{Body: newIncarnationGetReply(body)}, nil
	})
}

// registerHumaIncarnationUpgradePaths mounts GET /v1/incarnations/{name}/upgrade-paths
// (READ, no audit; ADR-0068 §6). The scope predicate action=upgrade (the read facet, the same
// permission incarnation.upgrade as POST .../upgrade) → out of scope 404. incH nil → no-op.
func registerHumaIncarnationUpgradePaths(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incUpgradePathsOperation(), func(ctx context.Context, in *incUpgradePathsInput) (*incUpgradePathsOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		view, err := incH.UpgradePathsTyped(ctx, in.Name, in.To, incH.GetInScopeFor(claims, "upgrade"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incUpgradePathsOutput{Body: newIncarnationUpgradePathsReply(view)}, nil
	})
}

// registerHumaIncarnationList mounts GET /v1/incarnations (READ with typed query,
// no audit). state.<field> filters are NOT bound by huma as typed parameters (dynamic
// keys) — we extract them from the raw query via humaQueryFromContext. incH nil → no-op.
func registerHumaIncarnationList(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incListOperation(), func(ctx context.Context, in *incListInput) (*incListOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		q := handlers.IncarnationListQuery{
			Offset:      int(in.Offset),
			Limit:       int(in.Limit),
			Service:     in.Service,
			Status:      in.Status,
			Coven:       in.Coven,
			SortBy:      in.SortBy,
			SortDir:     in.SortDir,
			StateParams: stateParamsFromContext(ctx),
		}
		reply, err := incH.ListTyped(ctx, q, incH.ResolveListScopeFor(ctx, claims))
		if err != nil {
			return nil, incProblem(err)
		}
		items := make([]IncarnationGetReply, len(reply.Items))
		for i := range reply.Items {
			items[i] = newIncarnationGetReply(reply.Items[i])
		}
		return &incListOutput{Body: incarnationListReply{
			Items:  items,
			Offset: int32(reply.Offset),
			Limit:  int32(reply.Limit),
			Total:  int32(reply.Total),
		}}, nil
	})
}

// registerHumaIncarnationHistory mounts GET /v1/incarnations/{name}/history (READ with
// typed query, no audit). The scope predicate (action=history) → out of scope 404. incH
// nil → no-op.
func registerHumaIncarnationHistory(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incHistoryOperation(), func(ctx context.Context, in *incHistoryInput) (*incHistoryOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		reply, err := incH.HistoryTyped(ctx, in.Name, in.ApplyID, int(in.Offset), int(in.Limit), incH.GetInScopeFor(claims, "history"))
		if err != nil {
			return nil, incProblem(err)
		}
		items := make([]StateHistoryEntry, len(reply.Items))
		for i := range reply.Items {
			items[i] = newStateHistoryEntry(reply.Items[i])
		}
		return &incHistoryOutput{Body: incarnationHistoryReply{
			Items:  items,
			Offset: int32(reply.Offset),
			Limit:  int32(reply.Limit),
			Total:  int32(reply.Total),
		}}, nil
	})
}

// registerHumaIncarnationRuns mounts GET /v1/incarnations/{name}/runs (READ with
// typed query, no audit). The scope predicate is the same as History (action=history) → out of
// scope 404. incH nil → no-op.
func registerHumaIncarnationRuns(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRunsOperation(), func(ctx context.Context, in *incRunsInput) (*incRunsOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		reply, err := incH.RunsTyped(ctx, in.Name, int(in.Offset), int(in.Limit), incH.GetInScopeFor(claims, "history"))
		if err != nil {
			return nil, incProblem(err)
		}
		items := make([]RunSummaryEntry, len(reply.Items))
		for i := range reply.Items {
			items[i] = newRunSummaryEntry(reply.Items[i])
		}
		return &incRunsOutput{Body: incarnationRunsReply{
			Items:  items,
			Offset: int32(reply.Offset),
			Limit:  int32(reply.Limit),
			Total:  int32(reply.Total),
		}}, nil
	})
}

// registerHumaIncarnationRunDetail mounts GET /v1/incarnations/{name}/runs/{apply_id}
// (READ with path, no audit). The scope predicate is the same as History (action=history).
// incH nil → no-op.
func registerHumaIncarnationRunDetail(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRunDetailOperation(), func(ctx context.Context, in *incRunDetailInput) (*incRunDetailOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		reply, err := incH.RunDetailTyped(ctx, in.Name, in.ApplyID, incH.GetInScopeFor(claims, "history"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incRunDetailOutput{Body: newRunDetailReply(reply)}, nil
	})
}

// registerHumaIncarnationRunTasks mounts GET /v1/incarnations/{name}/runs/{apply_id}/tasks
// (READ with path, no audit, NIM-37). The scope predicate is the same as History/RunDetail
// (action=history). incH nil → no-op.
func registerHumaIncarnationRunTasks(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRunTasksOperation(), func(ctx context.Context, in *incRunTasksInput) (*incRunTasksOutput, error) {
		claims, _ := apimiddleware.ClaimsFromContext(ctx)
		reply, err := incH.RunTasksTyped(ctx, in.Name, in.ApplyID, incH.GetInScopeFor(claims, "history"))
		if err != nil {
			return nil, incProblem(err)
		}
		return &incRunTasksOutput{Body: newRunTasksReply(reply)}, nil
	})
}

// --- helpers ---

// rawQueryCtxKey — the context key for the raw url.Values stashed by the [stashRawQuery]
// middleware BEFORE huma (huma typed-query cannot handle dynamic keys `state.<field>`).
type rawQueryCtxKey struct{}

// stashRawQuery — a chi middleware that puts r.URL.Query() into the request context BEFORE
// the huma dispatch. Wired on the List group /v1/incarnations (router.go): the huma handler
// receives ctx = r.Context() and reads the dynamic `state.<field>` filters via
// [stateParamsFromContext]. Typed offset/limit/service/status/coven/sort are bound by huma
// itself — this stash is needed ONLY for state.* keys.
func stashRawQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), rawQueryCtxKey{}, r.URL.Query())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// stateParamsFromContext extracts the `state.<field>` query filters from the raw query
// stashed by [stashRawQuery]. Keys without the `state.` prefix are skipped. Empty → nil.
func stateParamsFromContext(ctx context.Context) map[string][]string {
	q, _ := ctx.Value(rawQueryCtxKey{}).(url.Values)
	if q == nil {
		return nil
	}
	var out map[string][]string
	for key, vals := range q {
		field, ok := strings.CutPrefix(key, "state.")
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string][]string)
		}
		out[field] = vals
	}
	return out
}

// incMissingClaims — a defensive response when claims are absent (unreachable: RequireJWT
// puts claims before huma). problem+json (parity with cadenceMissingClaims).
func incMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// incProblem delivers a *Typed-function error through huma as problem+json. A domain
// *handlers.problemError → humaProblemError; a non-problem → 500 (parity with cadenceProblem).
func incProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaIncarnationSpecYAML assembles the OpenAPI fragment of ALL incarnation routes migrated
// to huma as a YAML string, without mounting on a real router. A hook for the spec merge
// target of the rollout and a guard test. Delegates to generic [humaDumpSpec].
func HumaIncarnationSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.IncarnationSpecStub()
		registerHumaIncarnationCreate(api, stub)
		registerHumaIncarnationList(api, stub)
		registerHumaIncarnationGet(api, stub)
		registerHumaIncarnationUpgradePaths(api, stub)
		registerHumaIncarnationFormPrefill(api, stub)
		registerHumaIncarnationHistory(api, stub)
		registerHumaIncarnationRuns(api, stub)
		registerHumaIncarnationRunDetail(api, stub)
		registerHumaIncarnationRunTasks(api, stub)
		registerHumaIncarnationRun(api, stub)
		registerHumaIncarnationUnlock(api, stub)
		registerHumaIncarnationUpgrade(api, stub)
		registerHumaIncarnationRerunLast(api, stub)
		registerHumaIncarnationCheckDrift(api, stub)
		registerHumaIncarnationDestroy(api, stub)
		registerHumaIncarnationUpdateHosts(api, stub)
		registerHumaIncarnationSetTraits(api, stub)
		registerHumaIncarnationRevealSecret(api, stub)
		registerHumaIncarnationRevealableSecrets(api, stub)
		return nil
	})
}
