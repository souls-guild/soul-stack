//go:build integration

// L2 — driver против LocalStack (реальный EC2-API в контейнере) через
// testcontainers. Только AWS это умеет out-of-the-box — поэтому AWS пилот
// тиража (GCP/Azure/YC/Proxmox/OpenStack своего LocalStack не имеют). L2
// гоняется `go test -tags=integration` при наличии docker; без docker —
// skip (если не задан REQUIRE_DOCKER).
//
// LocalStack community поддерживает базовые EC2 RunInstances/DescribeInstances/
// TerminateInstances с mock-инстансами (сразу running) — этого хватает для
// проверки реального code-path драйвера end-to-end.

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func requireDocker() bool {
	v := os.Getenv("SOUL_STACK_INTEGRATION_REQUIRE_DOCKER")
	return v == "1" || v == "true"
}

func TestL2_LocalStack_CreateListDestroy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ctr, err := localstack.Run(ctx, "localstack/localstack:3.6")
	if err != nil {
		if requireDocker() {
			t.Fatalf("localstack setup (REQUIRE_DOCKER): %v", err)
		}
		t.Skipf("localstack unavailable, skipping L2: %v", err)
	}
	defer func() {
		tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer tcancel()
		_ = testcontainers.TerminateContainer(ctr, testcontainers.StopContext(tctx))
	}()

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "4566/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	endpoint := "http://" + host + ":" + port.Port()

	creds := map[string]any{
		"access_key_id":     "test",
		"secret_access_key": "test",
		"region":            "us-east-1",
		"endpoint":          endpoint,
	}

	d := &AwsDriver{}

	// --- Create ---
	cs := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 2,
		Profile: l2Struct(t, map[string]any{
			"region": "us-east-1", "ami": "ami-0abc1234de56789f0", "instance_type": "t3.micro",
			"tags": map[string]any{runTagKey: "l2-run"},
		}),
		Credentials: l2Struct(t, creds),
		Userdata:    "#cloud-config\npackages: [curl]\n",
	}, cs); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := cs.last()
	if last == nil || last.Failed {
		t.Fatalf("Create final=%+v", last)
	}
	if len(last.Vms) != 2 {
		t.Fatalf("created vms=%d, want 2", len(last.Vms))
	}
	var vmIDs []string
	for _, vm := range last.Vms {
		if vm.VmId == "" {
			t.Error("vm_id empty from LocalStack")
		}
		vmIDs = append(vmIDs, vm.VmId)
	}

	// --- Idempotency: повторный Create по тому же runTag не плодит дубли ---
	cs2 := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 2,
		Profile: l2Struct(t, map[string]any{
			"region": "us-east-1", "ami": "ami-0abc1234de56789f0", "instance_type": "t3.micro",
			"tags": map[string]any{runTagKey: "l2-run"},
		}),
		Credentials: l2Struct(t, creds),
	}, cs2); err != nil {
		t.Fatalf("Create (idempotent): %v", err)
	}
	if cs2.last().Failed {
		t.Fatalf("idempotent Create final=%+v", cs2.last())
	}
	if len(cs2.last().Vms) != 2 {
		t.Errorf("idempotent run returned %d vms, want 2 (reuse, no dupes)", len(cs2.last().Vms))
	}

	// --- List ---
	ls := &listStreamL2{ctx: ctx}
	if err := d.List(&pluginv1.ListRequest{
		Filter: l2Struct(t, mergeMaps(creds, map[string]any{runTagKey: "l2-run"})),
	}, ls); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ls.sent) < 2 {
		t.Errorf("List returned %d vms, want >=2", len(ls.sent))
	}

	// --- Destroy ---
	ds := &destroyStreamL2{ctx: ctx}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       vmIDs,
		Credentials: l2Struct(t, creds),
	}, ds); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	for _, ev := range ds.sent {
		if ev.Failed {
			t.Errorf("destroy event failed: %s", ev.Message)
		}
	}
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

type listStreamL2 struct {
	pluginv1.CloudDriver_ListServer
	ctx  context.Context
	sent []*pluginv1.VmInfo
}

func (s *listStreamL2) Send(v *pluginv1.VmInfo) error { s.sent = append(s.sent, v); return nil }
func (s *listStreamL2) Context() context.Context      { return s.ctx }

type destroyStreamL2 struct {
	pluginv1.CloudDriver_DestroyServer
	ctx  context.Context
	sent []*pluginv1.DestroyEvent
}

func (s *destroyStreamL2) Send(e *pluginv1.DestroyEvent) error {
	s.sent = append(s.sent, e)
	return nil
}
func (s *destroyStreamL2) Context() context.Context { return s.ctx }

func l2Struct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}
