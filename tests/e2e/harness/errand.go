//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// ErrandResult — read projection of ErrandResult (POST /v1/souls/{sid}/exec,
// ADR-033) for single-Errand e2e asserts. Fields form the handler's public
// JSON contract.
type ErrandResult struct {
	ErrandID string `json:"errand_id"`
	SID      string `json:"sid"`
	Module   string `json:"module"`
	Status   string `json:"status"`
}

// errandTerminalStatuses — terminal statuses of a single Errand (ADR-033).
// Duplicated as a literal (tests/e2e is a separate go module without a
// dependency on keeper/internal).
var errandTerminalStatuses = map[string]struct{}{
	"success":            {},
	"failed":             {},
	"timed_out":          {},
	"cancelled":          {},
	"module_not_allowed": {},
}

// ExecErrand sends POST /v1/souls/{sid}/exec (single-Errand ad-hoc exec,
// ADR-033) and returns a terminal ErrandResult. On 200 — a sync result; on
// 202 (async escalation) — polls GET /v1/errands/{errand_id} until terminal.
// module — fully-qualified (Errand whitelist: core.cmd.shell / core.exec.run).
// Any other status — t.Fatal with the response body.
func (s *Stack) ExecErrand(t *testing.T, sid, module string, input map[string]any) ErrandResult {
	t.Helper()
	return s.execErrand(t, sid, module, input, false)
}

// ExecErrandRaw sends the request and hands back the raw status + body without
// interpreting either. dryRun=true can be refused two different ways, and both are
// statuses [Stack.ExecErrand] would turn into a t.Fatal, so a test that wants to judge
// a refusal itself has to see it raw:
//
//   - 400 malformed-request — the module is verb-shell (ADR-033, NIM-489). Decided
//     from the request alone, so it lands before the checks below; picking
//     `core.cmd.shell` for a dry_run fixture means testing this and nothing else.
//   - 409 soul-capability-unsupported — the Keeper-side capability gate
//     (ADR-0076(i), NIM-456): the target never announced `dry_run`.
func (s *Stack) ExecErrandRaw(t *testing.T, sid, module string, input map[string]any, dryRun bool) (int, string) {
	t.Helper()
	c := s.opClient(t)
	resp, status, err := c.post(context.Background(), "/v1/souls/"+sid+"/exec",
		errandBody(module, input, dryRun))
	if err != nil {
		t.Fatalf("ExecErrandRaw %s/%s: http: %v", sid, module, err)
	}
	return status, string(resp)
}

func errandBody(module string, input map[string]any, dryRun bool) map[string]any {
	body := map[string]any{"module": module}
	if input != nil {
		body["input"] = input
	}
	if dryRun {
		body["dry_run"] = true
	}
	return body
}

func (s *Stack) execErrand(t *testing.T, sid, module string, input map[string]any, dryRun bool) ErrandResult {
	t.Helper()
	c := s.opClient(t)
	resp, status, err := c.post(context.Background(), "/v1/souls/"+sid+"/exec",
		errandBody(module, input, dryRun))
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
			t.Fatalf("ExecErrand %s/%s: 202 without errand_id (body=%s)", sid, module, string(resp))
		}
		return s.waitErrandTerminal(t, acc.ErrandID, 60)
	default:
		t.Fatalf("ExecErrand %s/%s: status %d, body=%s", sid, module, status, string(resp))
		return ErrandResult{}
	}
}

// waitErrandTerminal polls GET /v1/errands/{id} until a single-Errand terminal.
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
	t.Fatalf("waitErrandTerminal %s: terminal not reached within %ds (status=%q)", errandID, timeoutSec, last.Status)
	return last
}
