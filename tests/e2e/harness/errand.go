//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// ErrandResult — read-проекция ErrandResult (POST /v1/souls/{sid}/exec, ADR-033)
// для e2e-ассертов single-Errand. Поля — публичный JSON-контракт handler-а.
type ErrandResult struct {
	ErrandID string `json:"errand_id"`
	SID      string `json:"sid"`
	Module   string `json:"module"`
	Status   string `json:"status"`
}

// errandTerminalStatuses — терминальные статусы single-Errand (ADR-033).
// Дублируется литералом (tests/e2e — отдельный go-модуль без зависимости на
// keeper/internal).
var errandTerminalStatuses = map[string]struct{}{
	"success":            {},
	"failed":             {},
	"timed_out":          {},
	"cancelled":          {},
	"module_not_allowed": {},
}

// ExecErrand отправляет POST /v1/souls/{sid}/exec (single-Errand ad-hoc exec,
// ADR-033) и возвращает терминальный ErrandResult. При 200 — sync-результат; при
// 202 (async-эскалация) — поллит GET /v1/errands/{errand_id} до терминала.
// module — fully-qualified (whitelist Errand-а: core.cmd.shell / core.exec.run).
// Любой иной статус — t.Fatal с телом ответа.
func (s *Stack) ExecErrand(t *testing.T, sid, module string, input map[string]any) ErrandResult {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{"module": module}
	if input != nil {
		body["input"] = input
	}
	resp, status, err := c.post(context.Background(), "/v1/souls/"+sid+"/exec", body)
	if err != nil {
		t.Fatalf("ExecErrand %s/%s: http: %v", sid, module, err)
	}
	switch status {
	case http.StatusOK:
		var res ErrandResult
		if jerr := json.Unmarshal(resp, &res); jerr != nil {
			t.Fatalf("ExecErrand %s/%s: decode: %v (body=%s)", sid, module, jerr, string(resp))
		}
		return res
	case http.StatusAccepted:
		var acc struct {
			ErrandID string `json:"errand_id"`
		}
		if jerr := json.Unmarshal(resp, &acc); jerr != nil || acc.ErrandID == "" {
			t.Fatalf("ExecErrand %s/%s: 202 без errand_id (body=%s)", sid, module, string(resp))
		}
		return s.waitErrandTerminal(t, acc.ErrandID, 60)
	default:
		t.Fatalf("ExecErrand %s/%s: status %d, body=%s", sid, module, status, string(resp))
		return ErrandResult{}
	}
}

// waitErrandTerminal поллит GET /v1/errands/{id} до терминала single-Errand.
func (s *Stack) waitErrandTerminal(t *testing.T, errandID string, timeoutSec int) ErrandResult {
	t.Helper()
	c := s.opClient(t)
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var last ErrandResult
	for time.Now().Before(deadline) {
		resp, status, err := c.get(context.Background(), "/v1/errands/"+errandID)
		if err != nil {
			t.Fatalf("waitErrandTerminal %s: http: %v", errandID, err)
		}
		if status == http.StatusOK {
			if jerr := json.Unmarshal(resp, &last); jerr != nil {
				t.Fatalf("waitErrandTerminal %s: decode: %v (body=%s)", errandID, jerr, string(resp))
			}
			if _, ok := errandTerminalStatuses[last.Status]; ok {
				return last
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("waitErrandTerminal %s: терминал не достигнут за %ds (status=%q)", errandID, timeoutSec, last.Status)
	return last
}
