package handlers

// The owner of a machine operator (ADR-014 amendment NIM-1057): setting it
// (`PUT` / `DELETE /v1/operators/{aid}/owner`, operator.set-owner) and the
// owner's own routes over the operators it owns (`/v1/me/operators…`,
// authentication-only, ownership decided in operator.Service).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// ownerProblem maps the owner sentinels to 422; ok=false for any other error.
func ownerProblem(err error) (error, bool) {
	switch {
	case errors.Is(err, operator.ErrInvalidOwner), errors.Is(err, operator.ErrOwnerNotAllowed):
		return &problemError{problem.New(problem.TypeValidationFailed, "", strings.TrimPrefix(err.Error(), "operator: "))}, true
	}
	return nil, false
}

// OperatorOwnerReply — the result of [OperatorHandler.SetOwnerTyped]: audit
// fields only (the HTTP response is an empty 204).
type OperatorOwnerReply struct {
	AID      string
	Owner    operator.Owner
	Previous operator.Owner
}

// AuditPayload — payload of `operator.owner-set`.
func (r OperatorOwnerReply) AuditPayload() middleware.AuditPayload {
	p := middleware.AuditPayload{"aid": r.AID}
	if r.Owner.AID != "" {
		p["owner_aid"] = r.Owner.AID
	}
	if r.Owner.Synod != "" {
		p["owner_synod"] = r.Owner.Synod
	}
	if r.Previous.AID != "" {
		p["previous_owner_aid"] = r.Previous.AID
	}
	if r.Previous.Synod != "" {
		p["previous_owner_synod"] = r.Previous.Synod
	}
	return p
}

// OperatorOwnerInput — the owner a request names: at most one field set; both
// empty clears it.
type OperatorOwnerInput struct {
	OwnerAID   string
	OwnerSynod string
}

// SetOwnerTyped — PUT /v1/operators/{aid}/owner (one field set) and DELETE
// /v1/operators/{aid}/owner (zero input): sets, changes or clears the owner of
// a machine operator. PUT with neither field is refused by the caller of this
// function, which knows the route; here zero means "clear".
func (h *OperatorHandler) SetOwnerTyped(ctx context.Context, claims *jwt.Claims, targetAID string, in OperatorOwnerInput) (OperatorOwnerReply, error) {
	var zero OperatorOwnerReply
	if !operator.ValidAID(targetAID) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}
	owner := operator.Owner{AID: in.OwnerAID, Synod: in.OwnerSynod}
	res, err := h.svc.SetOwner(ctx, operator.SetOwnerInput{AID: targetAID, Owner: owner})
	if p, ok := ownerProblem(err); ok {
		return zero, p
	}
	switch {
	case err == nil:
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "", "operator "+targetAID+" not found")}
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return zero, &problemError{problem.New(problem.TypeOperatorRevoked, "", "operator "+targetAID+" is revoked")}
	default:
		h.logger.Error("operator.set-owner: service failed",
			slog.String("aid", targetAID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "set owner failed")}
	}
	return OperatorOwnerReply{AID: res.AID, Owner: res.Owner, Previous: res.Previous}, nil
}

// ListOwnedTyped — GET /v1/me/operators: the active operators the caller owns,
// directly or through a Synod it is a person member of.
func (h *OperatorHandler) ListOwnedTyped(ctx context.Context, claims *jwt.Claims, offset, limit int) (OperatorListPage, error) {
	var zero OperatorListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	ops, total, err := h.svc.ListOwned(ctx, claims.Subject, offset, limit)
	if err != nil {
		h.logger.Error("me.operators: service failed", slog.String("aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list owned operators failed")}
	}
	items := make([]OperatorView, 0, len(ops))
	for _, op := range ops {
		items = append(items, toOperatorView(op))
	}
	return OperatorListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// ownedNotFound — the answer for an operator the caller does not own, the same
// as for one that does not exist.
func ownedNotFound(targetAID string) error {
	return &problemError{problem.New(problem.TypeNotFound, "", "operator "+targetAID+" not found among the operators you own")}
}

func validOwnedPath(targetAID string) error {
	if !operator.ValidAID(targetAID) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'aid' must match "+operator.AIDPattern)}
	}
	return nil
}

