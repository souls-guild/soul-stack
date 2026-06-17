//go:build integration

package api

// E2E на реальной PG/роутере: POST /v1/voyages/preview (ADR-043 amendment §4) —
// dry-resolve scope БЕЗ создания Voyage. Гарантии:
//   - тот же резолв/гейты, что Create (RBAC-by-kind, target ∩ Purview для
//     command, max_scope-cap) — preview отказывает ТАМ ЖЕ, где Create;
//   - ответ НЕ раскрывает SID-список (только числа);
//   - persist не происходит (тело Voyage в БД не появляется — проверяется через
//     отсутствие 202/voyage_id и косвенно через consistency-кейс).
//
// max_scope-cap / window-арифметика / отсутствие SID-полей в DTO покрыты
// дополнительно unit-тестами handler-а (TestVoyagePreview_* в handlers/), где
// maxScope конфигурируем и резолвер детерминирован.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// postPreviewVoyage — POST /v1/voyages/preview; возвращает статус + тело.
func postPreviewVoyage(t *testing.T, base, tok, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/voyages/preview", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/voyages/preview: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// previewReply — числовой ответ preview (без SID-списка).
type previewReply struct {
	Kind               string `json:"kind"`
	ScopeSize          int    `json:"scope_size"`
	TotalBatches       int    `json:"total_batches"`
	BatchMode          string `json:"batch_mode"`
	EffectiveBatchSize *int   `json:"effective_batch_size"`
}

func decodePreview(t *testing.T, body string) previewReply {
	t.Helper()
	var r previewReply
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode preview reply: %v; body=%s", err, body)
	}
	return r
}

// Guard: scenario preview — scope_size = число инкарнаций; total_batches и
// effective_batch_size корректны при batch=N и batch=N%.
func TestIntegration_VoyagePreview_Scenario_ScopeAndBatches(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-a", "redis", "archon-alice")
	seedIncarnation(t, "redis-b", "redis", "archon-alice")
	seedIncarnation(t, "redis-c", "redis", "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// batch=2 → 3 инкарнации, Leg-и [2,1] = 2 батча, effective_batch_size=2.
	code, body := postPreviewVoyage(t, base, tok,
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"redis"},"batch":"2"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	rep := decodePreview(t, body)
	if rep.Kind != "scenario" || rep.ScopeSize != 3 {
		t.Errorf("reply = %+v, want kind=scenario scope_size=3", rep)
	}
	if rep.BatchMode != "barrier" {
		t.Errorf("batch_mode = %q, want barrier", rep.BatchMode)
	}
	if rep.EffectiveBatchSize == nil || *rep.EffectiveBatchSize != 2 {
		t.Errorf("effective_batch_size = %v, want 2", rep.EffectiveBatchSize)
	}
	if rep.TotalBatches != 2 {
		t.Errorf("total_batches = %d, want 2 (ceil 3/2)", rep.TotalBatches)
	}

	// batch=50% → ceil(3*50/100)=2 → effective_batch_size=2, total_batches=2.
	code, body = postPreviewVoyage(t, base, tok,
		`{"kind":"scenario","scenario_name":"deploy","target":{"service":"redis"},"batch":"50%"}`)
	if code != http.StatusOK {
		t.Fatalf("status (percent) = %d, want 200; body=%s", code, body)
	}
	rep = decodePreview(t, body)
	if rep.EffectiveBatchSize == nil || *rep.EffectiveBatchSize != 2 {
		t.Errorf("percent: effective_batch_size = %v, want 2 (ceil 3*50%%)", rep.EffectiveBatchSize)
	}
	if rep.TotalBatches != 2 {
		t.Errorf("percent: total_batches = %d, want 2", rep.TotalBatches)
	}
}

// Guard: command preview — scope_size = число хостов; scoped-Архонт coven=A,
// target coven=A∪B → scope_size = подмножество A (наследует Purview, НЕ весь
// флот). Тот же резолвер, что у Create.
func TestIntegration_VoyagePreview_Command_ScopedSubset(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "a-02.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-b"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	// Широкий target coven=shared (3 хоста); scope=A урезает до 2 (a-01,a-02).
	code, body := postPreviewVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["shared"]}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	rep := decodePreview(t, body)
	if rep.Kind != "command" {
		t.Errorf("kind = %q, want command", rep.Kind)
	}
	if rep.ScopeSize != 2 {
		t.Errorf("scope_size = %d, want 2 (подмножество coven-a, НЕ весь shared=3)", rep.ScopeSize)
	}
}

