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

// noMatch — stub StateMatchFunc for tests that don't exercise a match
// predicate (set-only / map-add by key). A call means a test bug (we can't
// t.Fatal through a closure, so we return an error that merge propagates).
func noMatch(string, any, any) (bool, error) {
	return false, errInvariant
}

// noOpEval — stub StateOpEvalFunc for tests without modify/remove. A call
// means a test bug (set/add must never invoke opEval).
func noOpEval(string, map[string]any, map[string]any, bool) (any, error) {
	return nil, errInvariant
}

var errInvariant = errors.New("matchEval/opEval не должен вызываться в этом тесте")

// opEvalForTest builds a real render.Pipeline.EvalStateOpExpr (CEL for
// modify/remove match+patch with the full scenario context + element bindings).
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

	// Empty ops → state unchanged (deep-copy).
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

	// Mutating the copy doesn't touch the original (deep-copy, not a reference).
	after["count"] = float64(99)
	if before["count"] != float64(1) {
		t.Errorf("before mutated through copy: %v", before["count"])
	}
}

func TestMergeStateChanges_AppliesSets(t *testing.T) {
	before := map[string]any{"existing": "keep", "count": float64(1)}
	ops := []render.RenderedOp{
		setOp("greeting_file", "/tmp/soul-stack-hello"), // new field
		setOp("count", float64(42)),                     // overwrite existing
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
	// Original untouched.
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

	// nil before + non-empty set → state comes from the set op.
	after, err = mergeStateChanges(nil, []render.RenderedOp{setOp("x", "y")}, nil, noMatch, noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if after["x"] != "y" {
		t.Errorf("after = %+v, want {x:y}", after)
	}
}

// --- Guard tests for the new state_changes grammar (add + on_conflict). The
// pattern is replicated (modify/remove in the next batch), so the cost of a mistake multiplies. ---

// redisHostsSchema — a redis-cluster state_schema fragment (redis_hosts is an
// array). The source of collection-type materialization for add into a
// missing field.
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

// matchEvalForTest builds a real render.Pipeline.EvalStateMatch (CEL elem/value).
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

// TestMergeStateChanges_AddNewSID_Grows — add of a new SID grows redis_hosts
// by 1 (★ closes a latent bug: the old appends form was ignored, redis_hosts
// never grew).
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
	// Original untouched (deep-copy).
	if len(before["redis_hosts"].([]any)) != 1 {
		t.Errorf("before мутирован: redis_hosts len = %d", len(before["redis_hosts"].([]any)))
	}
}

// TestMergeStateChanges_AddExistingSID_Idempotent — ★ MAIN INVARIANT: add of an
// existing SID with on_conflict=skip (default) → NO-OP, length unchanged
// ("add if absent"). Idempotency for a repeated add_replica run.
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

// TestMergeStateChanges_AddExistingSID_ErrorBlocks — on_conflict=error on an
// existing element → error (run.go maps it to error_locked, state NOT committed).
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

// TestMergeStateChanges_AddReplaceExisting — on_conflict=replace overwrites
// the existing element with the new value (length unchanged).
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

// TestMergeStateChanges_AddMaterializesFromSchema — add into a MISSING field:
// the collection materializes with the right type from state_schema
// (redis_hosts: array → list).
func TestMergeStateChanges_AddMaterializesFromSchema(t *testing.T) {
	before := map[string]any{"redis_version": "7.2"} // redis_hosts absent
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

// TestMergeStateChanges_AddMapByKey — add into a map collection by key
// (redis_users): object materialization from schema, idempotency by key.
func TestMergeStateChanges_AddMapByKey(t *testing.T) {
	addUser := func(key string, oc config.OnConflict) render.RenderedOp {
		return render.RenderedOp{
			Verb: config.VerbAdd, Field: "redis_users", Key: key,
			Value: map[string]any{"acl": "+@read", "state": "on"}, OnConflict: oc,
		}
	}
	before := map[string]any{} // redis_users absent

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

	// Repeating the same key (skip) → no-op (map length unchanged).
	after2, err := mergeStateChanges(after, []render.RenderedOp{addUser("alice", config.OnConflictSkip)}, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}
	if len(after2["redis_users"].(map[string]any)) != 1 {
		t.Errorf("повтор key=alice (skip) должен быть no-op, got len=%d", len(after2["redis_users"].(map[string]any)))
	}
}

// --- Guard tests for modify/remove/expect (new verbs, ADR-057). ---

// modifyHostsOp builds a modify op on redis_hosts (list of objects) with a
// precomputed Context (input/vars) for merge-time CEL.
func modifyHostsOp(match string, patch map[string]any, ctx map[string]any, expect config.Expect) render.RenderedOp {
	return render.RenderedOp{
		Verb: config.VerbModify, Field: "redis_hosts",
		Match: match, Patch: patch, Context: ctx, Expect: expect,
	}
}

// TestMergeStateChanges_ModifyAllByPredicate — ★ modify of ALL elements
// matching the predicate (3 replicas role→standby) → all 3 changed, primary untouched.
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
	// Original untouched (deep-copy + per-element copy in applyPatch).
	if before["redis_hosts"].([]any)[1].(map[string]any)["role"] != "replica" {
		t.Errorf("before мутирован")
	}
}

// TestMergeStateChanges_ModifyEmptyMatch_Noop — empty match → no-op (not an error).
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

// TestMergeStateChanges_ModifyNestedPatch — ★ a dotted-path patch (config.x) →
// the nested field is updated, SIBLING fields stay intact (merge, not overwrite).
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

// TestMergeStateChanges_ModifyMapByKey — modify of a map collection
// (redis_users): match sees key/value, patch merges into the entry's value.
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

// TestMergeStateChanges_RemoveAllByPredicate — remove of all matches; others
// untouched. remove with an empty match → no-op.
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

	// empty match (no replicas) → no-op.
	noop, err := mergeStateChanges(after, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("★ remove empty-match должен быть no-op: %v", err)
	}
	if len(noop["redis_hosts"].([]any)) != 1 {
		t.Errorf("empty-match remove изменил коллекцию: %+v", noop["redis_hosts"])
	}
}

