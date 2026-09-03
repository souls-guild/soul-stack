package mcp

// keeper.incarnation.bind-member / .unbind-member / .members — parity with REST
// POST/DELETE/GET /v1/incarnations/{id}/members (ADR-008 amendment 2026-07-28,
// NIM-209). The OPERATOR path for incarnation membership: before it the only bind
// act was `core.soul.registered` INSIDE a scenario run, so a scenario deploying
// onto a ready roster was unreachable from any operator surface.
//
// SECURITY — two gates, exactly as on REST:
//
//	(a) the incarnation: body-scoped OR-Check over its coven/service scope
//	    ([Handler.checkIncarnationScope], mirroring the REST middleware). MCP has
//	    no chi middleware, so this is applied here explicitly.
//	(b) each host: the SID must sit inside the caller's soul visibility. Shared
//	    with REST through [incarnation.ScreenBindCandidates] — the screening lives
//	    in the domain precisely so the two surfaces cannot drift apart, which is
//	    how an MCP tool would otherwise become a bypass of a REST-only check.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// dedupeSortedSIDs / diffSortedSIDs / nonNilSIDs — the small set operations the
// bind tool needs; kept local rather than exported from handlers, since they are
// generic slice work with no domain meaning of their own.
func dedupeSortedSIDs(xs []string) []string {
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

func diffSortedSIDs(all, sub []string) []string {
	in := make(map[string]struct{}, len(sub))
	for _, s := range sub {
		in[s] = struct{}{}
	}
	var out []string
	for _, s := range all {
		if _, ok := in[s]; !ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func nonNilSIDs(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

type incarnationBindMemberArgs struct {
	ID   string   `json:"id"`
	SIDs []string `json:"sids"`
}

// incarnationBindMemberOutput mirrors the REST 200 body: the bind is idempotent,
// so the outcome is split into what THIS call wrote and what was already there.
type incarnationBindMemberOutput struct {
	Incarnation   string   `json:"incarnation"`
	Bound         []string `json:"bound"`
	AlreadyMember []string `json:"already_member"`
}

func (h *Handler) callIncarnationBindMember(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.bind-member"

	var a incarnationBindMemberArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !incarnation.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+incarnation.IDPattern)
	}
	if len(a.SIDs) == 0 {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'sids' must contain at least one SID")
	}
	if len(a.SIDs) > handlers.MaxBindMembersPerRequest {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'sids' carries too many entries")
	}
	for _, sid := range a.SIDs {
		if !soul.ValidSID(sid) {
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "sids entry "+sid+" must match "+soul.SIDPattern)
		}
	}
	sids := dedupeSortedSIDs(a.SIDs)

	// Gate (a). A failed probe → fail-closed on the scoped check (unlock/destroy
	// pattern); a bare/`*` holder passes through and meets the 404 below.
	inc, probeErr := incarnation.SelectByID(ctx, h.deps.IncarnationDB, a.ID)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "bind-member", a.ID, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.bind-member")
		}
		if errors.Is(probeErr, incarnation.ErrIncarnationNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "incarnation "+a.ID+" not found")
		}
		h.deps.Logger.Error("mcp: incarnation.bind-member probe failed",
			slog.String("name", a.ID), slog.Any("error", probeErr))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "select incarnation failed")
	}
	if scopeErr := h.checkIncarnationScope(claims, "bind-member", inc.ID, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.bind-member")
	}

	// Gate (b) + host status — the same domain screening REST runs.
	if h.deps.PurviewResolver == nil {
		h.deps.Logger.Error("mcp: incarnation.bind-member purview resolver not configured")
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "membership bind unavailable")
	}
	scope := soulpurview.Resolve(h.deps.PurviewResolver.ResolvePurview(claims.Subject, "soul", "list"))
	rej, err := incarnation.ScreenBindCandidates(ctx, h.deps.IncarnationDB, sids, scope)
	if err != nil {
		h.deps.Logger.Error("mcp: incarnation.bind-member screen failed",
			slog.String("name", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "select souls failed")
	}
	if !rej.Empty() {
		// Same bucket order as REST — an operator who may not see a host is told
		// "forbidden", never that the host is disconnected.
		switch {
		case len(rej.UnknownSIDs) > 0:
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"unknown SID(s) (not in the soul registry): "+strings.Join(rej.UnknownSIDs, ", "))
		case len(rej.OutOfScope) > 0:
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"SID(s) outside the operator's soul scope: "+strings.Join(rej.OutOfScope, ", "))
		default:
			return h.toolError(req.ID, toolName, mcpCodeValidationFailed,
				"SID(s) not connected — only an onboarded, connected host can be bound: "+strings.Join(rej.NotConnected, ", "))
		}
	}

	boundBy := claims.Subject
	bound, err := incarnation.AddMembersReporting(ctx, h.deps.IncarnationDB, a.ID, sids, &boundBy)
	if err != nil {
		h.deps.Logger.Error("mcp: incarnation.bind-member failed",
			slog.String("name", a.ID), slog.String("by_aid", claims.Subject), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "bind members failed")
	}
	already := diffSortedSIDs(sids, bound)

	h.writeAudit(audit.EventIncarnationMemberBound, claims.Subject, map[string]any{
		"id":             a.ID,
		"sids":           sids,
		"bound":          bound,
		"already_member": already,
	})

	return h.toolResult(req.ID, incarnationBindMemberOutput{
		Incarnation:   a.ID,
		Bound:         nonNilSIDs(bound),
		AlreadyMember: nonNilSIDs(already),
	})
}

