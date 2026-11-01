package scenario

import (
	"context"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// The task time rows (NIM-1170) are best-effort, as the plan they hang off is: a
// failed write costs the run view its times, never the run.

// stampTimeout bounds a time write made past the run's own ctx.
const stampTimeout = 5 * time.Second

// sendTimed sends sid its tasks over its agent stream, their time rows written
// first: a TaskEvent can come back before SendApply returns, and it finishes its
// row only if the row is already there. A failed send drops the rows: the run
// counts the host as not sent, and its tasks must not read as started. A failed
// send is not always an undelivered one; if the host runs the tasks anyway, its
// TaskEvents find no rows and those tasks show no times.
//
// Every stream send of a run's tasks goes through here; a guard holds the package
// to it.
func sendTimed(ctx context.Context, out ApplyDispatcher, db applyrun.ExecQueryRower, log *slog.Logger, sid string, req *keeperv1.ApplyRequest) error {
	stampSent(ctx, db, log, sid, req)
	err := out.SendApply(ctx, sid, req)
	if err != nil {
		forgetSent(ctx, db, log, sid, req)
	}
	return err
}

func stampSent(ctx context.Context, db applyrun.ExecQueryRower, log *slog.Logger, sid string, req *keeperv1.ApplyRequest) {
	if db == nil {
		return
	}
	if err := applyrun.StartHostTasks(ctx, db, req.GetApplyId(), sid, sentPlanIndexes(req)); err != nil {
		log.Warn("scenario: recording the start of the host's tasks failed, the run view shows no times for them",
			slog.String("sid", sid), slog.Any("error", err))
	}
}

// forgetSent outlives ctx: a send fails on a cancelled ctx too, and its stamp must
// not then read as a task the host ran.
func forgetSent(ctx context.Context, db applyrun.ExecQueryRower, log *slog.Logger, sid string, req *keeperv1.ApplyRequest) {
	if db == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stampTimeout)
	defer cancel()
	if err := applyrun.ForgetHostTasks(wctx, db, req.GetApplyId(), sid, sentPlanIndexes(req)); err != nil {
		log.Warn("scenario: dropping the times of an unsent step failed, the run view shows its first task started",
			slog.String("sid", sid), slog.Any("error", err))
	}
}

func sentPlanIndexes(req *keeperv1.ApplyRequest) []int {
	out := make([]int, len(req.GetTasks()))
	for i, t := range req.GetTasks() {
		out[i] = int(t.GetPlanIndex())
	}
	return out
}

// stampKeeperTask records a Keeper task starting (finished=false) or finishing.
// The start is written before the module runs, so a task that hangs reads as
// running from its first second.
func (r *Runner) stampKeeperTask(ctx context.Context, applyID string, rt *render.RenderedTask, finished bool, log *slog.Logger) {
	if r.applyDB == nil {
		return
	}
	var err error
	if finished {
		// A Cancel or the run timeout ends ctx while the task runs, and the finish of
		// a hung task is the one the operator needs; a Cancel also leaves running
		// apply_runs rows open, so the read would not settle this row either.
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stampTimeout)
		defer cancel()
		err = applyrun.FinishHostTask(wctx, r.applyDB, applyID, render.KeeperTargetSID, rt.Index, true)
	} else {
		err = applyrun.StartHostTasks(ctx, r.applyDB, applyID, render.KeeperTargetSID, []int{rt.Index})
	}
	if err != nil {
		log.Warn("scenario: recording the time of a keeper-side task failed, the run view shows none for it",
			slog.Int("task_idx", rt.Index), slog.Bool("finished", finished), slog.Any("error", err))
	}
}