// TestMergeStateChanges_RemoveMapByKey — remove from a map collection by a key predicate.
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

// TestMergeStateChanges_ExpectOne — ★ expect: one matching 2 elements → error
// (state NOT committed); matching 1 → ok.
func TestMergeStateChanges_ExpectOne(t *testing.T) {
	twoReplicas := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-b", "role": "replica"},
		map[string]any{"sid": "host-c", "role": "replica"},
	}}
	tooMany := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.role == 'replica'", Expect: config.ExpectOne}
	if _, err := mergeStateChanges(twoReplicas, []render.RenderedOp{tooMany}, redisHostsSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("★ expect: one зацепил 2 — ожидали ошибку (error_locked, state не коммитнут)")
	}

	// Matched exactly one → ok.
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

// TestForeachListAdd_GrowsByN — ★ foreach over a list (add N) end-to-end via
// render→merge: RenderStateOps expands foreach into N adds, mergeStateChanges
// grows the collection by N. Idempotent via on_conflict (a repeat doesn't duplicate).
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

	// Idempotency: repeating the same ops → length doesn't grow (on_conflict: skip).
	again, err := mergeStateChanges(after, ops, schema, p.EvalStateMatch, p.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}
	if len(again["redis_hosts"].([]any)) != 4 {
		t.Errorf("★ повтор foreach-add не идемпотентен: len = %d, want 4", len(again["redis_hosts"].([]any)))
	}
}

// TestForeachMapModify_PerEntryBinding — ★ foreach over a map (modify N
// users): each entry is patched with ITS OWN value (change.key/change.value
// binding), end-to-end render→merge.
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
	// state field intact (patch touches only acl, merge not overwrite).
	if users["alice"].(map[string]any)["state"] != "on" {
		t.Errorf("alice.state затёрт patch-ем: %v", users["alice"])
	}
}

// stateMirrorFixture/stateMirrorOps/stateMirrorExpected — ★ a shared fixture
// for the anti-drift check between prod merge (this test) and trial merge
// (trial.TestMergeMirror_* in diff_test.go). Both sides apply an IDENTICAL
// input against an IDENTICAL expectation; if the mergeStateChanges bodies
// (a duplicate) diverge, one of the two tests fails.
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
		addRedisHost("host-b", "replica", config.OnConflictSkip), // new → grows
		addRedisHost("host-a", "primary", config.OnConflictSkip), // existing → no-op
	}
}

// stateMirrorExpectedJSON — the canonical expected state_after (JSON for a
// deterministic comparison regardless of map key order).
const stateMirrorExpectedJSON = `{"redis_version":"7.4","redis_hosts":[{"sid":"host-a","role":"primary"},{"sid":"host-b","role":"replica"}]}`

// TestMergeStateChanges_MirrorProd — the prod side of the anti-drift check.
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

