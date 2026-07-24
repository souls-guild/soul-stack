package util

import "context"

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
