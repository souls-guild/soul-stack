package api

// Registration and spec dump of the ORACLE domain (vigils + decrees) on huma full-typed
// (handler-native T5d-2c, following role/operator/augur, ADR-054 §Pattern). vigil
// create/delete + decree create/delete — WRITE+AUDIT (variant B, huma audit
// middleware; events vigil.created/vigil.deleted/decree.created/decree.deleted);
// vigil/decree list/get — read (no audit). The domain *Typed functions
// (handlers/oracle.go) take NATIVE request types and return domain results
// with flat wire fields; the register func projects them into the native wire DTO
// (huma_oracle_reply.go) DIRECTLY — the legacy generated code is not involved. MCP oracle-tools call
// oracle.Service directly (bypassing the handler).

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

// registerHumaVigilCreate mounts POST /v1/vigils via huma (WRITE+AUDIT variant
// B — event vigil.created). oracleH nil → no-op. Handler: claims → CreateVigilTyped →
// audit payload on huma-ctx → 201 typed output.
func registerHumaVigilCreate(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilCreateOperation(), func(ctx context.Context, in *vigilCreateInput) (*vigilCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, oracleMissingClaims()
		}
		reply, err := oracleH.CreateVigilTyped(ctx, claims, toVigilCreateInput(in.Body))
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &vigilCreateOutput{Status: 201, Body: newVigilView(reply.View)}, nil
	})
}

// registerHumaVigilList mounts GET /v1/vigils via huma (READ-with-typed-query,
// no audit). oracleH nil → no-op. Handler: typed query → ListVigilsTyped → typed
// envelope output. RBAC vigil.list — on the group.
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

// registerHumaVigilGet mounts GET /v1/vigils/{id} via huma (READ-with-path,
// no audit). oracleH nil → no-op. Handler: GetVigilTyped(name) → typed output
// (404/422 via problem). RBAC vigil.list (read is covered by the list permission) — on the group.
func registerHumaVigilGet(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilGetOperation(), func(ctx context.Context, in *vigilGetInput) (*vigilGetOutput, error) {
		reply, err := oracleH.GetVigilTyped(ctx, in.ID)
		if err != nil {
			return nil, oracleProblem(err)
		}
		return &vigilGetOutput{Body: newVigilView(reply)}, nil
	})
}

// registerHumaVigilSetLabel mounts PUT /v1/vigils/{id}/label via huma
// (WRITE+AUDIT variant B — event vigil.label_changed). oracleH nil → no-op.
func registerHumaVigilSetLabel(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilSetLabelOperation(), func(ctx context.Context, in *vigilSetLabelInput) (*vigilSetLabelOutput, error) {
		reply, err := oracleH.SetVigilLabelTyped(ctx, in.ID, handlers.LabelSetInput{Label: in.Body.Label})
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &vigilSetLabelOutput{Body: newVigilView(reply.Body)}, nil
	})
}

// registerHumaVigilDelete mounts DELETE /v1/vigils/{id} via huma (WRITE+AUDIT
// variant B — event vigil.deleted). oracleH nil → no-op. Handler: DeleteVigilTyped →
// audit payload → empty 204 output.
func registerHumaVigilDelete(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, vigilDeleteOperation(), func(ctx context.Context, in *vigilDeleteInput) (*oracleNoContentOutput, error) {
		reply, err := oracleH.DeleteVigilTyped(ctx, in.ID)
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &oracleNoContentOutput{Status: 204}, nil
	})
}

// registerHumaDecreeCreate mounts POST /v1/decrees via huma (WRITE+AUDIT variant
// B — event decree.created). oracleH nil → no-op. Handler: claims → CreateDecreeTyped
// → audit payload → 201 typed output.
func registerHumaDecreeCreate(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeCreateOperation(), func(ctx context.Context, in *decreeCreateInput) (*decreeCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, oracleMissingClaims()
		}
		reply, err := oracleH.CreateDecreeTyped(ctx, claims, toDecreeCreateInput(in.Body))
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &decreeCreateOutput{Status: 201, Body: newDecreeView(reply.View)}, nil
	})
}

