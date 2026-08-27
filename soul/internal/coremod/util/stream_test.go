package util_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

func TestSendFinal_WithOutput(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFinal(s, true, map[string]any{"name": "redis", "installed": true}); err != nil {
		t.Fatalf("SendFinal: %v", err)
	}
	if len(s.Events) != 1 {
		t.Fatalf("events=%d want 1", len(s.Events))
	}
	ev := s.Events[0]
	if !ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v", ev.Changed, ev.Failed)
	}
	if ev.Output == nil || ev.Output.Fields["name"].GetStringValue() != "redis" {
		t.Fatalf("output=%v", ev.Output)
	}
}

func TestSendFinal_NilOutput(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFinal(s, false, nil); err != nil {
		t.Fatalf("SendFinal: %v", err)
	}
	if s.Events[0].Output != nil {
		t.Fatalf("Output=%v want nil (omitted)", s.Events[0].Output)
	}
	if s.Events[0].Changed {
		t.Fatal("Changed=true want false")
	}
}

// A value that isn't structpb-serializable → SendFinal returns an error, no
// event is sent.
func TestSendFinal_UnserializableOutputErrors(t *testing.T) {
	s := &internaltest.ApplyStream{}
	bad := map[string]any{"fn": func() {}}
	if err := util.SendFinal(s, true, bad); err == nil {
		t.Fatal("SendFinal: want error on unserializable output")
	}
	if len(s.Events) != 0 {
		t.Fatalf("events=%d want 0 (nothing to send on error)", len(s.Events))
	}
}

func TestSendFailed(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFailed(s, "boom"); err != nil {
		t.Fatalf("SendFailed: %v", err)
	}
	if !s.Events[0].Failed || s.Events[0].Message != "boom" {
		t.Fatalf("events[0]=%+v", s.Events[0])
	}
	if s.Events[0].Output != nil {
		t.Fatalf("Output=%v want nil — SendFailed is for a module that produced none; "+
			"one that did must use SendFailedWithOutput", s.Events[0].Output)
	}
}

// The failure carries its output (NIM-687). Dropping it costs two things at
// once: the operator loses the stderr that explains the failure, and a
// `failed_when:` predicate over register.self.exit_code becomes unevaluable —
// so the sanctioned way to waive the failure stops working exactly where it is
// needed.
func TestSendFailedWithOutput(t *testing.T) {
	s := &internaltest.ApplyStream{}
	out := map[string]any{"stdout": "out\n", "stderr": "denied\n", "exit_code": float64(3)}
	if err := util.SendFailedWithOutput(s, "exit code 3 is not accepted", out); err != nil {
		t.Fatalf("SendFailedWithOutput: %v", err)
	}
	if len(s.Events) != 1 {
		t.Fatalf("events=%d want 1", len(s.Events))
	}
	ev := s.Events[0]
	if !ev.Failed {
		t.Fatal("Failed=false")
	}
	if ev.Message != "exit code 3 is not accepted" {
		t.Fatalf("Message=%q", ev.Message)
	}
	if ev.Output == nil {
		t.Fatal("Output=nil on a failure that had output — the stderr and the exit code are gone")
	}
	if got := ev.Output.Fields["stderr"].GetStringValue(); got != "denied\n" {
		t.Fatalf("stderr=%q want %q", got, "denied\n")
	}
	if got := ev.Output.Fields["exit_code"].GetNumberValue(); got != 3 {
		t.Fatalf("exit_code=%v want 3", got)
	}
}

func TestSendFailedWithOutput_NilOutput(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFailedWithOutput(s, "boom", nil); err != nil {
		t.Fatalf("SendFailedWithOutput: %v", err)
	}
	ev := s.Events[0]
	if !ev.Failed || ev.Output != nil {
		t.Fatalf("events[0]=%+v want failed only", ev)
	}
}

// Deliberately NOT the same contract as SendFinal, which is the only reason this
// test is worth its lines. SendFinal's output IS the result: nothing survives
// dropping it, so erroring is right. Here the verdict survives it — the operator
// still needs to be told the command failed and why, and a command's stdout is
// arbitrary bytes that structpb can legitimately refuse (`printf '\xff'`). So the
// event goes out with whatever encoded, the loss named in the message rather than
// silent, and the caller gets no error to turn into a different failure. Here
// there is one field and it is the bad one, so nothing is left; the partial case
// is the next test, and it is the common one.
func TestSendFailedWithOutput_UnserializableOutputDegradesAndSaysSo(t *testing.T) {
	s := &internaltest.ApplyStream{}
	if err := util.SendFailedWithOutput(s, "exit code 3 is not accepted", map[string]any{"fn": func() {}}); err != nil {
		t.Fatalf("SendFailedWithOutput returned %v — the verdict must reach the operator even when "+
			"the output cannot; erroring here replaces \"exit code 3 is not accepted\" with an "+
			"encoding complaint from whoever handles the error", err)
	}
	if len(s.Events) != 1 {
		t.Fatalf("events=%d want 1 — the failure must still be reported", len(s.Events))
	}
	ev := s.Events[0]
	if !ev.Failed {
		t.Fatal("Failed=false — the degraded event lost the one thing it had to keep")
	}
	if ev.Output != nil {
		t.Fatalf("Output=%v want nil — nothing serializable was left to send", ev.Output)
	}
	if !strings.Contains(ev.Message, "exit code 3 is not accepted") {
		t.Errorf("Message=%q — the original reason must survive the degradation", ev.Message)
	}
	if !strings.Contains(ev.Message, "output serialization failed") {
		t.Errorf("Message=%q — output that vanishes without a word is the data loss this "+
			"degradation exists to make visible", ev.Message)
	}
	if !strings.Contains(ev.Message, "fn") {
		t.Errorf("Message=%q — the dropped key must be named; \"something failed to encode\" "+
			"does not tell the operator which field is missing from register.self", ev.Message)
	}
}

