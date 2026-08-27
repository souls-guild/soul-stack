package cmd_test

import (
	"context"
	"strings"
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

// grep exiting 1 is an answer, not an error — but the module cannot tell the
// two apart, so since NIM-687 the author says so: without exit_codes only 0 is
// accepted and the task fails; with `exit_codes: [0, 1]` it passes.
func TestApply_NonZeroExit_FailsUnlessDeclared(t *testing.T) {
	run := func(t *testing.T, params map[string]any) *pluginv1.ApplyEvent {
		t.Helper()
		r := internaltest.NewRunner()
		r.Results["sh -c grep foo /etc/hosts"] = []util.Result{{ExitCode: 1}}
		m := newModule(r, func(string) (bool, error) { return false, nil })

		stream := &internaltest.ApplyStream{}
		if err := m.Apply(&pluginv1.ApplyRequest{
			State:  "shell",
			Params: mustStruct(t, params),
		}, stream); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		return stream.Last()
	}

	ev := run(t, map[string]any{"cmd": "grep foo /etc/hosts"})
	if !ev.Failed {
		t.Fatal("Failed=false for exit 1 with no exit_codes; the default set is [0]")
	}
	if ev.Output == nil {
		t.Fatalf("failed event carries no output; the code must travel with the verdict: msg=%q", ev.Message)
	}
	if got := ev.Output.Fields["exit_code"].GetNumberValue(); got != 1 {
		t.Fatalf("exit_code=%v want 1 (output must survive the failure)", got)
	}

	ev = run(t, map[string]any{
		"cmd":        "grep foo /etc/hosts",
		"exit_codes": []any{0, 1},
	})
	if ev.Failed {
		t.Fatalf("Failed=true for exit 1 listed in exit_codes: %s", ev.Message)
	}
}

// A command's stdout is arbitrary bytes; structpb rejects invalid UTF-8. The
// rejected-code path must not answer that with nothing at all: returning the
// serialization error from Apply would take the verdict down with the output
// and hand the operator an encoding complaint in place of "exit code 3 is not
// accepted". The reason is worth more than the bytes — the event goes out
// either way, with the loss recorded in the message.
func TestApply_RejectedCode_UnserialisableOutput_StillReportsTheReason(t *testing.T) {
	r := internaltest.NewRunner()
	r.Results["sh -c emit-binary"] = []util.Result{{ExitCode: 3, Stdout: "head\xff\xfetail"}}
	m := newModule(r, func(string) (bool, error) { return false, nil })

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "shell",
		Params: mustStruct(t, map[string]any{"cmd": "emit-binary"}),
	}, stream); err != nil {
		t.Fatalf("Apply returned an error instead of a failed event: %v.\nThe task then fails with a "+
			"structpb complaint and the operator never learns the exit code was rejected.", err)
	}
	ev := stream.Last()
	if ev == nil {
		t.Fatal("no event sent at all — the verdict was lost with the output")
	}
	if !ev.Failed {
		t.Fatalf("Failed=false for exit 3 with no exit_codes: %q", ev.Message)
	}
	if !strings.Contains(ev.Message, "3") {
		t.Errorf("message = %q, want the rejected code named in it", ev.Message)
	}
	if !strings.Contains(ev.Message, "serialization failed") {
		t.Errorf("message = %q, want the dropped output noted — output that silently vanishes is the "+
			"data loss this degradation exists to make visible", ev.Message)
	}
}
