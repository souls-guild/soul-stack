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
// keeper-side core module. `core.state.present` resolves which properties of a
// state field are declared secrets, and the declaration lives in the manifest —
// which the runner has already loaded and the module has no way to reach.
type stateSchemaKey struct{}

// WithStateSchema returns ctx carrying the run service's `state_schema`. A nil
// schema is a no-op.
//
// The map is NOT copied: it belongs to the loaded service artifact and every
// reader treats it as immutable (the render path shares the same map). A module
// that mutated it would corrupt the artifact cache for the rest of the process.
func WithStateSchema(ctx context.Context, schema map[string]any) context.Context {
	if schema == nil {
		return ctx
	}
	return context.WithValue(ctx, stateSchemaKey{}, schema)
}

// StateSchemaFrom returns the run service's `state_schema`, or nil outside a
// scenario run.
func StateSchemaFrom(ctx context.Context) map[string]any {
	schema, _ := ctx.Value(stateSchemaKey{}).(map[string]any)
	return schema
}
