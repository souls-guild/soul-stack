package render

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// TestRenderStateOps_ForeachList_FanOut — ★ foreach по LIST раскрывается в N
// RenderedOp (по одной add-операции на элемент); биндинг `as` → элемент списка
// доступен в value/match (`${ sid }` / `elem == sid`).
func TestRenderStateOps_ForeachList_FanOut(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "add_replicas",
		StateChanges: &config.StateChanges{
			IsList: true,
			Ops: []config.StateChange{{
				Verb: config.VerbForeach, In: "${ input.replicas }", As: "sid",
				Do: []config.StateChange{{
					Verb: config.VerbAdd, Field: "redis_hosts",
					Value: "${ sid }", Match: "elem == sid", OnConflict: config.OnConflictSkip,
				}},
			}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"replicas": []any{"r1.example.com", "r2.example.com", "r3.example.com"}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	ops, err := p.RenderStateOps(in)
	if err != nil {
		t.Fatalf("RenderStateOps: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("★ ops = %d, want 3 (foreach по 3 элементам → 3 add)", len(ops))
	}
	want := []string{"r1.example.com", "r2.example.com", "r3.example.com"}
	for i, op := range ops {
		if op.Verb != config.VerbAdd || op.Field != "redis_hosts" {
			t.Errorf("ops[%d] = %+v, want add redis_hosts", i, op)
		}
		if op.Value != want[i] {
			t.Errorf("ops[%d].Value = %v, want %v (биндинг sid → элемент)", i, op.Value, want[i])
		}
		if op.Match != "elem == sid" {
			t.Errorf("ops[%d].Match = %q (протягивается строкой)", i, op.Match)
		}
	}
}

// TestRenderStateOps_ForeachMap_KeyValueBinding — ★ foreach по MAP: `as`=объект-
// запись {key, value}. Каждая итерация даёт modify-операцию; в её Context биндинг
// `change` несёт .key (имя пользователя) и .value (объект с acl). Порядок
// детерминирован (сортировка ключей map).
func TestRenderStateOps_ForeachMap_KeyValueBinding(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "update_acl",
		StateChanges: &config.StateChanges{
			IsList: true,
			Ops: []config.StateChange{{
				Verb: config.VerbForeach, In: "${ input.changes }", As: "change",
				Do: []config.StateChange{{
					Verb: config.VerbModify, Field: "redis_users",
					Match: "key == change.key",
					Patch: map[string]any{"acl": "${ change.value.acl }"},
				}},
			}},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{"changes": map[string]any{
			"alice": map[string]any{"acl": "+@all"},
			"bob":   map[string]any{"acl": "+@read"},
		}},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a", []string{"svc"}, nil)},
	}
	ops, err := p.RenderStateOps(in)
	if err != nil {
		t.Fatalf("RenderStateOps: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("★ ops = %d, want 2 (foreach по 2 записям map → 2 modify)", len(ops))
	}
	// Порядок детерминирован: alice < bob.
	for i, wantKey := range []string{"alice", "bob"} {
		op := ops[i]
		if op.Verb != config.VerbModify || op.Field != "redis_users" {
			t.Fatalf("ops[%d] = %+v, want modify redis_users", i, op)
		}
		// Биндинг change.* лёг в Context (merge-time резолвит .key/.value).
		change, ok := op.Context["change"].(map[string]any)
		if !ok {
			t.Fatalf("ops[%d].Context[change] = %T, want map {key,value}", i, op.Context["change"])
		}
		if change["key"] != wantKey {
			t.Errorf("ops[%d] change.key = %v, want %v", i, change["key"], wantKey)
		}
		val, ok := change["value"].(map[string]any)
		if !ok || val["acl"] == nil {
			t.Errorf("ops[%d] change.value = %+v, want map с acl", i, change["value"])
		}
	}
}

// TestEvalStateOpExpr_MatchSeesContextAndBinding — modify-match `key ==
// input.username` видит и биндинг элемента (key), и scenario-контекст (input.*).
func TestEvalStateOpExpr_MatchSeesContextAndBinding(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	ctx := map[string]any{"input": map[string]any{"username": "alice"}}

	res, err := p.EvalStateOpExpr("key == input.username", ctx, map[string]any{"key": "alice"}, true)
	if err != nil {
		t.Fatalf("EvalStateOpExpr: %v", err)
	}
	if res != true {
		t.Errorf("match (key==input.username, key=alice) = %v, want true", res)
	}

	res2, _ := p.EvalStateOpExpr("key == input.username", ctx, map[string]any{"key": "bob"}, true)
	if res2 != false {
		t.Errorf("match (key=bob) = %v, want false", res2)
	}

	// patch-значение (boolOut=false) — interpolation, native-тип.
	val, err := p.EvalStateOpExpr("${ input.username }", ctx, nil, false)
	if err != nil {
		t.Fatalf("EvalStateOpExpr patch: %v", err)
	}
	if val != "alice" {
		t.Errorf("patch value = %v, want alice", val)
	}
}

// TestForeachBindings_ListVsMap — форма биндинга: list → элемент; map → {key,value}.
func TestForeachBindings_ListVsMap(t *testing.T) {
	listB, err := foreachBindings("sid", []any{"a", "b"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listB) != 2 || listB[0]["sid"] != "a" || listB[1]["sid"] != "b" {
		t.Errorf("list биндинги = %+v, want sid→элемент", listB)
	}

	mapB, err := foreachBindings("change", map[string]any{"bob": map[string]any{"acl": "x"}, "alice": map[string]any{"acl": "y"}})
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	// Детерминированный порядок: alice < bob.
	first := mapB[0]["change"].(map[string]any)
	if first["key"] != "alice" {
		t.Errorf("map биндинг[0].key = %v, want alice (сортировка ключей)", first["key"])
	}
	if first["value"].(map[string]any)["acl"] != "y" {
		t.Errorf("map биндинг[0].value.acl = %v, want y", first["value"])
	}

	// Скаляр/nil → ошибка (foreach требует коллекцию).
	if _, err := foreachBindings("x", "scalar"); err == nil {
		t.Error("foreach по скаляру должен дать ошибку")
	}
}
