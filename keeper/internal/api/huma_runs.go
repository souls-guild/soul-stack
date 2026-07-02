package api

// Регистрация глобального RUNS-read-view на huma full-typed (GET /v1/runs +
// /v1/runs/stats). Оба роута — READ (БЕЗ audit, newHumaCadenceAPI), одна chi-группа
// /v1/runs под RequireAction(incarnation.history) (router.go); huma наследует
// chi-middleware. Доменные функции — на IncarnationHandler (scope-резолв и store —
// его зона: прогоны принадлежат инкарнациям).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// registerHumaRunsList монтирует GET /v1/runs (READ-with-typed-query, БЕЗ audit).
// Purview-scope — in-handler (fail-closed). incH nil → no-op.
func registerHumaRunsList(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, runsListOperation(), func(ctx context.Context, in *runsListInput) (*runsListOutput, error) {
		reply, err := incH.AllRunsTyped(ctx, claimsOrNil(ctx), handlers.AllRunsInput{
			Status:      in.Status,
			Incarnation: in.Incarnation,
			Offset:      int(in.Offset),
			Limit:       int(in.Limit),
		})
		if err != nil {
			return nil, incProblem(err)
		}
		items := make([]GlobalRunEntry, len(reply.Items))
		for i := range reply.Items {
			items[i] = newGlobalRunEntry(reply.Items[i])
		}
		return &runsListOutput{Body: runsListReply{
			Items:  items,
			Offset: int32(reply.Offset),
			Limit:  int32(reply.Limit),
			Total:  int32(reply.Total),
		}}, nil
	})
}

// registerHumaRunsStats монтирует GET /v1/runs/stats (READ-агрегат, БЕЗ audit).
// incH nil → no-op.
func registerHumaRunsStats(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, runsStatsOperation(), func(ctx context.Context, _ *runsStatsInput) (*runsStatsOutput, error) {
		reply, err := incH.RunsStatsTyped(ctx, claimsOrNil(ctx))
		if err != nil {
			return nil, incProblem(err)
		}
		return &runsStatsOutput{Body: newRunsStatsReply(reply)}, nil
	})
}