// Guard: command preview — явный чужой SID → 403 (anti-escalation, parity Create).
func TestIntegration_VoyagePreview_Command_ExplicitForeignSID_403(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"coven-a"}, "archon-a")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	code, body := postPreviewVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"sids":["b-01.example.com"]}}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (явный чужой SID); body=%s", code, body)
	}
}

// Guard: command preview — пустое пересечение (чужой coven урезан в ноль) → 422
// voyage_empty_target (parity Create).
func TestIntegration_VoyagePreview_Command_EmptyIntersection_422(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"coven-a"}, "archon-a")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"coven-b"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	code, body := postPreviewVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["coven-b"]}}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", code, body)
	}
	if !strings.Contains(body, "voyage_empty_target") {
		t.Errorf("detail не содержит voyage_empty_target: %s", body)
	}
}

// Guard: window-режим — корректный ответ (batch_mode=window, total_batches=1,
// effective_batch_size опущен — без null-мусора). window только для command.
func TestIntegration_VoyagePreview_Command_Window_NoNullJunk(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-admin", "")
	seedSoulFull(t, "w-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-admin")
	seedSoulFull(t, "w-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-admin")

	base, stop := startServer(t, errandRunUnrestrictedRBAC("archon-admin"))
	defer stop()
	tok := newValidTokenFor(t, "archon-admin", []string{"cmd-admin"})

	code, body := postPreviewVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]},"batch_mode":"window","concurrency":5}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	rep := decodePreview(t, body)
	if rep.BatchMode != "window" {
		t.Errorf("batch_mode = %q, want window", rep.BatchMode)
	}
	if rep.ScopeSize != 2 || rep.TotalBatches != 1 {
		t.Errorf("scope_size=%d total_batches=%d, want 2/1 (плоское окно)", rep.ScopeSize, rep.TotalBatches)
	}
	if rep.EffectiveBatchSize != nil {
		t.Errorf("effective_batch_size = %v, want отсутствие (window — поле неприменимо)", *rep.EffectiveBatchSize)
	}
	// Явная проверка: в сыром JSON нет ключа effective_batch_size (omitempty).
	if strings.Contains(body, "effective_batch_size") {
		t.Errorf("window-ответ содержит effective_batch_size (должен быть опущен): %s", body)
	}
}

// Guard: ответ preview НЕ содержит SID-список / hosts / incarnations (раскрытие
// узлов запрещено, ADR-043 amendment §4) — явная проверка сырого JSON.
func TestIntegration_VoyagePreview_NoSIDDisclosure(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-admin", "")
	seedSoulFull(t, "secret-host-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-admin")
	seedSoulFull(t, "secret-host-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-admin")

	base, stop := startServer(t, errandRunUnrestrictedRBAC("archon-admin"))
	defer stop()
	tok := newValidTokenFor(t, "archon-admin", []string{"cmd-admin"})

	code, body := postPreviewVoyage(t, base, tok,
		`{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", code, body)
	}
	for _, forbidden := range []string{"secret-host", "\"sids\"", "\"hosts\"", "\"incarnations\"", "\"target_resolved\""} {
		if strings.Contains(body, forbidden) {
			t.Errorf("preview-ответ раскрывает узлы (нашёл %q): %s", forbidden, body)
		}
	}
}

// Guard: консистентность Create↔Preview — тот же body даёт тот же scope_size.
// Preview-числа = то, что Create реально зарезолвил бы (и зарезолвил).
func TestIntegration_VoyagePreview_ConsistentWithCreate(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-admin", "")
	seedSoulFull(t, "c-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-admin")
	seedSoulFull(t, "c-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-admin")
	seedSoulFull(t, "c-03.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-admin")

	base, stop := startServer(t, errandRunUnrestrictedRBAC("archon-admin"))
	defer stop()
	tok := newValidTokenFor(t, "archon-admin", []string{"cmd-admin"})

	const body = `{"kind":"command","module":"core.cmd.shell","target":{"coven":["prod"]},"batch":"2"}`

	codeP, bodyP := postPreviewVoyage(t, base, tok, body)
	if codeP != http.StatusOK {
		t.Fatalf("preview status = %d, want 200; body=%s", codeP, bodyP)
	}
	prevScope := decodePreview(t, bodyP).ScopeSize

	codeC, bodyC := postCommandVoyage(t, base, tok, body)
	if codeC != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202; body=%s", codeC, bodyC)
	}
	createScope := scopeSizeFromReply(t, bodyC)

	if prevScope != createScope {
		t.Errorf("preview scope_size=%d != create scope_size=%d (рассинхрон резолва)", prevScope, createScope)
	}
	if prevScope != 3 {
		t.Errorf("scope_size = %d, want 3", prevScope)
	}
}
