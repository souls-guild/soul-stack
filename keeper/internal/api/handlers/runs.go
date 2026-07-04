package handlers

// FULL-TYPED доменные функции глобального read-view прогонов (GET /v1/runs +
// /v1/runs/stats, страница «All Runs» UI). READ, audit НЕ пишут. Permission —
// incarnation.history (reuse read-tier per-incarnation runs, гейт RequireAction в
// router.go); сужение по Purview — здесь, тем же resolveListScope, что у List
// (action=history), fail-closed: нет claims / nil-scoper / пустой Purview →
// пустой список / нулевой агрегат (200, не 403) — parity souls/stats.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// maxAllRunsPageLimit — cap страницы GET /v1/runs (уже общего MaxPageLimit=1000:
// глобальная свёртка apply_runs дороже плоского списка).
const maxAllRunsPageLimit = 100

// AllRunsInput — вход GET /v1/runs (huma-слой биндит typed-query и передаёт сюда).
type AllRunsInput struct {
	Status      string
	Incarnation string
	// Sort — поле сортировки (started_at/finished_at/status/incarnation/scenario,
	// "" = started_at); SortDir — asc|desc ("" = desc). Невалидное → 422. ADR-068 §B1.
	Sort    string
	SortDir string
	Offset  int
	Limit   int
}

// AllRunsReply — typed envelope GET /v1/runs (handler-native, element — доменный
// RunSummaryView с заполненным Incarnation).
type AllRunsReply = sharedapi.PagedResponse[RunSummaryView]

// AllRunsTyped — доменная функция GET /v1/runs: страница прогонов через все
// инкарнации в границах Purview-scope оператора. Валидация: limit 1..100 → 400,
// невалидный status/incarnation-фильтр → 422.
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
	// Валидность sort/sort_dir — в store (whitelist), sentinel-ошибки → 422 ниже.
	filter.Sort = in.Sort
	filter.SortDir = in.SortDir

	scope, ok := h.resolveListScope(ctx, claims, "history", "")
	if !ok {
		// fail-closed: scope не определён → пустая страница, НЕ прогоны всего флота.
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

// RunsStatsBucketView — счётчики прогонов по агрегатному статусу (одна корзина
// сводки: за всё время либо за 24 часа).
type RunsStatsBucketView struct {
	Total     int
	Applying  int
	Success   int
	Failed    int
	Cancelled int
}

// RunsStatsView — плоская доменная проекция 200-тела GET /v1/runs/stats. Пакет
// api проецирует её в native wire-DTO.
type RunsStatsView struct {
	All     RunsStatsBucketView
	Last24h RunsStatsBucketView
}

// RunsStatsTyped — доменная функция GET /v1/runs/stats: сводные счётчики прогонов
// в границах Purview-scope оператора (тот же fail-closed резолв, что AllRunsTyped).
func (h *IncarnationHandler) RunsStatsTyped(ctx context.Context, claims *jwt.Claims) (RunsStatsView, error) {
	scope, ok := h.resolveListScope(ctx, claims, "history", "")
	if !ok {
		// fail-closed: нулевой агрегат (200, не 403) — не палим прогоны вне scope.
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

// runsStatsBucketView проецирует store-счётчики в доменный view.
func runsStatsBucketView(c applyrun.RunsStatusCounts) RunsStatsBucketView {
	return RunsStatsBucketView{
		Total:     c.Total,
		Applying:  c.Applying,
		Success:   c.Success,
		Failed:    c.Failed,
		Cancelled: c.Cancelled,
	}
}