// verbsMirrorFixture/verbsMirrorOps/verbsMirrorExpectedJSON — ★ anti-drift
// check for the NEW modify/remove verbs (foreach is expanded in render → merge
// receives ready-made add/modify/remove). Duplicated byte-for-byte in
// trial.TestMergeVerbsMirror_* (diff_test.go): a divergence between the
// applyModifyOp/applyRemoveOp bodies would split Trial from prod. The modify
// context (input.*) is precomputed (as the render side does).
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
		// modify map by key: alice.acl → +@all (state intact).
		{Verb: config.VerbModify, Field: "redis_users", Match: "key == input.username",
			Patch: map[string]any{"acl": "${ input.acl }"}, Context: modifyCtx},
		// remove list by sid: host-c removed (expect: one).
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == input.sid",
			Expect: config.ExpectOne, Context: removeCtx},
	}
}

// verbsMirrorExpectedJSON — must match trial.verbsMirrorExpectedJSON.
const verbsMirrorExpectedJSON = `{"redis_users":{"alice":{"acl":"+@all","state":"on"},"bob":{"acl":"+@read","state":"on"}},"redis_hosts":[{"sid":"host-a","role":"primary"},{"sid":"host-b","role":"replica"}]}`

// TestMergeVerbsMirror_Prod — the prod side of the new-verbs anti-drift check.
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

// --- Guard tests for coverage gaps (ADR-057): composition within a block,
// scalar lists, empty collections, patch clobber. ---

