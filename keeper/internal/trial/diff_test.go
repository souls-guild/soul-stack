package trial

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ Анти-дрейф-сверка: trial-сторона зеркала прод-merge. Фикстура/операции/
// ожидание ДОЛЖНЫ совпадать байт-в-байт с scenario.TestMergeStateChanges_MirrorProd
// (state_test.go). Если тела mergeStateChanges (state.go vs diff.go) разойдутся —
// один из двух тестов упадёт на одном и том же входе.
//
// Зеркальные значения дублированы здесь намеренно (пакеты scenario/trial
// изолированы; общая фикстура потребовала бы экспортного хелпера в render — over-
// engineering ради одного теста).

func mirrorSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"redis_hosts": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "object"},
			},
		},
	}
}

func mirrorFixture() map[string]any {
	return map[string]any{
		"redis_version": "7.2",
		"redis_hosts": []any{
			map[string]any{"sid": "host-a", "role": "primary"},
		},
	}
}

func mirrorOps() []render.RenderedOp {
	add := func(sid, role string) render.RenderedOp {
		return render.RenderedOp{
			Verb: config.VerbAdd, Field: "redis_hosts",
			Value: map[string]any{"sid": sid, "role": role},
			Match: "elem.sid == value.sid", OnConflict: config.OnConflictSkip,
		}
	}
	return []render.RenderedOp{
		{Verb: config.VerbSet, Field: "redis_version", Value: "7.4"},
		add("host-b", "replica"), // новый → растёт
		add("host-a", "primary"), // существующий → no-op
	}
}

// mirrorExpectedJSON — обязан совпадать со scenario.stateMirrorExpectedJSON.
const mirrorExpectedJSON = `{"redis_version":"7.4","redis_hosts":[{"sid":"host-a","role":"primary"},{"sid":"host-b","role":"replica"}]}`

func TestMergeMirror_Trial(t *testing.T) {
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	pl := render.NewPipeline(nil, eng, nil, nil)
	matchEval := pl.EvalStateMatch

	after, err := mergeStateChanges(mirrorFixture(), mirrorOps(), mirrorSchema(), matchEval, pl.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("trial merge: %v", err)
	}
	got, _ := json.Marshal(after)

	var ma, mb map[string]any
	if err := json.Unmarshal(got, &ma); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(mirrorExpectedJSON), &mb); err != nil {
		t.Fatalf("unmarshal expected: %v", err)
	}
	if !reflect.DeepEqual(ma, mb) {
		t.Errorf("★ trial state_after = %s, want %s (дрейф дубля mergeStateChanges)", got, mirrorExpectedJSON)
	}
}

// --- Анти-дрейф для НОВЫХ глаголов modify/remove. Фикстура/операции/ожидание
// ДОЛЖНЫ совпадать байт-в-байт с scenario.TestMergeVerbsMirror_Prod (state_test.go).
// Расхождение тел applyModifyOp/applyRemoveOp/applyPatch разведёт Trial с продом.

func verbsMirrorFixture() map[string]any {
	return map[string]any{
		"redis_users": map[string]any{
			"alice": map[string]any{"acl": "+@read", "state": "on"},
			"bob":   map[string]any{"acl": "+@read", "state": "on"},
		},
		"redis_hosts": []any{
			map[string]any{"sid": "host-a", "role": "primary"},
			map[string]any{"sid": "host-b", "role": "replica"},
			map[string]any{"sid": "host-c", "role": "replica"},
		},
	}
}

func verbsMirrorOps() []render.RenderedOp {
	modifyCtx := map[string]any{"input": map[string]any{"username": "alice", "acl": "+@all"}}
	removeCtx := map[string]any{"input": map[string]any{"sid": "host-c"}}
	return []render.RenderedOp{
		{Verb: config.VerbModify, Field: "redis_users", Match: "key == input.username",
			Patch: map[string]any{"acl": "${ input.acl }"}, Context: modifyCtx},
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == input.sid",
			Expect: config.ExpectOne, Context: removeCtx},
	}
}