// registerHumaDecreeList mounts GET /v1/decrees via huma (READ-with-typed-query,
// no audit). oracleH nil → no-op. Handler: typed query → ListDecreesTyped → typed
// envelope output. RBAC decree.list — on the group.
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

// registerHumaDecreeGet mounts GET /v1/decrees/{id} via huma (READ-with-path,
// no audit). oracleH nil → no-op. Handler: GetDecreeTyped(name) → typed output
// (404/422 via problem). RBAC decree.list (read is covered by the list permission) — on the group.
func registerHumaDecreeGet(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeGetOperation(), func(ctx context.Context, in *decreeGetInput) (*decreeGetOutput, error) {
		reply, err := oracleH.GetDecreeTyped(ctx, in.ID)
		if err != nil {
			return nil, oracleProblem(err)
		}
		return &decreeGetOutput{Body: newDecreeView(reply)}, nil
	})
}

// registerHumaDecreeSetLabel mounts PUT /v1/decrees/{id}/label via huma
// (WRITE+AUDIT variant B — event decree.label_changed). oracleH nil → no-op.
func registerHumaDecreeSetLabel(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeSetLabelOperation(), func(ctx context.Context, in *decreeSetLabelInput) (*decreeSetLabelOutput, error) {
		reply, err := oracleH.SetDecreeLabelTyped(ctx, in.ID, handlers.LabelSetInput{Label: in.Body.Label})
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &decreeSetLabelOutput{Body: newDecreeView(reply.Body)}, nil
	})
}

// registerHumaDecreeDelete mounts DELETE /v1/decrees/{id} via huma (WRITE+AUDIT
// variant B — event decree.deleted). oracleH nil → no-op. Handler: DeleteDecreeTyped
// → audit payload → empty 204 output.
func registerHumaDecreeDelete(humaAPI huma.API, oracleH *handlers.OracleHandler) {
	if oracleH == nil {
		return
	}
	huma.Register(humaAPI, decreeDeleteOperation(), func(ctx context.Context, in *decreeDeleteInput) (*oracleNoContentOutput, error) {
		reply, err := oracleH.DeleteDecreeTyped(ctx, in.ID)
		if err != nil {
			return nil, oracleProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &oracleNoContentOutput{Status: 204}, nil
	})
}

// oracleMissingClaims — a defensive response for missing claims in ctx (unreachable:
// RequireJWT puts claims in before huma). problem+json (parity with roleMissingClaims).
func oracleMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// oracleProblem delivers a *Typed function's error through huma as problem+json.
// A domain *handlers.problemError → humaProblemError; a non-problem error → 500 (parity
// with roleProblem).
func oracleProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaOracleAPI assembles a huma.API on top of a chi group with huma audit middleware
// (variant B) for the given event type (parity with newHumaRoleAPI). Each oracle write route
// (vigil create/delete, decree create/delete) is mounted on ITS OWN chi group
// with its own event type.
func newHumaOracleAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaOracleSpecYAML assembles the OpenAPI fragment of ALL oracle routes migrated to huma
// as a YAML string, WITHOUT mounting on a real router. A hook for the rollout's spec-merge
// target and for the guard test. Delegates to the generic [humaDumpSpec] via the same
// register functions (a single register path). Returns a 3.1.0 spec (huma default).
func HumaOracleSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.OracleSpecStub()
		registerHumaVigilCreate(api, stub)
		registerHumaVigilList(api, stub)
		registerHumaVigilGet(api, stub)
		registerHumaVigilSetLabel(api, stub)
		registerHumaVigilDelete(api, stub)
		registerHumaDecreeCreate(api, stub)
		registerHumaDecreeList(api, stub)
		registerHumaDecreeGet(api, stub)
		registerHumaDecreeSetLabel(api, stub)
		registerHumaDecreeDelete(api, stub)
		return nil
	})
}