// TestMergeStateChanges_Composition_SetThenAdd — ★ set creates a collection,
// and an add into it in the SAME block sees the intermediate state (ops apply
// in order against the intermediate result, ADR-057 §e). Deterministic order.
func TestMergeStateChanges_Composition_SetThenAdd(t *testing.T) {
	before := map[string]any{} // redis_hosts absent
	ops := []render.RenderedOp{
		{Verb: config.VerbSet, Field: "redis_hosts", Value: []any{}}, // create an empty list
		addRedisHost("host-a", "primary", config.OnConflictSkip),     // add sees the created list
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

// TestMergeStateChanges_Composition_AddThenRemove — ★ add X → remove X by match
// within one block: the element ends up absent (remove sees add's result).
// Intermediate state is visible to the following op.
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

// TestMergeStateChanges_ScalarList_ModifyRemove — modify/remove over a list
// of scalars (elem=scalar): remove works by a predicate over the scalar;
// modify (a dotted-path patch) produces a CLEAR error, not a panic.
func TestMergeStateChanges_ScalarList_ModifyRemove(t *testing.T) {
	scalarSchema := map[string]any{"type": "object", "properties": map[string]any{
		"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}}
	before := func() map[string]any {
		return map[string]any{"tags": []any{"a", "b", "c"}}
	}

	// remove over a scalar list by an elem predicate — works.
	rm := render.RenderedOp{Verb: config.VerbRemove, Field: "tags", Match: "elem == 'b'", Context: map[string]any{}}
	after, err := mergeStateChanges(before(), []render.RenderedOp{rm}, scalarSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("remove над scalar-list: %v", err)
	}
	tags := after["tags"].([]any)
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "c" {
		t.Fatalf("★ remove scalar 'b': got %+v, want [a c]", tags)
	}

	// modify (dotted-path patch) over a scalar element — a clear error, not a panic.
	mod := render.RenderedOp{Verb: config.VerbModify, Field: "tags", Match: "elem == 'a'",
		Patch: map[string]any{"x": "${ 'y' }"}, Context: map[string]any{}}
	if _, err := mergeStateChanges(before(), []render.RenderedOp{mod}, scalarSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("★ modify scalar-элемента точечным patch должен дать ошибку (patch применим только к объекту)")
	}
}

// TestMergeStateChanges_RemoveAll_EmptyNotNil — ★ removing ALL elements yields
// an EMPTY collection ([]any{} / map{}), NOT nil: a following add must see
// the empty collection and materialize into it, not crash on nil.
func TestMergeStateChanges_RemoveAll_EmptyNotNil(t *testing.T) {
	// list: remove-all → []any{} (not nil), then add into it grows by 1.
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

	// map: remove-all → map{} (not nil), add by key grows by 1.
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

// TestMergeStateChanges_PatchClobber_MissingVsExistingScalar — ★ QA
// observation: patching the nested path config.maxmemory.
//   - MISSING intermediate path (no config) → materialize a map (ADR-057 §f);
//   - EXISTING non-map intermediate node (config="string") → ERROR, not a
//     silent clobber (data loss is unsafe).
func TestMergeStateChanges_PatchClobber_MissingVsExistingScalar(t *testing.T) {
	ctx := map[string]any{"input": map[string]any{"mem": "512mb"}}
	patchOp := func() render.RenderedOp {
		return render.RenderedOp{Verb: config.VerbModify, Field: "redis_hosts",
			Match: "elem.sid == 'host-a'", Patch: map[string]any{"config.maxmemory": "${ input.mem }"}, Context: ctx}
	}

	// missing → materialize config as a map.
	beforeMissing := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"}, // config absent
	}}
	after, err := mergeStateChanges(beforeMissing, []render.RenderedOp{patchOp()}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("★ missing промежуточный путь должен материализоваться, не ошибка: %v", err)
	}
	cfg := after["redis_hosts"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if cfg["maxmemory"] != "512mb" {
		t.Errorf("★ config.maxmemory = %v, want 512mb (config материализован)", cfg["maxmemory"])
	}

	// existing-scalar → ERROR (config is a string; descending the nested path = clobber).
	beforeScalar := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary", "config": "some-string-value"},
	}}
	if _, err := mergeStateChanges(beforeScalar, []render.RenderedOp{patchOp()}, redisHostsSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("★ patch config.maxmemory поверх config=\"string\" должен дать ошибку (silent-clobber небезопасен), не молча затереть")
	}
}

// TestSetNestedPath_ProdNoSilentClobber — the prod side of the
// setNestedPath unit guard (mirrors trial.TestSetNestedPath_NoSilentClobber):
// missing gets created, an existing non-map → error without mutation.
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

// TestMergeStateChanges_AddConflictReason_NoSecretLeak — ★ BUG-3 (security): add
// into a map with key=a resolved secret + on_conflict:error. The error
// reason (which ends up unmasked in incarnation.status_details.error —
// audit.MaskSecrets only catches `vault:` refs, not plaintext values) must
// NOT contain the key's value — only the collection field name. Same for a
// list add-conflict (resolved value/elem).
func TestMergeStateChanges_AddConflictReason_NoSecretLeak(t *testing.T) {
	const secret = "s3cr3t-vault-resolved-value"

	// map add-conflict: key is already resolved to a secret (as after render `${ vault(...) }`).
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

	// list add-conflict: value carries a secret, the element already exists (deep-equal).
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

// equalJSONState compares two JSON states semantically (key order doesn't matter).
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
		{Index: 1, Register: ""}, // task without register: — its rows are ignored
		{Index: 2, Register: "probe_b"},
	}
	// Correlation by global PlanIndex (ADR-056 §S1 fix Variant B); N=1 →
	// PlanIndex==TaskIdx (single Passage, local==global index).
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "1a"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 2, TaskIdx: 2, RegisterData: map[string]any{"stdout": "1b"}},
		{ApplyID: "a", SID: "host-2", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "2a"}},
		{ApplyID: "a", SID: "host-2", PlanIndex: 1, TaskIdx: 1, RegisterData: map[string]any{"stdout": "ignored"}}, // task without register:
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
	// task_idx=1 without register: must not show up.
	if _, ok := got["host-2"]["probe_b"]; ok {
		t.Errorf("host-2.probe_b не должен существовать (task без register:)")
	}
	if len(got["host-2"]) != 1 {
		t.Errorf("host-2 register-ключей = %d, want 1", len(got["host-2"]))
	}
}

// Variant B: a task's register with NoLog=true is NOT accumulated into the
// per-host map — its register name never lands in nameByIdx, so the row is
// skipped and the sensitive value never reaches the state graph
// (orchestration.md §7).
func TestBuildRegisterByHost_NoLogTaskExcluded(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "plain"},                     // ordinary — accumulated
		{Index: 1, Register: "secret_probe", NoLog: true}, // no_log — NOT accumulated
	}
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "ok"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 1, TaskIdx: 1, RegisterData: map[string]any{"stdout": "s3cr3t"}},
	}

	got := buildRegisterByHost(rows, tasks)

	// Ordinary register is intact (variant B doesn't break non-no_log tasks).
	if v := got["host-1"]["plain"].(map[string]any)["stdout"]; v != "ok" {
		t.Errorf("host-1.plain.stdout = %v, want ok", v)
	}
	// no_log register is absent → a set referencing it gets no-such-key.
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

