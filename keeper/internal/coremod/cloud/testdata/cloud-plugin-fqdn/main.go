// Минимальный CloudDriver-плагин для integration-теста PluginAdapter +
// core.cloud.provisioned. Отличается от keeper/internal/pluginhost/testdata/cloud-plugin
// только тем, что VmInfo в Create несёт fqdn (`provisioned` модуль
// требует non-empty fqdn для использования как SID, см. applyCreated).
package main

import (
	"context"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
)

type fakeCloud struct {
	clouddriver.BaseDriver
}

func (fakeCloud) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return &pluginv1.SchemaReply{}, nil
}

func (fakeCloud) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	count := int(req.GetCount())
	if count <= 0 {
		count = 1
	}
	vms := make([]*pluginv1.VmInfo, 0, count)
	for i := 0; i < count; i++ {
		vms = append(vms, &pluginv1.VmInfo{
			VmId:      fmt.Sprintf("vm-%d", i+1),
			Fqdn:      fmt.Sprintf("host-%d.example.com", i+1),
			PrimaryIp: fmt.Sprintf("10.0.0.%d", i+1),
		})
	}
	return stream.Send(&pluginv1.CreateEvent{Message: "done", Vms: vms})
}

func (fakeCloud) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	for _, id := range req.GetVmIds() {
		if err := stream.Send(&pluginv1.DestroyEvent{VmId: id, Message: "destroyed"}); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&fakeCloud{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-fqdn:", err)
		os.Exit(1)
	}
}
