//go:build integration

package scenario

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ★ GUARD (draft ADR NIM-907, item 15). A run records the model it executed in:
// on success in engine_compat, and on success and failure alike in its
// incarnation.run_completed event — engine_compat is not stamped by a failure,
// and a failed run is the one whose model gets asked about.
//
// Mutations: drop the field from the stamp, or the key from the payload → red.
func TestIntegration_ExecutionModelRecorded(t *testing.T) {
	cases := []struct {
		name   string
		model  ExecutionModel
		result applyrun.Status
		final  incarnation.Status
		want   ExecutionModel
	}{
		{"staged by default, success", "", applyrun.StatusSuccess, incarnation.StatusReady, ExecutionStaged},
		{"linear, success", ExecutionLinear, applyrun.StatusSuccess, incarnation.StatusReady, ExecutionLinear},
		{"linear, failure", ExecutionLinear, applyrun.StatusFailed, incarnation.StatusErrorLocked, ExecutionLinear},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAll(t)
			seedOperator(t, "archon-alice")
			seedIncarnation(t, "noop-prod")
			seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})
			gitURL := noopServiceRepo(t)

			disp := &mockDispatcher{t: t, result: tc.result}
			if tc.result == applyrun.StatusFailed {
				summary := "module failed"
				disp.summary = &summary
			}
			r := newRunnerWithAudit(t, disp)
			r.deps.ExecutionModel = tc.model

			applyID := audit.NewULID()
			if err := r.Start(context.Background(), RunSpec{
				ApplyID:         applyID,
				IncarnationName: "noop-prod",
				ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
				ScenarioName:    "create",
				StartedByAID:    "archon-alice",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitRunDone(t, "noop-prod", applyID, tc.final)

			ev := waitRunCompletedEvents(t, applyID)
			if ev["execution_model"] != string(tc.want) {
				t.Errorf("run_completed execution_model = %v, want %q", ev["execution_model"], tc.want)
			}
			stamp := readIncarnationStamp(t, "noop-prod")
			if tc.final == incarnation.StatusReady {
				if stamp == nil || stamp.ExecutionModel != string(tc.want) {
					t.Errorf("engine_compat = %+v, want execution_model %q", stamp, tc.want)
				}
			} else if stamp != nil {
				t.Errorf("engine_compat = %+v after a failed run, want none (a failure stamps nothing)", stamp)
			}
		})
	}
}
