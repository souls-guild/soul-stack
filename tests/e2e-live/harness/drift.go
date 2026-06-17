//go:build e2e_live

package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// DriftReport — read-проекция 200-body POST /v1/incarnations/{name}/check-drift
// (scenario.DriftReport, ADR-031 Slice B). Поля — публичный JSON-контракт; типы
// keeper-side не импортируем (E2E тестирует OpenAPI как чёрный ящик). Порт
// tests/e2e/harness/drift.go под L3b (тот же direct-HTTP opClient.post).
type DriftReport struct {
	CheckedAt   time.Time         `json:"checked_at"`
	Incarnation string            `json:"incarnation"`
	ScenarioRef string            `json:"scenario_ref"`
	Hosts       []DriftHostReport `json:"hosts"`
	Summary     DriftSummary      `json:"summary"`
}

// DriftHostReport — per-host агрегат DriftReport.Hosts[].
type DriftHostReport struct {
	SID    string            `json:"sid"`
	Status string            `json:"status"` // drifted | clean | unsupported | failed.
	Tasks  []DriftTaskResult `json:"tasks"`
}

// DriftTaskResult — per-task результат внутри DriftHostReport.Tasks[].
type DriftTaskResult struct {
	Idx     int    `json:"idx"`
	Module  string `json:"module"`
	Action  string `json:"action,omitempty"`
	Changed bool   `json:"changed"`
	Message string `json:"message,omitempty"`
}

// DriftSummary — counts-агрегат DriftReport.Summary.
type DriftSummary struct {
	HostsDrifted     int `json:"hosts_drifted"`
	HostsClean       int `json:"hosts_clean"`
	HostsUnsupported int `json:"hosts_unsupported"`
	HostsFailed      int `json:"hosts_failed"`
}

// CheckDrift вызывает POST /v1/incarnations/{name}/check-drift (sync, ADR-031
// Slice B) и возвращает разобранный DriftReport. input — converge-input-override
// (nil = auto-from-state по конвенции имени). Любой не-200 — t.Fatal с телом.
//
// L3b-контракт (отличие от L3a): drift собирается из РЕАЛЬНОГО SoulModule.Plan
// core-модуля внутри soul-контейнера (а не stub.SetDryRunPlan) — Keeper рендерит
// converge, рассылает ApplyRequest{dry_run:true}, Soul зовёт core.file.Plan
// (read-safe) и отдаёт per-task changed.
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
		t.Fatalf("CheckDrift %s: status %d, ожидался 200; body=%s", incarnationName, status, string(resp))
	}
	var rep DriftReport
	if err := json.Unmarshal(resp, &rep); err != nil {
		t.Fatalf("CheckDrift %s: decode DriftReport: %v (body=%s)", incarnationName, err, string(resp))
	}
	return rep
}
