package api

// Registers the global RUNS read view on huma full-typed (GET /v1/runs +
// /v1/runs/stats). Both routes are READ (no audit, newHumaCadenceAPI), one chi group
// /v1/runs under RequireAction(incarnation.history) (router.go); huma inherits the
// chi middleware. Domain functions live on IncarnationHandler (scope resolution and
// store are its zone: runs belong to incarnations).

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// registerHumaRunsList mounts GET /v1/runs (READ with typed query, no audit).
// Purview-scope — in-handler (fail-closed). incH nil → no-op.
func registerHumaRunsList(humaAPI huma.API, incH *handlers.IncarnationHandler) {
	if incH == nil {
		return
	}
	huma.Register(humaAPI, runsListOperation(), func(ctx context.Context, in *runsListInput) (*runsListOutput, error) {
		reply, err := incH.AllRunsTyped(ctx, claimsOrNil(ctx), handlers.AllRunsInput{
			Status:        in.Status,
			Incarnation:   in.Incarnation,
			Service:       in.Service,
			Q:             in.Q,
			StartedAfter:  in.StartedAfter,
			StartedBefore: in.StartedBefore,
			Sort:          in.Sort,
			SortDir:       in.SortDir,
			Offset:        int(in.Offset),
			Limit:         int(in.Limit),
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

// registerHumaRunsStats mounts GET /v1/runs/stats (READ aggregate, no audit).
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
