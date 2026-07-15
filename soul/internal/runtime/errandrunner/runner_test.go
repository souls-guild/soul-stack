package errandrunner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
)

// --- fakes ---

type mapRegistry map[string]sdkmodule.SoulModule

func (m mapRegistry) Lookup(name string) (sdkmodule.SoulModule, bool) {
	mod, ok := m[name]
	return mod, ok
}

type fakeModule struct {
	sdkmodule.BaseModule
	applyFunc func(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error
	planFunc  func(*pluginv1.PlanRequest, grpc.ServerStreamingServer[pluginv1.PlanEvent]) error
}

func (f *fakeModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if f.applyFunc != nil {
		return f.applyFunc(req, stream)
	}
	return nil
}

func (f *fakeModule) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	if f.planFunc != nil {
		return f.planFunc(req, stream)
	}
	return nil
}

// readSafeModule is a fakeModule with the ErrandReadSafe marker (opt-in to the whitelist).
type readSafeModule struct{ fakeModule }

func (readSafeModule) ErrandReadSafe() {}

// planSafeModule is a fakeModule with both markers (ErrandReadSafe + PlanReadSafe);
// exercises the dry_run branch.
type planSafeModule struct{ fakeModule }

func (planSafeModule) ErrandReadSafe() {}
func (planSafeModule) PlanReadSafe()   {}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// --- tests ---

func TestRun_Success_Shell(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{
		"core.cmd": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				out, _ := structpb.NewStruct(map[string]any{
					"stdout":    "hello\n",
					"stderr":    "",
					"exit_code": float64(0),
				})
				return stream.Send(&pluginv1.ApplyEvent{Changed: true, Output: out})
			},
		},
	}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId:       "e-1",
		Module:         "core.cmd.shell",
		Input:          mustStruct(t, map[string]any{"cmd": "echo hello"}),
		TimeoutSeconds: 5,
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
		t.Fatalf("status = %v; want SUCCESS (err=%q)", res.GetStatus(), res.GetErrorMessage())
	}
	if res.GetStdout() != "hello\n" {
		t.Errorf("stdout = %q", res.GetStdout())
	}
	if res.GetStderr() != "" {
		t.Errorf("stderr = %q", res.GetStderr())
	}
	if res.GetExitCode() != 0 {
		t.Errorf("exit_code = %d", res.GetExitCode())
	}
	if res.GetErrandId() != "e-1" {
		t.Errorf("errand_id = %q", res.GetErrandId())
	}
	if res.GetDurationMs() < 0 {
		t.Errorf("duration_ms = %d", res.GetDurationMs())
	}
}

func TestRun_Failed_ModuleError(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{
		"core.cmd": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return errors.New("sh -c: exit 1")
			},
		},
	}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-2",
		Module:   "core.cmd.shell",
		Input:    mustStruct(t, map[string]any{"cmd": "false"}),
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_FAILED {
		t.Fatalf("status = %v; want FAILED", res.GetStatus())
	}
	if !strings.Contains(res.GetErrorMessage(), "sh -c") {
		t.Errorf("error_message = %q", res.GetErrorMessage())
	}
}

func TestRun_Failed_ApplyEventFailed(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{
		"core.cmd": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "bad params"})
			},
		},
	}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-3",
		Module:   "core.cmd.shell",
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_FAILED {
		t.Fatalf("status = %v", res.GetStatus())
	}
	if res.GetErrorMessage() != "bad params" {
		t.Errorf("error_message = %q", res.GetErrorMessage())
	}
}

func TestRun_ModuleNotAllowed_Unknown(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-4",
		Module:   "core.pkg.installed",
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED {
		t.Fatalf("status = %v; want MODULE_NOT_ALLOWED", res.GetStatus())
	}
	if !strings.HasPrefix(res.GetErrorMessage(), "errand_module_not_allowed:") {
		t.Errorf("error_message = %q", res.GetErrorMessage())
	}
}

func TestRun_ModuleNotAllowed_NoMarker(t *testing.T) {
	t.Parallel()
	// Module is registered but does NOT have ErrandReadSafe and isn't in the
	// hardcoded list → reject, defense-in-depth.
	reg := mapRegistry{
		"core.pkg": &fakeModule{},
	}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-5",
		Module:   "core.pkg.installed",
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED {
		t.Fatalf("status = %v", res.GetStatus())
	}
}

func TestRun_AllowedByMarker(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{
		"core.http": &readSafeModule{fakeModule: fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				out, _ := structpb.NewStruct(map[string]any{
					"status":     float64(200),
					"elapsed_ms": float64(42),
				})
				return stream.Send(&pluginv1.ApplyEvent{Changed: false, Output: out})
			},
		}},
	}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-6",
		Module:   "core.http.probe",
		Input:    mustStruct(t, map[string]any{"url": "https://example.com"}),
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
		t.Fatalf("status = %v; want SUCCESS", res.GetStatus())
	}
	if res.GetOutput() == nil {
		t.Fatalf("output is nil; want structured")
	}
	if v := res.GetOutput().GetFields()["status"].GetNumberValue(); v != 200 {
		t.Errorf("output.status = %v", v)
	}
	// stdout/stderr must be empty — a read-safe module doesn't write them.
	if res.GetStdout() != "" || res.GetStderr() != "" {
		t.Errorf("stdout/stderr non-empty: %q / %q", res.GetStdout(), res.GetStderr())
	}
}

