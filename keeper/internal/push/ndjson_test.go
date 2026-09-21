package push

import (
	"errors"
	"strings"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func taskLine(t *testing.T, ev *keeperv1.TaskEvent) string {
	t.Helper()
	b, err := protojson.MarshalOptions{Multiline: false}.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal TaskEvent: %v", err)
	}
	return string(b)
}

func runLine(t *testing.T, rr *keeperv1.RunResult) string {
	t.Helper()
	b, err := protojson.MarshalOptions{Multiline: false}.Marshal(rr)
	if err != nil {
		t.Fatalf("marshal RunResult: %v", err)
	}
	return string(b)
}

func TestParseStream_HappyPath_EventsThenRunResult(t *testing.T) {
	lines := []string{
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a1", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_OK}),
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a1", TaskIdx: 1, Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED}),
		runLine(t, &keeperv1.RunResult{ApplyId: "a1", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}),
	}
	stream := strings.Join(lines, "\n") + "\n"

	var got []*keeperv1.TaskEvent
	rr, err := ParseStream(strings.NewReader(stream), func(ev *keeperv1.TaskEvent) {
		got = append(got, ev)
	})
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("TaskEvent count = %d, want 2", len(got))
	}
	if got[0].GetTaskIdx() != 0 || got[1].GetTaskIdx() != 1 {
		t.Errorf("TaskEvent order violated: %d, %d", got[0].GetTaskIdx(), got[1].GetTaskIdx())
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("RunResult.status = %v, want SUCCESS", rr.GetStatus())
	}
	if rr.GetApplyId() != "a1" {
		t.Errorf("RunResult.apply_id = %q", rr.GetApplyId())
	}
}

func TestParseStream_FailureRunResult(t *testing.T) {
	lines := []string{
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a2", TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_FAILED}),
		runLine(t, &keeperv1.RunResult{ApplyId: "a2", Status: keeperv1.RunStatus_RUN_STATUS_FAILED}),
	}
	stream := strings.Join(lines, "\n") + "\n"

	rr, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("ParseStream: %v (a FAILED run is a valid outcome, not an error)", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("RunResult.status = %v, want FAILED", rr.GetStatus())
	}
}

func TestParseStream_NoRunResult(t *testing.T) {
	// Only TaskEvents, the stream was cut off before RunResult (crash/session drop).
	stream := taskLine(t, &keeperv1.TaskEvent{ApplyId: "a3", Status: keeperv1.TaskStatus_TASK_STATUS_OK}) + "\n"

	_, err := ParseStream(strings.NewReader(stream), nil)
	if !errors.Is(err, ErrNoRunResult) {
		t.Fatalf("err = %v, want ErrNoRunResult", err)
	}
}

func TestParseStream_EmptyStream(t *testing.T) {
	_, err := ParseStream(strings.NewReader(""), nil)
	if !errors.Is(err, ErrNoRunResult) {
		t.Fatalf("err = %v, want ErrNoRunResult for an empty stream", err)
	}
}

func TestParseStream_BlankLinesSkipped(t *testing.T) {
	// Double '\n' between messages must not break parsing.
	stream := "\n" +
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a4", Status: keeperv1.TaskStatus_TASK_STATUS_OK}) + "\n\n" +
		runLine(t, &keeperv1.RunResult{ApplyId: "a4", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}) + "\n"

	rr, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v", rr.GetStatus())
	}
}

func TestParseStream_BrokenJSON(t *testing.T) {
	stream := "{ not valid json\n"
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected a parse error for the broken line")
	}
	if errors.Is(err, ErrNoRunResult) {
		t.Errorf("a broken line must not yield ErrNoRunResult, err=%v", err)
	}
	if !strings.Contains(err.Error(), "invalid NDJSON line") {
		t.Errorf("uninformative error: %v", err)
	}
}

func TestParseStream_PartialLine_NoTrailingNewline(t *testing.T) {
	// A line missing the trailing '\n' (writer got cut off): bufio.Scanner
	// returns the last line, but it's partial and doesn't parse as JSON.
	stream := `{"applyId":"a5","stat` // cut off mid-protojson
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected an error on a partial line")
	}
	if errors.Is(err, ErrNoRunResult) {
		t.Errorf("partial line -> parse error, not ErrNoRunResult: %v", err)
	}
}

func TestParseStream_UnclassifiableLine(t *testing.T) {
	// Valid JSON, but status isn't one of our enum names.
	stream := `{"applyId":"a6","status":"SOMETHING_ELSE"}` + "\n"
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected an error for an unclassifiable line")
	}
	if !strings.Contains(err.Error(), "unclassifiable") {
		t.Errorf("error is not about classification: %v", err)
	}
}

func TestParseStream_LineAfterRunResult(t *testing.T) {
	// RunResult is final: any line after it is a protocol error.
	stream := runLine(t, &keeperv1.RunResult{ApplyId: "a7", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}) + "\n" +
		taskLine(t, &keeperv1.TaskEvent{ApplyId: "a7", Status: keeperv1.TaskStatus_TASK_STATUS_OK}) + "\n"
	_, err := ParseStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatal("expected an error for a line after RunResult")
	}
	if !strings.Contains(err.Error(), "after final RunResult") {
		t.Errorf("error is not about ordering: %v", err)
	}
}
