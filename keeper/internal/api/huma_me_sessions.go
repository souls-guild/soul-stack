package api

// POST /v1/me/sessions/revoke — the caller ends its own web sessions (ADR-058
// amendment NIM-1050). Auth-only like `/v1/me/jwts`: the owner is the claims'
// subject. Listing sessions and ending one are `/v1/me/jwts` with kind
// `session`; this route is the one thing that list cannot do in one request.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

type meSessionsRevokeInput struct {
	Body *MySessionsRevokeRequest
}

// MySessionsRevokeRequest — the optional body of POST /v1/me/sessions/revoke.
type MySessionsRevokeRequest struct {
	KeepCurrent bool `json:"keep_current,omitempty" doc:"true - end every session but the one this request is made with; omitted or false - every session, this one included"`
}

// MySessionsRevokeReply — how many sessions the request ended.
type MySessionsRevokeReply struct {
	Revoked int64 `json:"revoked" doc:"the number of web sessions ended"`
}

type meSessionsRevokeOutput struct {
	Body MySessionsRevokeReply
}

func meSessionsRevokeOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "revokeMySessions",
		Method:        http.MethodPost,
		Path:          "/me/sessions/revoke",
		Summary:       "End your web sessions",
		Description:   "Ends the caller's web sessions (tokens of kind session): every one, or with keep_current every one but the session this request is made with. Each ended session, and every Bearer exchanged from it, is refused (401 jwt-revoked) on its next request on every Keeper node. Personal and issued tokens are untouched. No permission. List sessions with GET /v1/me/jwts?kind=session; end one with POST /v1/me/jwts/{jwt_id}/revoke.",
		Tags:          []string{"operator"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

func registerHumaMeSessionsRevoke(humaAPI huma.API, opH *handlers.OperatorHandler) {
	if opH == nil {
		return
	}
	huma.Register(humaAPI, meSessionsRevokeOperation(), func(ctx context.Context, in *meSessionsRevokeInput) (*meSessionsRevokeOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, operatorMissingClaims()
		}
		keepCurrent := in.Body != nil && in.Body.KeepCurrent
		reply, err := opH.RevokeMySessionsTyped(ctx, claims, keepCurrent)
		if err != nil {
			return nil, operatorProblem(err)
		}
		apimiddleware.SetHumaAuditPayload(ctx, apimiddleware.AuditPayload(reply.AuditPayload()))
		return &meSessionsRevokeOutput{Body: MySessionsRevokeReply{Revoked: reply.Revoked}}, nil
	})
}
