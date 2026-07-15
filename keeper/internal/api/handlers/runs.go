package handlers

// FULL-TYPED domain functions for the global read view of runs (GET /v1/runs +
// /v1/runs/stats, the "All Runs" UI page). READ, no audit. Permission —
// incarnation.history (reuse the read-tier per-incarnation runs, gated by
// RequireAction in router.go); Purview narrowing is here, via the same
// resolveListScope as List (action=history), fail-closed: no claims / nil scoper /
// empty Purview → empty list / zero aggregate (200, not 403) — parity with
// souls/stats.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// maxAllRunsPageLimit — page cap for GET /v1/runs (tighter than the shared
// MaxPageLimit=1000: the global apply_runs rollup is costlier than a flat list).
const maxAllRunsPageLimit = 100

// maxRunsFilterLen — length cap for the service/q text filters (GET /v1/runs) in
// CHARACTERS (runes): longer → 422. Neither service nor q is a domain validator:
// service is a bound exact-match on incarnation.service (the column has no format
// CHECK, migration 005; registry domain, migration 034, no length cap), q is a free
// ILIKE; we cap only length, so as not to cut legitimate values or Cyrillic by
// bytes.
const maxRunsFilterLen = 128

// AllRunsInput is the input for GET /v1/runs (the huma layer binds the typed query and passes it here).
type AllRunsInput struct {
	Status      string
	Incarnation string
	// Service — filter by the owning incarnation's service ("" = all); exact match
	// (bound), not a domain validator; longer than maxRunsFilterLen → 422.
	Service string
	// Q — free-text search (substring) over incarnation/scenario/service/started_by
	// ("" = no search); longer than maxRunsFilterLen characters → 422.
	Q string
	// StartedAfter/StartedBefore — RFC3339 bounds on the run's start time
	// (inclusive; "" = not set). Invalid format → 422.
	StartedAfter  string
	StartedBefore string
	// Sort — sort field (started_at/finished_at/status/incarnation/service/
	// scenario, "" = started_at); SortDir — asc|desc ("" = desc). Invalid → 422. ADR-068 §B1.
	Sort    string
	SortDir string
	Offset  int
	Limit   int
}

// AllRunsReply — typed envelope for GET /v1/runs (handler-native, element — the
// domain RunSummaryView with Incarnation populated).
type AllRunsReply = sharedapi.PagedResponse[RunSummaryView]

