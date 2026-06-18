package api

// Регистрация и spec-dump SOUL-домена на huma full-typed (ТИРАЖ-БАТЧ-2e по эталонам
// role/operator + audit-endpoint, ADR-054 §Pattern). create/coven-assign/issue-token/
// ssh-target — WRITE+AUDIT (вариант B, huma-audit-middleware; события soul.created/.coven-
// changed/.token-issued/.ssh-target.updated); list/get/soulprint/history — read (БЕЗ audit).
// Доменные *Typed-функции (handlers/soul.go) извлечены из (w,r); старый (w,r) — тонкая
// strict-оболочка (MCP soul-tools зовут soul.Service/bootstraptoken напрямую, мимо handler —
// извлечение не затрагивает; см. keeper/internal/mcp/soul_*.go).
//
// POST /v1/souls/{sid}/exec (ErrandExec) — WRITE+AUDIT (errand.invoked) с dual-status
// 200/202 + Location-header. Handler — *handlers.ErrandHandler (ExecTyped), монтируется
// на ту же /souls-группу с RBAC errand.run + ErrandSIDSelector.

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaSoulCreate монтирует POST /v1/souls через huma (WRITE+AUDIT вариант B —
// event soul.created). soulH nil → no-op. Handler: claims → конверт typed-body → CreateTyped →
// audit-payload на huma-ctx → 201 С ТЕЛОМ.
func registerHumaSoulCreate(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulCreateOperation(), func(ctx context.Context, in *soulCreateInput) (*soulCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := soulH.CreateTyped(ctx, claims, handlers.NewSoulCreateRequest(in.Body.SID, in.Body.Transport, in.Body.Covens, in.Body.Note))
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &soulCreateOutput{Status: 201, Body: newSoulCreateReply(reply.Body)}, nil
	})
}

// registerHumaSoulCovenAssign монтирует POST /v1/souls/coven через huma (WRITE+AUDIT вариант B —
// event soul.coven-changed). soulH nil → no-op. Handler: claims → конверт typed-body →
// AssignCovenTyped → audit-payload → 200 С ТЕЛОМ (custom MarshalJSON XOR label↔labels).
func registerHumaSoulCovenAssign(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulCovenAssignOperation(), func(ctx context.Context, in *soulCovenAssignInput) (*soulCovenAssignOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := soulH.AssignCovenTyped(ctx, claims, toSoulCovenAssignInput(in.Body), in.DryRun)
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload))
		return &soulCovenAssignOutput{Status: 200, Body: reply.Body}, nil
	})
}

// registerHumaSoulIssueToken монтирует POST /v1/souls/{sid}/issue-token через huma (WRITE+AUDIT
// вариант B — event soul.token-issued). soulH nil → no-op. Handler: claims → IssueTokenTyped →
// audit-payload → 200 С ТЕЛОМ (jwt; parity operator issue-token).
func registerHumaSoulIssueToken(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulIssueTokenOperation(), func(ctx context.Context, in *soulIssueTokenInput) (*soulIssueTokenOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := soulH.IssueTokenTyped(ctx, claims, in.SID, in.Force)
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &soulIssueTokenOutput{Status: 200, Body: newSoulIssueTokenReply(reply.Body)}, nil
	})
}

// registerHumaSoulSshTarget монтирует PUT /v1/souls/{sid}/ssh-target через huma (WRITE+AUDIT
// вариант B — event soul.ssh-target.updated). soulH nil → no-op. Handler: конверт typed-body →
// UpdateSshTargetTyped → audit-payload → 200 С ТЕЛОМ (snapshot).
func registerHumaSoulSshTarget(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulSshTargetOperation(), func(ctx context.Context, in *soulSshTargetInput) (*soulSshTargetOutput, error) {
		reply, err := soulH.UpdateSshTargetTyped(ctx, in.SID, toSoulSshTargetInput(in.Body))
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload))
		return &soulSshTargetOutput{Status: 200, Body: newSoulSshTargetReply(reply.Body)}, nil
	})
}

