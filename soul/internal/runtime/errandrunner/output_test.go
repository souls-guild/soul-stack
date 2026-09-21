package errandrunner

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

func TestMaskAndCapBytes_Cap(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a", OutputCapBytes+100)
	out, trunc := MaskAndCapBytes(big)
	if !trunc {
		t.Fatalf("trunc=false for a string of %d > %d", len(big), OutputCapBytes)
	}
	if len(out) != OutputCapBytes {
		t.Errorf("len(out) = %d; want %d", len(out), OutputCapBytes)
	}
}

func TestMaskAndCapBytes_NoCap(t *testing.T) {
	t.Parallel()
	small := "hello world"
	out, trunc := MaskAndCapBytes(small)
	if trunc {
		t.Errorf("trunc=true for %q", small)
	}
	if out != small {
		t.Errorf("out = %q; want %q", out, small)
	}
}

func TestMaskAndCapBytes_Empty(t *testing.T) {
	t.Parallel()
	out, trunc := MaskAndCapBytes("")
	if out != "" || trunc {
		t.Errorf("(%q, %v) for empty input", out, trunc)
	}
}

// TestOutputCollector_ExtractFinal_Shell — a final ApplyEvent with
// stdout/stderr/exit_code (core.cmd / core.exec format) decomposes correctly.
func TestOutputCollector_ExtractFinal_Shell(t *testing.T) {
	t.Parallel()
	c := newOutputCollector(context.Background(), OutputCapBytes)
	out, _ := structpb.NewStruct(map[string]any{
		"stdout":    "line1\nline2",
		"stderr":    "err1",
		"exit_code": float64(2),
		// Extra field — should remain in structured.
		"trace": "x",
	})
	if err := c.Send(&pluginv1.ApplyEvent{Changed: true, Output: out}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stdout, stderr, exit, structured := c.extractFinal()
	if stdout != "line1\nline2" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "err1" {
		t.Errorf("stderr = %q", stderr)
	}
	if exit != 2 {
		t.Errorf("exit_code = %d", exit)
	}
	if structured == nil {
		t.Fatalf("structured = nil; should contain the remaining trace field")
	}
	if v := structured.GetFields()["trace"].GetStringValue(); v != "x" {
		t.Errorf("structured.trace = %q", v)
	}
	// shell channels must NOT leak into structured.
	for _, k := range []string{"stdout", "stderr", "exit_code"} {
		if _, ok := structured.GetFields()[k]; ok {
			t.Errorf("structured contains shell field %q", k)
		}
	}
}

// TestOutputCollector_ExtractFinal_ReadSafe — for a read-safe module without
// shell fields, the whole output is kept in structured as-is.
func TestOutputCollector_ExtractFinal_ReadSafe(t *testing.T) {
	t.Parallel()
	c := newOutputCollector(context.Background(), OutputCapBytes)
	out, _ := structpb.NewStruct(map[string]any{
		"status":     float64(200),
		"elapsed_ms": float64(42),
	})
	_ = c.Send(&pluginv1.ApplyEvent{Output: out})
	stdout, stderr, exit, structured := c.extractFinal()
	if stdout != "" || stderr != "" || exit != 0 {
		t.Errorf("shell fields populated: %q / %q / %d", stdout, stderr, exit)
	}
	if structured == nil || len(structured.GetFields()) != 2 {
		t.Fatalf("structured = %v", structured)
	}
}

// TestOutputCollector_ExtractFinal_NoEvent — the module sent nothing.
func TestOutputCollector_ExtractFinal_NoEvent(t *testing.T) {
	t.Parallel()
	c := newOutputCollector(context.Background(), OutputCapBytes)
	stdout, stderr, exit, structured := c.extractFinal()
	if stdout != "" || stderr != "" || exit != 0 || structured != nil {
		t.Errorf("non-zero values: %q / %q / %d / %v", stdout, stderr, exit, structured)
	}
}

// TestOutputCollector_ExtractFinal_NilOutput — final ApplyEvent without Output.
func TestOutputCollector_ExtractFinal_NilOutput(t *testing.T) {
	t.Parallel()
	c := newOutputCollector(context.Background(), OutputCapBytes)
	_ = c.Send(&pluginv1.ApplyEvent{Changed: true})
	stdout, stderr, exit, structured := c.extractFinal()
	if stdout != "" || stderr != "" || exit != 0 || structured != nil {
		t.Errorf("non-zero values: %q / %q / %d / %v", stdout, stderr, exit, structured)
	}
}

// TestOutputCollector_LastEvent — picks exactly the last event.
func TestOutputCollector_LastEvent(t *testing.T) {
	t.Parallel()
	c := newOutputCollector(context.Background(), OutputCapBytes)
	_ = c.Send(&pluginv1.ApplyEvent{Message: "progress 1"})
	_ = c.Send(&pluginv1.ApplyEvent{Message: "progress 2"})
	_ = c.Send(&pluginv1.ApplyEvent{Changed: true, Message: "final"})
	last := c.lastEvent()
	if last == nil || last.GetMessage() != "final" {
		t.Errorf("lastEvent = %+v", last)
	}
}
