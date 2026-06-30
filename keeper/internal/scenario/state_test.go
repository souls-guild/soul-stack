package scenario

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// noMatch — заглушка StateMatchFunc для тестов, не использующих match-предикат
// (set-only / map-add по key). Вызов = тестовый баг (помечаем t.Fatal через
// замыкание не получится — возвращаем ошибку, которую merge пробросит).
func noMatch(string, any, any) (bool, error) {
	return false, errInvariant
}

// noOpEval — заглушка StateOpEvalFunc для тестов без modify/remove. Вызов =
// тестовый баг (set/add не должны звать opEval).
func noOpEval(string, map[string]any, map[string]any, bool) (any, error) {
	return nil, errInvariant
}

var errInvariant = errors.New("matchEval/opEval не должен вызываться в этом тесте")

// opEvalForTest строит реальный render.Pipeline.EvalStateOpExpr (CEL для
// modify/remove match+patch с полным scenario-контекстом + биндингами элемента).
func opEvalForTest(t *testing.T) render.StateOpEvalFunc {
	t.Helper()
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return render.NewPipeline(nil, eng, nil, nil).EvalStateOpExpr
}

func setOp(field string, val any) render.RenderedOp {
	return render.RenderedOp{Verb: config.VerbSet, Field: field, Value: val}
}

func TestMergeStateChanges_EmptyNoop(t *testing.T) {
	before := map[string]any{"users": []any{"alice"}, "count": float64(1)}

	// Пустой ops → state не меняется (deep-copy).
	after, err := mergeStateChanges(before, nil, nil, noMatch, noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("after = %+v, want copy of before", after)
	}
	if after["count"] != float64(1) {
		t.Errorf("count = %v", after["count"])
	}

	// Мутация копии не задевает оригинал (deep-copy, не ссылка).
	after["count"] = float64(99)
	if before["count"] != float64(1) {
		t.Errorf("before mutated through copy: %v", before["count"])
	}
}

func TestMergeStateChanges_AppliesSets(t *testing.T) {
	before := map[string]any{"existing": "keep", "count": float64(1)}
	ops := []render.RenderedOp{
		setOp("greeting_file", "/tmp/soul-stack-hello"), // новое поле
		setOp("count", float64(42)),                     // перезапись существующего
	}
	after, err := mergeStateChanges(before, ops, nil, noMatch, noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	if after["greeting_file"] != "/tmp/soul-stack-hello" {
		t.Errorf("greeting_file = %v, want /tmp/soul-stack-hello", after["greeting_file"])
	}
	if after["count"] != float64(42) {
		t.Errorf("count = %v, want 42 (set overrides)", after["count"])
	}
	if after["existing"] != "keep" {
		t.Errorf("existing = %v, want keep (untouched fields preserved)", after["existing"])
	}
	// Оригинал не задет.
	if before["count"] != float64(1) {
		t.Errorf("before mutated: count = %v", before["count"])
	}
	if _, ok := before["greeting_file"]; ok {
		t.Errorf("before mutated: greeting_file leaked into stateBefore")
	}
}

func TestMergeStateChanges_NilBefore(t *testing.T) {
	after, err := mergeStateChanges(nil, nil, nil, noMatch, noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if after == nil {
		t.Fatal("after = nil, want empty map")
	}
	if len(after) != 0 {
		t.Errorf("after = %+v, want empty", after)
	}

	// nil before + непустой set → state из set-операции.
	after, err = mergeStateChanges(nil, []render.RenderedOp{setOp("x", "y")}, nil, noMatch, noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if after["x"] != "y" {
		t.Errorf("after = %+v, want {x:y}", after)
	}
}

// --- Guard-тесты новой грамматики state_changes (add + on_conflict). Паттерн
// тиражируется (modify/remove следующим батчем), цена ошибки умножается. ---

// redisHostsSchema — state_schema redis-cluster (фрагмент: redis_hosts — array).
// Источник материализации типа коллекции для add в отсутствующее поле.
var redisHostsSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"redis_hosts": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "object"},
		},
		"redis_users": map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object"},
		},
	},
}

// matchEvalForTest строит реальный render.Pipeline.EvalStateMatch (CEL elem/value).
func matchEvalForTest(t *testing.T) render.StateMatchFunc {
	t.Helper()
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	return render.NewPipeline(nil, eng, nil, nil).EvalStateMatch
}

func addRedisHost(sid, role string, onConflict config.OnConflict) render.RenderedOp {
	return render.RenderedOp{
		Verb:       config.VerbAdd,
		Field:      "redis_hosts",
		Value:      map[string]any{"sid": sid, "role": role},
		Match:      "elem.sid == value.sid",
		OnConflict: onConflict,
	}
}

// TestMergeStateChanges_AddNewSID_Grows — add нового SID растит redis_hosts на 1
// (★ закрытие латентного бага: старая appends-форма игнорировалась, redis_hosts
// не рос).
func TestMergeStateChanges_AddNewSID_Grows(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "replica", config.OnConflictSkip)}

	after, err := mergeStateChanges(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("redis_hosts len = %d, want 2 (add нового sid растит коллекцию)", len(hosts))
	}
	newHost := hosts[1].(map[string]any)
	if newHost["sid"] != "host-b" || newHost["role"] != "replica" {
		t.Errorf("новый элемент = %+v, want {sid:host-b, role:replica}", newHost)
	}
	// Оригинал не задет (deep-copy).
	if len(before["redis_hosts"].([]any)) != 1 {
		t.Errorf("before мутирован: redis_hosts len = %d", len(before["redis_hosts"].([]any)))
	}
}

// TestMergeStateChanges_AddExistingSID_Idempotent — ★ ГЛАВНЫЙ ИНВАРИАНТ: add
// существующего SID при on_conflict=skip (default) → NO-OP, длина не меняется
// («добавляет если нет»). Идемпотентность повторного прогона add_replica.
func TestMergeStateChanges_AddExistingSID_Idempotent(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "replica", config.OnConflictSkip)}

	after, err := mergeStateChanges(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("★ redis_hosts len = %d, want 2 (повтор существующего sid = NO-OP, on_conflict=skip)", len(hosts))
	}
}

