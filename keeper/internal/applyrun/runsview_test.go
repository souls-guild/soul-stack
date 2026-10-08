package applyrun

import "testing"

func TestAggregateRunStatus(t *testing.T) {
	tests := []struct {
		name     string
		in       []Status
		dropped  int
		lockHeld bool
		want     RunStatus
	}{
		{"empty → applying", nil, 0, false, RunStatusApplying},
		// Between one step's barrier and the next step's rows every row is
		// terminal; only the lock says the run is not over (NIM-972).
		{"lock held, every row terminal → applying", []Status{StatusSuccess, StatusSuccess}, 0, true, RunStatusApplying},
		{"lock held dominates failure", []Status{StatusFailed}, 0, true, RunStatusApplying},
		{"single running → applying", []Status{StatusRunning}, 0, false, RunStatusApplying},
		{"planned pending → applying", []Status{StatusSuccess, StatusPlanned}, 0, false, RunStatusApplying},
		{"claimed pending → applying", []Status{StatusClaimed}, 0, false, RunStatusApplying},
		{"dispatched pending → applying", []Status{StatusSuccess, StatusDispatched}, 0, false, RunStatusApplying},
		{"all success → success", []Status{StatusSuccess, StatusSuccess}, 0, false, RunStatusSuccess},
		{"success + no_match → success", []Status{StatusSuccess, StatusNoMatch}, 0, false, RunStatusSuccess},
		{"only no_match → success", []Status{StatusNoMatch}, 0, false, RunStatusSuccess},
		{"failed dominates success → failed", []Status{StatusSuccess, StatusFailed}, 0, false, RunStatusFailed},
		{"orphaned → failed", []Status{StatusSuccess, StatusOrphaned}, 0, false, RunStatusFailed},
		{"failed dominates cancelled → failed", []Status{StatusCancelled, StatusFailed}, 0, false, RunStatusFailed},
		{"cancelled without failure → cancelled", []Status{StatusSuccess, StatusCancelled}, 0, false, RunStatusCancelled},
		// applying has the highest priority: an incomplete run cannot be
		// declared failed/cancelled prematurely (otherwise the UI shows terminal
		// on a still-running job).
		{"non-terminal dominates failure", []Status{StatusFailed, StatusRunning}, 0, false, RunStatusApplying},
		{"non-terminal dominates cancel", []Status{StatusCancelled, StatusDispatched}, 0, false, RunStatusApplying},
		// NIM-948: a run that finished without its dropped hosts.
		{"every failure a drop → partial_failed", []Status{StatusSuccess, StatusFailed, StatusFailed}, 2, false, RunStatusPartialFailed},
		{"orphaned drop → partial_failed", []Status{StatusSuccess, StatusOrphaned}, 1, false, RunStatusPartialFailed},
		{"a failure that is not a drop → failed", []Status{StatusSuccess, StatusFailed, StatusFailed}, 1, false, RunStatusFailed},
		{"cancelled beats a drop", []Status{StatusCancelled, StatusFailed}, 1, false, RunStatusCancelled},
		{"non-terminal dominates a drop", []Status{StatusFailed, StatusRunning}, 1, false, RunStatusApplying},
		{"lock held beats a drop", []Status{StatusSuccess, StatusFailed}, 1, true, RunStatusApplying},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateRunStatus(tc.in, tc.dropped, tc.lockHeld); got != tc.want {
				t.Errorf("AggregateRunStatus(%v, %d, %v) = %q, want %q", tc.in, tc.dropped, tc.lockHeld, got, tc.want)
			}
		})
	}
}
