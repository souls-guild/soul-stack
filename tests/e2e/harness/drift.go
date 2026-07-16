//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// DriftReport — read projection of the 200 body of POST
// /v1/incarnations/{name}/check-drift (scenario.DriftReport, ADR-031 Slice B).
// Fields form the public JSON contract; keeper-side types are not imported
// (E2E tests OpenAPI as a black box).
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
// Slice B) and returns the parsed DriftReport. input — converge input override
// (nil = auto-from-state). Any non-200 — t.Fatal with the response body.
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

// SoulHistoryItem — read projection of one items[] element from
// GET /v1/souls/{sid}/history. Fields form the handler's public JSON contract.
type SoulHistoryItem struct {
	Type        string `json:"type"` // scenario | errand.
	ID          string `json:"id"`
	Incarnation string `json:"incarnation,omitempty"`
	Scenario    string `json:"scenario,omitempty"`
	Module      string `json:"module,omitempty"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// SoulHistoryReply — the 200 body of GET /v1/souls/{sid}/history.
type SoulHistoryReply struct {
	SID    string            `json:"sid"`
	Items  []SoulHistoryItem `json:"items"`
	Offset int               `json:"offset"`
	Limit  int               `json:"limit"`
	Total  int               `json:"total"`
}

// SoulHistory calls GET /v1/souls/{sid}/history with an optional type query
// filter (scenario|errand; empty = both sources) and returns the parsed
// response. Any non-200 — t.Fatal with the body.
func (s *Stack) SoulHistory(t *testing.T, sid, typeFilter string) SoulHistoryReply {
	t.Helper()
	c := s.opClient(t)
	path := "/v1/souls/" + sid + "/history"
	if typeFilter != "" {
		path += "?type=" + typeFilter
	}
	resp, status, err := c.get(context.Background(), path)
	if err != nil {
		t.Fatalf("SoulHistory %s: http: %v", sid, err)
	}
	if status != http.StatusOK {
		t.Fatalf("SoulHistory %s: status %d, expected 200; body=%s", sid, status, string(resp))
	}
	var reply SoulHistoryReply
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("SoulHistory %s: decode reply: %v (body=%s)", sid, err, string(resp))
	}
	return reply
}
