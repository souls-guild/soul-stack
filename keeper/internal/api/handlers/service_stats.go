package handlers

import (
	"context"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// ServiceStatsView is one per-service row of GET /v1/stats/services.
type ServiceStatsView struct {
	Service      string
	Incarnations int
	Hosts        int
}

// ServiceStatsTyped aggregates the incarnations the caller could list, per service.
// Two scopes: the incarnation.list Purview picks the incarnations, and only members
// inside the caller's soul.list Purview are counted — the soul half of the roster
// read ([IncarnationHandler.ListMembersTyped]), so a count never includes a host
// the caller cannot see as a Soul. The roster read itself is gated by
// incarnation.get, which this aggregate does not ask for. Fail-closed: an
// incarnation scope that does not resolve yields an empty aggregate (200).
func (h *IncarnationHandler) ServiceStatsTyped(ctx context.Context, claims *jwt.Claims) ([]ServiceStatsView, error) {
	scope, ok := h.resolveListScope(ctx, claims, "list", "")
	if !ok {
		return []ServiceStatsView{}, nil
	}
	souls := soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "soul", "list"))
	hosts := func(startIdx int) (string, []any, int) {
		return souls.WhereSQL(soulpurview.Columns, startIdx)
	}
	stats, err := incarnation.SelectServiceStats(ctx, h.db, scope, hosts)
	if err != nil {
		h.logger.Error("stats.services: select failed", slog.Any("error", err))
		return nil, incProblem(problem.TypeInternalError, "service stats failed")
	}
	out := make([]ServiceStatsView, len(stats))
	for i, s := range stats {
		out[i] = ServiceStatsView{Service: s.Service, Incarnations: s.Incarnations, Hosts: s.Hosts}
	}
	return out, nil
}
