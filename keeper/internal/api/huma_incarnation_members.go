package api

// huma mounting of the incarnation MEMBERSHIP sub-resource (ADR-008 amendment
// 2026-07-28, NIM-209). The typed shapes and operation metadata live in
// huma_incarnation_members_op.go; the RBAC gate per route is wired in router.go.

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
)

// registerHumaIncarnationBindMembers mounts POST /v1/incarnations/{name}/members
// (SELF-AUDIT incarnation.member_bound — written BY the handler itself inside
// BindMembersTyped). incH nil → no-op.
func registerHumaIncarnationBindMembers(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, memberBindOperation(), func(ctx context.Context, in *memberBindInput) (*memberBindOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		view, err := incH.BindMembersTyped(ctx, claims, in.Name, in.Body.SIDs)
		if err != nil {
			return nil, incProblem(err)
		}
		return &memberBindOutput{Body: newIncarnationMemberBindReply(view)}, nil
	})
}

// registerHumaIncarnationListMembers mounts GET /v1/incarnations/{name}/members
// (READ, no audit). incH nil → no-op.
func registerHumaIncarnationListMembers(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, memberListOperation(), func(ctx context.Context, in *memberListInput) (*memberListOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		page, err := incH.ListMembersTyped(ctx, claims, in.Name)
		if err != nil {
			return nil, incProblem(err)
		}
		return &memberListOutput{Body: newIncarnationMemberListReply(page)}, nil
	})
}

// registerHumaIncarnationUnbindMember mounts DELETE /v1/incarnations/{name}/members/{sid}
// (SELF-AUDIT incarnation.member_unbound — written BY the handler itself). incH nil → no-op.
func registerHumaIncarnationUnbindMember(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, memberUnbindOperation(), func(ctx context.Context, in *memberUnbindInput) (*memberUnbindOutput, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok {
			return nil, incMissingClaims()
		}
		if err := incH.UnbindMemberTyped(ctx, claims, in.Name, in.SID); err != nil {
			return nil, incProblem(err)
		}
		return &memberUnbindOutput{Status: http.StatusNoContent}, nil
	})
}

// === projection of the handler's domain results → native wire DTO ===

// newIncarnationMemberBindReply projects the domain handlers.MemberBindView into the
// native envelope (both lists non-nil — the handler already normalizes them).
func newIncarnationMemberBindReply(v handlers.MemberBindView) IncarnationMemberBindReply {
	return IncarnationMemberBindReply{
		Incarnation:   v.Incarnation,
		Bound:         v.Bound,
		AlreadyMember: v.AlreadyMember,
	}
}

// newIncarnationMemberListReply projects the domain handlers.MemberListPage into the
// native envelope (items non-nil []; offset 0, limit/total = len — a full list with no
// server-side pagination, parity with the choir voices read).
func newIncarnationMemberListReply(p handlers.MemberListPage) IncarnationMemberListReply {
	items := make([]IncarnationMember, 0, len(p.Items))
	for _, m := range p.Items {
		items = append(items, IncarnationMember{
			SID:        m.SID,
			Status:     m.Status,
			BoundAt:    m.BoundAt,
			BoundByAID: m.BoundByAID,
		})
	}
	return IncarnationMemberListReply{Items: items, Offset: 0, Limit: len(items), Total: len(items)}
}
