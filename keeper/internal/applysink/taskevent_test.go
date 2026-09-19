package applysink

import (
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func TestComposeTaskErrorSummary(t *testing.T) {
	tests := []struct {
		name string
		idx  int
		te   *keeperv1.TaskError
		want string
	}{
		{"full", 0, &keeperv1.TaskError{Module: "core.pkg.installed", Message: "boom"}, "task 0 core.pkg.installed: boom"},
		{"no module", 3, &keeperv1.TaskError{Message: "boom"}, "task 3: boom"},
		{"no message", 1, &keeperv1.TaskError{Module: "core.file.present"}, "task 1 core.file.present"},
		{"nil error", 2, nil, "task 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := composeTaskErrorSummary(tt.idx, tt.te); got != tt.want {
				t.Errorf("composeTaskErrorSummary = %q, want %q", got, tt.want)
			}
		})
	}
}

// A zero Deps must not panic on either entry point: every channel is optional,
// and a caller that has no PG, no audit and no SSE (a unit build, a dev
// single-instance) still hands events to the sink.
func TestZeroDepsSwallowsEverything(t *testing.T) {
	s := New(Deps{})
	s.TaskEvent(t.Context(), "host.example.com", &keeperv1.TaskEvent{
		ApplyId: "a1",
		Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
		Error:   &keeperv1.TaskError{Module: "core.exec.run", Message: "boom"},
	})
	s.RunResult(t.Context(), "host.example.com", &keeperv1.RunResult{
		ApplyId: "a1",
		Status:  keeperv1.RunStatus_RUN_STATUS_FAILED,
	})
	s.TaskEvent(t.Context(), "host.example.com", nil)
	s.RunResult(t.Context(), "host.example.com", nil)
}
