package applyrun

import "testing"

func TestAggregateRunStatus(t *testing.T) {
	tests := []struct {
		name     string
		in       []Status
		lockHeld bool
		want     RunStatus
	}{
		{"empty → applying", nil, false, RunStatusApplying},
		// Between one step's barrier and the next step's rows every row is
		// terminal; only the lock says the run is not over (NIM-972).
		{"lock held, every row terminal → applying", []Status{StatusSuccess, StatusSuccess}, true, RunStatusApplying},
		{"lock held dominates failure", []Status{StatusFailed}, true, RunStatusApplying},
		{"single running → applying", []Status{StatusRunning}, false, RunStatusApplying},
		{"planned pending → applying", []Status{StatusSuccess, StatusPlanned}, false, RunStatusApplying},
		{"claimed pending → applying", []Status{StatusClaimed}, false, RunStatusApplying},
		{"dispatched pending → applying", []Status{StatusSuccess, StatusDispatched}, false, RunStatusApplying},
		{"all success → success", []Status{StatusSuccess, StatusSuccess}, false, RunStatusSuccess},
		{"success + no_match → success", []Status{StatusSuccess, StatusNoMatch}, false, RunStatusSuccess},
		{"only no_match → success", []Status{StatusNoMatch}, false, RunStatusSuccess},
		{"failed dominates success → failed", []Status{StatusSuccess, StatusFailed}, false, RunStatusFailed},
		{"orphaned → failed", []Status{StatusSuccess, StatusOrphaned}, false, RunStatusFailed},
		{"failed dominates cancelled → failed", []Status{StatusCancelled, StatusFailed}, false, RunStatusFailed},
		{"cancelled without failure → cancelled", []Status{StatusSuccess, StatusCancelled}, false, RunStatusCancelled},
		// applying has the highest priority: an incomplete run cannot be
		// declared failed/cancelled prematurely (otherwise the UI shows terminal
		// on a still-running job).
		{"non-terminal dominates failure", []Status{StatusFailed, StatusRunning}, false, RunStatusApplying},
		{"non-terminal dominates cancel", []Status{StatusCancelled, StatusDispatched}, false, RunStatusApplying},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateRunStatus(tc.in, tc.lockHeld); got != tc.want {
				t.Errorf("AggregateRunStatus(%v, %v) = %q, want %q", tc.in, tc.lockHeld, got, tc.want)
			}
		})
	}
}
