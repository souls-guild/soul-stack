package util

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// incarnationKey carries the incarnation of the current scenario run into a
// keeper-side core module. pluginv1.ApplyRequest deliberately has no run
// context (state + params only, ADR-012 only-add), and `on: keeper` modules run
// IN-PROCESS on the runner's context — so the runner attaches it there
// (keeper_dispatch.go). Out-of-process plugins never see this value: context
// values do not cross gRPC, and the keeper-side core is the only consumer.
type incarnationKey struct{}

// WithIncarnation returns ctx carrying the run's incarnation name. An empty
// name is a no-op (a keeper task can run without an incarnation).
func WithIncarnation(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, incarnationKey{}, name)
}

// IncarnationFrom returns the run's incarnation name, or "" when the module was
// called outside a scenario run (direct call, tests). Callers must treat "" as
// "unknown", never as "no owner".
func IncarnationFrom(ctx context.Context) string {
	name, _ := ctx.Value(incarnationKey{}).(string)
	return name
}

// serviceKey carries the run's service name into a keeper-side core module.
// Same channel and same reasoning as [incarnationKey]: a module that derives a
// Vault path from (service, incarnation, state field, key) ([ADR-0083] §1)
// needs the owner, and ApplyRequest has no run context to put it in.
type serviceKey struct{}

// WithService returns ctx carrying the run's service name. An empty name is a
// no-op (a keeper task can run outside a service run).
func WithService(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, serviceKey{}, name)
}

// ServiceFrom returns the run's service name, or "" when the module was called
// outside a scenario run (direct call, tests). "" means "unknown", never "no
// owner" — a module deriving a path from it must fail rather than build one
// with an empty segment.
func ServiceFrom(ctx context.Context) string {
	name, _ := ctx.Value(serviceKey{}).(string)
	return name
}

// stateSchemaKey carries the service manifest's `state_schema` into a
// keeper-side core module. `core.state.*` resolves which properties of a
// state field are declared secrets, and the declaration lives in the manifest —
// which the runner has already loaded and the module has no way to reach.
type stateSchemaKey struct{}

// WithStateSchema returns ctx carrying the run service's `state_schema`. A nil
// schema is a no-op.
//
// The map is NOT copied: it belongs to the loaded service artifact and every
// reader treats it as immutable (the render path shares the same map). A module
// that mutated it would corrupt the artifact cache for the rest of the process.
func WithStateSchema(ctx context.Context, schema config.InputSchemaMap) context.Context {
	if schema == nil {
		return ctx
	}
	return context.WithValue(ctx, stateSchemaKey{}, schema)
}

// StateSchemaFrom returns the run service's `state_schema`, or nil outside a
// scenario run.
func StateSchemaFrom(ctx context.Context) config.InputSchemaMap {
	schema, _ := ctx.Value(stateSchemaKey{}).(config.InputSchemaMap)
	return schema
}

// runScopeKey carries the identity of the run itself into a keeper-side core
// module. Same channel and same reasoning as [incarnationKey]: a module that
// WRITES `incarnation.state` at the step ([ADR-0084]) must record which run
// caused the change, and ApplyRequest has no run context to put it in.
type runScopeKey struct{}

// RunScope is what a state capture needs in order to name its own cause: which
// scenario is running, under which apply, on whose authority.
type RunScope struct {
	Scenario string
	ApplyID  string
	// StartedByAID is the operator who started the run. Empty means "no
	// operator" (a schedule, an internal path) and is written as NULL — the FK
	// on `state_history.changed_by_aid` allows it.
	StartedByAID string
}

// WithRunScope returns ctx carrying the run's identity. A scope without an
// apply_id is a no-op: a capture keyed on an empty apply is worse than a capture
// that fails, because it looks correlated and is not.
func WithRunScope(ctx context.Context, s RunScope) context.Context {
	if s.ApplyID == "" {
		return ctx
	}
	return context.WithValue(ctx, runScopeKey{}, s)
}

// RunScopeFrom returns the run's identity and whether it was set. A module that
// writes state must treat false as a hard failure, not as a default.
func RunScopeFrom(ctx context.Context) (RunScope, bool) {
	s, ok := ctx.Value(runScopeKey{}).(RunScope)
	return s, ok
}

// stateOpEvaluatorsKey carries the merge-time CEL evaluators into a keeper-side
// core module. Same channel and same reasoning as [incarnationKey]: `add` dedup
// and `modify`/`remove` matching evaluate a predicate PER ELEMENT, so they
// cannot be folded render-side, and only the render Pipeline can build an
// evaluator that stays inside the [ADR-0083] §7 vault fence.
type stateOpEvaluatorsKey struct{}

// StateOpEvaluators is the pair handed out together by
// render.Pipeline.StateOpEvaluators. They travel together because splitting
// them is what would let a match predicate escape the fence.
//
// A `core.state.<verb>` step gets NO scenario-context snapshot to go with them:
// its `match:` is a task param, so `${ … }` inside it was already interpolated
// against the FULL render context one phase earlier. Snapshotting the context a
// second time would freeze a staler copy of the same thing.
type StateOpEvaluators struct {
	Match render.StateMatchFunc
	Op    render.StateOpEvalFunc
}

// WithStateOpEvaluators returns ctx carrying the merge-time evaluators. A pair
// with either half missing is a no-op — a module that got one of the two would
// fail on whichever verb needs the other, at merge time, after the mint.
func WithStateOpEvaluators(ctx context.Context, e StateOpEvaluators) context.Context {
	if e.Match == nil || e.Op == nil {
		return ctx
	}
	return context.WithValue(ctx, stateOpEvaluatorsKey{}, e)
}

// StateOpEvaluatorsFrom returns the merge-time evaluators and whether they were
// set. A module reaching a verb that needs them must treat false as a hard
// failure: without an evaluator every predicate would match nothing, which
// looks like a successful no-op.
func StateOpEvaluatorsFrom(ctx context.Context) (StateOpEvaluators, bool) {
	e, ok := ctx.Value(stateOpEvaluatorsKey{}).(StateOpEvaluators)
	return e, ok
}