// TestMergeStateChanges_AddExistingSID_ErrorBlocks — on_conflict=error на
// существующем → ошибка (run.go переведёт в error_locked, state НЕ коммитнут).
func TestMergeStateChanges_AddExistingSID_ErrorBlocks(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "replica", config.OnConflictError)}

	_, err := mergeStateChanges(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err == nil {
		t.Fatal("★ ожидали ошибку (on_conflict=error на существующем) — state не должен коммититься")
	}
}

// TestMergeStateChanges_AddReplaceExisting — on_conflict=replace перезаписывает
// существующий элемент новым value (длина не меняется).
func TestMergeStateChanges_AddReplaceExisting(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "primary", config.OnConflictReplace)}

	after, err := mergeStateChanges(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("redis_hosts len = %d, want 1 (replace не растит)", len(hosts))
	}
	if hosts[0].(map[string]any)["role"] != "primary" {
		t.Errorf("элемент не перезаписан: %+v", hosts[0])
	}
}

// TestMergeStateChanges_AddMaterializesFromSchema — add в ОТСУТСТВУЮЩЕЕ поле:
// коллекция материализуется нужного типа из state_schema (redis_hosts: array → list).
func TestMergeStateChanges_AddMaterializesFromSchema(t *testing.T) {
	before := map[string]any{"redis_version": "7.2"} // redis_hosts отсутствует
	ops := []render.RenderedOp{addRedisHost("host-a", "primary", config.OnConflictSkip)}

	after, err := mergeStateChanges(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts, ok := after["redis_hosts"].([]any)
	if !ok {
		t.Fatalf("redis_hosts = %T, want []any (материализован list из schema)", after["redis_hosts"])
	}
	if len(hosts) != 1 {
		t.Fatalf("redis_hosts len = %d, want 1", len(hosts))
	}
}

// TestMergeStateChanges_AddMapByKey — add в map-коллекцию по key (redis_users):
// материализация object из schema, идемпотентность по key.
func TestMergeStateChanges_AddMapByKey(t *testing.T) {
	addUser := func(key string, oc config.OnConflict) render.RenderedOp {
		return render.RenderedOp{
			Verb: config.VerbAdd, Field: "redis_users", Key: key,
			Value: map[string]any{"acl": "+@read", "state": "on"}, OnConflict: oc,
		}
	}
	before := map[string]any{} // redis_users отсутствует

	after, err := mergeStateChanges(before, []render.RenderedOp{addUser("alice", config.OnConflictSkip)}, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users, ok := after["redis_users"].(map[string]any)
	if !ok {
		t.Fatalf("redis_users = %T, want map (материализован object из schema)", after["redis_users"])
	}
	if _, has := users["alice"]; !has {
		t.Fatal("ключ alice не добавлен")
	}

	// Повтор того же key (skip) → no-op (длина map не меняется).
	after2, err := mergeStateChanges(after, []render.RenderedOp{addUser("alice", config.OnConflictSkip)}, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}
	if len(after2["redis_users"].(map[string]any)) != 1 {
		t.Errorf("повтор key=alice (skip) должен быть no-op, got len=%d", len(after2["redis_users"].(map[string]any)))
	}
}

// --- Guard-тесты modify/remove/expect (новые глаголы ADR-057). ---

// modifyHostsOp строит modify-операцию по redis_hosts (list of objects) с
// предвычисленным Context (input/vars) для merge-time CEL.
func modifyHostsOp(match string, patch map[string]any, ctx map[string]any, expect config.Expect) render.RenderedOp {
	return render.RenderedOp{
		Verb: config.VerbModify, Field: "redis_hosts",
		Match: match, Patch: patch, Context: ctx, Expect: expect,
	}
}

// TestMergeStateChanges_ModifyAllByPredicate — ★ modify ВСЕХ подходящих под
// предикат (3 реплики role→standby) → все 3 изменены, primary цел.
func TestMergeStateChanges_ModifyAllByPredicate(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
		map[string]any{"sid": "host-b", "role": "replica"},
		map[string]any{"sid": "host-c", "role": "replica"},
		map[string]any{"sid": "host-d", "role": "replica"},
	}}
	op := modifyHostsOp("elem.role == 'replica'", map[string]any{"role": "${ 'standby' }"}, nil, "")

	after, err := mergeStateChanges(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	standby := 0
	for _, h := range hosts {
		if h.(map[string]any)["role"] == "standby" {
			standby++
		}
	}
	if standby != 3 {
		t.Fatalf("★ standby = %d, want 3 (все реплики пропатчены)", standby)
	}
	if hosts[0].(map[string]any)["role"] != "primary" {
		t.Errorf("primary задет: %+v (не подходил под предикат)", hosts[0])
	}
	// Оригинал не задет (deep-copy + per-element copy в applyPatch).
	if before["redis_hosts"].([]any)[1].(map[string]any)["role"] != "replica" {
		t.Errorf("before мутирован")
	}
}

// TestMergeStateChanges_ModifyEmptyMatch_Noop — empty-match → no-op (не ошибка).
func TestMergeStateChanges_ModifyEmptyMatch_Noop(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	op := modifyHostsOp("elem.role == 'replica'", map[string]any{"role": "${ 'standby' }"}, nil, "")

	after, err := mergeStateChanges(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("★ empty-match modify должен быть no-op, не ошибка: %v", err)
	}
	if after["redis_hosts"].([]any)[0].(map[string]any)["role"] != "primary" {
		t.Errorf("no-op нарушен: %+v", after["redis_hosts"])
	}
}

// TestMergeStateChanges_ModifyNestedPatch — ★ patch точечного пути (config.x) →
// вложенное поле обновлено, СОСЕДНИЕ поля записи целы (merge, не перезапись).
func TestMergeStateChanges_ModifyNestedPatch(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{
			"sid": "host-a", "role": "primary",
			"config": map[string]any{"maxmemory": "256mb", "appendonly": "yes"},
		},
	}}
	ctx := map[string]any{"input": map[string]any{"mem": "512mb"}}
	op := modifyHostsOp("elem.sid == 'host-a'",
		map[string]any{"config.maxmemory": "${ input.mem }"}, ctx, "")

	after, err := mergeStateChanges(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	host := after["redis_hosts"].([]any)[0].(map[string]any)
	cfg := host["config"].(map[string]any)
	if cfg["maxmemory"] != "512mb" {
		t.Fatalf("★ config.maxmemory = %v, want 512mb (вложенное поле обновлено)", cfg["maxmemory"])
	}
	if cfg["appendonly"] != "yes" {
		t.Errorf("★ config.appendonly = %v, want yes (соседнее поле затёрто — patch перезаписал запись целиком)", cfg["appendonly"])
	}
	if host["role"] != "primary" {
		t.Errorf("top-level role затёрт: %+v", host)
	}
}

