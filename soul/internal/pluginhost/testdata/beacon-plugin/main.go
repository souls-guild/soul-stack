// Минимальный SoulBeacon-плагин для integration-теста pluginhost-а.
// Сборка: `go build -o soul-beacon-echo .` в эту директорию.
//
// Поведение:
//   - Validate: возвращает Ok=true, если в params есть "topic".
//   - Check: возвращает state="alerted", payload={"topic": <topic>}, и
//     state_cookie="echo-cookie".
//
// params.topic == "fail" → Validate возвращает Ok=false, Check возвращает
// gRPC error.
package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/beacon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type Echo struct {
	beacon.BaseBeacon
}

func (Echo) Validate(_ context.Context, req *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	topic := topicOf(req.GetParams())
	if topic == "fail" {
		return &pluginv1.ValidateVigilReply{Ok: false, Errors: []string{"topic=fail"}}, nil
	}
	if topic == "" {
		return &pluginv1.ValidateVigilReply{Ok: false, Errors: []string{"missing param: topic"}}, nil
	}
	return &pluginv1.ValidateVigilReply{Ok: true}, nil
}

func (Echo) Check(_ context.Context, req *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	topic := topicOf(req.GetParams())
	if topic == "fail" {
		return nil, status.Error(codes.FailedPrecondition, "topic=fail requested")
	}
	payload, _ := structpb.NewStruct(map[string]any{"topic": topic})
	return &pluginv1.CheckReply{
		State:       "alerted",
		Payload:     payload,
		StateCookie: []byte("echo-cookie"),
	}, nil
}

func topicOf(s *structpb.Struct) string {
	if s == nil {
		return ""
	}
	return s.GetFields()["topic"].GetStringValue()
}

func main() {
	if err := beacon.Serve(&Echo{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-beacon-echo:", err)
		os.Exit(1)
	}
}
