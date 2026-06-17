// Минимальный CloudDriver-плагин для integration-теста pluginhost-а.
// Сборка: `go build -o soul-cloud-fake .` в эту директорию.
//
// Поведение:
//   - Schema: возвращает пустой profile_schema (структуру со ссылкой "type":"object").
//   - Validate: Ok=true, если Profile.Fields["region"] непуст; иначе Ok=false.
//   - Create: эхо двух CreateEvent-ов с message="creating-1", "creating-2" + финальный с vms[].
//   - Destroy: эхо одного DestroyEvent с message="destroyed".
//   - Status: возвращает StatusReply{State: "running"}.
//   - List: эхо двух VmInfo (vm-1, vm-2).
package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

type FakeCloud struct {
	clouddriver.BaseDriver
}

func (FakeCloud) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	schema, _ := structpb.NewStruct(map[string]any{"type": "object"})
	return &pluginv1.SchemaReply{ProfileSchema: schema}, nil
}

func (FakeCloud) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	if req.GetProfile() == nil || req.GetProfile().GetFields()["region"].GetStringValue() == "" {
		return &pluginv1.ValidateProfileReply{Ok: false, Errors: []string{"missing region"}}, nil
	}
	return &pluginv1.ValidateProfileReply{Ok: true}, nil
}

func (FakeCloud) Create(_ *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	if err := stream.Send(&pluginv1.CreateEvent{Message: "creating-1"}); err != nil {
		return err
	}
	if err := stream.Send(&pluginv1.CreateEvent{Message: "creating-2"}); err != nil {
		return err
	}
	return stream.Send(&pluginv1.CreateEvent{
		Message: "done",
		Vms:     []*pluginv1.VmInfo{{VmId: "vm-new-1", PrimaryIp: "10.0.0.1"}},
	})
}

func (FakeCloud) Destroy(_ *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	return stream.Send(&pluginv1.DestroyEvent{Message: "destroyed", VmId: "vm-x"})
}

func (FakeCloud) Status(_ context.Context, _ *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return &pluginv1.StatusReply{State: "running"}, nil
}

func (FakeCloud) List(_ *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	for _, id := range []string{"vm-1", "vm-2"} {
		if err := stream.Send(&pluginv1.VmInfo{VmId: id, PrimaryIp: "10.0.0.99"}); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&FakeCloud{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-fake:", err)
		os.Exit(1)
	}
}
