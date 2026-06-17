package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// L1 — driver-as-plugin через РЕАЛЬНЫЙ gRPC server+client (тот же
// RegisterCloudDriverServer, что sdk/clouddriver.Serve навешивает после
// handshake). Проверяет, что GcpDriver корректно работает по proto-контракту
// CloudDriver — включая credentials/userdata — поверх настоящего gRPC-стрима,
// а не in-proc вызова метода. Симметрично AWS-пилоту.

// serveDriverGRPC поднимает CloudDriver-сервис на TCP-loopback и возвращает
// клиент + teardown.
func serveDriverGRPC(t *testing.T, impl *GcpDriver) (pluginv1.CloudDriverClient, func()) {
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
	impl *GcpDriver
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
	f := &fakeInstances{
		getSeq: []*computepb.Instance{runningInstance("soul-l1-0", "10.2.2.2")},
	}
	withFakeInstances(t, f)

	client, teardown := serveDriverGRPC(t, &GcpDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Create(ctx, &pluginv1.CreateRequest{
		Count: 1,
		Profile: l1Struct(t, map[string]any{
			"project":      "my-project",
			"zone":         "europe-west1-b",
			"machine_type": "e2-medium",
			"source_image": "projects/debian-cloud/global/images/family/debian-12",
			"labels":       map[string]any{runLabelKey: "l1"},
		}),
		Credentials: l1Struct(t, map[string]any{
			"service_account_key": `{"type":"service_account"}`,
			"project":             "my-project",
			"zone":                "europe-west1-b",
		}),
		Userdata: "#cloud-config\n",
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
	if len(lastVms) != 1 || lastVms[0].VmId != "soul-l1-0" || lastVms[0].Fqdn == "" {
		t.Errorf("vms over gRPC = %+v", lastVms)
	}
	// userdata доехал через proto-стрим до драйвера и осел в metadata.items.
	if f.lastInsertReq == nil {
		t.Fatal("Insert not called over gRPC")
	}
	md := f.lastInsertReq.GetInstanceResource().GetMetadata()
	if md == nil {
		t.Fatal("metadata lost over gRPC marshaling")
	}
	found := false
	for _, item := range md.Items {
		if item.GetKey() == "user-data" && item.GetValue() == "#cloud-config\n" {
			found = true
		}
	}
	if !found {
		t.Error("user-data metadata item lost over gRPC marshaling")
	}
}

func TestL1_StatusOverGRPC(t *testing.T) {
	inst := runningInstance("soul-l1-stat", "10.8.8.8")
	f := &fakeInstances{getSeq: []*computepb.Instance{inst}}
	withFakeInstances(t, f)

	client, teardown := serveDriverGRPC(t, &GcpDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rep, err := client.Status(ctx, &pluginv1.StatusRequest{
		VmId: "soul-l1-stat",
		Credentials: l1Struct(t, map[string]any{
			"service_account_key": `{"type":"service_account"}`,
			"project":             "my-project",
			"zone":                "europe-west1-b",
		}),
	})
	if err != nil {
		t.Fatalf("Status rpc: %v", err)
	}
	if rep.State != "RUNNING" {
		t.Errorf("state over gRPC=%q, want RUNNING", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes lost over gRPC marshaling")
	}
}

func TestL1_DestroyOverGRPC(t *testing.T) {
	f := &fakeInstances{}
	withFakeInstances(t, f)

	client, teardown := serveDriverGRPC(t, &GcpDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Destroy(ctx, &pluginv1.DestroyRequest{
		VmIds: []string{"soul-l1-0"},
		Credentials: l1Struct(t, map[string]any{
			"service_account_key": `{"type":"service_account"}`,
			"project":             "my-project",
			"zone":                "europe-west1-b",
		}),
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
}

func l1Struct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}