// TestBuildRegisterByHost_MultiTaskPassage0_NoCollision — ★ GUARD (ADR-056 §S1
// fix Variant B): a latent task_idx collision bug.
//
// Plan: #0 probe-A `register: X` (Passage 0), #1 another task (Passage 0, no
// register), #2 an action `where: register.X` (Passage 1, register: Y). On
// the wire, the Passage-0 ApplyRequest carries #0,#1 (local idx 0,1); the
// Passage-1 ApplyRequest carries #2 (local idx 0). Soul emits
// TaskEvent.task_idx LOCALLY:
//   - probe-A (Passage 0) → task_idx 0, plan_index 0;
//   - action-Y (Passage 1) → task_idx 0 (!), plan_index 2.
//
// Before the fix, correlation went by task_idx → probe-X (task_idx 0) and
// action-Y (task_idx 0) shared a key; ON CONFLICT clobbered probe-X, and
// nameByIdx[t.Index] (global 0) vs rows.TaskIdx (local 0) would happen to
// match on passage 0, BUT the passage-1 action (task_idx 0) would
// clobber/mix up the name. ASSERT after the fix: probe X isn't clobbered AND
// resolves to the correct probe-A value.
func TestBuildRegisterByHost_MultiTaskPassage0_NoCollision(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "X", Passage: 0}, // probe-A, Passage 0
		{Index: 1, Register: "", Passage: 0},  // another task, Passage 0, no register
		{Index: 2, Register: "Y", Passage: 1}, // action, Passage 1
	}
	// Register rows as accumulateRegister writes them: each carries a GLOBAL
	// plan_index (echoing TaskEvent.plan_index) + a LOCAL task_idx (echoing
	// TaskEvent.task_idx). probe-X in Passage 0 sits at local 0; action-Y in
	// Passage 1 is ALSO at local 0 (a different slice) — task_idx collides,
	// plan_index doesn't.
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, Passage: 0, RegisterData: map[string]any{"stdout": "probe-A-value"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 2, TaskIdx: 0, Passage: 1, RegisterData: map[string]any{"stdout": "action-Y-value"}},
	}

	got := buildRegisterByHost(rows, tasks)

	// ★ probe-register X is NOT clobbered by action-Y (the task_idx=0 collision didn't merge them).
	x, ok := got["host-1"]["X"]
	if !ok {
		t.Fatalf("★ register X отсутствует — probe-register затёрт коллизией task_idx (баг)")
	}
	if v := x.(map[string]any)["stdout"]; v != "probe-A-value" {
		t.Errorf("★ register X.stdout = %v, want probe-A-value (имя резолвится по глобальному plan_index)", v)
	}
	// Y resolves to its own value (plan_index 2 → Index 2 → name Y).
	if v := got["host-1"]["Y"].(map[string]any)["stdout"]; v != "action-Y-value" {
		t.Errorf("register Y.stdout = %v, want action-Y-value", v)
	}
	if len(got["host-1"]) != 2 {
		t.Errorf("host-1 register-ключей = %d, want 2 (X и Y)", len(got["host-1"]))
	}
}

