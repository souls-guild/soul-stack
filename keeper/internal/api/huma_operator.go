package api

// Registration and spec-dump of the OPERATOR domain on huma full-typed (ROLLOUT BATCH 2a
// against the 5 references, ADR-054 §Pattern). create/revoke/issue-token — WRITE+AUDIT (variant B,
// huma-audit-middleware; full-typed huma writes the response ITSELF, StatusRecorder does not
// apply — audit holds hctx.Status() + carrier-payload, otherwise S6 recurs). list — read-with-
// typed-query (no audit). get — read-with-path (no audit). The domain *Typed functions
// (handlers/operator.go) are extracted from (w,r); the old (w,r) is a thin strict wrapper
// (MCP operator-tools call operator.Service directly, bypassing the handler — their extraction
// does not affect them).

import (
	"context"
	"log/slog"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// registerHumaOperatorCreate mounts POST /v1/operators via huma (WRITE+AUDIT
// variant B — event operator.created). opH nil → no-op (opt-in-domain pattern).
// Handler: claims → CreateTyped → audit-payload on the huma-ctx (SetHumaAuditPayload) →
// 201 typed output. Domain problem errors — via humaProblemError.
func registerHumaOperatorCreate(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorCreateOperation(), func(ctx context.Context, in *operatorCreateInput) (*operatorCreateOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.CreateTyped(ctx, claims, handlers.OperatorCreateInput{
			AID:         in.Body.AID,
			DisplayName: in.Body.DisplayName,
			Roles:       in.Body.Roles,
		})
		if err != nil {
			return nil, operatorProblem(err)
		}
		// Audit-payload on the huma-ctx — the SINGLE source reply.AuditPayload() (the same
		// method as revoke/issue-token).
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorCreateOutput{Status: 201, Body: newOperatorCreateReply(reply)}, nil
	})
}

// registerHumaOperatorList mounts GET /v1/operators via huma (READ-with-typed-
// query, no audit). opH nil → no-op. Handler: typed-query → convert to the domain
// filter → ListTyped → typed envelope-output. RBAC operator.list — on the group.
func registerHumaOperatorList(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorListOperation(), func(ctx context.Context, in *operatorListInput) (*operatorListOutput, error) {
		filter := operator.ListFilter{
			AuthMethod:     operator.AuthMethod(in.AuthMethod), // empty → filter not applied; the huma enum already rejected out-of-set (422)
			IncludeRevoked: in.Revoked,
			Q:              in.Q, // pass-through: empty → no filter (parity /v1/runs)
		}
		page, err := opH.ListTyped(ctx, filter, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, operatorProblem(err)
		}
		return &operatorListOutput{Body: newOperatorListBody(page)}, nil
	})
}

// registerHumaOperatorGet mounts GET /v1/operators/{aid} via huma (READ-with-
// path, no audit). opH nil → no-op. Handler: GetTyped(aid) → typed output (404/422
// via problem). RBAC operator.list (read is covered by the list right) — on the group.
func registerHumaOperatorGet(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorGetOperation(), func(ctx context.Context, in *operatorGetInput) (*operatorGetOutput, error) {
		view, err := opH.GetTyped(ctx, in.AID)
		if err != nil {
			return nil, operatorProblem(err)
		}
		return &operatorGetOutput{Body: newOperator(view)}, nil
	})
}

// registerHumaOperatorRevoke mounts POST /v1/operators/{aid}/revoke via huma
// (WRITE+AUDIT variant B — event operator.revoked). opH nil → no-op. Handler:
// claims → RevokeTyped(aid, reason) → audit-payload → empty 204 output.
func registerHumaOperatorRevoke(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorRevokeOperation(), func(ctx context.Context, in *operatorRevokeInput) (*operatorNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.RevokeTyped(ctx, claims, in.AID, in.Body.Reason)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorNoContentOutput{Status: 204}, nil
	})
}

// registerHumaOperatorIssueToken mounts POST /v1/operators/{aid}/issue-token via
// huma (WRITE+AUDIT variant B — event operator.token-issued). opH nil → no-op.
// Handler: claims → IssueTokenTyped(aid) → audit-payload → 200 WITH BODY (jwt; unlike
// the 204 write routes — issue-token returns the issued token).
func registerHumaOperatorIssueToken(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, operatorIssueTokenOperation(), func(ctx context.Context, in *operatorIssueTokenInput) (*operatorIssueTokenOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.IssueTokenTyped(ctx, claims, in.AID)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorIssueTokenOutput{Status: 200, Body: newIssueTokenReply(reply)}, nil
	})
}