// TestMergeStateChanges_ModifyMapByKey — modify map-коллекции (redis_users):
// match видит key/value, patch мержится в значение записи.
func TestMergeStateChanges_ModifyMapByKey(t *testing.T) {
	before := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read", "state": "on"},
		"bob":   map[string]any{"acl": "+@read", "state": "on"},
	}}
	ctx := map[string]any{"input": map[string]any{"username": "alice", "acl": "+@all", "state": "off"}}
	op := render.RenderedOp{
		Verb: config.VerbModify, Field: "redis_users",
		Match:   "key == input.username",
		Patch:   map[string]any{"acl": "${ input.acl }", "state": "${ input.state }"},
		Context: ctx,
	}
	after, err := mergeStateChanges(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users := after["redis_users"].(map[string]any)
	alice := users["alice"].(map[string]any)
	if alice["acl"] != "+@all" || alice["state"] != "off" {
		t.Errorf("alice не пропатчен: %+v", alice)
	}
	if users["bob"].(map[string]any)["acl"] != "+@read" {
		t.Errorf("bob задет (не подходил под key == input.username): %+v", users["bob"])
	}
}

// TestMergeStateChanges_RemoveAllByPredicate — remove всех подходящих; прочие
// целы. remove empty-match → no-op.
func TestMergeStateChanges_RemoveAllByPredicate(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
		map[string]any{"sid": "host-b", "role": "replica"},
		map[string]any{"sid": "host-c", "role": "replica"},
	}}
	op := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.role == 'replica'"}

	after, err := mergeStateChanges(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 1 || hosts[0].(map[string]any)["sid"] != "host-a" {
		t.Fatalf("★ remove реплик: осталось %+v, want [host-a]", hosts)
	}

	// empty-match (нет реплик) → no-op.
	noop, err := mergeStateChanges(after, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("★ remove empty-match должен быть no-op: %v", err)
	}
	if len(noop["redis_hosts"].([]any)) != 1 {
		t.Errorf("empty-match remove изменил коллекцию: %+v", noop["redis_hosts"])
	}
}

// TestMergeStateChanges_RemoveMapByKey — remove из map-коллекции по предикату key.
func TestMergeStateChanges_RemoveMapByKey(t *testing.T) {
	before := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read"},
		"bob":   map[string]any{"acl": "+@read"},
	}}
	ctx := map[string]any{"input": map[string]any{"username": "bob"}}
	op := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_users", Match: "key == input.username", Context: ctx}

	after, err := mergeStateChanges(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users := after["redis_users"].(map[string]any)
	if _, ok := users["bob"]; ok {
		t.Errorf("bob не удалён: %+v", users)
	}
	if _, ok := users["alice"]; !ok {
		t.Errorf("alice удалён ошибочно: %+v", users)
	}
}

// TestMergeStateChanges_ExpectOne — ★ expect: one зацепил 2 → ошибка (state НЕ
// коммитнут); зацепил 1 → ок.
func TestMergeStateChanges_ExpectOne(t *testing.T) {
	twoReplicas := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-b", "role": "replica"},
		map[string]any{"sid": "host-c", "role": "replica"},
	}}
	tooMany := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.role == 'replica'", Expect: config.ExpectOne}
	if _, err := mergeStateChanges(twoReplicas, []render.RenderedOp{tooMany}, redisHostsSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("★ expect: one зацепил 2 — ожидали ошибку (error_locked, state не коммитнут)")
	}

	// Зацепил ровно один → ок.
	ctx := map[string]any{"input": map[string]any{"sid": "host-b"}}
	one := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == input.sid", Expect: config.ExpectOne, Context: ctx}
	after, err := mergeStateChanges(twoReplicas, []render.RenderedOp{one}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("★ expect: one зацепил 1 должен быть ок: %v", err)
	}
	if len(after["redis_hosts"].([]any)) != 1 {
		t.Errorf("после remove одного осталось %+v", after["redis_hosts"])
	}
}

// TestForeachListAdd_GrowsByN — ★ foreach по list (add N) end-to-end через
// render→merge: RenderStateOps раскрывает foreach в N add, mergeStateChanges
// растит коллекцию на N. Идемпотентно по on_conflict (повтор не дублирует).
func TestForeachListAdd_GrowsByN(t *testing.T) {
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
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := render.NewPipeline(nil, eng, nil, nil)
	in := render.RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"replicas": []any{"r1", "r2", "r3"}},
		Incarnation: render.IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{{SID: "a", Coven: []string{"svc"}}},
	}
	ops, err := p.RenderStateOps(in)
	if err != nil {
		t.Fatalf("RenderStateOps: %v", err)
	}

	// list of scalars schema.
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"redis_hosts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}}
	before := map[string]any{"redis_hosts": []any{"r0"}}

	after, err := mergeStateChanges(before, ops, schema, p.EvalStateMatch, p.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 4 {
		t.Fatalf("★ redis_hosts len = %d, want 4 (r0 + 3 foreach-add)", len(hosts))
	}

	// Идемпотентность: повтор тех же ops → длина не растёт (on_conflict: skip).
	again, err := mergeStateChanges(after, ops, schema, p.EvalStateMatch, p.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}
	if len(again["redis_hosts"].([]any)) != 4 {
		t.Errorf("★ повтор foreach-add не идемпотентен: len = %d, want 4", len(again["redis_hosts"].([]any)))
	}
}

