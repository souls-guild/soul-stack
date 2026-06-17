package main

import (
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// runApplyWith гоняет runApply с заданным stdin, перехватывая stdout.
// Возвращает (exitCode, stdout). --config указывает на несуществующий путь —
// runApply должен деградировать к core-модулям (config опционален в push).
func runApplyWith(t *testing.T, stdin string) (int, string) {
	t.Helper()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdin: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}

	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = origIn, origOut }()

	go func() {
		_, _ = inW.WriteString(stdin)
		_ = inW.Close()
	}()

	// Читаем stdout в фоне, чтобы writer не блокировался на полном pipe-буфере.
	outCh := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := outR.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		outCh <- sb.String()
	}()

	code := runApply([]string{"--config", "/nonexistent/soul.yml"})
	_ = outW.Close()
	out := <-outCh
	return code, out
}

func mustApplyRequestJSON(t *testing.T, req *keeperv1.ApplyRequest) string {
	t.Helper()
	b, err := protojson.Marshal(req)
	if err != nil {
		t.Fatalf("marshal ApplyRequest: %v", err)
	}
	return string(b)
}

func ndjsonLines(s string) []string {
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestRunApply_HappyPath(t *testing.T) {
	// core.exec.run "true" — детерминированный no-op без сайд-эффектов.
	req := &keeperv1.ApplyRequest{
		ApplyId: "push-1",
		Tasks: []*keeperv1.RenderedTask{
			{
				Name:   "noop",
				Module: "core.exec.run",
				Params: mustStruct(t, map[string]any{"cmd": "true"}),
			},
		},
	}
	code, out := runApplyWith(t, mustApplyRequestJSON(t, req))

	lines := ndjsonLines(out)
	if len(lines) != 2 {
		t.Fatalf("stdout lines = %d, want 2 (TaskEvent + RunResult):\n%s", len(lines), out)
	}

	ev := &keeperv1.TaskEvent{}
	if err := protojson.Unmarshal([]byte(lines[0]), ev); err != nil {
		t.Fatalf("unmarshal TaskEvent: %v\nline: %s", err, lines[0])
	}
	if ev.GetApplyId() != "push-1" {
		t.Errorf("TaskEvent.apply_id = %q", ev.GetApplyId())
	}
	if ev.GetStatus() == keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("TaskEvent.status = FAILED, err=%v", ev.GetError())
	}

	rr := &keeperv1.RunResult{}
	if err := protojson.Unmarshal([]byte(lines[1]), rr); err != nil {
		t.Fatalf("unmarshal RunResult: %v\nline: %s", err, lines[1])
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("RunResult.status = %v, want SUCCESS", rr.GetStatus())
	}
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
}

func TestRunApply_ModuleNotFound_ExitsNonZero(t *testing.T) {
	req := &keeperv1.ApplyRequest{
		ApplyId: "push-2",
		Tasks: []*keeperv1.RenderedTask{
			{Name: "ghost", Module: "core.ghost.alive"},
		},
	}
	code, out := runApplyWith(t, mustApplyRequestJSON(t, req))

	lines := ndjsonLines(out)
	if len(lines) != 2 {
		t.Fatalf("stdout lines = %d, want 2:\n%s", len(lines), out)
	}
	ev := &keeperv1.TaskEvent{}
	if err := protojson.Unmarshal([]byte(lines[0]), ev); err != nil {
		t.Fatalf("unmarshal TaskEvent: %v", err)
	}
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("TaskEvent.status = %v, want FAILED", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "module.not_found" {
		t.Errorf("TaskEvent.error.code = %q", ev.GetError().GetCode())
	}
	rr := &keeperv1.RunResult{}
	if err := protojson.Unmarshal([]byte(lines[1]), rr); err != nil {
		t.Fatalf("unmarshal RunResult: %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("RunResult.status = %v, want FAILED", rr.GetStatus())
	}
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
}

func TestRunApply_EmptyStdin(t *testing.T) {
	code, out := runApplyWith(t, "")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d (usage)", code, exitUsage)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("stdout should be empty on usage error, got: %q", out)
	}
}

func TestRunApply_InvalidJSON(t *testing.T) {
	code, _ := runApplyWith(t, "{ not valid protojson")
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	st, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return st
}