// registerHumaSoulExec монтирует POST /v1/souls/{sid}/exec через huma (WRITE+AUDIT вариант B —
// event errand.invoked). errandH nil → no-op (parity router-навеска под `if errandH != nil`).
// Handler: claims → конверт typed-body → ExecTyped → audit-payload на huma-ctx (на ОБЕИХ
// ветках 200/202) → 202 С ТЕЛОМ Accepted + Location (async) либо 200 С ТЕЛОМ Result (sync).
// Body пред-маршалится в json.RawMessage (форма errand GET). dispatcher также пишет свой
// audit-event source=api внутри Dispatch (single source of truth); middleware-event здесь —
// security navigation-trail (паритет cancel / push.apply — дубль намеренный).
func registerHumaSoulExec(humaAPI huma.API, errandH *handlers.ErrandHandler) {
	if errandH == nil {
		return
	}
	huma.Register(humaAPI, errandExecOperation(), func(ctx context.Context, in *errandExecInput) (*errandExecOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, soulMissingClaims()
		}
		reply, err := errandH.ExecTyped(ctx, claims, in.SID, toErrandExecRequest(in.Body))
		if err != nil {
			return nil, soulProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		if reply.Async {
			// 202-тело — native ErrandAccepted (errand-домен): handler-view БЕЗ json-тегов
			// нельзя маршалить напрямую (дал бы UpperCamel-ключи). Проекция byte-exact
			// с прежним ErrandAccepted (errand_id/status).
			body, merr := json.Marshal(newErrandAccepted(reply.Accepted))
			if merr != nil {
				return nil, soulProblem(merr)
			}
			return &errandExecOutput{Status: 202, Location: "/v1/errands/" + reply.ErrandID, Body: body}, nil
		}
		// 200-тело — native ErrandResult (errand-домен): см. выше (проекция view → native
		// wire-DTO даёт byte-exact json-теги/omitempty/порядок полей).
		body, merr := json.Marshal(newErrandResult(reply.Result))
		if merr != nil {
			return nil, soulProblem(merr)
		}
		return &errandExecOutput{Status: 200, Body: body}, nil
	})
}

// registerHumaSoulList монтирует GET /v1/souls через huma (READ-with-typed-query, БЕЗ audit).
// soulH nil → no-op. Пагинацию разбирает ParsePageWithCursor над теми же query-значениями,
// что huma-биндит (offset+cursor конфликт → 422, битый cursor → 400, out-of-range → 400) —
// единый source-of-truth с (w,r). RBAC soul.list — на группе.
func registerHumaSoulList(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulListOperation(), func(ctx context.Context, in *soulListInput) (*soulListOutput, error) {
		page, cursor, perr := soulParsePage(int(in.Offset), int(in.Limit), in.Cursor)
		if perr != nil {
			return nil, perr
		}
		reply, err := soulH.ListTyped(ctx, claimsOrNil(ctx), handlers.SoulListInput{
			Coven:     in.Coven,
			Status:    in.Status,
			Transport: in.Transport,
			Page:      page,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, soulProblem(err)
		}
		items := make([]SoulListEntry, len(reply.Items))
		for i := range reply.Items {
			items[i] = newSoulListEntry(reply.Items[i])
		}
		out := soulListReply{
			Items:      items,
			Offset:     int32(reply.Offset),
			Limit:      int32(reply.Limit),
			Total:      int32(reply.Total),
			NextCursor: reply.NextCursor,
		}
		// total_approximate — omitempty в обоих формах: keyset-режим даёт true →
		// *bool(&true) (ключ present), offset-режим false → nil (ключ опущен,
		// byte-exact с PagedResponse.TotalApproximate `bool omitempty`).
		if reply.TotalApproximate {
			ta := true
			out.TotalApproximate = &ta
		}
		return &soulListOutput{Body: out}, nil
	})
}

// registerHumaSoulGet монтирует GET /v1/souls/{sid} через huma (READ-with-path, БЕЗ audit).
func registerHumaSoulGet(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulGetOperation(), func(ctx context.Context, in *soulGetInput) (*soulGetOutput, error) {
		reply, err := soulH.GetTyped(ctx, claimsOrNil(ctx), in.SID)
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulGetOutput{Body: newSoulListEntry(reply)}, nil
	})
}

// registerHumaSoulSoulprint монтирует GET /v1/souls/{sid}/soulprint через huma (READ-with-path,
// БЕЗ audit).
func registerHumaSoulSoulprint(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulSoulprintOperation(), func(ctx context.Context, in *soulSoulprintInput) (*soulSoulprintOutput, error) {
		reply, err := soulH.GetSoulprintTyped(ctx, claimsOrNil(ctx), in.SID)
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulSoulprintOutput{Body: reply}, nil
	})
}

// registerHumaSoulHistory монтирует GET /v1/souls/{sid}/history через huma (READ-with-typed-
// query, БЕЗ audit). CheckPageBounds → 400 (диапазон enforce-ит ДОМЕН).
func registerHumaSoulHistory(humaAPI huma.API, soulH *handlers.SoulHandler) {
	if soulH == nil {
		return
	}
	huma.Register(humaAPI, soulHistoryOperation(), func(ctx context.Context, in *soulHistoryInput) (*soulHistoryOutput, error) {
		reply, err := soulH.HistoryTyped(ctx, claimsOrNil(ctx), handlers.SoulHistoryInput{
			SID:    in.SID,
			Types:  in.Types,
			Since:  in.Since,
			Offset: int(in.Offset),
			Limit:  int(in.Limit),
		})
		if err != nil {
			return nil, soulProblem(err)
		}
		return &soulHistoryOutput{Body: newSoulHistoryReply(reply)}, nil
	})
}