// The whole map used to go down with one bad field, which cost exactly the two
// fields the escape hatch is built on: `failed_when: register.self.exit_code == 3`
// is the documented way to waive a rejected code, and a command that writes
// binary to stdout is the ordinary case, not a corner one.
func TestSendFailedWithOutput_OneBadFieldDoesNotTakeTheOthers(t *testing.T) {
	s := &internaltest.ApplyStream{}
	out := map[string]any{"stdout": "head\xff\xfetail", "stderr": "denied\n", "exit_code": float64(3)}
	if err := util.SendFailedWithOutput(s, "exit code 3 is not accepted", out); err != nil {
		t.Fatalf("SendFailedWithOutput: %v", err)
	}
	ev := s.Events[0]
	if ev.Output == nil {
		t.Fatal("Output=nil — one unencodable field dropped the two that were fine")
	}
	if got := ev.Output.Fields["exit_code"].GetNumberValue(); got != 3 {
		t.Errorf("exit_code=%v want 3 — the field failed_when: reads must outlive a binary stdout", got)
	}
	if got := ev.Output.Fields["stderr"].GetStringValue(); got != "denied\n" {
		t.Errorf("stderr=%q want %q — the diagnostics the failure was declared for", got, "denied\n")
	}
	if _, ok := ev.Output.Fields["stdout"]; ok {
		t.Error("stdout present — it is the field that cannot encode; keeping it would mean the " +
			"conversion never actually failed and this test proves nothing")
	}
	if !strings.Contains(ev.Message, "stdout") {
		t.Errorf("Message=%q — the dropped key must be named", ev.Message)
	}
}

// A shell writing binary to both streams is one command, not two coincidences —
// `printf '\xff'; printf '\xff' >&2; exit 3` does it — so the plural is the case
// to pin, not an exotic one. Two things ride on it. Every bad key is dropped
// individually rather than the first one deciding for the rest, and the names
// come out sorted: map iteration is randomised, so an unsorted list would give a
// different message on identical input and no test elsewhere has two bad fields
// to notice.
func TestSendFailedWithOutput_TwoBadFieldsAreBothNamedInAStableOrder(t *testing.T) {
	const want = "output serialization failed for stderr, stdout"
	// Sent repeatedly on purpose. One send cannot tell a sorted list from an
	// unsorted one: Go randomises the map walk, so dropping the sort still
	// produces "stderr, stdout" about half the time and the guard would pass on
	// the broken code every other run. Eight sends leave that under a percent,
	// and cost nothing on the correct code, where every one is identical.
	for i := 0; i < 8; i++ {
		s := &internaltest.ApplyStream{}
		out := map[string]any{"stdout": "\xff", "stderr": "\xfe", "exit_code": float64(3)}
		if err := util.SendFailedWithOutput(s, "exit code 3 is not accepted", out); err != nil {
			t.Fatalf("SendFailedWithOutput: %v", err)
		}
		ev := s.Events[0]
		if ev.Output == nil || len(ev.Output.Fields) != 1 {
			t.Fatalf("Output=%v — exit_code encodes fine and must survive both bad neighbours", ev.Output)
		}
		if got := ev.Output.Fields["exit_code"].GetNumberValue(); got != 3 {
			t.Errorf("exit_code=%v want 3", got)
		}
		if !strings.Contains(ev.Message, want) {
			t.Fatalf("send %d: Message=%q does not contain %q.\nBoth keys have to be named, and in an "+
				"order that does not depend on which one Go's map walk reached first — otherwise the "+
				"same command reports differently on consecutive runs.", i, ev.Message, want)
		}
	}
}

// The rationale this replaces claimed the bytes were traded away for the
// reason. They were not: structpb's error embeds a %q of the whole offending
// value, so pasting it into the message re-attached the output, escaped and
// several times larger — and applyrunner copies the message into TaskError
// verbatim, with no cap on the destiny path.
func TestSendFailedWithOutput_DroppedValueDoesNotComeBackInTheMessage(t *testing.T) {
	s := &internaltest.ApplyStream{}
	huge := strings.Repeat("\xff", 64*1024)
	if err := util.SendFailedWithOutput(s, "exit code 3 is not accepted", map[string]any{"stdout": huge}); err != nil {
		t.Fatalf("SendFailedWithOutput: %v", err)
	}
	msg := s.Events[0].Message
	if len(msg) > 512 {
		t.Errorf("len(Message)=%d for a 64 KiB unencodable stdout — the value is riding back in the "+
			"text it was supposed to be replaced by; at 8 MB of binary this is a 32 MB TaskError", len(msg))
	}
	if !strings.Contains(msg, "stdout") {
		t.Errorf("Message=%q — bounded is not the same as silent; the key still has to be named", msg)
	}
}
