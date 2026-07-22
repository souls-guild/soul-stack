package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// L1 is driver-as-plugin through a REAL gRPC server+client (the same
// RegisterCloudDriverServer that sdk/clouddriver.Serve attaches after
// handshake). It verifies that ProxmoxDriver correctly works through the
// CloudDriver proto contract - including credentials/userdata + composite vm_id
// format `<node>/<vmid>` - over a real gRPC stream, not an in-proc method call.

// serveDriverGRPC starts the CloudDriver service on TCP loopback and returns a
// client + teardown.
func serveDriverGRPC(t *testing.T, impl *ProxmoxDriver) (pluginv1.CloudDriverClient, func()) {
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

// l1Adapter bridges impl -> CloudDriverServer (embedding Unimplemented for
// forward compatibility), matching sdk/clouddriver.serverAdapter (not exported).
type l1Adapter struct {
	pluginv1.UnimplementedCloudDriverServer
	impl *ProxmoxDriver
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
	withFastBackoff(t, 4)
	f := &fakePVE{
		statusSeq: []VMStatus{runningStatus("soul-10000")},
		agentSeq:  []string{"10.2.2.2"},
	}
	withFakePVE(t, f)

	client, teardown := serveDriverGRPC(t, &ProxmoxDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Create(ctx, &pluginv1.CreateRequest{
		Count:       1,
		Profile:     l1Struct(t, baseProfile(nil)),
		Credentials: l1Struct(t, baseCreds()),
		Userdata:    "#cloud-config\nhostname: t\n",
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
	if len(lastVms) != 1 {
		t.Fatalf("vms=%d, want 1", len(lastVms))
	}
	if lastVms[0].VmId != "pve1/10000" {
		t.Errorf("vm_id over gRPC=%q, want composite `pve1/10000`", lastVms[0].VmId)
	}
	if lastVms[0].PrimaryIp != "10.2.2.2" {
		t.Errorf("primary_ip over gRPC=%q", lastVms[0].PrimaryIp)
	}
	// userdata arrived - the driver put it in base64 in the description field.
	if desc, ok := f.lastSetFields["description"]; !ok || !strings.Contains(desc, "soul-stack userdata") {
		t.Errorf("userdata lost over gRPC: description=%q", desc)
	}
}

func TestL1_StatusOverGRPC(t *testing.T) {
	f := &fakePVE{statusSeq: []VMStatus{runningStatus("soul-100")}}
	withFakePVE(t, f)

	client, teardown := serveDriverGRPC(t, &ProxmoxDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rep, err := client.Status(ctx, &pluginv1.StatusRequest{
		VmId:        "pve1/100",
		Credentials: l1Struct(t, baseCreds()),
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
	f := &fakePVE{}
	withFakePVE(t, f)

	client, teardown := serveDriverGRPC(t, &ProxmoxDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Destroy(ctx, &pluginv1.DestroyRequest{
		VmIds:       []string{"pve1/100"},
		Credentials: l1Struct(t, baseCreds()),
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
