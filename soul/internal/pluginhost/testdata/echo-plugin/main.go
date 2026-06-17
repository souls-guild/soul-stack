// Минимальный SoulModule-плагин для integration-теста pluginhost-а.
// Сборка: `go build -o soul-mod-echo .` в эту директорию.
//
// Поведение:
//   - Validate: возвращает Ok=true, если в Params присутствует "name".
//   - Plan: эхо двух событий — "plan: phase-1" и "plan: phase-2".
//   - Apply: эхо одного финального события с changed=true и output={"echo": <name>}.
//
// state="fail" → Validate возвращает Ok=false, Apply возвращает gRPC error.
package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type Echo struct {
	module.BaseModule
}

func (Echo) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	if req.GetState() == "fail" {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{"state=fail"}}, nil
	}
	if req.GetParams() == nil || req.GetParams().GetFields()["name"] == nil {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{"missing param: name"}}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

func (Echo) Plan(_ *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	for _, m := range []string{"plan: phase-1", "plan: phase-2"} {
		if err := stream.Send(&pluginv1.PlanEvent{Message: m}); err != nil {
			return err
		}
	}
	return nil
}

func (Echo) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.GetState() == "fail" {
		return status.Error(codes.FailedPrecondition, "state=fail requested")
	}
	name := ""
	if req.GetParams() != nil {
		name = req.GetParams().GetFields()["name"].GetStringValue()
	}
	out, _ := structpb.NewStruct(map[string]any{"echo": name})
	return stream.Send(&pluginv1.ApplyEvent{
		Message: "applied",
		Changed: true,
		Output:  out,
	})
}

func main() {
	if err := module.Serve(&Echo{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-mod-echo:", err)
		os.Exit(1)
	}
}
