// A minimal SoulModule bundle for the pluginhost integration test.
// Build: `go build -o echo .` in this directory, then stamp it
// (`<artifact> schema` piped into the trailer) — see stampArtifact in the test.
//
// The artifact serves TWO modules so the test can prove that dispatch actually reaches
// the module named on the command line rather than "the only one" or "the first one":
//
//   - echo    — Apply returns output {"echo": <name>}; capability network_outbound.
//   - reverse — Apply returns output {"echo": <name reversed>}; capability vault_access.
//
// The two capabilities differ on purpose: a host that checked the union rather than the
// spawned module would treat them as interchangeable.
//
// state="fail" → Validate returns Ok=false and Apply returns a gRPC error.
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

// echoStates is the shared state contract of both modules — same input, different
// answer, which is what makes the dispatch check meaningful.
var echoStates = map[string]module.State{
	"applied": {
		Description: "Echo applied.",
		Input: module.Input{
			"name": {Type: module.String, Required: true, Description: "value to echo back"},
		},
	},
	"fail": {
		Description: "Force failure for tests.",
	},
}

// Echo answers with the name it was given.
type Echo struct {
	module.BaseModule
}

func (Echo) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return validate(req)
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
	return apply(req, stream, func(s string) string { return s })
}

// Reverse answers with the name reversed — a different answer from the same input, so
// the test can tell which module actually ran.
type Reverse struct {
	module.BaseModule
}

func (Reverse) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return validate(req)
}

func (Reverse) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return apply(req, stream, reverse)
}

func validate(req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	if req.GetState() == "fail" {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{"state=fail"}}, nil
	}
	if req.GetParams() == nil || req.GetParams().GetFields()["name"] == nil {
		return &pluginv1.ValidateReply{Ok: false, Errors: []string{"missing param: name"}}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

func apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], transform func(string) string) error {
	if req.GetState() == "fail" {
		return status.Error(codes.FailedPrecondition, "state=fail requested")
	}
	name := ""
	if req.GetParams() != nil {
		name = req.GetParams().GetFields()["name"].GetStringValue()
	}
	out, _ := structpb.NewStruct(map[string]any{"echo": transform(name)})
	return stream.Send(&pluginv1.ApplyEvent{
		Message: "applied",
		Changed: true,
		Output:  out,
	})
}

func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

func main() {
	code := module.ServeBundleArgs(module.Bundle{
		Modules: []module.Def{
			{
				Name:         "echo",
				Description:  "Echoes a value back",
				Capabilities: []module.Capability{module.NetworkOutbound},
				States:       echoStates,
				Impl:         &Echo{},
			},
			{
				Name:         "reverse",
				Description:  "Echoes a value back, reversed",
				Capabilities: []module.Capability{module.VaultAccess},
				States:       echoStates,
				Impl:         &Reverse{},
			},
		},
	}, os.Args[1:], os.Stdout, os.Stderr)
	if code != 0 {
		fmt.Fprintln(os.Stderr, "echo: exit", code)
	}
	os.Exit(code)
}