// TestForeachMapModify_PerEntryBinding — ★ foreach по map (modify N юзеров):
// каждая запись пропатчена СВОИМ значением (биндинг change.key/change.value),
// end-to-end render→merge.
func TestForeachMapModify_PerEntryBinding(t *testing.T) {
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
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := render.NewPipeline(nil, eng, nil, nil)
	in := render.RenderInput{
		Scenario: manifest,
		Input: map[string]any{"changes": map[string]any{
			"alice": map[string]any{"acl": "+@all"},
			"bob":   map[string]any{"acl": "+@write"},
		}},
		Incarnation: render.IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{{SID: "a", Coven: []string{"svc"}}},
	}
	ops, err := p.RenderStateOps(in)
	if err != nil {
		t.Fatalf("RenderStateOps: %v", err)
	}

	before := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read", "state": "on"},
		"bob":   map[string]any{"acl": "+@read", "state": "on"},
		"carol": map[string]any{"acl": "+@read", "state": "on"},
	}}
	after, err := mergeStateChanges(before, ops, redisHostsSchema, p.EvalStateMatch, p.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users := after["redis_users"].(map[string]any)
	if users["alice"].(map[string]any)["acl"] != "+@all" {
		t.Errorf("★ alice.acl = %v, want +@all (свой биндинг)", users["alice"])
	}
	if users["bob"].(map[string]any)["acl"] != "+@write" {
		t.Errorf("★ bob.acl = %v, want +@write (свой биндинг)", users["bob"])
	}
	if users["carol"].(map[string]any)["acl"] != "+@read" {
		t.Errorf("carol задет (не во input.changes): %v", users["carol"])
	}
	// state-поле цело (patch только acl, merge не перезапись записи).
	if users["alice"].(map[string]any)["state"] != "on" {
		t.Errorf("alice.state затёрт patch-ем: %v", users["alice"])
	}
}

// stateMirrorFixture/stateMirrorOps/stateMirrorExpected — ★ общая фикстура для
// анти-дрейф-сверки прод-merge (этот тест) и trial-merge (trial.TestMergeMirror_*
// в diff_test.go). Обе стороны применяют ИДЕНТИЧНЫЙ вход к ИДЕНТИЧНОМУ ожиданию;
// если тела mergeStateChanges разойдутся (дубль), один из тестов упадёт.
func stateMirrorFixture() map[string]any {
	return map[string]any{
		"redis_version": "7.2",
		"redis_hosts": []any{
			map[string]any{"sid": "host-a", "role": "primary"},
		},
	}
}

func stateMirrorOps() []render.RenderedOp {
	return []render.RenderedOp{
		{Verb: config.VerbSet, Field: "redis_version", Value: "7.4"},
		addRedisHost("host-b", "replica", config.OnConflictSkip), // новый → растёт
		addRedisHost("host-a", "primary", config.OnConflictSkip), // существующий → no-op
	}
}

// stateMirrorExpectedJSON — канонический ожидаемый state_after (JSON для
// детерминированной сверки независимо от порядка map-ключей).
const stateMirrorExpectedJSON = `{"redis_version":"7.4","redis_hosts":[{"sid":"host-a","role":"primary"},{"sid":"host-b","role":"replica"}]}`

// TestMergeStateChanges_MirrorProd — прод-сторона анти-дрейф-сверки.
func TestMergeStateChanges_MirrorProd(t *testing.T) {
	after, err := mergeStateChanges(stateMirrorFixture(), stateMirrorOps(), redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := json.Marshal(after)
	if !equalJSONState(t, string(got), stateMirrorExpectedJSON) {
		t.Errorf("★ прод state_after = %s, want %s", got, stateMirrorExpectedJSON)
	}
}

// verbsMirrorFixture/verbsMirrorOps/verbsMirrorExpectedJSON — ★ анти-дрейф-сверка
// для НОВЫХ глаголов modify/remove (foreach раскрыт в render → в merge приходят
// готовые add/modify/remove). Дублируется байт-в-байт в trial.TestMergeVerbsMirror_*
// (diff_test.go): расхождение тел applyModifyOp/applyRemoveOp разведёт Trial с
// продом. Контекст modify (input.*) предвычислен (как делает render-сторона).
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
		// modify map по key: alice.acl → +@all (state цел).
		{Verb: config.VerbModify, Field: "redis_users", Match: "key == input.username",
			Patch: map[string]any{"acl": "${ input.acl }"}, Context: modifyCtx},
		// remove list по sid: host-c удалён (expect: one).
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == input.sid",
			Expect: config.ExpectOne, Context: removeCtx},
	}
}

// verbsMirrorExpectedJSON — обязан совпадать с trial.verbsMirrorExpectedJSON.
const verbsMirrorExpectedJSON = `{"redis_users":{"alice":{"acl":"+@all","state":"on"},"bob":{"acl":"+@read","state":"on"}},"redis_hosts":[{"sid":"host-a","role":"primary"},{"sid":"host-b","role":"replica"}]}`

