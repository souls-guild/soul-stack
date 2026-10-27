package task_test

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/task"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func apply(t *testing.T, state string, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	s, err := structpb.NewStruct(params)
	if err != nil {
		t.Fatalf("params: %v", err)
	}
	stream := &internaltest.ApplyStream{}
	if err := task.New().Apply(&pluginv1.ApplyRequest{State: state, Params: s}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	if len(stream.Events) != 1 {
		t.Fatalf("Apply sent %d events, want exactly one final event", len(stream.Events))
	}
	return stream.Last()
}

func TestApply_AlwaysFails(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		message string
	}{
		{"message from params", map[string]any{"message": "the service is still down"}, "the service is still down"},
		{"no message falls back to the generic text", map[string]any{}, task.GenericMessage},
		{"an empty message falls back to the generic text", map[string]any{"message": ""}, task.GenericMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := apply(t, "failed", tc.params)
			if !ev.GetFailed() {
				t.Fatalf("Failed = false, want true: core.task.failed must fail its task (%+v)", ev)
			}
			if ev.GetChanged() {
				t.Errorf("Changed = true on a failure that touched nothing")
			}
			if ev.GetMessage() != tc.message {
				t.Errorf("Message = %q, want %q", ev.GetMessage(), tc.message)
			}
		})
	}
}

func TestGenericMessage_NamesTheModule(t *testing.T) {
	if !strings.Contains(task.GenericMessage, "core.task.failed") {
		t.Fatalf("GenericMessage %q does not name the module that failed the task", task.GenericMessage)
	}
}

func TestApply_UnknownStateFails(t *testing.T) {
	ev := apply(t, "passed", map[string]any{"message": "x"})
	if !ev.GetFailed() || !strings.Contains(ev.GetMessage(), "unknown state") {
		t.Fatalf("unknown state: got %+v, want a failure naming the state", ev)
	}
}

func TestValidate_AcceptsFailedWithAndWithoutMessage(t *testing.T) {
	for _, params := range []map[string]any{{}, {"message": "boom"}} {
		s, _ := structpb.NewStruct(params)
		reply, err := task.New().Validate(context.Background(), &pluginv1.ValidateRequest{State: "failed", Params: s})
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !reply.GetOk() {
			t.Errorf("Validate(%v) refused: %v", params, reply.GetErrors())
		}
	}
}