// TestBuildRegisterByHost_PerHostDifferentWhere_NoMismatch — ★ GUARD (ADR-056
// §S1 fix Variant B): a per-host different where: within one Passage gives
// the register task a DIFFERENT LOCAL task_idx on different hosts, but
// correlation by global plan_index resolves both correctly.
//
// Scenario: Passage 0 carries #0 (where: host-A only) + #1 probe `R` (both
// hosts). On host-A the slice = [#0, #1] → probe R at local 1; on host-B the
// slice = [#1] (since #0 is filtered by where) → probe R at local 0. R's
// task_idx differs (1 vs 0), plan_index is the same (1) on both. ASSERT:
// both hosts' register R resolves to R.
func TestBuildRegisterByHost_PerHostDifferentWhere_NoMismatch(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "", Passage: 0},  // where: host-A only
		{Index: 1, Register: "R", Passage: 0}, // probe — both hosts
	}
	rows := []applyrun.TaskRegister{
		// host-A: R at local 1 (slice [#0,#1]); plan_index 1.
		{ApplyID: "a", SID: "host-A", PlanIndex: 1, TaskIdx: 1, Passage: 0, RegisterData: map[string]any{"stdout": "A-R"}},
		// host-B: R at local 0 (slice [#1], #0 filtered by where); plan_index 1.
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

	// No facts → "".
	if got := osFamilyOf(&topology.HostFacts{}); got != "" {
		t.Errorf("osFamilyOf(empty) = %q, want \"\"", got)
	}
	// os present, family absent.
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

	// No spec.essence → nil.
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

// TestKeeperRegisterBucket_FromRegisterByHost — ★ GUARD for Slice 1
// (keeper→keeper register-chaining, staged-render). The gap Slice 1 closes:
// keeper tasks accumulate register under the synthetic host KeeperTargetSID
// ("keeper") in the run's per-host table (accumulateKeeperRegister),
// buildRegisterByHost puts it in RegisterByHost["keeper"], but keeperVars
// (render/dispatch.go) reads register ONLY from the FLAT in.Register.
// keeperRegisterBucket is the bridge: it pulls the keeper bucket of previous
// Passages into flat form, so the stage-loop in run.go can place it into
// renderIn.Register before per-passage render of the active Passage's keeper
// tasks.
//
// The guard scenario replicates the stage-loop's input at P>0: register from
// two Passages in the per-host table — a keeper task (under KeeperTargetSID)
// + a host task (under a normal SID). buildRegisterByHost resolves both by
// PlanIndex (as loadRegisterByHostUpToPassage does), and
// keeperRegisterBucket extracts exactly the keeper bucket. This is the unit
// form of the Slice 1 guard; the end-to-end 2-passage chain
// (bootstrap.delivered sees register.provision.*) is covered by the Slice 2
// guard (per-passage keeper-dispatch).
func TestKeeperRegisterBucket_FromRegisterByHost(t *testing.T) {
	// Plan: Passage 0 — a keeper task (on: keeper, register: provision),
	// Passage 1 — a host task (register: probe). Index is stable across
	// Passages (same plan).
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "provision"}, // keeper task — accumulates under KeeperTargetSID
		{Index: 1, Register: "probe"},     // host task — accumulates under a normal SID
	}
	// The run's register rows (as SelectTaskRegistersByApplyIDUpToPassage
	// would return them): a keeper register under KeeperTargetSID + a host
	// register under host-1, correlated by global PlanIndex.
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: render.KeeperTargetSID, PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"ip": "10.0.0.5"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 1, TaskIdx: 1, RegisterData: map[string]any{"stdout": "master"}},
	}

	reg := buildRegisterByHost(rows, tasks)

	bucket := keeperRegisterBucket(reg)
	if bucket == nil {
		t.Fatal("keeperRegisterBucket вернул nil — keeper-register предыдущего Passage потерян (дыра Слайса 1)")
	}
	// keeper register is available in flat form under the name provision →
	// keeperVars will see register.provision.* on the active Passage's keeper task.
	prov, ok := bucket["provision"].(map[string]any)
	if !ok {
		t.Fatalf("bucket[provision] = %T, want map (register keeper-задачи)", bucket["provision"])
	}
	if prov["ip"] != "10.0.0.5" {
		t.Errorf("bucket[provision].ip = %v, want 10.0.0.5", prov["ip"])
	}
	// The host register (probe under host-1) does NOT end up in the keeper
	// bucket: the flattening into Register carries ONLY keeper tasks, the
	// host bucket stays in RegisterByHost[sid] (the host path reads it
	// per-host, not from the flat Register).
	if _, leaked := bucket["probe"]; leaked {
		t.Error("bucket[probe] присутствует — host-register протёк в keeper-bucket")
	}
}

// TestKeeperRegisterBucket_NoKeeperRegister_Nil — a host-only Passage (no
// keeper register): keeperRegisterBucket → nil, the stage-loop leaves the
// flat Register UNTOUCHED (bit-for-bit: on P>0 with no keeper tasks, the flat
// Register stays empty, as before Slice 1). This guarantees the flattening
// doesn't widen visibility for host tasks whose per-host map is empty (the
// fallback hostRegister stays empty as before).
func TestKeeperRegisterBucket_NoKeeperRegister_Nil(t *testing.T) {
	tasks := []*render.RenderedTask{{Index: 0, Register: "probe"}}
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "x"}},
	}
	reg := buildRegisterByHost(rows, tasks)
	if bucket := keeperRegisterBucket(reg); bucket != nil {
		t.Errorf("keeperRegisterBucket = %v, want nil (нет register под KeeperTargetSID)", bucket)
	}
	// Empty/nil map → nil.
	if bucket := keeperRegisterBucket(nil); bucket != nil {
		t.Errorf("keeperRegisterBucket(nil) = %v, want nil", bucket)
	}
	if bucket := keeperRegisterBucket(map[string]map[string]any{}); bucket != nil {
		t.Errorf("keeperRegisterBucket(empty) = %v, want nil", bucket)
	}
}
