package runtime

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func TestNDJSONSink_LineFraming(t *testing.T) {
	var buf bytes.Buffer
	sink := NewNDJSONSink(&buf)

	if err := sink.SendTaskEvent(&keeperv1.TaskEvent{
		ApplyId: "a1",
		TaskIdx: 0,
		Status:  keeperv1.TaskStatus_TASK_STATUS_CHANGED,
	}); err != nil {
		t.Fatalf("SendTaskEvent: %v", err)
	}
	if err := sink.SendRunResult(&keeperv1.RunResult{
		ApplyId: "a1",
		Status:  keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	}); err != nil {
		t.Fatalf("SendRunResult: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	// Each line is standalone protojson (round-trip of both messages).
	ev := &keeperv1.TaskEvent{}
	if err := protojson.Unmarshal([]byte(lines[0]), ev); err != nil {
		t.Fatalf("unmarshal line 0: %v", err)
	}
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("line 0 status = %v", ev.GetStatus())
	}
	rr := &keeperv1.RunResult{}
	if err := protojson.Unmarshal([]byte(lines[1]), rr); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("line 1 status = %v", rr.GetStatus())
	}
}

func TestNDJSONSink_LastStatus(t *testing.T) {
	sink := NewNDJSONSink(&bytes.Buffer{})
	if got := sink.LastStatus(); got != keeperv1.RunStatus_RUN_STATUS_UNSPECIFIED {
		t.Errorf("before run: LastStatus = %v, want UNSPECIFIED", got)
	}
	_ = sink.SendRunResult(&keeperv1.RunResult{Status: keeperv1.RunStatus_RUN_STATUS_FAILED})
	if got := sink.LastStatus(); got != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("LastStatus = %v, want FAILED", got)
	}
}
