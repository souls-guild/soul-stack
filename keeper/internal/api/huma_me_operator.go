package api

// The operators the caller owns and their tokens: `/v1/me/operators…` (ADR-014
// amendment NIM-1057). Authentication-only like `/v1/me/jwts`: the owner is the
// claims' subject, ownership is decided by operator.Service, and an operator
// the caller does not own answers 404 as an unknown one does. REST only.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// === GET /v1/me/operators (owned list) — READ (no audit) ===

type meOperatorListInput struct {
	Offset int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

func meOperatorListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listMyOperators",
		Method:        http.MethodGet,
		Path:          "/me/operators",
		Summary:       "Operators you own",
		Description:   "The active machine operators the caller owns, directly (owner_aid) or through a Synod it is a person member of (owner_synod; a machine operator owns nothing through a Synod), by AID (ADR-014 amendment NIM-1057). No permission. Read-only, no audit.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusInternalServerError},
	}
}

func registerHumaMeOperatorList(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meOperatorListOperation(), func(ctx context.Context, in *meOperatorListInput) (*operatorListOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		page, err := opH.ListOwnedTyped(ctx, claims, int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, operatorProblem(err)
		}
		return &operatorListOutput{Body: newOperatorListBody(page)}, nil
	})
}

// === GET /v1/me/operators/{aid}/jwts (owned token list) — READ (no audit) ===

type meOperatorJWTListInput struct {
	AID string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID of an operator the caller owns"`
	JWTListFilterQuery
	Offset int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

func meOperatorJWTListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listMyOperatorJWTs",
		Method:        http.MethodGet,
		Path:          "/me/operators/{aid}/jwts",
		Summary:       "Tokens of an operator you own",
		Description:   "GET /v1/operators/{aid}/jwts for an operator the caller owns, without operator.list (ADR-014 amendment NIM-1057). Same filters. 404 - not an operator the caller owns. Read-only, no audit.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

func registerHumaMeOperatorJWTList(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meOperatorJWTListOperation(), func(ctx context.Context, in *meOperatorJWTListInput) (*operatorJWTListOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		page, err := opH.ListOwnedJWTsTyped(ctx, claims, in.AID, toOperatorJWTFilterInput(in.JWTListFilterQuery), int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, operatorProblem(err)
		}
		items := make([]OperatorJWT, 0, len(page.Items))
		for _, v := range page.Items {
			items = append(items, newOperatorJWT(v))
		}
		return &operatorJWTListOutput{Body: sharedapi.PagedResponse[OperatorJWT]{
			Items: items, Offset: page.Offset, Limit: page.Limit, Total: page.Total,
		}}, nil
	})
}

// === POST /v1/me/operators/{aid}/jwts (owner issues) — WRITE+AUDIT operator.token-issued ===

type meOperatorJWTIssueInput struct {
	AID  string `path:"aid" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$" doc:"AID of an operator the caller owns"`
	Body *OperatorIssueTokenRequest
}

func meOperatorJWTIssueOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "issueMyOperatorJWT",
		Method:        http.MethodPost,
		Path:          "/me/operators/{aid}/jwts",
		Summary:       "Issue a token of an operator you own",
		Description:   "Mints a token (kind issued) of an operator the caller owns, without operator.issue-token (ADR-014 amendment NIM-1057). Optional body: ttl (up to auth.jwt.ttl_max; omitted -> auth.jwt.ttl_default) and label. The caller's effective permissions must cover the operator's (403 owner-lacks-permissions, naming the first one missing), and the request must not be made with a personal token (403 personal-jwt-from-personal) or with a token already revoked in the database (401 jwt-revoked). 404 - not an operator the caller owns. 409 - operator revoked. Returns the JWT once.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

func registerHumaMeOperatorJWTIssue(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meOperatorJWTIssueOperation(), func(ctx context.Context, in *meOperatorJWTIssueInput) (*meJWTIssueOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		var req handlers.OperatorIssueTokenInput
		if in.Body != nil {
			req = toOperatorIssueTokenInput(*in.Body)
		}
		reply, err := opH.IssueOwnedJWTTyped(ctx, claims, in.AID, req)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &meJWTIssueOutput{Status: http.StatusCreated, Body: newIssueTokenReply(reply)}, nil
	})
}

// === POST /v1/me/operators/{aid}/jwts/{jwt_id}/revoke (owner revokes) — WRITE+AUDIT operator.jwt-revoked ===

func meOperatorJWTRevokeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokeMyOperatorJWT",
		Method:        http.MethodPost,
		Path:          "/me/operators/{aid}/jwts/{jwt_id}/revoke",
		Summary:       "Revoke one token of an operator you own",
		Description:   "POST /v1/operators/{aid}/jwts/{jwt_id}/revoke for an operator the caller owns, without operator.jwt-revoke (ADR-014 amendment NIM-1057): the token is refused (401 jwt-revoked) on its next request on every Keeper node. 404 - not an operator the caller owns, or not a token of it. 409 - already revoked.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

func registerHumaMeOperatorJWTRevoke(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meOperatorJWTRevokeOperation(), func(ctx context.Context, in *operatorJWTRevokeInput) (*operatorNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.RevokeOwnedJWTTyped(ctx, claims, in.AID, in.JWTID)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorNoContentOutput{Status: http.StatusNoContent}, nil
	})
}