type incarnationUnbindMemberArgs struct {
	ID  string `json:"id"`
	SID string `json:"sid"`
}

// incarnationUnbindMemberOutput — `removed` is false when the SID was not a
// member (idempotent no-op), so a caller can tell a real unbind from a repeat.
type incarnationUnbindMemberOutput struct {
	Incarnation string `json:"incarnation"`
	SID         string `json:"sid"`
	Removed     bool   `json:"removed"`
}

func (h *Handler) callIncarnationUnbindMember(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.unbind-member"

	var a incarnationUnbindMemberArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !incarnation.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+incarnation.IDPattern)
	}
	if !soul.ValidSID(a.SID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'sid' must match "+soul.SIDPattern)
	}

	inc, probeErr := incarnation.SelectByID(ctx, h.deps.IncarnationDB, a.ID)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "unbind-member", a.ID, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.unbind-member")
		}
		if errors.Is(probeErr, incarnation.ErrIncarnationNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "incarnation "+a.ID+" not found")
		}
		h.deps.Logger.Error("mcp: incarnation.unbind-member probe failed",
			slog.String("name", a.ID), slog.Any("error", probeErr))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "select incarnation failed")
	}
	if scopeErr := h.checkIncarnationScope(claims, "unbind-member", inc.ID, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.unbind-member")
	}

	if h.deps.PurviewResolver == nil {
		h.deps.Logger.Error("mcp: incarnation.unbind-member purview resolver not configured")
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "membership unbind unavailable")
	}
	scope := soulpurview.Resolve(h.deps.PurviewResolver.ResolvePurview(claims.Subject, "soul", "list"))
	inScope, known, err := incarnation.HostInScope(ctx, h.deps.IncarnationDB, a.SID, scope)
	if err != nil {
		h.deps.Logger.Error("mcp: incarnation.unbind-member scope probe failed",
			slog.String("name", a.ID), slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "select soul failed")
	}
	if known && !inScope {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"SID "+a.SID+" is outside the operator's soul scope")
	}

	removed, err := incarnation.RemoveMember(ctx, h.deps.IncarnationDB, a.ID, a.SID)
	if err != nil {
		h.deps.Logger.Error("mcp: incarnation.unbind-member failed",
			slog.String("name", a.ID), slog.String("sid", a.SID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "unbind member failed")
	}

	h.writeAudit(audit.EventIncarnationMemberUnbound, claims.Subject, map[string]any{
		"id":      a.ID,
		"sid":     a.SID,
		"removed": removed,
	})

	return h.toolResult(req.ID, incarnationUnbindMemberOutput{
		Incarnation: a.ID,
		SID:         a.SID,
		Removed:     removed,
	})
}