// TestMergeVerbsMirror_Prod — прод-сторона анти-дрейф-сверки новых глаголов.
func TestMergeVerbsMirror_Prod(t *testing.T) {
	after, err := mergeStateChanges(verbsMirrorFixture(), verbsMirrorOps(), redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := json.Marshal(after)
	if !equalJSONState(t, string(got), verbsMirrorExpectedJSON) {
		t.Errorf("★ прод state_after = %s, want %s (дрейф modify/remove)", got, verbsMirrorExpectedJSON)
	}
}

// --- Guard-тесты пробелов покрытия (ADR-057): композиция в блоке, scalar-list,
// пустые коллекции, patch-clobber. ---

// TestMergeStateChanges_Composition_SetThenAdd — ★ set создаёт коллекцию, add в
// неё в ТОМ ЖЕ блоке видит промежуточный state (ops применяются по порядку к
// промежуточному результату, ADR-057 §e). Детерминированный порядок.
func TestMergeStateChanges_Composition_SetThenAdd(t *testing.T) {
	before := map[string]any{} // redis_hosts отсутствует
	ops := []render.RenderedOp{
		{Verb: config.VerbSet, Field: "redis_hosts", Value: []any{}}, // создаём пустой list
		addRedisHost("host-a", "primary", config.OnConflictSkip),     // add видит созданный list
		addRedisHost("host-b", "replica", config.OnConflictSkip),
	}
	after, err := mergeStateChanges(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("★ redis_hosts len = %d, want 2 (add в созданную set-ом коллекцию)", len(hosts))
	}
	if hosts[0].(map[string]any)["sid"] != "host-a" || hosts[1].(map[string]any)["sid"] != "host-b" {
		t.Errorf("★ порядок add нарушен: %+v", hosts)
	}
}

// TestMergeStateChanges_Composition_AddThenRemove — ★ add X → remove X по match в
// одном блоке: элемента в итоге нет (remove видит результат add). Промежуточный
// state виден последующей операции.
func TestMergeStateChanges_Composition_AddThenRemove(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	removeCtx := map[string]any{}
	ops := []render.RenderedOp{
		addRedisHost("host-b", "replica", config.OnConflictSkip),                                           // +host-b
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == 'host-b'", Context: removeCtx}, // -host-b
	}
	after, err := mergeStateChanges(before, ops, redisHostsSchema, matchEvalForTest(t), opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 1 || hosts[0].(map[string]any)["sid"] != "host-a" {
		t.Fatalf("★ add X → remove X: ожидали только host-a, got %+v", hosts)
	}
}

// TestMergeStateChanges_ScalarList_ModifyRemove — modify/remove над list of
// scalars (elem=скаляр): remove работает по предикату над скаляром; modify
// (точечный patch) даёт ПОНЯТНУЮ ошибку, не панику.
func TestMergeStateChanges_ScalarList_ModifyRemove(t *testing.T) {
	scalarSchema := map[string]any{"type": "object", "properties": map[string]any{
		"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}}
	before := func() map[string]any {
		return map[string]any{"tags": []any{"a", "b", "c"}}
	}

	// remove над scalar-list по предикату elem — работает.
	rm := render.RenderedOp{Verb: config.VerbRemove, Field: "tags", Match: "elem == 'b'", Context: map[string]any{}}
	after, err := mergeStateChanges(before(), []render.RenderedOp{rm}, scalarSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("remove над scalar-list: %v", err)
	}
	tags := after["tags"].([]any)
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "c" {
		t.Fatalf("★ remove scalar 'b': got %+v, want [a c]", tags)
	}

	// modify (точечный patch) над scalar-элементом — понятная ошибка, не паника.
	mod := render.RenderedOp{Verb: config.VerbModify, Field: "tags", Match: "elem == 'a'",
		Patch: map[string]any{"x": "${ 'y' }"}, Context: map[string]any{}}
	if _, err := mergeStateChanges(before(), []render.RenderedOp{mod}, scalarSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("★ modify scalar-элемента точечным patch должен дать ошибку (patch применим только к объекту)")
	}
}

// TestMergeStateChanges_RemoveAll_EmptyNotNil — ★ remove ВСЕХ элементов даёт
// ПУСТУЮ коллекцию ([]any{} / map{}), НЕ nil: следующий add должен видеть пустую
// коллекцию и материализовать в неё, не упасть на nil.
func TestMergeStateChanges_RemoveAll_EmptyNotNil(t *testing.T) {
	// list: remove всех → []any{} (не nil), затем add в неё растит на 1.
	beforeList := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "replica"},
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.role == 'replica'", Context: map[string]any{}},
		addRedisHost("host-c", "primary", config.OnConflictSkip),
	}
	after, err := mergeStateChanges(beforeList, ops, redisHostsSchema, matchEvalForTest(t), opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts, ok := after["redis_hosts"].([]any)
	if !ok {
		t.Fatalf("★ redis_hosts = %T, want []any (remove-всех оставляет пустой list, не nil)", after["redis_hosts"])
	}
	if len(hosts) != 1 || hosts[0].(map[string]any)["sid"] != "host-c" {
		t.Fatalf("★ add после remove-всех: got %+v, want [host-c]", hosts)
	}

	// map: remove всех → map{} (не nil), add по key растит на 1.
	beforeMap := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read"},
	}}
	opsMap := []render.RenderedOp{
		{Verb: config.VerbRemove, Field: "redis_users", Match: "true == true", Context: map[string]any{}},
		{Verb: config.VerbAdd, Field: "redis_users", Key: "bob",
			Value: map[string]any{"acl": "+@all"}, OnConflict: config.OnConflictSkip},
	}
	afterMap, err := mergeStateChanges(beforeMap, opsMap, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge map: %v", err)
	}
	users, ok := afterMap["redis_users"].(map[string]any)
	if !ok {
		t.Fatalf("★ redis_users = %T, want map (remove-всех оставляет пустой map, не nil)", afterMap["redis_users"])
	}
	if len(users) != 1 {
		t.Fatalf("★ add после remove-всех map: got %+v, want {bob}", users)
	}
	if _, has := users["bob"]; !has {
		t.Errorf("★ bob не добавлен в опустевший map: %+v", users)
	}
}

// TestMergeStateChanges_PatchClobber_MissingVsExistingScalar — ★ QA observation:
// patch вложенного пути config.maxmemory.
//   - ОТСУТСТВУЮЩИЙ промежуточный путь (config нет) → материализуем map (ADR-057 §f);
//   - СУЩЕСТВУЮЩИЙ не-map промежуточный узел (config="string") → ERROR, не молчаливый
//     клоббер (потеря данных небезопасна).
func TestMergeStateChanges_PatchClobber_MissingVsExistingScalar(t *testing.T) {
	ctx := map[string]any{"input": map[string]any{"mem": "512mb"}}
	patchOp := func() render.RenderedOp {
		return render.RenderedOp{Verb: config.VerbModify, Field: "redis_hosts",
			Match: "elem.sid == 'host-a'", Patch: map[string]any{"config.maxmemory": "${ input.mem }"}, Context: ctx}
	}

	// missing → материализуем config как map.
	beforeMissing := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"}, // config отсутствует
	}}
	after, err := mergeStateChanges(beforeMissing, []render.RenderedOp{patchOp()}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("★ missing промежуточный путь должен материализоваться, не ошибка: %v", err)
	}
	cfg := after["redis_hosts"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if cfg["maxmemory"] != "512mb" {
		t.Errorf("★ config.maxmemory = %v, want 512mb (config материализован)", cfg["maxmemory"])
	}

	// existing-scalar → ERROR (config — строка, спустить вложенный путь = клоббер).
	beforeScalar := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary", "config": "some-string-value"},
	}}
	if _, err := mergeStateChanges(beforeScalar, []render.RenderedOp{patchOp()}, redisHostsSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("★ patch config.maxmemory поверх config=\"string\" должен дать ошибку (silent-clobber небезопасен), не молча затереть")
	}
}