func TestRun_DryRun_NotPlanReadSafe(t *testing.T) {
	t.Parallel()
	// core.cmd.shell is on the hardcoded whitelist but WITHOUT PlanReadSafe → dry_run reject.
	reg := mapRegistry{"core.cmd": &fakeModule{}}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-7",
		Module:   "core.cmd.shell",
		DryRun:   true,
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_FAILED {
		t.Fatalf("status = %v", res.GetStatus())
	}
	if res.GetErrorMessage() != "errand_dry_run_unsupported" {
		t.Errorf("error_message = %q", res.GetErrorMessage())
	}
}

func TestRun_DryRun_PlanReadSafeOK(t *testing.T) {
	t.Parallel()
	planCalled := false
	reg := mapRegistry{
		"core.http": &planSafeModule{fakeModule: fakeModule{
			planFunc: func(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
				planCalled = true
				return stream.Send(&pluginv1.PlanEvent{Changed: false})
			},
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				t.Errorf("Apply вызван на dry_run; должен был быть Plan")
				return nil
			},
		}},
	}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-8",
		Module:   "core.http.probe",
		DryRun:   true,
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
		t.Fatalf("status = %v; want SUCCESS (err=%q)", res.GetStatus(), res.GetErrorMessage())
	}
	if !planCalled {
		t.Errorf("Plan не вызван")
	}
}

func TestRun_TimedOut(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{
		"core.cmd": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				// Respects ctx — a real shell-exec does too.
				<-stream.Context().Done()
				return stream.Context().Err()
			},
		},
	}
	r := New(reg, nil, nil)
	start := time.Now()
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId:       "e-9",
		Module:         "core.cmd.shell",
		TimeoutSeconds: 1,
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run заблокировался дольше таймаута: %s", elapsed)
	}
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT {
		t.Fatalf("status = %v; want TIMED_OUT (err=%q)", res.GetStatus(), res.GetErrorMessage())
	}
	if res.GetErrorMessage() != "errand_timeout_exceeded" {
		t.Errorf("error_message = %q", res.GetErrorMessage())
	}
}

func TestRun_BadModuleAddress(t *testing.T) {
	t.Parallel()
	r := New(mapRegistry{}, nil, nil)
	// Invalid shape — FAILED bad_module_address. `core.cmd` without `.shell`
	// formally parses as (core, cmd) — that's already a valid split address,
	// and is treated as MODULE_NOT_ALLOWED (module `core` doesn't exist),
	// see the separate assertion below.
	cases := []string{"", "core", "core.cmd."}
	for _, m := range cases {
		res := r.Run(context.Background(), &keeperv1.ErrandRequest{
			ErrandId: "e-bad",
			Module:   m,
		})
		if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_FAILED {
			t.Errorf("module=%q: status = %v; want FAILED", m, res.GetStatus())
		}
	}
}

func TestRun_NilRequest(t *testing.T) {
	t.Parallel()
	r := New(mapRegistry{}, nil, nil)
	res := r.Run(context.Background(), nil)
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_FAILED {
		t.Fatalf("status = %v", res.GetStatus())
	}
}

// TestRun_CancelByExternalSignal — slice E5: Runner.Cancel(errandID) cancels
// the active Run goroutine → returns status CANCELLED without blocking longer.
//
// Scenario: the module blocks until ctx.Done(); a parallel goroutine calls
// Cancel after a short interval. Run must return CANCELLED + duration_ms
// < 1s (not wait out the timeout).
func TestRun_CancelByExternalSignal(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{
		"core.cmd": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				// blocks until ctx is cancelled (or timeout — it's 30s, we won't hit it).
				<-stream.Context().Done()
				return stream.Context().Err()
			},
		},
	}
	r := New(reg, nil, nil)

	done := make(chan *keeperv1.ErrandResult, 1)
	go func() {
		done <- r.Run(context.Background(), &keeperv1.ErrandRequest{
			ErrandId:       "e-cancel",
			Module:         "core.cmd.shell",
			TimeoutSeconds: 30, // large enough to never fire
		})
	}()

	// Give Run time to register itself in the active map.
	time.Sleep(50 * time.Millisecond)
	if !r.Cancel("e-cancel") {
		t.Fatalf("Cancel(e-cancel) = false, want true")
	}

	select {
	case res := <-done:
		if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED {
			t.Fatalf("status = %v; want CANCELLED (err=%q)", res.GetStatus(), res.GetErrorMessage())
		}
		if res.GetErrorMessage() == "" {
			t.Errorf("error_message пусто, ожидали маркер cancel-а")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run не завершился после Cancel")
	}
}

// TestRun_CancelUnknown — Cancel on an unknown errand_id returns false (race
// with its own terminal state — a safe no-op).
func TestRun_CancelUnknown(t *testing.T) {
	t.Parallel()
	r := New(mapRegistry{}, nil, nil)
	if r.Cancel("nonexistent") {
		t.Fatalf("Cancel(nonexistent) = true, want false")
	}
}

func TestRun_HardcodedWhitelist_ExecRun(t *testing.T) {
	t.Parallel()
	reg := mapRegistry{
		"core.exec": &fakeModule{
			applyFunc: func(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				out, _ := structpb.NewStruct(map[string]any{
					"stdout":    "ok",
					"stderr":    "",
					"exit_code": float64(0),
				})
				return stream.Send(&pluginv1.ApplyEvent{Changed: true, Output: out})
			},
		},
	}
	r := New(reg, nil, nil)
	res := r.Run(context.Background(), &keeperv1.ErrandRequest{
		ErrandId: "e-10",
		Module:   "core.exec.run",
	})
	if res.GetStatus() != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
		t.Fatalf("status = %v", res.GetStatus())
	}
}