type incarnationMembersArgs struct {
	ID string `json:"id"`
}

// incarnationMemberEntry / incarnationMembersOutput mirror the REST roster read.
type incarnationMemberEntry struct {
	SID        string  `json:"sid"`
	Status     string  `json:"status"`
	BoundAt    string  `json:"bound_at"`
	BoundByAID *string `json:"bound_by_aid,omitempty"`
}

type incarnationMembersOutput struct {
	Incarnation string                   `json:"incarnation"`
	Items       []incarnationMemberEntry `json:"items"`
	Total       int                      `json:"total"`
}

func (h *Handler) callIncarnationMembers(ctx context.Context, claims *jwt.Claims, req jsonRPCRequest, args json.RawMessage) jsonRPCResponse {
	const toolName = "keeper.incarnation.members"

	var a incarnationMembersArgs
	if len(args) > 0 {
		if err := strictUnmarshal(args, &a); err != nil {
			return h.toolError(req.ID, toolName, mcpCodeMalformedRequest, "invalid arguments: "+err.Error())
		}
	}
	if a.ID == "" {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' is required")
	}
	if !incarnation.ValidID(a.ID) {
		return h.toolError(req.ID, toolName, mcpCodeValidationFailed, "field 'id' must match "+incarnation.IDPattern)
	}

	inc, probeErr := incarnation.SelectByID(ctx, h.deps.IncarnationDB, a.ID)
	if probeErr != nil {
		if scopeErr := h.checkIncarnationScope(claims, "get", a.ID, "", nil); scopeErr != nil {
			return h.toolError(req.ID, toolName, mcpCodeForbidden,
				"operator lacks required permission incarnation.get")
		}
		if errors.Is(probeErr, incarnation.ErrIncarnationNotFound) {
			return h.toolError(req.ID, toolName, mcpCodeNotFound, "incarnation "+a.ID+" not found")
		}
		h.deps.Logger.Error("mcp: incarnation.members probe failed",
			slog.String("name", a.ID), slog.Any("error", probeErr))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "select incarnation failed")
	}
	if scopeErr := h.checkIncarnationScope(claims, "get", inc.ID, inc.Service, inc.Covens); scopeErr != nil {
		return h.toolError(req.ID, toolName, mcpCodeForbidden,
			"operator lacks required permission incarnation.get")
	}

	members, err := incarnation.ListMembers(ctx, h.deps.IncarnationDB, a.ID)
	if err != nil {
		h.deps.Logger.Error("mcp: incarnation.members failed",
			slog.String("name", a.ID), slog.Any("error", err))
		return h.toolError(req.ID, toolName, mcpCodeInternalError, "list members failed")
	}

	// Narrowed to the caller's soul visibility, like the REST read. Fail-closed
	// without a resolver: an unevaluatable boundary yields nothing.
	items := make([]incarnationMemberEntry, 0, len(members))
	if h.deps.PurviewResolver != nil {
		scope := soulpurview.Resolve(h.deps.PurviewResolver.ResolvePurview(claims.Subject, "soul", "list"))
		for _, m := range members {
			if !soulpurview.InScope(scope, m.SID, m.Covens, soulpurview.TraitsFromJSON(m.TraitsRaw)) {
				continue
			}
			items = append(items, incarnationMemberEntry{
				SID:        m.SID,
				Status:     m.Status,
				BoundAt:    m.BoundAt.UTC().Format(time.RFC3339),
				BoundByAID: m.BoundByAID,
			})
		}
	}

	return h.toolResult(req.ID, incarnationMembersOutput{
		Incarnation: a.ID,
		Items:       items,
		Total:       len(items),
	})
}
