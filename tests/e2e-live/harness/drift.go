//go:build e2e_live

package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// DriftReport — read projection of the 200 body of POST
// /v1/incarnations/{name}/check-drift (scenario.DriftReport, ADR-031 Slice
// B). Fields are the public JSON contract; keeper-side types are not
// imported (E2E tests OpenAPI as a black box). Port of
// tests/e2e/harness/drift.go for L3b (same direct-HTTP opClient.post).
type DriftReport struct {
	CheckedAt   time.Time         `json:"checked_at"`
	Incarnation string            `json:"incarnation"`
	ScenarioRef string            `json:"scenario_ref"`
	Hosts       []DriftHostReport `json:"hosts"`
	Summary     DriftSummary      `json:"summary"`
}

// DriftHostReport — per-host aggregate of DriftReport.Hosts[].
type DriftHostReport struct {
	SID    string            `json:"sid"`
	Status string            `json:"status"` // drifted | clean | unsupported | failed.
	Tasks  []DriftTaskResult `json:"tasks"`
}

// DriftTaskResult — per-task result within DriftHostReport.Tasks[].
type DriftTaskResult struct {
	Idx     int    `json:"idx"`
	Module  string `json:"module"`
	Action  string `json:"action,omitempty"`
	Changed bool   `json:"changed"`
	Message string `json:"message,omitempty"`
}

// DriftSummary — counts aggregate of DriftReport.Summary.
type DriftSummary struct {
	HostsDrifted     int `json:"hosts_drifted"`
	HostsClean       int `json:"hosts_clean"`
	HostsUnsupported int `json:"hosts_unsupported"`
	HostsFailed      int `json:"hosts_failed"`
}

// CheckDrift calls POST /v1/incarnations/{name}/check-drift (sync, ADR-031
// Slice B) and returns the decoded DriftReport. input is the converge input
// override (nil = auto-from-state by naming convention). Any non-200 is a
// t.Fatal with the body.
//
// L3b contract (difference from L3a): drift is collected from the REAL
// SoulModule.Plan of the core module inside the soul container (not
// stub.SetDryRunPlan) — Keeper renders the converge, sends
// ApplyRequest{dry_run:true}, Soul calls core.file.Plan (read-safe) and
// returns per-task changed.
func (s *Stack) CheckDrift(t *testing.T, incarnationName string, input map[string]any) DriftReport {
	t.Helper()
	c := s.opClient(t)
	var body map[string]any
	if input != nil {
		body = map[string]any{"input": input}
	}
	resp, status, err := c.post(context.Background(),
		"/v1/incarnations/"+incarnationName+"/check-drift", body)
	if err != nil {
		t.Fatalf("CheckDrift %s: http: %v", incarnationName, err)
	}
	if status != http.StatusOK {
		t.Fatalf("CheckDrift %s: status %d, expected 200; body=%s", incarnationName, status, string(resp))
	}
	var rep DriftReport
	if err := json.Unmarshal(resp, &rep); err != nil {
		t.Fatalf("CheckDrift %s: decode DriftReport: %v (body=%s)", incarnationName, err, string(resp))
	}
	return rep
}
