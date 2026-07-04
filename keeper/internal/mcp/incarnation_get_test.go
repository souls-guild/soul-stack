package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// incRow — pgx.Row для scanIncarnation: 16 колонок (name, service,
// service_version, state_schema_version, spec, state, status, status_details,
// created_by_aid, created_at, updated_at, covens, traits, last_drift_check_at,
// last_drift_summary, created_scenario). spec/state/status_details/traits/
// last_drift_summary сериализуются в JSONB-bytes — ровно как читает
// scanIncarnation из реального pool-а (db.QueryRow(selectByNameSQL)). covens —
// text[], scanIncarnation читает её в *[]string (env-RBAC, миграция 046);
// last_drift_* — ADR-031 Slice C, миграция 050; created_scenario — механизм
// нескольких create-сценариев (TEXT NOT NULL DEFAULT 'create'), миграция 089.
type incRow struct{ vals []any }

func newIncRow(inc *incarnation.Incarnation) incRow {
	mustJSON := func(m map[string]any) []byte {
		if m == nil {
			return []byte("null")
		}
		b, _ := json.Marshal(m)
		return b
	}
	var statusDetails []byte
	if inc.StatusDetails != nil {
		statusDetails = mustJSON(inc.StatusDetails)
	}
	var driftSummary []byte
	if inc.LastDriftSummary != nil {
		driftSummary, _ = json.Marshal(inc.LastDriftSummary)
	}
	// created_scenario — NULLABLE *string (миграции 089+090): nil = bare-инкарнация
	// (NULL). Передаём указатель как есть — scan вернёт nil при NULL.
	return incRow{vals: []any{
		inc.Name,
		inc.Service,
		inc.ServiceVersion,
		inc.StateSchemaVersion,
		mustJSON(inc.Spec),
		mustJSON(inc.State),
		string(inc.Status),
		statusDetails,
		inc.CreatedByAID,
		inc.CreatedAt,
		inc.UpdatedAt,
		inc.Covens,
		mustJSON(inc.Traits),
		inc.LastDriftCheckAt,
		driftSummary,
		inc.CreatedScenario,
		inc.ApplyingApplyID, // ADR-068 §A1: non-null пока applying, nil на терминале
	}}
}

func (r incRow) Scan(dest ...any) error {
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = r.vals[i].(string)
		case *int:
			*d = r.vals[i].(int)
		case *[]byte:
			if r.vals[i] == nil {
				*d = nil
			} else {
				*d = r.vals[i].([]byte)
			}
		case **string:
			*d = r.vals[i].(*string)
		case *time.Time:
			*d = r.vals[i].(time.Time)
		case **time.Time:
			if r.vals[i] == nil {
				*d = nil
			} else {
				*d = r.vals[i].(*time.Time)
			}
		case *[]string:
			if r.vals[i] == nil {
				*d = nil
			} else {
				*d = r.vals[i].([]string)
			}
		default:
			return fmt.Errorf("incRow.Scan: unexpected dest type %T at %d", d, i)
		}
	}
	return nil
}

// callGet — прогон keeper.incarnation.get через реальный Dispatch (tools/call).
// Эталон для тиража: helper-ы (incarnationRBACContext, mapIncarnationErrorToMCP,
// MaskSecrets) проверяются через настоящий dispatch-путь, не в вакууме.
func callGet(t *testing.T, h *Handler, aid, name string) jsonRPCResponse {
	t.Helper()
	params, _ := json.Marshal(toolsCallParams{
		Name:      "keeper.incarnation.get",
		Arguments: json.RawMessage(`{"name":"` + name + `"}`),
	})
	req := jsonRPCRequest{JSONRPC: "2.0", ID: mustRawID(70), Method: "tools/call", Params: params}
	resp, isNot := h.Dispatch(context.Background(), claims(aid), req)
	if isNot {
		t.Fatal("tools/call must not be a notification")
	}
	return resp
}

// getterRBAC — RBAC, дающий archon-alice permission incarnation.get.
func getterRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "getter", Operators: []string{"archon-alice"}, Permissions: []string{"incarnation.get"}},
		},
	}
}

func TestToolsCall_IncarnationGet_Success(t *testing.T) {
	creator := "archon-creator"
	now := time.Now().UTC()
	pool := &fakePool{
		incFn: func(name string) (*incarnation.Incarnation, error) {
			return &incarnation.Incarnation{
				Name:               name,
				Service:            "redis",
				ServiceVersion:     "v1.2.0",
				StateSchemaVersion: 3,
				Spec:               map[string]any{"replicas": float64(2)},
				State:              map[string]any{"leader": "redis-01"},
				Status:             incarnation.StatusReady,
				CreatedByAID:       &creator,
				CreatedAt:          now,
				UpdatedAt:          now,
			}, nil
		},
	}
	h, _, rec := newTestHandler(t, pool, getterRBAC())

	resp := callGet(t, h, "archon-alice", "redis-prod")
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(res.StructuredContent) == 0 {
		t.Fatal("structuredContent is empty")
	}
	var out incarnationGetOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.Name != "redis-prod" || out.Service != "redis" || out.ServiceVersion != "v1.2.0" {
		t.Errorf("output mismatch: %+v", out)
	}
	if out.StateSchemaVersion != 3 {
		t.Errorf("StateSchemaVersion = %d", out.StateSchemaVersion)
	}
	if out.Status != "ready" {
		t.Errorf("Status = %q", out.Status)
	}
	if out.CreatedByAID == nil || *out.CreatedByAID != creator {
		t.Errorf("CreatedByAID = %v", out.CreatedByAID)
	}
	if out.Spec["replicas"] != float64(2) {
		t.Errorf("Spec.replicas = %v", out.Spec["replicas"])
	}
	if out.State["leader"] != "redis-01" {
		t.Errorf("State.leader = %v", out.State["leader"])
	}
	// reads НЕ аудируются (паритет с REST Get — без audit-payload).
	if len(rec.events) != 0 {
		t.Errorf("get must not write audit events, got %d", len(rec.events))
	}
}

