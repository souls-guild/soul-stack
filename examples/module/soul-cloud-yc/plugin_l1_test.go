package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	computev1 "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// L1 — driver-as-plugin через РЕАЛЬНЫЙ gRPC server+client (тот же
// RegisterCloudDriverServer, что sdk/clouddriver.Serve навешивает после
// handshake). Проверяет, что YcDriver корректно работает по proto-контракту
// CloudDriver — включая поля credentials/userdata — поверх настоящего
// gRPC-стрима, а не in-proc вызова метода.

// serveDriverGRPC поднимает CloudDriver-сервис на TCP-loopback и возвращает
// клиент + teardown.
func serveDriverGRPC(t *testing.T, impl *YcDriver) (pluginv1.CloudDriverClient, func()) {
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

// l1Adapter — мост impl→CloudDriverServer (embed Unimplemented для forward-
// compat), идентичный sdk/clouddriver.serverAdapter (тот неэкспортирован).
type l1Adapter struct {
	pluginv1.UnimplementedCloudDriverServer
	impl *YcDriver
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
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-l1"},
		getSeq: []*computev1.Instance{
			runningInstance("epd-l1", "10.2.2.2", "soul-l1-0.ru-central1.internal"),
		},
	}
	withFakeYC(t, f)

	client, teardown := serveDriverGRPC(t, &YcDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Create(ctx, &pluginv1.CreateRequest{
		Count:       1,
		Profile:     l1Struct(t, validProfile()),
		Credentials: l1Struct(t, validCredsIAM()),
		Userdata:    "#cloud-config\n",
		Name:        "soul-l1", // NIM-16: идентичность — иначе Create fail-closed
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
	if len(lastVms) != 1 || lastVms[0].VmId != "epd-l1" || lastVms[0].Fqdn == "" {
		t.Errorf("vms over gRPC = %+v", lastVms)
	}
	// userdata доехал через proto-стрим и попал в metadata["user-data"]
	// (YC-конвенция, plain string без base64).
	if got := f.lastCreateInput.GetMetadata()[userdataMetaKey]; got != "#cloud-config\n" {
		t.Errorf("userdata lost over gRPC marshaling: metadata[user-data]=%q", got)
	}
}

func TestL1_StatusOverGRPC(t *testing.T) {
	inst := runningInstance("epd-l1-stat", "10.8.8.8", "soul-l1-stat-0.ru-central1.internal")
	f := &fakeYC{getSeq: []*computev1.Instance{inst}}
	withFakeYC(t, f)

	client, teardown := serveDriverGRPC(t, &YcDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rep, err := client.Status(ctx, &pluginv1.StatusRequest{
		VmId:        "epd-l1-stat",
		Credentials: l1Struct(t, validCredsIAM()),
	})
	if err != nil {
		t.Fatalf("Status rpc: %v", err)
	}
	if rep.State != computev1.Instance_RUNNING.String() {
		t.Errorf("state over gRPC=%q, want RUNNING", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes lost over gRPC marshaling")
	}
}

func TestL1_DestroyOverGRPC(t *testing.T) {
	f := &fakeYC{}
	withFakeYC(t, f)

	client, teardown := serveDriverGRPC(t, &YcDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Destroy(ctx, &pluginv1.DestroyRequest{
		VmIds:       []string{"epd-l1"},
		Credentials: l1Struct(t, validCredsIAM()),
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
