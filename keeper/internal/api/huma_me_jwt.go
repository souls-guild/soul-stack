package api

// The caller's own tokens: `/v1/me/jwts` (ADR-014 amendment NIM-1052). Auth-only
// like `/v1/me/permissions`: the owner is the claims' subject, never a path or
// body field, so these routes reach one operator's rows only and carry no RBAC
// gate. REST only — there are no MCP tools for them.

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// JWTListFilterQuery — the token-list filters shared by GET /v1/operators/{aid}/jwts
// and GET /v1/me/jwts. Embedded so both routes declare the same parameters.
type JWTListFilterQuery struct {
	Kinds []string `query:"kind,explode" enum:"bootstrap,issued,session,personal" doc:"multi-value ?kind=personal&kind=issued - exact-match OR; value outside enum -> 422"`
	State string   `query:"state" enum:"active,revoked,expired" doc:"active - neither revoked nor expired; revoked - revoked (expired since or not); expired - expired without being revoked; omitted -> every state"`
	Q     string   `query:"q" maxLength:"200" doc:"case-insensitive substring of the label; empty -> no filter"`
}

// === POST /v1/me/jwts (issue to self) — WRITE+AUDIT operator.token-issued ===

// meJWTIssueInput — huma input for POST /v1/me/jwts. The body is optional, as on
// issue-token: an empty request mints for `auth.jwt.ttl_default` with no label.
type meJWTIssueInput struct {
	Body *OperatorIssueTokenRequest
}

type meJWTIssueOutput struct {
	Status int `json:"-"`
	Body   IssueTokenReply
}

func meJWTIssueOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "issueMyJWT",
		Method:        http.MethodPost,
		Path:          "/me/jwts",
		Summary:       "Issue a personal token to yourself",
		Description:   "Mints a token of kind personal for the caller (ADR-014 amendment NIM-1052). No permission: the token's rights are the caller's at each request, read from the RBAC data like every token's. Optional body: ttl and label. The ttl ceiling is auth.jwt.ttl_max, or the OIDC sign-in lifetime for an OIDC operator, whose token made from a sign-in also expires with that sign-in at the latest (GET /v1/me/jwts answers both as personal_ttl_max_seconds / personal_expires_by). At most 10 active personal tokens and 20 issued in 24 hours (409 personal-jwt-limit). A request made with a personal token is refused (403 personal-jwt-from-personal). Returns the JWT once.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

func registerHumaMeJWTIssue(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meJWTIssueOperation(), func(ctx context.Context, in *meJWTIssueInput) (*meJWTIssueOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		var req handlers.OperatorIssueTokenInput
		if in.Body != nil {
			req = toOperatorIssueTokenInput(*in.Body)
		}
		reply, err := opH.IssuePersonalJWTTyped(ctx, claims, req)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &meJWTIssueOutput{Status: http.StatusCreated, Body: newIssueTokenReply(reply)}, nil
	})
}

// === GET /v1/me/jwts (own list) — READ (no audit) ===

type meJWTListInput struct {
	JWTListFilterQuery
	Offset int32 `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit  int32 `query:"limit" default:"50" doc:"page size 1..1000 (out-of-range → 400)"`
}

// MyJWTListReply — the 200 body of GET /v1/me/jwts: a page of the caller's tokens
// and what POST /v1/me/jwts would accept from it right now.
type MyJWTListReply struct {
	Items                 []OperatorJWT `json:"items" doc:"page of the caller's tokens, newest first"`
	Offset                int32         `json:"offset" doc:"offset from start of set"`
	Limit                 int32         `json:"limit" doc:"page size"`
	Total                 int32         `json:"total" doc:"total number of entries under the filters"`
	PersonalTTLMaxSeconds int64         `json:"personal_ttl_max_seconds" doc:"the longest ttl POST /v1/me/jwts accepts from the caller, in seconds"`
	PersonalExpiresBy     *time.Time    `json:"personal_expires_by,omitempty" doc:"set when the request is made with an OIDC sign-in: a personal token issued now expires at this time at the latest"`
	PersonalActive        int32         `json:"personal_active" doc:"the caller's personal tokens that are neither revoked nor expired"`
	PersonalLimit         int32         `json:"personal_limit" doc:"the most active personal tokens an operator may hold"`
	PersonalLastDay       int32         `json:"personal_last_day" doc:"the caller's personal tokens issued in the last 24 hours, revoked ones included"`
	PersonalDailyLimit    int32         `json:"personal_daily_limit" doc:"the most personal tokens an operator may issue in 24 hours"`
}

type meJWTListOutput struct {
	Body MyJWTListReply
}

func meJWTListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listMyJWTs",
		Method:        http.MethodGet,
		Path:          "/me/jwts",
		Summary:       "Your own tokens",
		Description:   "Every token of the caller, of every kind, newest first, with where it was minted and last used; never the token itself. Filters kind/state/q as on GET /v1/operators/{aid}/jwts. Also answers what POST /v1/me/jwts would accept from the caller now: the ttl ceiling, the OIDC sign-in cut-off, and the personal-token counts against both limits. No permission (ADR-014 amendment NIM-1052). Read-only, no audit.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

func registerHumaMeJWTList(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meJWTListOperation(), func(ctx context.Context, in *meJWTListInput) (*meJWTListOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		page, err := opH.ListMyJWTsTyped(ctx, claims, toOperatorJWTFilterInput(in.JWTListFilterQuery), int(in.Offset), int(in.Limit))
		if err != nil {
			return nil, operatorProblem(err)
		}
		items := make([]OperatorJWT, 0, len(page.Items))
		for _, v := range page.Items {
			items = append(items, newOperatorJWT(v))
		}
		return &meJWTListOutput{Body: MyJWTListReply{
			Items:                 items,
			Offset:                int32(page.Offset),
			Limit:                 int32(page.Limit),
			Total:                 int32(page.Total),
			PersonalTTLMaxSeconds: int64(page.PersonalTTLMax.Seconds()),
			PersonalExpiresBy:     page.PersonalExpiresBy,
			PersonalActive:        int32(page.PersonalActive),
			PersonalLimit:         int32(page.PersonalLimit),
			PersonalLastDay:       int32(page.PersonalLastDay),
			PersonalDailyLimit:    int32(page.PersonalDailyLimit),
		}}, nil
	})
}

// === POST /v1/me/jwts/{jwt_id}/revoke (revoke own) — WRITE+AUDIT operator.jwt-revoked ===

type meJWTRevokeInput struct {
	JWTID string `path:"jwt_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" doc:"jwt_id of one of the caller's tokens"`
}

func meJWTRevokeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokeMyJWT",
		Method:        http.MethodPost,
		Path:          "/me/jwts/{jwt_id}/revoke",
		Summary:       "Revoke one of your own tokens",
		Description:   "Revokes one of the caller's tokens of any kind (ADR-014 amendment NIM-1052): refused (401 jwt-revoked) on its next request on every Keeper node. No permission. 404 - not a token of the caller (also for another operator's token). 409 - already revoked. Revoking the token the request is made with succeeds and signs the caller out of it.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

func registerHumaMeJWTRevoke(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meJWTRevokeOperation(), func(ctx context.Context, in *meJWTRevokeInput) (*operatorNoContentOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		reply, err := opH.RevokeMyJWTTyped(ctx, claims, in.JWTID)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &operatorNoContentOutput{Status: http.StatusNoContent}, nil
	})
}