// verbsMirrorExpectedJSON — обязан совпадать со scenario.verbsMirrorExpectedJSON.
const verbsMirrorExpectedJSON = `{"redis_users":{"alice":{"acl":"+@all","state":"on"},"bob":{"acl":"+@read","state":"on"}},"redis_hosts":[{"sid":"host-a","role":"primary"},{"sid":"host-b","role":"replica"}]}`

func TestMergeVerbsMirror_Trial(t *testing.T) {
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	pl := render.NewPipeline(nil, eng, nil, nil)

	after, err := mergeStateChanges(verbsMirrorFixture(), verbsMirrorOps(), mirrorSchema(), pl.EvalStateMatch, pl.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("trial merge: %v", err)
	}
	got, _ := json.Marshal(after)

	var ma, mb map[string]any
	if err := json.Unmarshal(got, &ma); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal([]byte(verbsMirrorExpectedJSON), &mb); err != nil {
		t.Fatalf("unmarshal expected: %v", err)
	}
	if !reflect.DeepEqual(ma, mb) {
		t.Errorf("★ trial state_after = %s, want %s (дрейф modify/remove дубля)", got, verbsMirrorExpectedJSON)
	}
}

// TestPatchClobber_Trial — ★ trial-сторона patch-clobber (синхронность с
// scenario.TestMergeStateChanges_PatchClobber_MissingVsExistingScalar): missing
// промежуточный путь материализуется, существующий не-map узел → ошибка. Расхождение
// поведения setNestedPath разведёт Trial с продом.
func TestPatchClobber_Trial(t *testing.T) {
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	pl := render.NewPipeline(nil, eng, nil, nil)
	ctx := map[string]any{"input": map[string]any{"mem": "512mb"}}
	patchOp := render.RenderedOp{Verb: config.VerbModify, Field: "redis_hosts",
		Match: "elem.sid == 'host-a'", Patch: map[string]any{"config.maxmemory": "${ input.mem }"}, Context: ctx}

	// missing → материализуем.
	beforeMissing := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	after, err := mergeStateChanges(beforeMissing, []render.RenderedOp{patchOp}, mirrorSchema(), pl.EvalStateMatch, pl.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("★ trial: missing промежуточный путь должен материализоваться: %v", err)
	}
	cfg := after["redis_hosts"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if cfg["maxmemory"] != "512mb" {
		t.Errorf("★ trial config.maxmemory = %v, want 512mb", cfg["maxmemory"])
	}

	// existing-scalar → ошибка.
	beforeScalar := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary", "config": "some-string-value"},
	}}
	if _, err := mergeStateChanges(beforeScalar, []render.RenderedOp{patchOp}, mirrorSchema(), pl.EvalStateMatch, pl.EvalStateOpExpr); err == nil {
		t.Fatal("★ trial: patch поверх config=\"string\" должен дать ошибку (синхронно с прод-веткой)")
	}
}

// TestSetNestedPath_NoSilentClobber — unit-guard прямо на setNestedPath: missing
// сегмент создаётся, существующий не-map сегмент → ошибка БЕЗ мутации.
func TestSetNestedPath_NoSilentClobber(t *testing.T) {
	// missing → создаём вложенный map.
	m := map[string]any{}
	if err := setNestedPath(m, "config.maxmemory", "256mb"); err != nil {
		t.Fatalf("setNestedPath missing: %v", err)
	}
	if m["config"].(map[string]any)["maxmemory"] != "256mb" {
		t.Errorf("setNestedPath не материализовал config: %+v", m)
	}

	// existing-scalar → ошибка, исходное значение НЕ затёрто.
	m2 := map[string]any{"config": "scalar"}
	if err := setNestedPath(m2, "config.maxmemory", "256mb"); err == nil {
		t.Fatal("★ setNestedPath поверх config=\"scalar\" должен вернуть ошибку, не клоббить")
	}
	if m2["config"] != "scalar" {
		t.Errorf("★ исходное скалярное значение затёрто: %+v (silent-clobber)", m2)
	}
}