// soulParsePage воспроизводит ParsePageWithCursor над уже-huma-биндингованными offset/limit/
// cursor: out-of-range диапазон → 400 (CheckPageBounds), битый cursor → 400, cursor+offset>0 →
// 422 (конфликт двух пагинаций). Единый контракт с (w,r)-обёрткой SoulHandler.List.
func soulParsePage(offset, limit int, cursorRaw string) (sharedapi.Page, *sharedapi.KeysetCursor, huma.StatusError) {
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return sharedapi.Page{}, nil, humaProblemError{Details: problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	var cursor *sharedapi.KeysetCursor
	if cursorRaw != "" {
		c, err := sharedapi.DecodeKeysetCursor(cursorRaw)
		if err != nil {
			return sharedapi.Page{}, nil, humaProblemError{Details: problem.New(problem.TypeMalformedRequest, "", err.Error())}
		}
		cursor = &c
	}
	if cursor != nil && offset > 0 {
		return sharedapi.Page{}, nil, humaProblemError{Details: problem.New(problem.TypeValidationFailed, "",
			"cursor and offset are mutually exclusive: use either keyset cursor or offset pagination, not both")}
	}
	return sharedapi.Page{Offset: offset, Limit: limit}, cursor, nil
}

// claimsOrNil извлекает claims из ctx (RequireJWT положил до huma); нет → nil (read-handler-ы
// fail-closed на nil claims через readScopeForClaims → 404/пустой список).
func claimsOrNil(ctx context.Context) *keeperjwt.Claims {
	claims, ok := apimiddleware.ClaimsFromContext(ctx)
	if !ok {
		return nil
	}
	return claims
}

// toSoulCovenAssignInput — конверт typed huma-body → NATIVE доменная модель
// handlers.SoulCovenAssignInput (handler-native §Pattern шаг 3). huma-форма value/slice
// пробрасывается напрямую (handler.derefCovenAssign трактует пустые поля как «не задано»).
func toSoulCovenAssignInput(b SoulCovenAssignRequest) handlers.SoulCovenAssignInput {
	return handlers.SoulCovenAssignInput{
		Mode:   b.Mode,
		Label:  b.Label,
		Labels: b.Labels,
		DryRun: b.DryRun,
		Selector: handlers.SoulCovenAssignSelectorInput{
			All:         b.Selector.All,
			SIDs:        b.Selector.Sids,
			Coven:       b.Selector.Coven,
			Incarnation: b.Selector.Incarnation,
			Status:      b.Selector.Status,
		},
	}
}

// toSoulSshTargetInput — конверт typed huma-body → NATIVE доменная модель
// handlers.SoulSshTargetInput. ssh_provider пусто → routing на coven/cluster default.
func toSoulSshTargetInput(b SoulSshTarget) handlers.SoulSshTargetInput {
	return handlers.SoulSshTargetInput{
		SSHPort:     b.SSHPort,
		SSHUser:     b.SSHUser,
		SoulPath:    b.SoulPath,
		SSHProvider: b.SSHProvider,
	}
}

// toErrandExecRequest — конверт typed huma-body → NATIVE request errand-домена
// (handlers.ErrandRunInput). Поля input/timeout_seconds/dry_run уже pointer-optional в
// huma-форме (handler разыменовывает); module — value. Прямой проброс без перепаковки.
func toErrandExecRequest(b ErrandRunRequest) handlers.ErrandRunInput {
	return handlers.ErrandRunInput{
		Module:         b.Module,
		Input:          b.Input,
		TimeoutSeconds: b.TimeoutSeconds,
		DryRun:         b.DryRun,
	}
}

// soulMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим: RequireJWT
// кладёт claims до huma). problem+json (parity roleMissingClaims).
func soulMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// soulProblem доставляет ошибку *Typed-функции через huma как problem+json. Доменный
// *handlers.problemError → humaProblemError; не-problem → 500 (parity roleProblem).
func soulProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaSoulAPI собирает huma.API поверх chi-группы с huma-audit-middleware (вариант B) под
// переданный event-тип (parity newHumaRoleAPI). Каждый write-роут soul (create/coven-assign/
// issue-token/ssh-target) монтируется на СВОЕЙ chi-группе с собственным event-типом.
func newHumaSoulAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaSoulSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma soul-роутов (create/
// coven-assign/issue-token/ssh-target/exec/list/get/soulprint/history) как YAML-строку, БЕЗ
// монтирования на реальный router. Хук для спека-мерж-таргета тиража и guard-теста.
// Делегирует generic [humaDumpSpec] через те же register-функции. exec монтируется через
// ErrandSpecStub (handler — *handlers.ErrandHandler). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaSoulSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.SoulSpecStub()
		registerHumaSoulCreate(api, stub)
		registerHumaSoulCovenAssign(api, stub)
		registerHumaSoulIssueToken(api, stub)
		registerHumaSoulSshTarget(api, stub)
		registerHumaSoulExec(api, handlers.ErrandSpecStub())
		registerHumaSoulList(api, stub)
		registerHumaSoulGet(api, stub)
		registerHumaSoulSoulprint(api, stub)
		registerHumaSoulHistory(api, stub)
		return nil
	})
}

// soulSentinel — линковка soul-пакета (используется в op-файле enum-наборах через домен;
// явный ref гарантирует, что рассинхрон enum↔домен ловится компилятором при правке).
var _ = soul.StatusPending