// AllRunsTyped — domain function for GET /v1/runs: a page of runs across all
// incarnations within the operator's Purview scope. Validation: limit 1..100 → 400,
// invalid status/incarnation filter → 422.
func (h *IncarnationHandler) AllRunsTyped(ctx context.Context, claims *jwt.Claims, in AllRunsInput) (AllRunsReply, error) {
	var zero AllRunsReply

	if err := sharedapi.CheckPageBounds(in.Offset, in.Limit); err != nil {
		return zero, incProblem(problem.TypeMalformedRequest, err.Error())
	}
	if in.Limit > maxAllRunsPageLimit {
		return zero, incProblem(problem.TypeMalformedRequest,
			fmt.Sprintf("invalid limit %d: must be <= %d", in.Limit, maxAllRunsPageLimit))
	}
	var filter applyrun.RunsFilter
	if in.Status != "" {
		st := applyrun.RunStatus(in.Status)
		if !applyrun.ValidRunStatus(st) {
			return zero, incProblem(problem.TypeValidationFailed,
				"invalid 'status' filter: must be one of applying/success/failed/cancelled")
		}
		filter.Status = st
	}
	if in.Incarnation != "" {
		if !incarnation.ValidName(in.Incarnation) {
			return zero, incProblem(problem.TypeValidationFailed,
				"query 'incarnation' must match "+incarnation.NamePattern)
		}
		filter.Incarnation = in.Incarnation
	}
	if in.Service != "" {
		// service — a bound exact-match on incarnation.service (i.service = $N), not
		// a domain validator: registry domain (migration 034) with no length cap, the
		// column has no format CHECK (005). Garbage is safe (empty output, not 500) —
		// we cap only length (in runes, like q).
		if utf8.RuneCountInString(in.Service) > maxRunsFilterLen {
			return zero, incProblem(problem.TypeValidationFailed,
				fmt.Sprintf("query 'service' too long: must be <= %d characters", maxRunsFilterLen))
		}
		filter.Service = in.Service
	}
	if in.Q != "" {
		if utf8.RuneCountInString(in.Q) > maxRunsFilterLen {
			return zero, incProblem(problem.TypeValidationFailed,
				fmt.Sprintf("query 'q' too long: must be <= %d characters", maxRunsFilterLen))
		}
		filter.Q = in.Q
	}
	// Start time — RFC3339 bounds on the aggregate (after>before is not strictly
	// validated: empty output is harmless). Invalid format → 422.
	if in.StartedAfter != "" {
		tm, err := time.Parse(time.RFC3339, in.StartedAfter)
		if err != nil {
			return zero, incProblem(problem.TypeValidationFailed,
				"invalid 'started_after': must be RFC3339")
		}
		filter.StartedAfter = &tm
	}
	if in.StartedBefore != "" {
		tm, err := time.Parse(time.RFC3339, in.StartedBefore)
		if err != nil {
			return zero, incProblem(problem.TypeValidationFailed,
				"invalid 'started_before': must be RFC3339")
		}
		filter.StartedBefore = &tm
	}
	// sort/sort_dir validity is in the store (whitelist), sentinel errors → 422 below.
	filter.Sort = in.Sort
	filter.SortDir = in.SortDir

	scope, ok := h.resolveListScope(ctx, claims, "history", "")
	if !ok {
		// fail-closed: scope undefined → empty page, NOT runs across all incarnations.
		return AllRunsReply{Items: []RunSummaryView{}, Offset: in.Offset, Limit: in.Limit}, nil
	}

	summaries, total, err := applyrun.ListRuns(ctx, h.db, filter, scope, in.Offset, in.Limit)
	if err != nil {
		if errors.Is(err, applyrun.ErrInvalidRunsSortField) || errors.Is(err, applyrun.ErrInvalidRunsSortDir) {
			return zero, incProblem(problem.TypeValidationFailed, err.Error())
		}
		h.logger.Error("runs.list: select failed", slog.Any("error", err))
		return zero, incProblem(problem.TypeInternalError, "list runs failed")
	}
	items := make([]RunSummaryView, 0, len(summaries))
	for _, s := range summaries {
		items = append(items, newRunSummaryView(s))
	}
	return AllRunsReply{Items: items, Offset: in.Offset, Limit: in.Limit, Total: total}, nil
}

// RunsStatsBucketView — run counters by aggregate status (one summary bucket: all
// time or the last 24 hours).
type RunsStatsBucketView struct {
	Total     int
	Applying  int
	Success   int
	Failed    int
	Cancelled int
}

// RunsStatsView — flat domain projection of the 200 body of GET /v1/runs/stats. The
// api package projects it into a native wire-DTO.
type RunsStatsView struct {
	All     RunsStatsBucketView
	Last24h RunsStatsBucketView
}

// RunsStatsTyped — domain function for GET /v1/runs/stats: summary run counters
// within the operator's Purview scope (the same fail-closed resolve as AllRunsTyped).
func (h *IncarnationHandler) RunsStatsTyped(ctx context.Context, claims *jwt.Claims) (RunsStatsView, error) {
	scope, ok := h.resolveListScope(ctx, claims, "history", "")
	if !ok {
		// fail-closed: zero aggregate (200, not 403) — do not leak out-of-scope runs.
		return RunsStatsView{}, nil
	}
	stats, err := applyrun.SelectRunsStats(ctx, h.db, scope)
	if err != nil {
		h.logger.Error("runs.stats: select failed", slog.Any("error", err))
		return RunsStatsView{}, incProblem(problem.TypeInternalError, "runs stats failed")
	}
	return RunsStatsView{
		All:     runsStatsBucketView(stats.All),
		Last24h: runsStatsBucketView(stats.Last24h),
	}, nil
}

// runsStatsBucketView projects the store counters into a domain view.
func runsStatsBucketView(c applyrun.RunsStatusCounts) RunsStatsBucketView {
	return RunsStatsBucketView{
		Total:     c.Total,
		Applying:  c.Applying,
		Success:   c.Success,
		Failed:    c.Failed,
		Cancelled: c.Cancelled,
	}
}
