//go:build integration

package scenario

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// ★ GUARD (ADR-0091, item 15; NIM-952). A run started with no model
// named runs linear, and records the model it executed in: on success in
// engine_compat, and on success and failure alike in its
// incarnation.run_completed event — engine_compat is not stamped by a failure,
// and a failed run is the one whose model gets asked about.
//
// A staged run, and only a staged run, logs that its model is deprecated.
//
// Mutations: drop the field from the stamp, or the key from the payload → red;
// make executionModel return staged for the zero value → "linear by default" red;
// drop the deprecation log.Warn in run() → "staged when named" red.
func TestIntegration_ExecutionModelRecorded(t *testing.T) {
	cases := []struct {
		name   string
		model  ExecutionModel
		result applyrun.Status
		final  incarnation.Status
		want   ExecutionModel
	}{
		{"linear by default, success", "", applyrun.StatusSuccess, incarnation.StatusReady, ExecutionLinear},
		{"linear by default, failure", "", applyrun.StatusFailed, incarnation.StatusErrorLocked, ExecutionLinear},
		{"staged when named, success", ExecutionStaged, applyrun.StatusSuccess, incarnation.StatusReady, ExecutionStaged},
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
			logs := &lockedBuffer{}
			r.logger = slog.New(slog.NewJSONHandler(logs, nil))

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
			warned := strings.Contains(logs.String(), "deprecated staged model")
			if warned != (tc.want == ExecutionStaged) {
				t.Errorf("deprecation warning logged = %v for a %s run, want it for a staged run only", warned, tc.want)
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

// forEachModel runs body under linear dispatch, the default, and under staged,
// the rollback kept for one release (NIM-952): for behaviour both must keep.
func forEachModel(t *testing.T, body func(t *testing.T, model ExecutionModel)) {
	for _, model := range []ExecutionModel{ExecutionLinear, ExecutionStaged} {
		t.Run(string(model), func(t *testing.T) { body(t, model) })
	}
}
