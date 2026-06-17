package api

// Регистрация и spec-dump ORACLE-домена (vigils + decrees) на huma full-typed
// (handler-native T5d-2c по эталонам role/operator/augur, ADR-054 §Pattern). vigil
// create/delete + decree create/delete — WRITE+AUDIT (вариант B, huma-audit-
// middleware; события vigil.created/vigil.deleted/decree.created/decree.deleted);
// vigil/decree list/get — read (БЕЗ audit). Доменные *Typed-функции
// (handlers/oracle.go) принимают NATIVE request-типы и возвращают доменные result-ы
// с плоскими wire-полями; register-func проецирует их в native wire-DTO
// (huma_oracle_reply.go) НАПРЯМУЮ — legacy-генерата не участвует. MCP oracle-tools зовут
// oracle.Service напрямую (мимо handler).

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaVigilCreate монтирует POST /v1/vigils через huma (WRITE+AUDIT вариант
// B — event vigil.created). oracleH nil → no-op. Handler: claims → CreateVigilTyped →
// audit-payload на huma-ctx → 201 typed output.
func registerHumaVigilCreate(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilCreateOperation(), func(ctx context.Context, in *vigilCreateInput) (*vigilCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, oracleMissingClaims()
		}
		reply, err := oracleH.CreateVigilTyped(ctx, claims, handlers.VigilCreateInput{
			Name:     in.Body.Name,
			Coven:    in.Body.Coven,
			SID:      in.Body.SID,
			Interval: in.Body.Interval,
			Check:    in.Body.Check,
			Params:   in.Body.Params,
			Enabled:  in.Body.Enabled,
		})
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &vigilCreateOutput{Status: 201, Body: newVigilView(reply.View)}, nil
	})
}

// registerHumaVigilList монтирует GET /v1/vigils через huma (READ-with-typed-query,
// БЕЗ audit). oracleH nil → no-op. Handler: typed-query → ListVigilsTyped → typed
// envelope-output. RBAC vigil.list — на группе.
func registerHumaVigilList(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilListOperation(), func(ctx context.Context, in *vigilListInput) (*vigilListOutput, error) {
		reply, err := oracleH.ListVigilsTyped(ctx, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, oracleProblem(err)
		}
		return &vigilListOutput{Body: newVigilListReply(reply)}, nil
	})
}

// registerHumaVigilGet монтирует GET /v1/vigils/{name} через huma (READ-with-path,
// БЕЗ audit). oracleH nil → no-op. Handler: GetVigilTyped(name) → typed output
// (404/422 через problem). RBAC vigil.list (read покрыт list-правом) — на группе.
func registerHumaVigilGet(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilGetOperation(), func(ctx context.Context, in *vigilGetInput) (*vigilGetOutput, error) {
		reply, err := oracleH.GetVigilTyped(ctx, in.Name)
		if err != nil {
			return nil, oracleProblem(err)
		}
		return &vigilGetOutput{Body: newVigilView(reply)}, nil
	})
}

// registerHumaVigilDelete монтирует DELETE /v1/vigils/{name} через huma (WRITE+AUDIT
// вариант B — event vigil.deleted). oracleH nil → no-op. Handler: DeleteVigilTyped →
// audit-payload → пустой 204-output.
func registerHumaVigilDelete(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilDeleteOperation(), func(ctx context.Context, in *vigilDeleteInput) (*oracleNoContentOutput, error) {
		reply, err := oracleH.DeleteVigilTyped(ctx, in.Name)
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &oracleNoContentOutput{Status: 204}, nil
	})
}

