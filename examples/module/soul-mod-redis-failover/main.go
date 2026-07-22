// Skeleton of a custom Soul Stack module in Go.
//
// Builds into a single static binary `soul-mod-redis-failover` via `go build`.
// Soul will run this binary when applying a Destiny step like `module: acme.redis-failover.promoted`
// as a sub-process, perform gRPC-stdio handshake (see sdk/handshake)
// and call SoulModule RPC methods.
//
// This is an illustration — for production code add vault link resolution, idempotency,
// OTel step tracing and real redis-cli interaction.

package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// RedisFailover is a SoulModule implementation for the `promoted` state (Redis replica switchover).
//
// BaseModule provides no-op defaults for Validate/Plan; here we override all three RPC methods,
// to demonstrate a typical set of steps.
type RedisFailover struct {
	module.BaseModule
}

// Validate performs runtime checks of parameters on top of static checks from soul-lint.
func (r *RedisFailover) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	// TODO: verify that new_master_sid is not equal to the current master; that a replica
	// with such SID exists in the cluster; that vault link password resolves.
	_ = req
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Plan is a dry-run: which steps will be executed.
func (r *RedisFailover) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	newMaster := paramString(req.Params, "new_master_sid")
	for _, msg := range []string{
		"demote-current-master: redis-cli REPLICAOF <new-master> 6379",
		"promote-new-master: redis-cli REPLICAOF NO ONE on " + newMaster,
		"verify: ensure one master and N-1 replicas",
	} {
		if err := stream.Send(&pluginv1.PlanEvent{Message: msg}); err != nil {
			return err
		}
	}
	return nil
}

// Apply performs real work with progress streaming. The final event carries
// changed/failed + output (see ApplyEvent in proto/plugin/v1/soulmodule.proto).
func (r *RedisFailover) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	password := paramString(req.Params, "password")
	newMaster := paramString(req.Params, "new_master_sid")
	if password == "" || newMaster == "" {
		return fmt.Errorf("missing required params: new_master_sid / password")
	}

	for _, msg := range []string{"demote-current-master", "promote-new-master", "verify"} {
		if err := stream.Send(&pluginv1.ApplyEvent{Message: msg + ": running"}); err != nil {
			return err
		}
		// TODO: real redis-cli interaction.
		if err := stream.Send(&pluginv1.ApplyEvent{Message: msg + ": ok"}); err != nil {
			return err
		}
	}

	output, _ := structpb.NewStruct(map[string]any{
		"new_master_sid": newMaster,
	})
	return stream.Send(&pluginv1.ApplyEvent{
		Message: "completed",
		Changed: true,
		Output:  output,
	})
}

func paramString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	v, ok := s.Fields[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}

func main() {
	if err := module.Serve(&RedisFailover{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-mod-redis-failover:", err)
		os.Exit(1)
	}
}
