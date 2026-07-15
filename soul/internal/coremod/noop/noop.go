// Package noop implements the core-module `core.noop` ([ADR-015]) — a no-op
// step that does nothing and always succeeds without changing state
// (changed=false).
//
// MVP verb:
//   - run: no-op. Doesn't read or change host state. Any `params:` are
//     accepted and ignored (empty schema) — the step exists as a syntactic
//     anchor, not an operation on a resource.
//
// Purpose:
//   - barrier anchor: a `core.noop.run` task referencing `register.*` of
//     several prior tasks gives a point where the framework waits for them
//     to finish (implicit barrier via register dependencies). The barrier
//     itself is the `require:`/register graph, not the module; noop is just
//     that task's empty body.
//   - placeholder: an empty step, handy as a stand-in in a destiny/scenario
//     skeleton before real logic exists, or as a carrier for an `output:`
//     projection (`output:` reads prior tasks' `register.*`, doing no work
//     of its own).
//
// changed semantics:
//   - changed = false ALWAYS, by construction and not configurable: no-op
//     never changes host state. Precedent — read-probe modules (`core.http`,
//     `core.exec`): the module doesn't declare drift, the scenario decides
//     the interpretation.
//
// Idempotency: no-op is idempotent by nature (empty operation).
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
package noop

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — the canonical address root.
const Name = "core.noop"

// Module — sdk/module.SoulModule implementation for core.noop. No state:
// the module holds no dependencies, its only verb `run` is a no-op.
type Module struct{}

func New() *Module { return &Module{} }

// Validate accepts only the verb `run`. `params:` aren't checked — the schema
// is empty (Apply ignores any keys), knowing the verb is enough.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no-op (no PlanReadSafe). core.noop has no desired host state to
// check with a pure-read: drift in the ADR-031 sense is undefined (changed is
// always false by construction). The host applies default-deny — dry_run for
// core.noop returns FAILED `plan.unsupported`, which is a deliberate refusal,
// not a false "no drift". The step is no-op by nature, but outside the
// Plan/Apply ADR-031 contract.
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

// ErrandReadSafe — marker [sdkmodule.ErrandReadSafe] (ADR-033 §2): no-op
// doesn't mutate host state and has no side effects, so it's safe for
// ad-hoc invocation via the Errand pull loop. Explicit opt-in to the
// Errand-runner's whitelist, symmetric with read-probe modules (core.http).
func (m *Module) ErrandReadSafe() {}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "run" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	return util.SendFinal(stream, false, nil)
}
