package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// doAssignTraits вызывает AssignTraitsTyped напрямую (handler-native) с заранее
// собранным native input-ом (traits-значения произвольных типов — strict-JSON-
// декод как у coven не нужен) и сериализует результат в recorder.
func doAssignTraits(t *testing.T, h *SoulHandler, in SoulTraitsAssignInput, dryRunQuery bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	reply, err := h.AssignTraitsTyped(context.Background(), claimsFor("archon-alice"), in, dryRunQuery)
	if err != nil {
		writeProblemError(rec, httptest.NewRequest(http.MethodPost, "/v1/souls/traits", nil), err)
		return rec
	}
	writeJSON(rec, http.StatusOK, reply.Body, h.logger)
	return rec
}

func unmarshalTraitsOut(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func TestAssignTraits_Merge_Happy(t *testing.T) {
	pool := &fakeSoulPool{listCount: 3, bulkScanned: 3, bulkChanged: 2}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"namespace": "dba", "tier": float64(1)},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := unmarshalTraitsOut(t, rec)
	if out["mode"] != "merge" {
		t.Errorf("mode = %v, want merge", out["mode"])
	}
	if out["matched"].(float64) != 3 || out["changed"].(float64) != 2 {
		t.Errorf("matched/changed = %v/%v, want 3/2", out["matched"], out["changed"])
	}
	keys, ok := out["keys"].([]any)
	if !ok || len(keys) != 2 {
		t.Fatalf("keys = %v, want 2-элементный массив", out["keys"])
	}
	// keys отсортированы детерминированно.
	if keys[0] != "namespace" || keys[1] != "tier" {
		t.Errorf("keys = %v, want [namespace tier] (sorted)", keys)
	}
	if pool.bulkChunkCalls != 1 {
		t.Errorf("bulkChunkCalls = %d, want 1", pool.bulkChunkCalls)
	}
}

// TestAssignTraits_DefaultMode_Merge — mode опущен → merge (дефолт).
func TestAssignTraits_DefaultMode_Merge(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Traits:   map[string]any{"x": "y"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if unmarshalTraitsOut(t, rec)["mode"] != "merge" {
		t.Errorf("default mode != merge")
	}
}

func TestAssignTraits_Replace_Happy(t *testing.T) {
	pool := &fakeSoulPool{listCount: 2, bulkScanned: 2, bulkChanged: 2}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "replace",
		Traits:   map[string]any{"only": "this"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := unmarshalTraitsOut(t, rec)
	if out["mode"] != "replace" {
		t.Errorf("mode = %v, want replace", out["mode"])
	}
	if pool.bulkChunkCalls != 1 {
		t.Errorf("bulkChunkCalls = %d, want 1 (replace дошёл до chunk-UPDATE)", pool.bulkChunkCalls)
	}
}

func TestAssignTraits_Remove_Happy(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "remove",
		Keys:     []string{"drop-me", "also-drop"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := unmarshalTraitsOut(t, rec)
	if out["mode"] != "remove" {
		t.Errorf("mode = %v, want remove", out["mode"])
	}
	keys := out["keys"].([]any)
	if len(keys) != 2 || keys[0] != "also-drop" || keys[1] != "drop-me" {
		t.Errorf("keys = %v, want [also-drop drop-me] (sorted)", keys)
	}
}

func TestAssignTraits_DryRun_NoUpdate(t *testing.T) {
	pool := &fakeSoulPool{listCount: 5}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"x": "y"},
		DryRun:   true,
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := unmarshalTraitsOut(t, rec)
	if out["matched"].(float64) != 5 || out["changed"].(float64) != 0 {
		t.Errorf("matched/changed = %v/%v, want 5/0 for dry_run", out["matched"], out["changed"])
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("bulkChunkCalls = %d, want 0 (dry_run must not UPDATE)", pool.bulkChunkCalls)
	}
}

func TestAssignTraits_DryRun_QueryParam(t *testing.T) {
	pool := &fakeSoulPool{listCount: 2}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)

	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"x": "y"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, true) // ?dry_run=true
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("?dry_run=true should suppress UPDATE, chunkCalls=%d", pool.bulkChunkCalls)
	}
}

func TestAssignTraits_BadMode_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "append",
		Traits:   map[string]any{"x": "y"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (unknown mode), body=%s", rec.Code, rec.Body.String())
	}
}

// TestAssignTraits_BadKey_422 — kebab-формат ключа enforce-ится (422 ДО БД).
func TestAssignTraits_BadKey_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"Bad_Key": "v"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (bad key), body=%s", rec.Code, rec.Body.String())
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("UPDATE выполнен на битом ключе")
	}
}