// === projection of the handler's domain results → native wire-DTO (handler-native
// PILOT: the api↔handlers boundary builds the schema body from flat domain fields; huma
// derives the OpenAPI schema from these native types, generated types are not involved). ===

// newOperatorCreateReply projects the domain handlers.OperatorCreateReply (flat
// fields) into the native 201 body. roles — `*[]string` with omitempty: an empty granted set →
// nil (field omitted, backward-compat with a request-without-roles).
func newOperatorCreateReply(r handlers.OperatorCreateReply) OperatorCreateReply {
	out := OperatorCreateReply{
		AID:          r.AID,
		CreatedAt:    r.CreatedAt,
		CreatedByAID: r.CreatedByAID,
		DisplayName:  r.DisplayName,
		JWT:          r.JWT,
	}
	if len(r.GrantedRoles) > 0 {
		roles := r.GrantedRoles
		out.Roles = &roles
	}
	return out
}

// newIssueTokenReply projects the domain handlers.OperatorIssueTokenReply into the native
// issue-token 200 body.
func newIssueTokenReply(r handlers.OperatorIssueTokenReply) IssueTokenReply {
	return IssueTokenReply{AID: r.AID, ExpiresAt: r.ExpiresAt, JWT: r.JWT}
}

// newOperator projects the domain handlers.OperatorView into the native Operator (get-200 /
// list-element). auth_method — the native enum OperatorAuthMethod; metadata — `*map`
// with omitempty (empty → nil, key omitted).
func newOperator(v handlers.OperatorView) Operator {
	out := Operator{
		AID:              v.AID,
		AuthMethod:       OperatorAuthMethod(v.AuthMethod),
		BootstrapInitial: v.BootstrapInitial,
		CreatedAt:        v.CreatedAt,
		CreatedByAID:     v.CreatedByAID,
		CreatedVia:       v.CreatedVia,
		DisplayName:      v.DisplayName,
		RevokedAt:        v.RevokedAt,
	}
	if len(v.Metadata) > 0 {
		m := map[string]interface{}(v.Metadata)
		out.Metadata = &m
	}
	return out
}

// newOperatorListBody projects the domain handlers.OperatorListPage into the native
// list-envelope PagedResponse[Operator] (items non-nil [], offset/limit/total).
func newOperatorListBody(p handlers.OperatorListPage) sharedapi.PagedResponse[Operator] {
	items := make([]Operator, 0, len(p.Items))
	for _, v := range p.Items {
		items = append(items, newOperator(v))
	}
	return sharedapi.PagedResponse[Operator]{
		Items:  items,
		Offset: p.Offset,
		Limit:  p.Limit,
		Total:  p.Total,
	}
}

// operatorMissingClaims — a defensive response when claims are absent from ctx (unreachable:
// RequireJWT puts claims before huma). problem+json (parity roleMissingClaims).
func operatorMissingClaims() huma.StatusError {
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "missing claims")}
}

// operatorProblem delivers a *Typed function error through huma as problem+json.
// A domain *handlers.problemError → humaProblemError; non-problem → 500 (parity
// roleProblem / auditProblem).
func operatorProblem(err error) huma.StatusError {
	if d, ok := handlers.AsProblemDetails(err); ok {
		return humaProblemError{Details: d}
	}
	return humaProblemError{Details: problem.New(problem.TypeInternalError, "", "internal error")}
}

// newHumaOperatorAPI assembles a huma.API over a chi group with the huma-audit-middleware
// (variant B) under the given event type (parity newHumaRoleAPI). Each operator write route
// (create/revoke/issue-token) is mounted on ITS OWN chi group with its own
// event type.
func newHumaOperatorAPI(r chi.Router, writer audit.Writer, evt audit.EventType, logger *slog.Logger) huma.API {
	return newHumaAuditAPI(r, writer, evt, logger)
}

// HumaOperatorSpecYAML assembles the OpenAPI fragment of ALL operator routes migrated to
// huma as a YAML string, WITHOUT mounting on a real router. A hook for the rollout spec-merge
// target and the guard test. Delegates to the generic [humaDumpSpec] through the same
// register functions (a single register path). Returns a 3.1.0 spec (huma default).
func HumaOperatorSpecYAML() (string, error) {
	return humaDumpSpec(func(api huma.API) error {
		stub := handlers.OperatorSpecStub()
		registerHumaOperatorCreate(api, stub)
		registerHumaOperatorList(api, stub)
		registerHumaOperatorGet(api, stub)
		registerHumaOperatorRevoke(api, stub)
		registerHumaOperatorIssueToken(api, stub)
		return nil
	})
}
