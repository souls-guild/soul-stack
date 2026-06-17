package beacon

import (
	"context"
	"errors"
	"sync"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestBaseBeaconValidateOk — дефолт BaseBeacon.Validate возвращает Ok=true.
func TestBaseBeaconValidateOk(t *testing.T) {
	var b BaseBeacon
	reply, err := b.Validate(context.Background(), &pluginv1.ValidateVigilRequest{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("Validate reply=%+v", reply)
	}
}

// TestBaseBeaconCheckUnknown — дефолт BaseBeacon.Check возвращает State="unknown"
// без payload/error.
func TestBaseBeaconCheckUnknown(t *testing.T) {
	var b BaseBeacon
	reply, err := b.Check(context.Background(), &pluginv1.CheckRequest{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if reply == nil || reply.State != "unknown" {
		t.Fatalf("Check reply=%+v, want state=unknown", reply)
	}
	if reply.Payload != nil || reply.Error != "" || len(reply.StateCookie) != 0 {
		t.Fatalf("Check reply payload/error/cookie must be empty: %+v", reply)
	}
}

// TestServerAdapterDelegates — adapter проксирует Validate/Check к user-impl и
// пробрасывает ошибки/значения. Покрывает контракт adapter (Spawn-RPC заранит
// эти же методы из shared/pluginhost; L1-тест pluginhost-а делает реальный
// gRPC-roundtrip через testdata/beacon-plugin).
func TestServerAdapterDelegates(t *testing.T) {
	wantErr := errors.New("check_boom")
	payload, _ := structpb.NewStruct(map[string]any{"path": "/etc/nginx.conf"})
	impl := &fakeBeacon{
		validateReply: &pluginv1.ValidateVigilReply{Ok: false, Errors: []string{"bad param"}},
		checkReply: &pluginv1.CheckReply{
			State:       "alerted",
			Payload:     payload,
			StateCookie: []byte("cookie-v1"),
		},
	}
	adapter := &serverAdapter{impl: impl}

	vr, err := adapter.Validate(context.Background(), &pluginv1.ValidateVigilRequest{})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if vr.Ok || len(vr.Errors) != 1 || vr.Errors[0] != "bad param" {
		t.Fatalf("Validate reply=%+v", vr)
	}

	cr, err := adapter.Check(context.Background(), &pluginv1.CheckRequest{Params: payload})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if cr.State != "alerted" || string(cr.StateCookie) != "cookie-v1" {
		t.Fatalf("Check reply=%+v", cr)
	}
	if !impl.checkCalled {
		t.Fatal("Check not called on impl")
	}

	// Теперь — ошибка пробрасывается.
	impl.mu.Lock()
	impl.checkErr = wantErr
	impl.checkCalled = false
	impl.mu.Unlock()
	if _, err := adapter.Check(context.Background(), &pluginv1.CheckRequest{}); !errors.Is(err, wantErr) {
		t.Fatalf("Check err=%v want %v", err, wantErr)
	}
}

// fakeBeacon — mock Beacon для adapter-тестов.
type fakeBeacon struct {
	mu            sync.Mutex
	validateReply *pluginv1.ValidateVigilReply
	checkReply    *pluginv1.CheckReply
	checkErr      error
	checkCalled   bool
}

func (f *fakeBeacon) Validate(_ context.Context, _ *pluginv1.ValidateVigilRequest) (*pluginv1.ValidateVigilReply, error) {
	return f.validateReply, nil
}

func (f *fakeBeacon) Check(_ context.Context, _ *pluginv1.CheckRequest) (*pluginv1.CheckReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalled = true
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	return f.checkReply, nil
}