// registerHumaDecreeCreate монтирует POST /v1/decrees через huma (WRITE+AUDIT вариант
// B — event decree.created). oracleH nil → no-op. Handler: claims → CreateDecreeTyped
// → audit-payload → 201 typed output.
func registerHumaDecreeCreate(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeCreateOperation(), func(ctx context.Context, in *decreeCreateInput) (*decreeCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, oracleMissingClaims()
		}
		reply, err := oracleH.CreateDecreeTyped(ctx, claims, handlers.DecreeCreateInput{
			Name:            in.Body.Name,
			OnBeacon:        in.Body.OnBeacon,
			Coven:           in.Body.Coven,
			SID:             in.Body.SID,
			IncarnationName: in.Body.IncarnationName,
			ActionScenario:  in.Body.ActionScenario,
			ActionInput:     in.Body.ActionInput,
			Where:           in.Body.Where,
			Cooldown:        in.Body.Cooldown,
			Enabled:         in.Body.Enabled,
		})
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &decreeCreateOutput{Status: 201, Body: newDecreeView(reply.View)}, nil
	})
}

// registerHumaDecreeList монтирует GET /v1/decrees через huma (READ-with-typed-query,
// БЕЗ audit). oracleH nil → no-op. Handler: typed-query → ListDecreesTyped → typed
// envelope-output. RBAC decree.list — на группе.
func registerHumaDecreeList(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeListOperation(), func(ctx context.Context, in *decreeListInput) (*decreeListOutput, error) {
		reply, err := oracleH.ListDecreesTyped(ctx, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, oracleProblem(err)
		}
		return &decreeListOutput{Body: newDecreeListReply(reply)}, nil
	})
}

// registerHumaDecreeGet монтирует GET /v1/decrees/{name} через huma (READ-with-path,
// БЕЗ audit). oracleH nil → no-op. Handler: GetDecreeTyped(name) → typed output
// (404/422 через problem). RBAC decree.list (read покрыт list-правом) — на группе.
func registerHumaDecreeGet(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeGetOperation(), func(ctx context.Context, in *decreeGetInput) (*decreeGetOutput, error) {
		reply, err := oracleH.GetDecreeTyped(ctx, in.Name)
		if err != nil {
			return nil, oracleProblem(err)
		}
		return &decreeGetOutput{Body: newDecreeView(reply)}, nil
	})
}

// registerHumaDecreeDelete монтирует DELETE /v1/decrees/{name} через huma (WRITE+AUDIT
// вариант B — event decree.deleted). oracleH nil → no-op. Handler: DeleteDecreeTyped
// → audit-payload → пустой 204-output.
func registerHumaDecreeDelete(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeDeleteOperation(), func(ctx context.Context, in *decreeDeleteInput) (*oracleNoContentOutput, error) {
		reply, err := oracleH.DeleteDecreeTyped(ctx, in.Name)
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &oracleNoContentOutput{Status: 204}, nil
	})
}

// oracleMissingClaims — defensive-ответ при отсутствии claims в ctx (недостижим:
// RequireJWT кладёт claims до huma). problem+json (parity roleMissingClaims).
func oracleMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// oracleProblem доставляет ошибку *Typed-функции через huma как problem+json.
// Доменный *handlers.problemError → humaProblemError; не-problem → 500 (parity
// roleProblem).
func oracleProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaOracleAPI собирает huma.API поверх chi-группы с huma-audit-middleware
// (вариант B) под переданный event-тип (parity newHumaRoleAPI). Каждый write-роут
// oracle (vigil create/delete, decree create/delete) монтируется на СВОЕЙ chi-группе
// с собственным event-типом.
func newHumaOracleAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaOracleSpecYAML собирает OpenAPI-фрагмент ВСЕХ мигрированных-на-huma oracle-
// роутов как YAML-строку, БЕЗ монтирования на реальный router. Хук для спека-мерж-
// таргета тиража и guard-теста. Делегирует generic [humaDumpSpec] через те же
// register-функции (единый register-путь). Возвращает 3.1.0-спеку (huma-дефолт).
func HumaOracleSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.OracleSpecStub()
		registerHumaVigilCreate(api, stub)
		registerHumaVigilList(api, stub)
		registerHumaVigilGet(api, stub)
		registerHumaVigilDelete(api, stub)
		registerHumaDecreeCreate(api, stub)
		registerHumaDecreeList(api, stub)
		registerHumaDecreeGet(api, stub)
		registerHumaDecreeDelete(api, stub)
		return nil
	})
}