// IssueOwnedJWTTyped — POST /v1/me/operators/{aid}/jwts: the owner mints a
// token of an operator it owns. 403 owner-lacks-permissions when the caller's
// rights do not cover the operator's, 403 personal-jwt-from-personal for a
// request made with a personal token, 401 when that token is revoked in the
// database, 404 for an operator the caller does not own.
func (h *OperatorHandler) IssueOwnedJWTTyped(ctx context.Context, claims *jwt.Claims, targetAID string, req OperatorIssueTokenInput) (OperatorIssueTokenReply, error) {
	var zero OperatorIssueTokenReply
	if err := validOwnedPath(targetAID); err != nil {
		return zero, err
	}
	ttl, err := operator.ParseTTL(req.TTL)
	if err != nil {
		p, _ := tokenTermsProblem(err)
		return zero, p
	}
	res, err := h.svc.IssueOwnedJWT(ctx, operator.IssueOwnedJWTInput{
		AID:         targetAID,
		CallerAID:   claims.Subject,
		CallerJWTID: claims.ID,
		TTL:         ttl,
		Label:       req.Label,
		Client:      clientMeta(ctx),
	})
	if p, ok := tokenTermsProblem(err); ok {
		return zero, p
	}
	switch {
	case err == nil:
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, ownedNotFound(targetAID)
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return zero, &problemError{problem.New(problem.TypeOperatorRevoked, "", "operator "+targetAID+" is revoked")}
	case errors.Is(err, operator.ErrOwnerLacksPermissions):
		return zero, &problemError{problem.New(problem.TypeOwnerLacksPermissions, "", ownerLacksDetail(err))}
	case errors.Is(err, operator.ErrOwnedJWTFromPersonal):
		return zero, &problemError{problem.New(problem.TypePersonalJWTFromPersonal, "", strings.TrimPrefix(err.Error(), "operator: "))}
	case errors.Is(err, operator.ErrCallerJWTRevoked):
		return zero, &problemError{problem.New(problem.TypeJWTRevoked, "", "token revoked")}
	default:
		h.logger.Error("me.operator-jwt-issue: service failed",
			slog.String("aid", targetAID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "issue JWT failed")}
	}
	return OperatorIssueTokenReply{
		AID:       res.AID,
		JWT:       res.JWT,
		JWTID:     res.JWTID,
		Kind:      operator.JWTKindIssued,
		TTL:       res.TTL,
		Label:     req.Label,
		ExpiresAt: res.ExpiresAt.UTC().Truncate(time.Second),
	}, nil
}

// ownerLacksDetail names the uncovered right: the wrapped rbac error ends with
// ": <permission>".
func ownerLacksDetail(err error) string {
	msg := err.Error()
	const lead = "you own this operator but do not hold every permission it holds"
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return lead + "; missing: " + msg[i+2:]
	}
	return lead
}

// ListOwnedJWTsTyped — GET /v1/me/operators/{aid}/jwts: the tokens of an
// operator the caller owns, under f, newest first.
func (h *OperatorHandler) ListOwnedJWTsTyped(ctx context.Context, claims *jwt.Claims, targetAID string, f OperatorJWTFilterInput, offset, limit int) (OperatorJWTPage, error) {
	var zero OperatorJWTPage
	if err := validOwnedPath(targetAID); err != nil {
		return zero, err
	}
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	recs, total, err := h.svc.ListOwnedJWTs(ctx, claims.Subject, targetAID, f.domain(), offset, limit)
	switch {
	case err == nil:
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, ownedNotFound(targetAID)
	case errors.Is(err, operator.ErrInvalidJWTFilter):
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", strings.TrimPrefix(err.Error(), "operator: "))}
	default:
		h.logger.Error("me.operator-jwt-list: service failed",
			slog.String("aid", targetAID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list jwts failed")}
	}
	items := make([]OperatorJWTView, 0, len(recs))
	for _, r := range recs {
		items = append(items, toOperatorJWTView(r))
	}
	return OperatorJWTPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// RevokeOwnedJWTTyped — POST /v1/me/operators/{aid}/jwts/{jwt_id}/revoke: the
// owner revokes one token of an operator it owns.
func (h *OperatorHandler) RevokeOwnedJWTTyped(ctx context.Context, claims *jwt.Claims, targetAID, jwtID string) (OperatorJWTRevokeReply, error) {
	var zero OperatorJWTRevokeReply
	if err := validOwnedPath(targetAID); err != nil {
		return zero, err
	}
	rec, err := h.svc.RevokeOwnedJWT(ctx, claims.Subject, targetAID, jwtID)
	switch {
	case err == nil:
	case errors.Is(err, operator.ErrOperatorNotFound):
		return zero, ownedNotFound(targetAID)
	case errors.Is(err, operator.ErrJWTNotFound):
		return zero, &problemError{problem.New(problem.TypeNotFound, "",
			"jwt "+jwtID+" of operator "+targetAID+" not found")}
	case errors.Is(err, operator.ErrJWTAlreadyRevoked):
		return zero, &problemError{problem.New(problem.TypeJWTAlreadyRevoked, "", "jwt "+jwtID+" is already revoked")}
	default:
		h.logger.Error("me.operator-jwt-revoke: service failed",
			slog.String("aid", targetAID), slog.String("jwt_id", jwtID),
			slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "revoke jwt failed")}
	}
	return OperatorJWTRevokeReply{AID: rec.AID, JWTID: rec.JWTID, Kind: string(rec.Kind)}, nil
}
