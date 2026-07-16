//go:build e2e_k8s

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scenario.go — Operator API HTTP client for L3c-5: create an incarnation,
// run a scenario, raw response for the Toll-degraded assert (needs the HTTP
// status + Retry-After header). Symmetric to L3b harness/operator.go +
// harness/stack.go::CreateIncarnation/RunScenario/WaitApplySuccess, but with
// port-forward to keeper:8080 + JWT from [Stack.JWT].

const opHTTPTimeout = 30 * time.Second

// CreateIncarnation — POST /v1/incarnations. Returns the incarnation name
// from the 202 response. serviceRef has the form `<service>@<ref>` -- we
// strip `@<ref>` (ADR-029).
func (s *Stack) CreateIncarnation(t *testing.T, name, serviceRef string, spec map[string]any) string {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("CreateIncarnation: Stack.JWT is empty; call Stack.BootstrapArchon(t) first")
	}
	service := stripServiceRef(serviceRef)
	body := map[string]any{
		"name":    name,
		"service": service,
	}
	if spec != nil {
		body["input"] = spec
	}
	resp, status, err := s.opPostJSON(t, "/v1/incarnations", body)
	if err != nil {
		t.Fatalf("CreateIncarnation %s: %v", name, err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("CreateIncarnation %s: status %d, body=%s", name, status, string(resp))
	}
	var out struct {
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateIncarnation %s: decode: %v (body=%s)", name, err, string(resp))
	}
	if out.Incarnation == "" {
		return name
	}
	return out.Incarnation
}

// RunScenario — POST /v1/incarnations/{name}/scenarios/{scenario}. Returns
// apply_id from the 202 response.
func (s *Stack) RunScenario(t *testing.T, incName, scenarioName string, input map[string]any) string {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("RunScenario: Stack.JWT is empty")
	}
	body := map[string]any{}
	if input != nil {
		body["input"] = input
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", incName, scenarioName)
	resp, status, err := s.opPostJSON(t, path, body)
	if err != nil {
		t.Fatalf("RunScenario %s/%s: %v", incName, scenarioName, err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("RunScenario %s/%s: status %d, body=%s", incName, scenarioName, status, string(resp))
	}
	var out struct {
		ApplyID string `json:"apply_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("RunScenario %s/%s: decode: %v (body=%s)", incName, scenarioName, err, string(resp))
	}
	if out.ApplyID == "" {
		t.Fatalf("RunScenario %s/%s: empty apply_id in 202 body=%s", incName, scenarioName, string(resp))
	}
	return out.ApplyID
}

// PostScenarioRaw — low-level POST /v1/incarnations/{name}/scenarios/{scenario}
// without a status check. Returns (response, statusCode, error). Needed for
// the Toll-degraded assert: the test expects 503 + Retry-After (RunScenario
// would t.Fatal on a non-202).
//
// The returned response has already been read and closed; headers are
// preserved in response.Header for the Retry-After assert.
func (s *Stack) PostScenarioRaw(t *testing.T, incName, scenarioName string, input map[string]any) (*http.Response, int, error) {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("PostScenarioRaw: Stack.JWT is empty")
	}
	body := map[string]any{}
	if input != nil {
		body["input"] = input
	}
	payload, _ := json.Marshal(body)
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", incName, scenarioName)

	pf := s.Cluster.PortForward(t, "svc/keeper", 8080, 30*time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", pf.LocalPort, path)

	ctx, cancel := context.WithTimeout(context.Background(), opHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.JWT)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: opHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	// Read the body right away and close it right away -- the caller gets
	// an already-read response.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, resp.StatusCode, nil
}

// opPostJSON — internal helper for a POST with a JSON body + JWT. Returns
// (body, status, err). Opens a port-forward to keeper:8080 and closes it via
// the t.Cleanup chain (see PortForward).
func (s *Stack) opPostJSON(t *testing.T, path string, body any) ([]byte, int, error) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal: %w", err)
	}
	pf := s.Cluster.PortForward(t, "svc/keeper", 8080, 30*time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d%s", pf.LocalPort, path)

	ctx, cancel := context.WithTimeout(context.Background(), opHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.JWT)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: opHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	return b, resp.StatusCode, nil
}

// stripServiceRef strips `@<ref>` (if present). The Operator API creates an
// incarnation from a bare service name (ADR-029).
func stripServiceRef(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// WaitApplySuccess blocks until all `apply_runs` rows with apply_id=applyID
// reach `success`. A terminal status != success -> t.Fatal with a status
// dump. Symmetric to L3b Stack.WaitApplySuccess; reads PG via port-forward.
func (s *Stack) WaitApplySuccess(t *testing.T, applyID string, timeoutSec int) {
	t.Helper()

	pool := openPGPool(t, s)
	defer pool.Close()

	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		rows, err := pool.Query(ctx, "SELECT sid, status FROM apply_runs WHERE apply_id = $1", applyID)
		cancel()
		if err != nil {
			t.Fatalf("WaitApplySuccess %s: query: %v", applyID, err)
		}
		statuses := map[string]string{}
		for rows.Next() {
			var sid, st string
			if err := rows.Scan(&sid, &st); err != nil {
				rows.Close()
				t.Fatalf("WaitApplySuccess %s: scan: %v", applyID, err)
			}
			statuses[sid] = st
		}
		rows.Close()

		if len(statuses) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		allSuccess := true
		for sid, st := range statuses {
			switch st {
			case "success":
				continue
			case "failed", "cancelled", "orphaned", "no_match":
				t.Fatalf("WaitApplySuccess %s: sid=%s reached terminal %q (statuses=%v)", applyID, sid, st, statuses)
			default:
				allSuccess = false
			}
		}
		if allSuccess {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("WaitApplySuccess %s: success not reached within %ds", applyID, timeoutSec)
}
