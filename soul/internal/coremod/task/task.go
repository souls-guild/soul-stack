// Package task implements the core module `core.task` (ADR-0091 amendment
// NIM-993, rule 8). Its one state, `failed`, always fails its task.
//
// Inside a rescue unit it is the explicit "this error is not handled": the unit
// fails, and the next unit of that source gets its chance. Anywhere else it fails
// the task as Ansible's `fail` does. The `when:` on it is how a unit decides
// whether the error is handled; the module itself never reads the host.
//
// `message:` is the failure text. When it is absent the module reports
// [GenericMessage]; filling in the original error of the unit's held sources is
// done by the Keeper and the Soul runtime, not here.
package task

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — the canonical address root.
const Name = "core.task"

// GenericMessage is the failure message of a `core.task.failed` step that carries
// no `message:`.
const GenericMessage = "core.task.failed: the task was failed on purpose and gave no message"

// Module — sdk/module.SoulModule implementation for core.task. No state.
type Module struct{}

func New() *Module { return &Module{} }

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// Plan — no PlanReadSafe marker, as for core.noop: a step that changes nothing has
// no drift to report, and the host's default-deny answers a dry run with
// `plan.unsupported` rather than a false "no drift".
func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != "failed" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	msg, err := util.OptStringParam(req.Params, "message")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if msg == "" {
		msg = GenericMessage
	}
	return util.SendFailed(stream, msg)
}
