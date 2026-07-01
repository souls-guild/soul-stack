package api

// Регистрация и spec-dump INCARNATION-домена на huma full-typed (батч-2g, ADR-054
// §Pattern). MIXED домен по audit-классу:
//
//   - MIDDLEWARE-AUDIT (create / run / unlock / upgrade): монтируются через
//     newHumaIncarnationAPI(evt) (huma-audit-middleware вариант B); register-func
//     кладёт payload из *Typed-reply.AuditPayload через SetHumaAuditPayload.
//   - SELF-AUDIT (rerun-create / check-drift / destroy / update-hosts): монтируются
//     через newHumaCadenceAPI (БЕЗ audit-навески); audit пишет САМ handler ВНУТРИ
//     *Typed (h.auditW.Write). Перепутать класс = S6-регрессия.
//   - READ (get / list / history): newHumaCadenceAPI, audit НЕ пишут.
//
// ВСЕ incarnation-op несут ПОЛНЫЙ путь /{name}[/...] относительно группы
// /v1/incarnations (chi.Route("/{name}") СНЯТ из router.go — иначе sibling-затенение
// узла /{name} → 405). Сосуществует с choir-mount (батч-2f) на той же группе.

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

// newHumaIncarnationAPI собирает huma.API поверх chi-группы /v1/incarnations с huma-
// audit-middleware (вариант B) под переданный event-тип. Параллель newHumaRoleAPI:
// incarnation-MIDDLEWARE-audit-роуты (create/run/unlock/upgrade) пишут audit СНАРУЖИ
// *Typed (через middleware) — SELF-audit-роуты пишут внутри (newHumaCadenceAPI).
func newHumaIncarnationAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// --- MIDDLEWARE-AUDIT ---

// registerHumaIncarnationCreate монтирует POST /v1/incarnations (MIDDLEWARE-AUDIT
// incarnation.created вариант B). incH nil → no-op.
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

// registerHumaIncarnationRun монтирует POST /v1/incarnations/{name}/scenarios/{scenario}
// (MIDDLEWARE-AUDIT incarnation.scenario_started). incH nil → no-op. Toll-middleware
// (503 на degraded) — chi-навеска группы (huma наследует).
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

// registerHumaIncarnationUnlock монтирует POST /v1/incarnations/{name}/unlock
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

// registerHumaIncarnationUpgrade монтирует POST /v1/incarnations/{name}/upgrade
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

// registerHumaIncarnationRerunCreate монтирует POST /v1/incarnations/{name}/rerun-create
// (SELF-AUDIT incarnation.create_rerun — пишет САМ handler внутри RerunCreateTyped,
// audit-middleware НЕ навешан). incH nil → no-op.
func registerHumaIncarnationRerunCreate(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, incRerunOperation(), func(ctx context.Context, in *incRerunInput) (*incRerunOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		body, err := incH.RerunCreateTyped(ctx, claims, in.Name, in.Body.Reason)
		if err != nil {
			return nil, incProblem(err)
		}
		return &incRerunOutput{Status: http.StatusAccepted, Body: newIncarnationRerunCreateReply(body)}, nil
	})
}

// registerHumaIncarnationCheckDrift монтирует POST /v1/incarnations/{name}/check-drift
// (SELF-AUDIT incarnation.drift_checked — пишет САМ handler внутри CheckDriftTyped).
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

// registerHumaIncarnationDestroy монтирует DELETE /v1/incarnations/{name} (SELF-AUDIT
// incarnation.destroy_started — пишет service-слой incarnation.Destroy; audit-middleware
// НЕ навешан). incH nil → no-op. allow_destroy — required boolean query (huma bind:
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

// registerHumaIncarnationUpdateHosts монтирует PATCH /v1/incarnations/{name}/hosts
// (SELF-AUDIT incarnation.hosts_updated — пишет САМ handler внутри UpdateHostsTyped).
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

// registerHumaIncarnationSetTraits монтирует PUT /v1/incarnations/{name}/traits
// (SELF-AUDIT incarnation.traits_changed — пишет САМ handler внутри SetTraitsTyped).
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

// registerHumaIncarnationGet монтирует GET /v1/incarnations/{name} (READ, БЕЗ audit).
// scope-предикат (ADR-047) строится из claims (вне scope → 404). incH nil → no-op.
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

// registerHumaIncarnationList монтирует GET /v1/incarnations (READ-with-typed-query,
// БЕЗ audit). state.<field>-фильтры huma как typed-параметры НЕ биндит (динамические
// ключи) — извлекаем их из исходного query через humaQueryFromContext. incH nil → no-op.
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

// registerHumaIncarnationHistory монтирует GET /v1/incarnations/{name}/history (READ-
// with-typed-query, БЕЗ audit). scope-предикат (action=history) → вне scope 404. incH
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

// registerHumaIncarnationRuns монтирует GET /v1/incarnations/{name}/runs (READ-with-
// typed-query, БЕЗ audit). scope-предикат тот же, что у History (action=history) → вне
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

// registerHumaIncarnationRunDetail монтирует GET /v1/incarnations/{name}/runs/{apply_id}
// (READ-with-path, БЕЗ audit). scope-предикат тот же, что у History (action=history).
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

// --- helpers ---

// rawQueryCtxKey — context-key для raw url.Values, засташенного [stashRawQuery]-
// middleware ДО huma (huma-typed-query НЕ умеет динамические ключи `state.<field>`).
type rawQueryCtxKey struct{}

// stashRawQuery — chi-middleware, кладущий r.URL.Query() в request-context ДО huma-
// диспатча. Навешивается на List-группу /v1/incarnations (router.go): huma-handler
// получает ctx = r.Context() и читает динамические `state.<field>`-фильтры через
// [stateParamsFromContext]. Типизированные offset/limit/service/status/coven/sort
// huma биндит сам — этот стэш нужен ТОЛЬКО для state.*-ключей.
func stashRawQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), rawQueryCtxKey{}, r.URL.Query())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// stateParamsFromContext извлекает `state.<field>`-query-фильтры из raw query,
// засташенного [stashRawQuery]. Ключи без префикса `state.` пропускаются. Пусто → nil.
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

// incMissingClaims — defensive-ответ при отсутствии claims (недостижим: RequireJWT
// кладёт claims до huma). problem+json (parity cadenceMissingClaims).
func incMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// incProblem доставляет ошибку *Typed-функции через huma как problem+json. Доменный
// *handlers.problemError → humaProblemError; не-problem → 500 (parity cadenceProblem).
func incProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// HumaIncarnationSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma
// incarnation-роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для
// спека-мерж-таргета тиража и guard-теста. Делегирует generic [humaDumpSpec].
func HumaIncarnationSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.IncarnationSpecStub()
		registerHumaIncarnationCreate(api, stub)
		registerHumaIncarnationList(api, stub)
		registerHumaIncarnationGet(api, stub)
		registerHumaIncarnationFormPrefill(api, stub)
		registerHumaIncarnationHistory(api, stub)
		registerHumaIncarnationRuns(api, stub)
		registerHumaIncarnationRunDetail(api, stub)
		registerHumaIncarnationRun(api, stub)
		registerHumaIncarnationUnlock(api, stub)
		registerHumaIncarnationUpgrade(api, stub)
		registerHumaIncarnationRerunCreate(api, stub)
		registerHumaIncarnationCheckDrift(api, stub)
		registerHumaIncarnationDestroy(api, stub)
		registerHumaIncarnationUpdateHosts(api, stub)
		registerHumaIncarnationSetTraits(api, stub)
		return nil
	})
}