// TestSetNestedPath_ProdNoSilentClobber — прод-сторона unit-guard setNestedPath
// (зеркало trial.TestSetNestedPath_NoSilentClobber): missing создаётся, существующий
// не-map → ошибка без мутации.
func TestSetNestedPath_ProdNoSilentClobber(t *testing.T) {
	m := map[string]any{}
	if err := setNestedPath(m, "config.maxmemory", "256mb"); err != nil {
		t.Fatalf("setNestedPath missing: %v", err)
	}
	if m["config"].(map[string]any)["maxmemory"] != "256mb" {
		t.Errorf("config не материализован: %+v", m)
	}
	m2 := map[string]any{"config": "scalar"}
	if err := setNestedPath(m2, "config.maxmemory", "256mb"); err == nil {
		t.Fatal("★ setNestedPath поверх config=\"scalar\" должен вернуть ошибку")
	}
	if m2["config"] != "scalar" {
		t.Errorf("★ скалярное значение затёрто: %+v", m2)
	}
}

// TestMergeStateChanges_AddConflictReason_NoSecretLeak — ★ BUG-3 (security): add в
// map с key=зарезолвленный-секрет + on_conflict:error. Reason ошибки (уезжает в
// incarnation.status_details.error немаскированным — audit.MaskSecrets ловит
// `vault:`-ref, не plaintext-значение) НЕ должен содержать значение ключа: только
// имя коллекции-поля. То же для list add-conflict (зарезолвленный value/elem).
func TestMergeStateChanges_AddConflictReason_NoSecretLeak(t *testing.T) {
	const secret = "s3cr3t-vault-resolved-value"

	// map add-conflict: key уже зарезолвлен в секрет (как после render `${ vault(...) }`).
	beforeMap := map[string]any{"redis_users": map[string]any{
		secret: map[string]any{"acl": "+@read"},
	}}
	mapOp := render.RenderedOp{Verb: config.VerbAdd, Field: "redis_users", Key: secret,
		Value: map[string]any{"acl": "+@all"}, OnConflict: config.OnConflictError}
	_, err := mergeStateChanges(beforeMap, []render.RenderedOp{mapOp}, redisHostsSchema, noMatch, noOpEval)
	if err == nil {
		t.Fatal("ожидали ошибку (on_conflict=error на существующем key)")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("★ secret-LEAK: reason содержит plaintext ключа: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redis_users") {
		t.Errorf("reason должен называть поле redis_users: %q", err.Error())
	}

	// list add-conflict: value несёт секрет, элемент уже есть (deep-equal).
	beforeList := map[string]any{"redis_hosts": []any{secret}}
	listOp := render.RenderedOp{Verb: config.VerbAdd, Field: "redis_hosts",
		Value: secret, OnConflict: config.OnConflictError}
	_, err = mergeStateChanges(beforeList, []render.RenderedOp{listOp}, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err == nil {
		t.Fatal("ожидали ошибку (on_conflict=error на существующем элементе)")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("★ secret-LEAK: list-reason содержит plaintext value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redis_hosts") {
		t.Errorf("list-reason должен называть поле redis_hosts: %q", err.Error())
	}
}

// equalJSONState сравнивает два JSON-стейта семантически (порядок ключей не важен).
func equalJSONState(t *testing.T, a, b string) bool {
	t.Helper()
	var ma, mb map[string]any
	if err := json.Unmarshal([]byte(a), &ma); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &mb); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	return reflect.DeepEqual(ma, mb)
}

func TestBuildRegisterByHost_ResolvesNamesPerHost(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "probe_a"},
		{Index: 1, Register: ""}, // задача без register: — её строки игнорируются
		{Index: 2, Register: "probe_b"},
	}
	// Корреляция по глобальному PlanIndex (ADR-056 §S1 fix Variant B); N=1 →
	// PlanIndex==TaskIdx (один Passage, локальный==глобальный индекс).
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "1a"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 2, TaskIdx: 2, RegisterData: map[string]any{"stdout": "1b"}},
		{ApplyID: "a", SID: "host-2", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "2a"}},
		{ApplyID: "a", SID: "host-2", PlanIndex: 1, TaskIdx: 1, RegisterData: map[string]any{"stdout": "ignored"}}, // task без register:
	}

	got := buildRegisterByHost(rows, tasks)

	if len(got) != 2 {
		t.Fatalf("hosts = %d, want 2", len(got))
	}
	if v := got["host-1"]["probe_a"].(map[string]any)["stdout"]; v != "1a" {
		t.Errorf("host-1.probe_a.stdout = %v, want 1a", v)
	}
	if v := got["host-1"]["probe_b"].(map[string]any)["stdout"]; v != "1b" {
		t.Errorf("host-1.probe_b.stdout = %v, want 1b", v)
	}
	if v := got["host-2"]["probe_a"].(map[string]any)["stdout"]; v != "2a" {
		t.Errorf("host-2.probe_a.stdout = %v, want 2a", v)
	}
	// task_idx=1 без register: не должен попасть.
	if _, ok := got["host-2"]["probe_b"]; ok {
		t.Errorf("host-2.probe_b не должен существовать (task без register:)")
	}
	if len(got["host-2"]) != 1 {
		t.Errorf("host-2 register-ключей = %d, want 1", len(got["host-2"]))
	}
}

// Вариант B: register задачи с NoLog=true НЕ аккумулируется в per-host map —
// его register-имя не попадает в nameByIdx, поэтому строка пропускается и
// чувствительное значение не доходит до state-графа (orchestration.md §7).
func TestBuildRegisterByHost_NoLogTaskExcluded(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "plain"},                     // обычная — аккумулируется
		{Index: 1, Register: "secret_probe", NoLog: true}, // no_log — НЕ аккумулируется
	}
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "ok"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 1, TaskIdx: 1, RegisterData: map[string]any{"stdout": "s3cr3t"}},
	}

	got := buildRegisterByHost(rows, tasks)

	// Обычный register на месте (вариант B не ломает не-no_log задачи).
	if v := got["host-1"]["plain"].(map[string]any)["stdout"]; v != "ok" {
		t.Errorf("host-1.plain.stdout = %v, want ok", v)
	}
	// no_log-register отсутствует → sets, ссылающийся на него, получит no-such-key.
	if _, ok := got["host-1"]["secret_probe"]; ok {
		t.Errorf("host-1.secret_probe не должен существовать (no_log-задача)")
	}
	if len(got["host-1"]) != 1 {
		t.Errorf("host-1 register-ключей = %d, want 1 (только plain)", len(got["host-1"]))
	}
}

