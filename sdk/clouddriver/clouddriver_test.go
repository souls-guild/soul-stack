package clouddriver

import (
	"context"
	"errors"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// TestBaseDriverSchemaEmpty — дефолт BaseDriver.Schema возвращает пустой
// SchemaReply без ошибки (no-op перед переопределением автором).
func TestBaseDriverSchemaEmpty(t *testing.T) {
	var b BaseDriver
	reply, err := b.Schema(context.Background(), &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if reply == nil || reply.ProfileSchema != nil {
		t.Fatalf("Schema reply=%+v, want empty", reply)
	}
}

// TestBaseDriverValidateOk — дефолт BaseDriver.Validate возвращает Ok=true.
func TestBaseDriverValidateOk(t *testing.T) {
	var b BaseDriver
	reply, err := b.Validate(context.Background(), &pluginv1.ValidateProfileRequest{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("Validate reply=%+v", reply)
	}
}

// TestBaseDriverStreamsEmpty — дефолт Create/Destroy/List закрывают stream
// без событий и возвращают nil.
func TestBaseDriverStreamsEmpty(t *testing.T) {
	var b BaseDriver
	createStream := &fakeCreateStream{}
	if err := b.Create(&pluginv1.CreateRequest{}, createStream); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(createStream.sent) != 0 {
		t.Fatalf("Create sent %d events, want 0", len(createStream.sent))
	}

	destroyStream := &fakeDestroyStream{}
	if err := b.Destroy(&pluginv1.DestroyRequest{}, destroyStream); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(destroyStream.sent) != 0 {
		t.Fatalf("Destroy sent %d events, want 0", len(destroyStream.sent))
	}

	listStream := &fakeListStream{}
	if err := b.List(&pluginv1.ListRequest{}, listStream); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listStream.sent) != 0 {
		t.Fatalf("List sent %d events, want 0", len(listStream.sent))
	}
}

// TestBaseDriverStatusEmpty — дефолт Status возвращает пустой StatusReply.
func TestBaseDriverStatusEmpty(t *testing.T) {
	var b BaseDriver
	reply, err := b.Status(context.Background(), &pluginv1.StatusRequest{VmId: "i-1"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if reply == nil || reply.State != "" {
		t.Fatalf("Status reply=%+v, want empty", reply)
	}
}

// TestServerAdapterDelegates — adapter проксирует вызовы к user-impl
// с правильными параметрами и пробрасывает ошибки.
func TestServerAdapterDelegates(t *testing.T) {
	wantErr := errors.New("boom")
	impl := &fakeDriver{
		validateReply: &pluginv1.ValidateProfileReply{Ok: false, Errors: []string{"bad ami"}},
		createErr:     wantErr,
		statusReply:   &pluginv1.StatusReply{State: "running"},
	}
	adapter := &serverAdapter{impl: impl}

	if _, err := adapter.Schema(context.Background(), &pluginv1.SchemaRequest{}); err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if !impl.schemaCalled {
		t.Fatal("Schema not called on impl")
	}

	reply, err := adapter.Validate(context.Background(), &pluginv1.ValidateProfileRequest{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if reply.Ok || reply.Errors[0] != "bad ami" {
		t.Fatalf("Validate reply=%+v", reply)
	}

	if err := adapter.Create(&pluginv1.CreateRequest{Count: 3}, &fakeCreateStream{}); !errors.Is(err, wantErr) {
		t.Fatalf("Create err=%v want %v", err, wantErr)
	}
	if impl.createCount != 3 {
		t.Fatalf("Create count=%d", impl.createCount)
	}

	if err := adapter.Destroy(&pluginv1.DestroyRequest{VmIds: []string{"i-1", "i-2"}}, &fakeDestroyStream{}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(impl.destroyIds) != 2 {
		t.Fatalf("Destroy vm_ids=%v", impl.destroyIds)
	}

	statusReply, err := adapter.Status(context.Background(), &pluginv1.StatusRequest{VmId: "i-9"})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if impl.statusVmId != "i-9" || statusReply.State != "running" {
		t.Fatalf("Status vm=%q reply=%+v", impl.statusVmId, statusReply)
	}

	if err := adapter.List(&pluginv1.ListRequest{}, &fakeListStream{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !impl.listCalled {
		t.Fatal("List not called on impl")
	}
}

// fakeDriver — mock-implementation CloudDriver для adapter-тестов.
type fakeDriver struct {
	schemaCalled  bool
	validateReply *pluginv1.ValidateProfileReply
	createErr     error
	createCount   int32
	destroyIds    []string
	statusVmId    string
	statusReply   *pluginv1.StatusReply
	listCalled    bool
}

func (f *fakeDriver) Schema(context.Context, *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	f.schemaCalled = true
	return &pluginv1.SchemaReply{}, nil
}

func (f *fakeDriver) Validate(_ context.Context, _ *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	return f.validateReply, nil
}

func (f *fakeDriver) Create(req *pluginv1.CreateRequest, _ grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	f.createCount = req.Count
	return f.createErr
}

func (f *fakeDriver) Destroy(req *pluginv1.DestroyRequest, _ grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	f.destroyIds = req.VmIds
	return nil
}

func (f *fakeDriver) Status(_ context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	f.statusVmId = req.VmId
	return f.statusReply, nil
}

func (f *fakeDriver) List(_ *pluginv1.ListRequest, _ grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	f.listCalled = true
	return nil
}

// fakeCreateStream / fakeDestroyStream / fakeListStream — минимальные mock
// grpc.ServerStreamingServer для in-process unit-тестов (без поднятия реального
// grpc-server-а). Симметрично fakePlanStream/fakeApplyStream в sdk/module.
type fakeCreateStream struct {
	grpc.ServerStreamingServer[pluginv1.CreateEvent]
	sent []*pluginv1.CreateEvent
}

func (s *fakeCreateStream) Send(e *pluginv1.CreateEvent) error {
	s.sent = append(s.sent, e)
	return nil
}

func (s *fakeCreateStream) Context() context.Context { return context.Background() }

type fakeDestroyStream struct {
	grpc.ServerStreamingServer[pluginv1.DestroyEvent]
	sent []*pluginv1.DestroyEvent
}

func (s *fakeDestroyStream) Send(e *pluginv1.DestroyEvent) error {
	s.sent = append(s.sent, e)
	return nil
}

func (s *fakeDestroyStream) Context() context.Context { return context.Background() }

type fakeListStream struct {
	grpc.ServerStreamingServer[pluginv1.VmInfo]
	sent []*pluginv1.VmInfo
}

func (s *fakeListStream) Send(v *pluginv1.VmInfo) error {
	s.sent = append(s.sent, v)
	return nil
}

func (s *fakeListStream) Context() context.Context { return context.Background() }
