package clouddriver

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// TestBaseDriverSchemaEmpty verifies the BaseDriver.Schema default returns an
// empty SchemaReply without error (no-op before the author overrides it).
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

// TestBaseDriverValidateOk verifies the BaseDriver.Validate default returns Ok=true.
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

// TestBaseDriverStreamsEmpty verifies the Create/Destroy/List defaults close
// the stream without events and return nil.
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

// TestBaseDriverStatusEmpty verifies the Status default returns an empty StatusReply.
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

// TestServerAdapterDelegates verifies the adapter proxies calls to the
// user-impl with the correct parameters and propagates errors.
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

// fakeDriver is a mock CloudDriver implementation for adapter tests.
type fakeDriver struct {
	schemaCalled  bool
	validateReply *pluginv1.ValidateProfileReply
	createErr     error
	createCount   int32
	destroyIds    []string
	statusVmId    string
	statusReply   *pluginv1.StatusReply
	listCalled    bool
	resizeVmIds   []string
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

func (f *fakeDriver) Resize(req *pluginv1.ResizeRequest, stream grpc.ServerStreamingServer[pluginv1.ResizeEvent]) error {
	f.resizeVmIds = req.VmIds
	results := make([]*pluginv1.VmResizeResult, len(req.VmIds))
	for i, id := range req.VmIds {
		results[i] = &pluginv1.VmResizeResult{VmId: id, CausedDowntime: req.AllowDowntime}
	}
	return stream.Send(&pluginv1.ResizeEvent{Results: results})
}

// resizableDriver is a fakeDriver that ADDITIONALLY declares the Resizable capability.
type resizableDriver struct{ fakeDriver }

func (*resizableDriver) Resizable() {}

// fakeCreateStream / fakeDestroyStream / fakeListStream are minimal mock
// grpc.ServerStreamingServer implementations for in-process unit tests
// (without spinning up a real grpc-server). Symmetric with
// fakePlanStream/fakeApplyStream in sdk/module.
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

type fakeResizeStream struct {
	grpc.ServerStreamingServer[pluginv1.ResizeEvent]
	sent []*pluginv1.ResizeEvent
}

func (s *fakeResizeStream) Send(e *pluginv1.ResizeEvent) error {
	s.sent = append(s.sent, e)
	return nil
}

func (s *fakeResizeStream) Context() context.Context { return context.Background() }

// TestBaseDriverNotResizable verifies BaseDriver DELIBERATELY does not
// implement Resizable: a driver built on BaseDriver gets default-deny on
// resize (the PlanReadSafe pattern).
func TestBaseDriverNotResizable(t *testing.T) {
	var b BaseDriver
	if _, ok := any(b).(Resizable); ok {
		t.Fatal("BaseDriver implements Resizable - it should NOT (default-deny)")
	}
}

// TestResizableDetect verifies an explicit Resizable implementation is
// detected via type assertion (the way serverAdapter/host does it).
func TestResizableDetect(t *testing.T) {
	var fake CloudDriver = &fakeDriver{}
	if _, ok := fake.(Resizable); ok {
		t.Fatal("fakeDriver without Resizable was detected as Resizable")
	}
	var rz CloudDriver = &resizableDriver{}
	if _, ok := rz.(Resizable); !ok {
		t.Fatal("explicit Resizable implementation not detected by type assertion")
	}
}

// TestBaseDriverResizeDefaultDeny verifies BaseDriver.Resize sends a
// failed-event resize.unsupported — NOT a panic, NOT a false success.
func TestBaseDriverResizeDefaultDeny(t *testing.T) {
	var b BaseDriver
	stream := &fakeResizeStream{}
	if err := b.Resize(&pluginv1.ResizeRequest{VmIds: []string{"i-1"}}, stream); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if len(stream.sent) != 1 || !stream.sent[0].Failed {
		t.Fatalf("Resize sent=%+v, want one failed event", stream.sent)
	}
	if !strings.Contains(stream.sent[0].Message, "resize.unsupported") {
		t.Fatalf("Resize message=%q, want resize.unsupported", stream.sent[0].Message)
	}
}

// TestServerAdapterResizeDefaultDeny verifies serverAdapter does not call
// impl.Resize and returns resize.unsupported if impl does NOT implement Resizable.
func TestServerAdapterResizeDefaultDeny(t *testing.T) {
	impl := &fakeDriver{}
	adapter := &serverAdapter{impl: impl}
	stream := &fakeResizeStream{}
	if err := adapter.Resize(&pluginv1.ResizeRequest{VmIds: []string{"i-1"}}, stream); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if impl.resizeVmIds != nil {
		t.Fatal("impl.Resize was called despite not implementing Resizable")
	}
	if len(stream.sent) != 1 || !stream.sent[0].Failed ||
		!strings.Contains(stream.sent[0].Message, "resize.unsupported") {
		t.Fatalf("Resize sent=%+v, want resize.unsupported failed event", stream.sent)
	}
}

// TestServerAdapterResizeDelegates verifies serverAdapter PROXIES Resize to
// impl if impl implements Resizable.
func TestServerAdapterResizeDelegates(t *testing.T) {
	impl := &resizableDriver{}
	adapter := &serverAdapter{impl: impl}
	stream := &fakeResizeStream{}
	if err := adapter.Resize(&pluginv1.ResizeRequest{VmIds: []string{"i-1", "i-2"}, AllowDowntime: true}, stream); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if len(impl.resizeVmIds) != 2 {
		t.Fatalf("impl.Resize vm_ids=%v, want 2", impl.resizeVmIds)
	}
	if len(stream.sent) != 1 || len(stream.sent[0].Results) != 2 {
		t.Fatalf("Resize sent=%+v, want one event with 2 results", stream.sent)
	}
	if !stream.sent[0].Results[0].CausedDowntime {
		t.Fatal("caused_downtime not propagated (allow_downtime=true)")
	}
}