func TestToolsCall_IncarnationGet_NotFound(t *testing.T) {
	pool := &fakePool{
		incFn: func(string) (*incarnation.Incarnation, error) {
			return nil, pgx.ErrNoRows // scanIncarnation → ErrIncarnationNotFound
		},
	}
	h, _, _ := newTestHandler(t, pool, getterRBAC())

	resp := callGet(t, h, "archon-alice", "ghost")
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeNotFound {
		t.Errorf("data.code = %q, want not-found", data.Code)
	}
}

func TestToolsCall_IncarnationGet_RBACForbidden(t *testing.T) {
	// RBAC пуст → deny. SelectByName РЕЗОЛВИТ scope (covens ∪ {name}) для
	// OR-Check (зеркало REST middleware), затем enforcer отказывает →
	// forbidden. Проверяем, что отказ доходит в реальном dispatch-пути.
	pool := &fakePool{
		incFn: func(name string) (*incarnation.Incarnation, error) {
			return &incarnation.Incarnation{Name: name, Status: incarnation.StatusReady}, nil
		},
	}
	h, _, _ := newTestHandler(t, pool, nil) // пустой RBAC → deny

	resp := callGet(t, h, "archon-alice", "redis-prod")
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
}

func TestToolsCall_IncarnationGet_InvalidName(t *testing.T) {
	h, _, _ := newTestHandler(t, &fakePool{}, getterRBAC())
	// `Bad_Name` нарушает NamePattern → validation-failed ДО RBAC/SelectByName.
	resp := callGet(t, h, "archon-alice", "Bad_Name")
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	data := mustToolErrorData(t, resp.Error.Data)
	if data.Code != mcpCodeValidationFailed {
		t.Errorf("data.code = %q, want validation-failed", data.Code)
	}
}

// TestToolsCall_IncarnationGet_SecretsMasked — критичный тест: spec/state с
// sensitive-key и vault-ref значениями уходят в MCP-вывод замаскированными
// (паритет с REST DTO-маскингом, defense-in-depth вариант D).
func TestToolsCall_IncarnationGet_SecretsMasked(t *testing.T) {
	const masked = "***MASKED***"
	pool := &fakePool{
		incFn: func(name string) (*incarnation.Incarnation, error) {
			return &incarnation.Incarnation{
				Name:               name,
				Service:            "redis",
				ServiceVersion:     "v1",
				StateSchemaVersion: 1,
				// `password` — sensitive-key; `tls_cert` — обычный ключ, но
				// значение содержит vault:secret/-маркер.
				Spec: map[string]any{
					"password": "hunter2",
					"replicas": float64(1),
				},
				State: map[string]any{
					"tls_cert": "vault:secret/redis/tls",
					"leader":   "redis-01",
				},
				Status:    incarnation.StatusReady,
				CreatedAt: time.Now().UTC(),
				UpdatedAt: time.Now().UTC(),
			}, nil
		},
	}
	h, _, _ := newTestHandler(t, pool, getterRBAC())

	resp := callGet(t, h, "archon-alice", "redis-prod")
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out incarnationGetOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}

	if out.Spec["password"] != masked {
		t.Errorf("spec.password = %v, want %q (sensitive-key not masked)", out.Spec["password"], masked)
	}
	if out.Spec["replicas"] != float64(1) {
		t.Errorf("spec.replicas = %v, must remain unmasked", out.Spec["replicas"])
	}
	if out.State["tls_cert"] != masked {
		t.Errorf("state.tls_cert = %v, want %q (vault-ref not masked)", out.State["tls_cert"], masked)
	}
	if out.State["leader"] != "redis-01" {
		t.Errorf("state.leader = %v, must remain unmasked", out.State["leader"])
	}

	// Двойная страховка: ни в structuredContent, ни в content[0].text не должно
	// быть raw-секрета (text дублирует JSON для legacy-клиентов).
	rawOut := string(res.StructuredContent)
	if contains(rawOut, "hunter2") || contains(rawOut, "vault:secret/redis/tls") {
		t.Errorf("raw secret leaked into structuredContent: %s", rawOut)
	}
	if len(res.Content) == 0 || contains(res.Content[0].Text, "hunter2") ||
		contains(res.Content[0].Text, "vault:secret/redis/tls") {
		t.Errorf("raw secret leaked into content[0].text")
	}
}