func TestBuildRegisterByHost_EmptyRows(t *testing.T) {
	got := buildRegisterByHost(nil, []*render.RenderedTask{{Index: 0, Register: "p"}})
	if got == nil || len(got) != 0 {
		t.Errorf("got = %v, want пустая map", got)
	}
}

// TestBuildRegisterByHost_MultiTaskPassage0_NoCollision — ★ GUARD (ADR-056 §S1 fix
// Variant B): латентный баг task_idx-коллизии.
//
// Plan: #0 probe-A `register: X` (Passage 0), #1 ещё одна задача (Passage 0, без
// register), #2 действие `where: register.X` (Passage 1, register: Y). На проводе
// Passage-0 ApplyRequest несёт #0,#1 (локальные idx 0,1); Passage-1 ApplyRequest
// несёт #2 (локальный idx 0). Soul эмитит TaskEvent.task_idx ЛОКАЛЬНО:
//   - probe-A (Passage 0) → task_idx 0, plan_index 0;
//   - действие-Y (Passage 1) → task_idx 0 (!), plan_index 2.
//
// До фикса корреляция шла по task_idx → probe-X (task_idx 0) и действие-Y
// (task_idx 0) делили ключ; ON CONFLICT затирал probe-X, а nameByIdx[t.Index]
// (глобальный 0) vs rows.TaskIdx (локальный 0) случайно совпали бы на passage 0,
// НО действие на passage 1 (task_idx 0) затёрло/перепутало бы имя. ASSERT после
// фикса: probe X не затёрт И резолвится в правильное значение probe-A.
func TestBuildRegisterByHost_MultiTaskPassage0_NoCollision(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "X", Passage: 0}, // probe-A, Passage 0
		{Index: 1, Register: "", Passage: 0},  // ещё задача, Passage 0, без register
		{Index: 2, Register: "Y", Passage: 1}, // действие, Passage 1
	}
	// Register-строки, как их пишет accumulateRegister: каждая несёт ГЛОБАЛЬНЫЙ
	// plan_index (эхо TaskEvent.plan_index) + ЛОКАЛЬНЫЙ task_idx (эхо
	// TaskEvent.task_idx). probe-X в Passage 0 на локальной 0; действие-Y в Passage
	// 1 ТОЖЕ на локальной 0 (другой срез) — task_idx коллидирует, plan_index — нет.
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, Passage: 0, RegisterData: map[string]any{"stdout": "probe-A-value"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 2, TaskIdx: 0, Passage: 1, RegisterData: map[string]any{"stdout": "action-Y-value"}},
	}

	got := buildRegisterByHost(rows, tasks)

	// ★ probe-register X НЕ затёрт действием-Y (коллизия task_idx=0 не схлопнула их).
	x, ok := got["host-1"]["X"]
	if !ok {
		t.Fatalf("★ register X отсутствует — probe-register затёрт коллизией task_idx (баг)")
	}
	if v := x.(map[string]any)["stdout"]; v != "probe-A-value" {
		t.Errorf("★ register X.stdout = %v, want probe-A-value (имя резолвится по глобальному plan_index)", v)
	}
	// Y резолвится в своё значение (plan_index 2 → Index 2 → имя Y).
	if v := got["host-1"]["Y"].(map[string]any)["stdout"]; v != "action-Y-value" {
		t.Errorf("register Y.stdout = %v, want action-Y-value", v)
	}
	if len(got["host-1"]) != 2 {
		t.Errorf("host-1 register-ключей = %d, want 2 (X и Y)", len(got["host-1"]))
	}
}

// TestBuildRegisterByHost_PerHostDifferentWhere_NoMismatch — ★ GUARD (ADR-056 §S1
// fix Variant B): per-host разный where: в одном Passage даёт register-задаче РАЗНЫЙ
// ЛОКАЛЬНЫЙ task_idx на разных хостах, но корреляция по глобальному plan_index
// резолвит обоих верно.
//
// Сценарий: Passage 0 несёт #0 (where: только host-A) + #1 probe `R` (оба хоста).
// На host-A срез = [#0, #1] → probe R на локальной 1; на host-B срез = [#1] (т.к.
// #0 отфильтрован where) → probe R на локальной 0. task_idx у R разный (1 vs 0),
// plan_index одинаковый (1) на обоих. ASSERT: register R обоих резолвится в R.
func TestBuildRegisterByHost_PerHostDifferentWhere_NoMismatch(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "", Passage: 0},  // where: только host-A
		{Index: 1, Register: "R", Passage: 0}, // probe — оба хоста
	}
	rows := []applyrun.TaskRegister{
		// host-A: R на локальной 1 (срез [#0,#1]); plan_index 1.
		{ApplyID: "a", SID: "host-A", PlanIndex: 1, TaskIdx: 1, Passage: 0, RegisterData: map[string]any{"stdout": "A-R"}},
		// host-B: R на локальной 0 (срез [#1], #0 отфильтрован where); plan_index 1.
		{ApplyID: "a", SID: "host-B", PlanIndex: 1, TaskIdx: 0, Passage: 0, RegisterData: map[string]any{"stdout": "B-R"}},
	}

	got := buildRegisterByHost(rows, tasks)

	if v := got["host-A"]["R"].(map[string]any)["stdout"]; v != "A-R" {
		t.Errorf("★ host-A.R.stdout = %v, want A-R (локальный task_idx=1)", v)
	}
	if v := got["host-B"]["R"].(map[string]any)["stdout"]; v != "B-R" {
		t.Errorf("★ host-B.R.stdout = %v, want B-R (локальный task_idx=0, тот же plan_index=1)", v)
	}
}

func TestDeepCopyMap(t *testing.T) {
	src := map[string]any{
		"nested": map[string]any{"k": "v"},
		"list":   []any{float64(1), float64(2)},
	}
	cp := deepCopyMap(src)
	nested := cp["nested"].(map[string]any)
	nested["k"] = "changed"
	if src["nested"].(map[string]any)["k"] != "v" {
		t.Errorf("deep copy не глубокая: original mutated")
	}
}

