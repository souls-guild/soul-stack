//go:build integration

package api

// E2E against real PG/router: POST /v1/voyages/preview (ADR-043 amendment §4) —
// dry-resolve scope without creating a Voyage. Guarantees:
//   - the same resolve/gates as Create (RBAC-by-kind, target ∩ Purview for
//     command, max_scope-cap) — preview rejects at the SAME points as Create;
//   - the response does NOT reveal the SID list (only numbers);
//   - no persist happens (no Voyage body appears in the DB — verified via
//     the absence of 202/voyage_id and indirectly via the consistency case).
//
// max_scope-cap / window arithmetic / absence of SID fields in the DTO are covered
// additionally by handler unit tests (TestVoyagePreview_* in handlers/), where
// maxScope is configurable and the resolver is deterministic.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// postPreviewVoyage — POST /v1/voyages/preview; returns status + body.
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

// previewReply — the numeric preview response (without the SID list).
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

// Guard: scenario preview — scope_size = the number of incarnations; total_batches and
// effective_batch_size are correct for batch=N and batch=N%.
func TestIntegration_VoyagePreview_Scenario_ScopeAndBatches(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-a", "redis", "archon-alice")
	seedIncarnation(t, "redis-b", "redis", "archon-alice")
	seedIncarnation(t, "redis-c", "redis", "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// batch=2 → 3 incarnations, Legs [2,1] = 2 batches, effective_batch_size=2.
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

// Guard: command preview — scope_size = the number of hosts; a scoped Archon coven=A,
// target coven=A∪B → scope_size = a subset of A (inherits Purview, NOT all
// souls). The same resolver as Create.
func TestIntegration_VoyagePreview_Command_ScopedSubset(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-a", "")
	seedSoulFull(t, "a-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "a-02.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-a"}, "archon-a")
	seedSoulFull(t, "b-01.example.com", "agent", soul.StatusConnected, []string{"shared", "coven-b"}, "archon-a")

	base, stop := startServer(t, errandRunScopeRBAC("archon-a", "coven-a"))
	defer stop()
	tok := newValidTokenFor(t, "archon-a", []string{"cmd-ops"})

	// Wide target coven=shared (3 hosts); scope=A trims to 2 (a-01,a-02).
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

// Guard: command preview — an explicit foreign SID → 403 (anti-escalation, parity Create).
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

// Guard: command preview — empty intersection (a foreign coven trimmed to zero) → 422
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

// Guard: window mode — correct response (batch_mode=window, total_batches=1,
// effective_batch_size omitted — no null junk). window is command-only.
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
	// Explicit check: the raw JSON has no effective_batch_size key (omitempty).
	if strings.Contains(body, "effective_batch_size") {
		t.Errorf("window-ответ содержит effective_batch_size (должен быть опущен): %s", body)
	}
}

// Guard: the preview response does NOT contain a SID list / hosts / incarnations (node
// disclosure is forbidden, ADR-043 amendment §4) — an explicit check of the raw JSON.
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

// Guard: Create↔Preview consistency — the same body yields the same scope_size.
// The preview numbers = what Create would actually resolve (and did resolve).
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
