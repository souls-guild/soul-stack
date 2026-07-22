package cmd_test

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/cmd"
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

func newModule(r util.Runner, stat func(string) (bool, error)) *cmd.Module {
	return &cmd.Module{Runner: r, StatFile: stat}
}

func TestValidate(t *testing.T) {
	m := cmd.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "shell",
		Params: mustStruct(t, map[string]any{}),
	})
	if reply.Ok {
		t.Fatal("Validate missing cmd: ok unexpectedly")
	}
}

func TestApply_Run_PipesIntoShell(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["[cwd=/var] sh -c ls | wc -l"] = []util.Result{{ExitCode: 0, Stdout: "3\n"}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "shell",
		Params: mustStruct(t, map[string]any{
			"cmd": "ls | wc -l",
			"cwd": "/var",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output.Fields["stdout"].GetStringValue() != "3\n" {
		t.Fatalf("stdout=%q", ev.Output.Fields["stdout"].GetStringValue())
	}
}

func TestApply_Creates_Skips(t *testing.T) {
	r := internaltest.NewRunner()
	m := newModule(r, func(p string) (bool, error) { return p == "/tmp/m", nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "shell",
		Params: mustStruct(t, map[string]any{
			"cmd":     "echo hi > /tmp/m",
			"creates": "/tmp/m",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("Changed=true when creates+file exists")
	}
}

func TestApply_NonZeroExit_NotFailed(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c grep foo /etc/hosts"] = []util.Result{{ExitCode: 1}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "shell",
		Params: mustStruct(t, map[string]any{"cmd": "grep foo /etc/hosts"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatal("Failed=true for non-zero exit (grep with exit 1 is normal)")
	}
}