func TestOSFamilyOf(t *testing.T) {
	h := &topology.HostFacts{Soulprint: map[string]any{
		"os": map[string]any{"family": "debian"},
	}}
	if got := osFamilyOf(h); got != "debian" {
		t.Errorf("osFamilyOf = %q, want debian", got)
	}

	// Нет фактов → "".
	if got := osFamilyOf(&topology.HostFacts{}); got != "" {
		t.Errorf("osFamilyOf(empty) = %q, want \"\"", got)
	}
	// os есть, family нет.
	h2 := &topology.HostFacts{Soulprint: map[string]any{"os": map[string]any{}}}
	if got := osFamilyOf(h2); got != "" {
		t.Errorf("osFamilyOf(no family) = %q", got)
	}
}

func TestSpecEssence(t *testing.T) {
	inc := &incarnation.Incarnation{Spec: map[string]any{
		"essence": map[string]any{"redis_version": "7.2"},
	}}
	got := specEssence(inc)
	if got["redis_version"] != "7.2" {
		t.Errorf("specEssence = %+v", got)
	}

	// Нет spec.essence → nil.
	if specEssence(&incarnation.Incarnation{}) != nil {
		t.Errorf("specEssence(empty) != nil")
	}
}

func TestStartedByPtr(t *testing.T) {
	if startedByPtr("") != nil {
		t.Errorf("startedByPtr(\"\") != nil")
	}
	p := startedByPtr("archon-alice")
	if p == nil || *p != "archon-alice" {
		t.Errorf("startedByPtr = %v", p)
	}
}

// TestKeeperRegisterBucket_FromRegisterByHost — ★ GUARD Слайса 1 (keeper→keeper
// register-chaining, staged-render). Дыра, которую закрывает Слайс 1: keeper-задачи
// копят register под синтетическим хостом KeeperTargetSID ("keeper") в per-host
// таблицу прогона (accumulateKeeperRegister), buildRegisterByHost кладёт его в
// RegisterByHost["keeper"], но keeperVars (render/dispatch.go) читает register
// ТОЛЬКО из ПЛОСКОЙ in.Register. keeperRegisterBucket — мост: достаёт keeper-bucket
// предыдущих Passage в плоскую форму, чтобы stage-loop run.go положил его в
// renderIn.Register перед per-passage render-ом keeper-задач активного Passage.
//
// Сценарий guard-а воспроизводит вход stage-loop на P>0: register двух Passage в
// per-host таблице — keeper-задача (под KeeperTargetSID) + host-задача (под обычным
// SID). buildRegisterByHost резолвит обе по PlanIndex (как loadRegisterByHostUpToPassage),
// keeperRegisterBucket выделяет ровно keeper-bucket. Это unit-форма guard-а Слайса 1;
// end-to-end 2-passage цепочку (bootstrap.delivered видит register.provision.*)
// проверяет guard Слайса 2 (per-passage keeper-dispatch).
func TestKeeperRegisterBucket_FromRegisterByHost(t *testing.T) {
	// План: Passage 0 — keeper-задача (on: keeper, register: provision), Passage 1 —
	// host-задача (register: probe). Index стабилен между Passage (тот же план).
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "provision"}, // keeper-задача — копит под KeeperTargetSID
		{Index: 1, Register: "probe"},     // host-задача — копит под обычным SID
	}
	// Register-строки прогона (как их вернёт SelectTaskRegistersByApplyIDUpToPassage):
	// keeper-register под KeeperTargetSID + host-register под host-1, корреляция по
	// глобальному PlanIndex.
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: render.KeeperTargetSID, PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"ip": "10.0.0.5"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 1, TaskIdx: 1, RegisterData: map[string]any{"stdout": "master"}},
	}

	reg := buildRegisterByHost(rows, tasks)

	bucket := keeperRegisterBucket(reg)
	if bucket == nil {
		t.Fatal("keeperRegisterBucket вернул nil — keeper-register предыдущего Passage потерян (дыра Слайса 1)")
	}
	// keeper-register доступен в плоской форме под именем provision → keeperVars
	// увидит register.provision.* у keeper-задачи активного Passage.
	prov, ok := bucket["provision"].(map[string]any)
	if !ok {
		t.Fatalf("bucket[provision] = %T, want map (register keeper-задачи)", bucket["provision"])
	}
	if prov["ip"] != "10.0.0.5" {
		t.Errorf("bucket[provision].ip = %v, want 10.0.0.5", prov["ip"])
	}
	// host-register (probe под host-1) в keeper-bucket НЕ попадает: проброс в плоскую
	// Register несёт ТОЛЬКО keeper-задачи, host-bucket остаётся в RegisterByHost[sid]
	// (host-путь читает его per-host, не из плоской Register).
	if _, leaked := bucket["probe"]; leaked {
		t.Error("bucket[probe] присутствует — host-register протёк в keeper-bucket")
	}
}

// TestKeeperRegisterBucket_NoKeeperRegister_Nil — host-only Passage (нет keeper-
// register): keeperRegisterBucket → nil, stage-loop плоскую Register НЕ трогает
// (БИТ-В-БИТ: на P>0 без keeper-задач плоская Register остаётся пустой, как до
// Слайса 1). Это гарантирует, что проброс не расширяет видимость host-задач, у
// которых per-host карта пуста (fallback hostRegister остаётся прежним пустым).
func TestKeeperRegisterBucket_NoKeeperRegister_Nil(t *testing.T) {
	tasks := []*render.RenderedTask{{Index: 0, Register: "probe"}}
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "x"}},
	}
	reg := buildRegisterByHost(rows, tasks)
	if bucket := keeperRegisterBucket(reg); bucket != nil {
		t.Errorf("keeperRegisterBucket = %v, want nil (нет register под KeeperTargetSID)", bucket)
	}
	// Пустая/nil карта → nil.
	if bucket := keeperRegisterBucket(nil); bucket != nil {
		t.Errorf("keeperRegisterBucket(nil) = %v, want nil", bucket)
	}
	if bucket := keeperRegisterBucket(map[string]map[string]any{}); bucket != nil {
		t.Errorf("keeperRegisterBucket(empty) = %v, want nil", bucket)
	}
}
