package auditpg

import (
	"strings"
	"testing"
)

// TestSelectTaskKeysByStatusSQL_Filters — docker-free guard on the shape of the
// aggregation query (T3 changed / R3 failed): catches a regression if the JSONB
// key name status/sid/plan_index/task_idx or the filter column
// (correlation_id/event_type) changes. Source is strictly `task.executed`;
// status is filtered by a SET of names (`= ANY($3)`, one SQL query serves both
// CHANGED and FAILED selections); address fields are sid +
// COALESCE(plan_index, task_idx) (global correlation key with the plan,
// fallback to the local one for old rows); payload values (register_data) are
// NOT read.
func TestSelectTaskKeysByStatusSQL_Filters(t *testing.T) {
	for _, want := range []string{
		"correlation_id = $1",
		"event_type = $2",
		"payload->>'status' = ANY($3)",
		"payload->>'sid'",
		// plan_index takes priority (correlation key with RenderedTask.Index under
		// staged/per-host); task_idx is the fallback for pre-T3 rows (COALESCE).
		"COALESCE(payload->>'plan_index', payload->>'task_idx')",
		"payload->>'plan_index'",
		"payload->>'task_idx'",
		"FROM audit_log",
	} {
		if !strings.Contains(selectTaskKeysByStatusSQL, want) {
			t.Errorf("selectTaskKeysByStatusSQL missing %q", want)
		}
	}
	// Secret hygiene: register_data / params must not be read by the query.
	for _, forbidden := range []string{"register_data", "params", "'error'"} {
		if strings.Contains(selectTaskKeysByStatusSQL, forbidden) {
			t.Errorf("selectTaskKeysByStatusSQL reads payload value %q — secret hygiene violated", forbidden)
		}
	}
}

// TestTaskStatusConsts — status strings match the keeperv1.TaskStatus names
// (the handler stores Status().String()). Pins the constants hard — drift from
// the proto enum would silently zero out either the changed aggregation or the
// cross-passage onfail-gating (failed/timed_out, ADR-056 R3).
func TestTaskStatusConsts(t *testing.T) {
	cases := map[string]string{
		taskStatusChanged:  "TASK_STATUS_CHANGED",
		taskStatusFailed:   "TASK_STATUS_FAILED",
		taskStatusTimedOut: "TASK_STATUS_TIMED_OUT",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("status const = %q, want %q", got, want)
		}
	}
}