// TestAssignTraits_NestedValue_422 — вложенный объект/массив-в-массиве отвергается.
func TestAssignTraits_NestedValue_422(t *testing.T) {
	for _, v := range []any{
		map[string]any{"nested": "obj"},
		[]any{[]any{"x"}},
		nil,
	} {
		pool := &fakeSoulPool{}
		h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
		rec := doAssignTraits(t, h, SoulTraitsAssignInput{
			Mode:     "merge",
			Traits:   map[string]any{"k": v},
			Selector: SoulCovenAssignSelectorInput{All: true},
		}, false)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("value %#v: status = %d, want 422 (nested rejected), body=%s", v, rec.Code, rec.Body.String())
		}
		if pool.bulkChunkCalls != 0 {
			t.Errorf("value %#v: UPDATE выполнен на вложенном значении", v)
		}
	}
}

// TestAssignTraits_XOR_TraitsForRemove_422 — remove + traits → XOR-нарушение.
func TestAssignTraits_XOR_TraitsForRemove_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "remove",
		Traits:   map[string]any{"k": "v"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (traits запрещён для remove)", rec.Code)
	}
}

// TestAssignTraits_XOR_KeysForMerge_422 — merge + keys → XOR-нарушение.
func TestAssignTraits_XOR_KeysForMerge_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Keys:     []string{"k"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (keys запрещён для merge)", rec.Code)
	}
}

// TestAssignTraits_Remove_EmptyKeys_422 — remove без ключей бессмысленно → 422.
func TestAssignTraits_Remove_EmptyKeys_422(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "remove",
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (пустой keys для remove)", rec.Code)
	}
}

// TestAssignTraits_EmptySelector_422 — selector без критериев → 422 (тот же
// гейт ErrBulkEmptySelector, что у coven).
func TestAssignTraits_EmptySelector_422(t *testing.T) {
	pool := &fakeSoulPool{}
	h := NewSoulHandler(pool, fakeScoper{unrestricted: true}, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:   "merge",
		Traits: map[string]any{"x": "y"},
	}, false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (пустой selector), body=%s", rec.Code, rec.Body.String())
	}
}

// TestAssignTraits_AuditPayload_NoValues — GUARD секрет-гигиены: audit-payload
// несёт keys, но НЕ trait-значения; source=api; scope_applied отражает scope.
func TestAssignTraits_AuditPayload_NoValues(t *testing.T) {
	pool := &fakeSoulPool{listCount: 1, bulkScanned: 1, bulkChanged: 1}
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)

	reply, err := h.AssignTraitsTyped(context.Background(), claimsFor("archon-alice"), SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"namespace": "secret-value"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if err != nil {
		t.Fatalf("AssignTraitsTyped: %v", err)
	}
	p := reply.AuditPayload
	if p["source"] != "api" {
		t.Errorf("audit source = %v, want api", p["source"])
	}
	if p["scope_applied"] != true {
		t.Errorf("scope_applied = %v, want true (coven-scoped operator)", p["scope_applied"])
	}
	keys, ok := p["keys"].([]string)
	if !ok || len(keys) != 1 || keys[0] != "namespace" {
		t.Fatalf("audit keys = %v, want [namespace]", p["keys"])
	}
	// Значение НЕ должно протечь в payload (грубая проверка: ни одно значение не
	// равно секрету).
	raw, _ := json.Marshal(p)
	if containsSubstr(string(raw), "secret-value") {
		t.Errorf("audit payload содержит trait-ЗНАЧЕНИЕ: %s", raw)
	}
}

// TestAssignTraits_ScopedOperator_HostOutOfScope_0Changed — GUARD least-privilege
// (гейт a): coven-scoped оператор (scope=dev) с selector=sids[prod-host] доходит
// до bulk-слоя, но scope-предикат в WHERE даёт 0 хостов (fakeDB listCount=0) →
// 200 + matched/changed=0, без UPDATE. trait-ключ scope-гейтом (b) НЕ проверяется
// (он не scope-измерение) — единственная защита тут гейт (a) на хосты.
func TestAssignTraits_ScopedOperator_HostOutOfScope_0Changed(t *testing.T) {
	pool := &fakeSoulPool{listCount: 0} // scope-фильтр сделал matched=0.
	h := NewSoulHandler(pool, fakeScoper{covens: []string{"dev"}}, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"x": "y"},
		Selector: SoulCovenAssignSelectorInput{SIDs: []string{"prod-host.example.com"}},
	}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	out := unmarshalTraitsOut(t, rec)
	if out["matched"].(float64) != 0 || out["changed"].(float64) != 0 {
		t.Errorf("matched/changed = %v/%v, want 0/0 (host out of scope)", out["matched"], out["changed"])
	}
	if pool.bulkChunkCalls != 0 {
		t.Errorf("UPDATE выполнен при matched=0 (out-of-scope host)")
	}
}

// TestAssignTraits_ScoperNil_500 — без scoper-а bulk недоступен (500), least-
// privilege не обходится (паритет coven).
func TestAssignTraits_ScoperNil_500(t *testing.T) {
	h := NewSoulHandler(&fakeSoulPool{}, nil, nil, nil)
	rec := doAssignTraits(t, h, SoulTraitsAssignInput{
		Mode:     "merge",
		Traits:   map[string]any{"x": "y"},
		Selector: SoulCovenAssignSelectorInput{All: true},
	}, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (scoper nil)", rec.Code)
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
