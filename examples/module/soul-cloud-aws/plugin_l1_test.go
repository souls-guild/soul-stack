package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// L1 — driver-as-plugin through a REAL gRPC server+client (the same
// RegisterCloudDriverServer that sdk/clouddriver.Serve attaches after
// handshake). Verifies that AwsDriver works correctly through the CloudDriver
// proto contract, including new credentials/userdata fields, over a real
// gRPC stream rather than an in-proc method call. This is L1 for the pilot:
// handshake-spawn under Sigil-gate is covered by
// keeper/internal/coremod/cloud/integration_adapter_test.go on the Keeper side
// (shared host there); here we cover the driver's own RPC contract.

// serveDriverGRPC starts CloudDriver service on bufless TCP-loopback and
// returns client + teardown.
func serveDriverGRPC(t *testing.T, impl *AwsDriver) (pluginv1.CloudDriverClient, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	// Register through the same serverAdapter path as SDK: SDK adapter
	// is unexported, but Serve attaches exactly CloudDriverServer; here
	// we register impl directly through generated Register (does impl already
	// satisfy CloudDriverServer? no — impl lacks mustEmbed). Therefore
	// wrap it in a local bridge identical to SDK adapter.
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

// l1Adapter is bridge impl→CloudDriverServer (embed Unimplemented for forward-compat),
// identical to sdk/clouddriver.serverAdapter (which is unexported).
type l1Adapter struct {
	pluginv1.UnimplementedCloudDriverServer
	impl *AwsDriver
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
	f := &fakeEC2{
		runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-l1")}}},
		describeSeq: []*ec2.DescribeInstancesOutput{
			describeOut(runningInstance("i-l1", "10.2.2.2", "ip-10-2-2-2.internal")),
		},
	}
	withFakeEC2(t, f)

	client, teardown := serveDriverGRPC(t, &AwsDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Create(ctx, &pluginv1.CreateRequest{
		Count:       1,
		Profile:     l1Struct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
		Credentials: l1Struct(t, map[string]any{"access_key_id": "AKIA", "region": "eu-west-1"}),
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
	if len(lastVms) != 1 || lastVms[0].VmId != "i-l1" || lastVms[0].Fqdn == "" {
		t.Errorf("vms over gRPC = %+v", lastVms)
	}
	// credentials/userdata reached the driver through the proto stream (UserData
	// is base64-encoded by the driver — EC2 requirement, see main.go).
	decoded, derr := base64.StdEncoding.DecodeString(aws.ToString(f.lastRunInput.UserData))
	if derr != nil || string(decoded) != "#cloud-config\n" {
		t.Errorf("userdata lost over gRPC marshaling: decoded=%q err=%v", decoded, derr)
	}
}

func TestL1_StatusOverGRPC(t *testing.T) {
	inst := runningInstance("i-l1-stat", "10.8.8.8", "ip-10-8-8-8.internal")
	f := &fakeEC2{describeSeq: []*ec2.DescribeInstancesOutput{describeOut(inst)}}
	withFakeEC2(t, f)

	client, teardown := serveDriverGRPC(t, &AwsDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rep, err := client.Status(ctx, &pluginv1.StatusRequest{
		VmId:        "i-l1-stat",
		Credentials: l1Struct(t, map[string]any{"access_key_id": "AKIA", "region": "eu-west-1"}),
	})
	if err != nil {
		t.Fatalf("Status rpc: %v", err)
	}
	if rep.State != string(ec2types.InstanceStateNameRunning) {
		t.Errorf("state over gRPC=%q, want running", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes lost over gRPC marshaling")
	}
}

func TestL1_DestroyOverGRPC(t *testing.T) {
	f := &fakeEC2{termOut: &ec2.TerminateInstancesOutput{
		TerminatingInstances: []ec2types.InstanceStateChange{
			{InstanceId: aws.String("i-l1"), CurrentState: &ec2types.InstanceState{Name: ec2types.InstanceStateNameShuttingDown}},
		},
	}}
	withFakeEC2(t, f)

	client, teardown := serveDriverGRPC(t, &AwsDriver{})
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.Destroy(ctx, &pluginv1.DestroyRequest{
		VmIds:       []string{"i-l1"},
		Credentials: l1Struct(t, map[string]any{"region": "eu-west-1"}),
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
