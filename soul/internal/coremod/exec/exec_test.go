package exec_test

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/exec"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func newModule(r util.Runner, stat func(string) (bool, error)) *exec.Module {
	return &exec.Module{Runner: r, StatFile: stat}
}

func TestValidate(t *testing.T) {
	m := exec.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"cmd": "ls"}),
	})
	if reply.Ok {
		t.Fatal("Validate bad state: ok unexpectedly")
	}
	reply, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "run",
		Params: mustStruct(t, map[string]any{}),
	})
	if reply.Ok {
		t.Fatal("Validate missing cmd: ok unexpectedly")
	}
	reply, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "run",
		Params: mustStruct(t, map[string]any{"cmd": "ls"}),
	})
	if !reply.Ok {
		t.Fatalf("Validate ok: %+v", reply)
	}
}

func TestApply_Run_BasicSuccess(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["echo hello"] = []util.Result{{ExitCode: 0, Stdout: "hello\n"}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "run",
		Params: mustStruct(t, map[string]any{
			"cmd":  "echo",
			"args": []any{"hello"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "hello\n" {
		t.Fatalf("stdout=%q", ev.Output.Fields["stdout"].GetStringValue())
	}
	if ev.Output.Fields["exit_code"].GetNumberValue() != 0 {
		t.Fatalf("exit_code=%v", ev.Output.Fields["exit_code"].GetNumberValue())
	}
}

func TestApply_Run_NonZeroExit_NotFailed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["false"] = []util.Result{{ExitCode: 1}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "run",
		Params: mustStruct(t, map[string]any{"cmd": "false"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatal("Failed=true для non-zero exit; должно проходить, пользователь решает через failed_when")
	}
	if !ev.Changed {
		t.Fatal("Changed=false для запущенной команды")
	}
	if ev.Output.Fields["exit_code"].GetNumberValue() != 1 {
		t.Fatalf("exit_code=%v want 1", ev.Output.Fields["exit_code"].GetNumberValue())
	}
}

func TestApply_Creates_SkipsIfFileExists(t *testing.T) {
	r := internaltest.NewRunner()
	m := newModule(r, func(p string) (bool, error) {
		if p == "/tmp/marker" {
			return true, nil
		}
		return false, nil
	})

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "run",
		Params: mustStruct(t, map[string]any{
			"cmd":     "touch",
			"args":    []any{"/tmp/marker"},
			"creates": "/tmp/marker",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Changed {
		t.Fatal("Changed=true при creates: файл существует")
	}
	if ev.Output.Fields["reason"].GetStringValue() != "creates" {
		t.Fatalf("reason=%q want creates", ev.Output.Fields["reason"].GetStringValue())
	}
	for _, c := range r.Calls {
		if c == "touch /tmp/marker" {
			t.Fatalf("неожиданный вызов основной команды при creates-skip")
		}
	}
}

func TestApply_Unless_SkipsIfExitZero(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("sh -c test -f /etc/passwd", util.Result{ExitCode: 0})
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "run",
		Params: mustStruct(t, map[string]any{
			"cmd":    "useradd",
			"args":   []any{"bob"},
			"unless": "test -f /etc/passwd",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true при unless+exit=0")
	}
}

func TestApply_Onlyif_SkipsIfExitNonZero(t *testing.T) {
	r := internaltest.NewRunner()
	r.On("sh -c test -d /opt/app", util.Result{ExitCode: 1})
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "run",
		Params: mustStruct(t, map[string]any{
			"cmd":    "make",
			"args":   []any{"build"},
			"onlyif": "test -d /opt/app",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true при onlyif+exit≠0")
	}
}

func TestApply_PassesCwdAndEnv(t *testing.T) {
	r := internaltest.NewRunner()
	// envSlice sorts: [A=1 B=2]
	r.Results["[cwd=/tmp] [env=A=1,B=2] mybin --flag"] = []util.Result{{ExitCode: 0, Stdout: "ok"}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "run",
		Params: mustStruct(t, map[string]any{
			"cmd":  "mybin",
			"args": []any{"--flag"},
			"cwd":  "/tmp",
			"env":  map[string]any{"A": "1", "B": "2"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "ok" {
		t.Fatalf("stdout=%q (cwd/env не пробросились в Runner)", ev.Output.Fields["stdout"].GetStringValue())
	}
}

func TestApply_MissingCmd_Fails(t *testing.T) {
	m := exec.New()
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "run",
		Params: mustStruct(t, map[string]any{}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("Failed=false для missing cmd")
	}
}
