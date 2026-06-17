package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// L1 — driver-as-plugin через РЕАЛЬНЫЙ gRPC server+client. Проверяет, что
// AzureDriver корректно работает по proto-контракту CloudDriver — включая
// credentials/userdata + composite vm_id для multi-resource VM — поверх
// настоящего gRPC-стрима, а не in-proc вызова метода. Симметрично
// soul-cloud-aws/plugin_l1_test.go.

func serveDriverGRPC(t *testing.T, impl *AzureDriver) (pluginv1.CloudDriverClient, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pluginv1.RegisterCloudDriverServer(srv, &l1Adapter{impl: impl})
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	teardown := func() {
		_ = conn.Close()
		srv.Stop()
	}
	return pluginv1.NewCloudDriverClient(conn), teardown
}

// l1Adapter — мост impl→CloudDriverServer (embed Unimplemented для forward-compat),
// идентичный sdk/clouddriver.serverAdapter (тот неэкспортирован).
type l1Adapter struct {
	pluginv1.UnimplementedCloudDriverServer
	impl *AzureDriver
}

func (a *l1Adapter) Schema(ctx context.Context, r *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	return a.impl.Schema(ctx, r)
}
func (a *l1Adapter) Validate(ctx context.Context, r *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return a.impl.Validate(ctx, r)
}
func (a *l1Adapter) Create(r *pluginv1.CreateRequest, s grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	return a.impl.Create(r, s)
}
func (a *l1Adapter) Destroy(r *pluginv1.DestroyRequest, s grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	return a.impl.Destroy(r, s)
}
func (a *l1Adapter) Status(ctx context.Context, r *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	return a.impl.Status(ctx, r)
}
func (a *l1Adapter) List(r *pluginv1.ListRequest, s grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	return a.impl.List(r, s)
}

func TestL1_CreateOverGRPC(t *testing.T) {
	withDeterministicSuffix(t, "l1c")
	vms := &fakeVMs{getSeq: []armcompute.VirtualMachinesClientGetResponse{runningVMResponse("soul-vm-l1c")}}
	nics := &fakeNICs{}
	pips := &fakePIPs{getResult: armnetwork.PublicIPAddress{
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			IPAddress:   to.Ptr("5.5.5.5"),
			DNSSettings: &armnetwork.PublicIPAddressDNSSettings{Fqdn: to.Ptr("soul-vm-l1c.westeurope.cloudapp.azure.com")},
		},
	}}
	withFakeClients(t, vms, nics, pips)

	client, teardown := serveDriverGRPC(t, &AzureDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Create(ctx, &pluginv1.CreateRequest{
		Count:       1,
		Profile:     l1Struct(t, profileMap()),
		Credentials: l1Struct(t, credsMap()),
		Userdata:    "#cloud-config\n",
	})
	if err != nil {
		t.Fatalf("Create rpc: %v", err)
	}

	var lastVms []*pluginv1.VmInfo
	var failed bool
	for {
		ev, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("recv: %v", recvErr)
		}
		if len(ev.Vms) > 0 {
			lastVms = ev.Vms
		}
		failed = ev.Failed
	}
	if failed {
		t.Fatal("create failed over gRPC")
	}
	if len(lastVms) != 1 || lastVms[0].VmId != "soul-vm-l1c" || lastVms[0].Fqdn == "" {
		t.Errorf("vms over gRPC = %+v", lastVms)
	}
	// userdata доехал через proto-стрим и был закодирован драйвером в base64
	// (Azure customData-требование, см. main.go).
	cd := vms.lastCreateVM.Properties.OSProfile.CustomData
	if cd == nil {
		t.Fatal("customData nil")
	}
	decoded, derr := base64.StdEncoding.DecodeString(*cd)
	if derr != nil || string(decoded) != "#cloud-config\n" {
		t.Errorf("userdata lost over gRPC: decoded=%q err=%v", decoded, derr)
	}
}

func TestL1_StatusOverGRPC(t *testing.T) {
	vms := &fakeVMs{getSeq: []armcompute.VirtualMachinesClientGetResponse{runningVMResponse("l1-stat")}}
	withFakeClients(t, vms, &fakeNICs{}, &fakePIPs{})

	client, teardown := serveDriverGRPC(t, &AzureDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rep, err := client.Status(ctx, &pluginv1.StatusRequest{
		VmId: "l1-stat", Credentials: l1Struct(t, credsMap()),
	})
	if err != nil {
		t.Fatalf("Status rpc: %v", err)
	}
	if rep.State != "running" {
		t.Errorf("state over gRPC=%q, want running", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes lost over gRPC marshaling")
	}
}

func TestL1_DestroyOverGRPC(t *testing.T) {
	withFastBackoff(t, 1)
	vms := &fakeVMs{}
	nics := &fakeNICs{}
	pips := &fakePIPs{}
	withFakeClients(t, vms, nics, pips)

	client, teardown := serveDriverGRPC(t, &AzureDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Destroy(ctx, &pluginv1.DestroyRequest{
		VmIds: []string{"l1-dest"}, Credentials: l1Struct(t, credsMap()),
	})
	if err != nil {
		t.Fatalf("Destroy rpc: %v", err)
	}
	got := 0
	for {
		ev, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("recv: %v", recvErr)
		}
		if ev.Failed {
			t.Fatalf("destroy failed: %s", ev.Message)
		}
		got++
	}
	if got != 1 {
		t.Errorf("destroy events=%d, want 1", got)
	}
	// 3-resource обратный порядок прошёл через gRPC.
	if len(vms.deleteCalls) != 1 || len(nics.deleteCalls) != 1 || len(pips.deleteCalls) != 1 {
		t.Errorf("3-resource destroy calls: vm=%d nic=%d pip=%d",
			len(vms.deleteCalls), len(nics.deleteCalls), len(pips.deleteCalls))
	}
}

func l1Struct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}
