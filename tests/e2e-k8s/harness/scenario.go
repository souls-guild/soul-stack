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

// scenario.go — Operator-API HTTP-клиент для L3c-5: создание incarnation,
// запуск scenario, raw-response для Toll-degraded assert-а (нужны HTTP-status
// + Retry-After header). Симметрично L3b harness/operator.go +
// harness/stack.go::CreateIncarnation/RunScenario/WaitApplySuccess, но
// port-forward к keeper:8080 + JWT из [Stack.JWT].

const opHTTPTimeout = 30 * time.Second

// CreateIncarnation — POST /v1/incarnations. Возвращает имя incarnation из
// 202-ответа. serviceRef формата `<service>@<ref>` — `@<ref>` отрезаем (ADR-029).
func (s *Stack) CreateIncarnation(t *testing.T, name, serviceRef string, spec map[string]any) string {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("CreateIncarnation: Stack.JWT пуст; нужно сначала Stack.BootstrapArchon(t)")
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

// RunScenario — POST /v1/incarnations/{name}/scenarios/{scenario}. Возвращает
// apply_id из 202-ответа.
func (s *Stack) RunScenario(t *testing.T, incName, scenarioName string, input map[string]any) string {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("RunScenario: Stack.JWT пуст")
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
		t.Fatalf("RunScenario %s/%s: пустой apply_id в 202 body=%s", incName, scenarioName, string(resp))
	}
	return out.ApplyID
}

// PostScenarioRaw — низкоуровневый POST /v1/incarnations/{name}/scenarios/{scenario}
// без проверки статуса. Возвращает (response, statusCode, error). Нужен для
// Toll-degraded assert-а: тест ожидает 503 + Retry-After (RunScenario рухнул
// бы t.Fatal на не-202).
//
// Возвращаемый response уже прочитан и закрыт; headers сохранены в response.Header
// для assert-а Retry-After.
func (s *Stack) PostScenarioRaw(t *testing.T, incName, scenarioName string, input map[string]any) (*http.Response, int, error) {
	t.Helper()
	if s.JWT == "" {
		t.Fatal("PostScenarioRaw: Stack.JWT пуст")
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
	// Тело прочитываем сразу, body-close-ить тоже сразу — caller получает уже
	// прочитанный response.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, resp.StatusCode, nil
}

// opPostJSON — внутренний helper для POST с JSON-body + JWT. Возвращает
// (body, status, err). Открывает port-forward на keeper:8080 и закрывает
// через t.Cleanup-цепочку (см. PortForward).
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

// stripServiceRef отрезает `@<ref>` (если есть). Operator API создаёт
// incarnation по bare service-name (ADR-029).
func stripServiceRef(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// WaitApplySuccess блокируется до перехода всех строк `apply_runs` с
// apply_id=applyID в статус `success`. Терминальный ≠ success → t.Fatal с
// дампом статусов. Симметрично L3b Stack.WaitApplySuccess; читает PG через
// port-forward.
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
	t.Fatalf("WaitApplySuccess %s: success не достигнут за %ds", applyID, timeoutSec)
}
