package scenario

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// runDeadline bounds the whole run. A linear run is bounded by its scenario_timeout
// (docs/scenario/settings.md §2.2, default 24h) and by nothing else: no 5-minute
// ceiling standing in for a dead-host detector — soul_timeout is that — and no
// keeper.yml floor, an operator's value having no say over a scenario's run. A
// staged run keeps [Runner.effectiveRunTimeout] until NIM-952 retires it.
func (r *Runner) runDeadline(spec RunSpec, scn *config.ScenarioManifest) time.Duration {
	if spec.executionModel != ExecutionLinear {
		return r.effectiveRunTimeout(scn.Tasks)
	}
	eff := config.DefaultScenarioSettings()
	if scn.Settings != nil {
		eff = config.MergeSettings(eff, *scn.Settings)
	}
	return *eff.ScenarioTimeout
}

// stepSoulTimeout is the soul_timeout of step passage's barrier owner — a lost
// stream is a failure tied to no task (settings.md §4).
func stepSoulTimeout(spec RunSpec, passage int) time.Duration {
	if passage >= 0 && passage < len(spec.stepSettings) {
		if d := spec.stepSettings[passage].SoulTimeout; d != nil {
			return *d
		}
	}
	return *config.DefaultScenarioSettings().SoulTimeout
}

// presenceSettle is how long a stream must have been there before a step that
// waited for it is sent ([stepWatch.await]): five barrier polls, a second in
// production — orders of magnitude past the reconnected Soul's WardRoster sweep.
func (r *Runner) presenceSettle() time.Duration {
	return 5 * r.pollInterval
}

// applyKeeperStep runs a keeper-side task under its task_timeout, as one step: the
// deadline covers the whole module call — every host of a `core.ssh.apply` at once
// (user decision, NIM-949). Its `timeout:` is the one spelling it has, `settings:`
// being refused on a keeper-side task. Linear runs only: a staged run keeps
// ignoring the key, as it always has.
func (r *Runner) applyKeeperStep(ctx context.Context, spec RunSpec, stateSchema config.InputSchemaMap, rt *render.RenderedTask, sealedPaths map[string]bool) (changed, failed bool, output map[string]any, message string) {
	if spec.executionModel != ExecutionLinear || rt.Timeout == "" {
		return r.applyKeeperTask(ctx, spec, stateSchema, rt, sealedPaths)
	}
	limit, err := config.ParseDuration(rt.Timeout)
	if err != nil || limit <= 0 {
		return r.applyKeeperTask(ctx, spec, stateSchema, rt, sealedPaths)
	}
	stepCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	changed, failed, output, message = r.applyKeeperTask(stepCtx, spec, stateSchema, rt, sealedPaths)
	if failed && errors.Is(stepCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		// The module's own account stays — core.soul.registered names the hosts
		// still pending in its output — after the cause.
		message = fmt.Sprintf("task timed out after %s (task_timeout): %s", limit, message)
	}
	return changed, failed, output, message
}
