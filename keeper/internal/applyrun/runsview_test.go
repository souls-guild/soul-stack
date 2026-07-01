package applyrun

import "testing"

func TestAggregateRunStatus(t *testing.T) {
	tests := []struct {
		name string
		in   []Status
		want RunStatus
	}{
		{"empty → applying", nil, RunStatusApplying},
		{"single running → applying", []Status{StatusRunning}, RunStatusApplying},
		{"planned pending → applying", []Status{StatusSuccess, StatusPlanned}, RunStatusApplying},
		{"claimed pending → applying", []Status{StatusClaimed}, RunStatusApplying},
		{"dispatched pending → applying", []Status{StatusSuccess, StatusDispatched}, RunStatusApplying},
		{"all success → success", []Status{StatusSuccess, StatusSuccess}, RunStatusSuccess},
		{"success + no_match → success", []Status{StatusSuccess, StatusNoMatch}, RunStatusSuccess},
		{"only no_match → success", []Status{StatusNoMatch}, RunStatusSuccess},
		{"failed dominates success → failed", []Status{StatusSuccess, StatusFailed}, RunStatusFailed},
		{"orphaned → failed", []Status{StatusSuccess, StatusOrphaned}, RunStatusFailed},
		{"failed dominates cancelled → failed", []Status{StatusCancelled, StatusFailed}, RunStatusFailed},
		{"cancelled without failure → cancelled", []Status{StatusSuccess, StatusCancelled}, RunStatusCancelled},
		// applying имеет наивысший приоритет: незавершённый прогон не может быть
		// объявлен failed/cancelled раньше времени (иначе UI покажет терминал на
		// ещё идущей джобе).
		{"non-terminal dominates failure", []Status{StatusFailed, StatusRunning}, RunStatusApplying},
		{"non-terminal dominates cancel", []Status{StatusCancelled, StatusDispatched}, RunStatusApplying},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateRunStatus(tc.in); got != tc.want {
				t.Errorf("AggregateRunStatus(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
