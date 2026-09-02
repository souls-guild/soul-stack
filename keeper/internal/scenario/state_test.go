package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/stateop"
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
func noOpEval(string, map[string]any, bool) (any, error) {
	return nil, errInvariant
}

var errInvariant = errors.New("matchEval/opEval must not be called in this test")

// opEvalForTest builds a real merge-time opEval (CEL for modify/remove
// match+patch with the full scenario context + element bindings).
func opEvalForTest(t *testing.T) render.StateOpEvalFunc {
	t.Helper()
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	_, opEval := render.NewPipeline(nil, eng, nil, nil).StateOpEvaluators(context.Background(), "")
	return opEval
}

func setOp(field string, val any) render.RenderedOp {
	return render.RenderedOp{Verb: config.VerbSet, Field: field, Value: val}
}

func TestMergeOps_EmptyNoop(t *testing.T) {
	before := map[string]any{"users": []any{"alice"}, "count": float64(1)}

	// Empty ops → state unchanged (deep-copy).
	after, err := stateop.Merge(before, nil, nil, noMatch, noOpEval)
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

func TestMergeOps_AppliesSets(t *testing.T) {
	before := map[string]any{"existing": "keep", "count": float64(1)}
	ops := []render.RenderedOp{
		setOp("greeting_file", "/tmp/soul-stack-hello"), // new field
		setOp("count", float64(42)),                     // overwrite existing
	}
	after, err := stateop.Merge(before, ops, nil, noMatch, noOpEval)
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

func TestMergeOps_NilBefore(t *testing.T) {
	after, err := stateop.Merge(nil, nil, nil, noMatch, noOpEval)
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
	after, err = stateop.Merge(nil, []render.RenderedOp{setOp("x", "y")}, nil, noMatch, noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if after["x"] != "y" {
		t.Errorf("after = %+v, want {x:y}", after)
	}
}

// --- Guard tests for the collection verbs (add + on_conflict). The pattern is
// replicated (modify/remove in the next batch), so the cost of a mistake
// multiplies. ---

// redisHostsSchema — a redis-cluster state_schema fragment (redis_hosts is an
// array). The source of collection-type materialization for add into a
// missing field.
var redisHostsSchema = config.InputSchemaMap{
	"redis_hosts": &config.InputSchema{
		Type:  "array",
		Items: &config.InputSchema{Type: "object"},
	},
	"redis_users": &config.InputSchema{
		Type:                 "object",
		AdditionalProperties: &config.InputSchema{Type: "object"},
	},
}

// matchEvalForTest builds a real merge-time matchEval (CEL elem/value).
func matchEvalForTest(t *testing.T) render.StateMatchFunc {
	t.Helper()
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	matchEval, _ := render.NewPipeline(nil, eng, nil, nil).StateOpEvaluators(context.Background(), "")
	return matchEval
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

// TestMergeOps_AddNewSID_Grows — add of a new SID grows redis_hosts
// by 1 (★ closes a latent bug: the old appends form was ignored, redis_hosts
// never grew).
func TestMergeOps_AddNewSID_Grows(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "replica", config.OnConflictSkip)}

	after, err := stateop.Merge(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("redis_hosts len = %d, want 2 (add of a new sid grows the collection)", len(hosts))
	}
	newHost := hosts[1].(map[string]any)
	if newHost["sid"] != "host-b" || newHost["role"] != "replica" {
		t.Errorf("new element = %+v, want {sid:host-b, role:replica}", newHost)
	}
	// Original untouched (deep-copy).
	if len(before["redis_hosts"].([]any)) != 1 {
		t.Errorf("before mutated: redis_hosts len = %d", len(before["redis_hosts"].([]any)))
	}
}

// TestMergeOps_AddExistingSID_Idempotent — ★ MAIN INVARIANT: add of an
// existing SID with on_conflict=skip (default) → NO-OP, length unchanged
// ("add if absent"). Idempotency for a repeated add_replica run.
func TestMergeOps_AddExistingSID_Idempotent(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "replica", config.OnConflictSkip)}

	after, err := stateop.Merge(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("* redis_hosts len = %d, want 2 (repeat of an existing sid = NO-OP, on_conflict=skip)", len(hosts))
	}
}

// TestMergeOps_AddExistingSID_ErrorBlocks — on_conflict=error on an
// existing element → error (run.go maps it to error_locked, state NOT committed).
func TestMergeOps_AddExistingSID_ErrorBlocks(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "replica", config.OnConflictError)}

	_, err := stateop.Merge(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err == nil {
		t.Fatal("* expected an error (on_conflict=error on existing) - state must not be committed")
	}
}

// TestMergeOps_AddReplaceExisting — on_conflict=replace overwrites
// the existing element with the new value (length unchanged).
func TestMergeOps_AddReplaceExisting(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{addRedisHost("host-b", "primary", config.OnConflictReplace)}

	after, err := stateop.Merge(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("redis_hosts len = %d, want 1 (replace does not grow)", len(hosts))
	}
	if hosts[0].(map[string]any)["role"] != "primary" {
		t.Errorf("element not overwritten: %+v", hosts[0])
	}
}

// TestMergeOps_AddMaterializesFromSchema — add into a MISSING field:
// the collection materializes with the right type from state_schema
// (redis_hosts: array → list).
func TestMergeOps_AddMaterializesFromSchema(t *testing.T) {
	before := map[string]any{"redis_version": "7.2"} // redis_hosts absent
	ops := []render.RenderedOp{addRedisHost("host-a", "primary", config.OnConflictSkip)}

	after, err := stateop.Merge(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts, ok := after["redis_hosts"].([]any)
	if !ok {
		t.Fatalf("redis_hosts = %T, want []any (list materialized from schema)", after["redis_hosts"])
	}
	if len(hosts) != 1 {
		t.Fatalf("redis_hosts len = %d, want 1", len(hosts))
	}
}

// TestMergeOps_AddMapByKey — add into a map collection by key
// (redis_users): object materialization from schema, idempotency by key.
func TestMergeOps_AddMapByKey(t *testing.T) {
	addUser := func(key string, oc config.OnConflict) render.RenderedOp {
		return render.RenderedOp{
			Verb: config.VerbAdd, Field: "redis_users", Key: key,
			Value: map[string]any{"acl": "+@read", "state": "on"}, OnConflict: oc,
		}
	}
	before := map[string]any{} // redis_users absent

	after, err := stateop.Merge(before, []render.RenderedOp{addUser("alice", config.OnConflictSkip)}, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users, ok := after["redis_users"].(map[string]any)
	if !ok {
		t.Fatalf("redis_users = %T, want map (object materialized from schema)", after["redis_users"])
	}
	if _, has := users["alice"]; !has {
		t.Fatal("key alice was not added")
	}

	// Repeating the same key (skip) → no-op (map length unchanged).
	after2, err := stateop.Merge(after, []render.RenderedOp{addUser("alice", config.OnConflictSkip)}, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}
	if len(after2["redis_users"].(map[string]any)) != 1 {
		t.Errorf("repeat key=alice (skip) should be a no-op, got len=%d", len(after2["redis_users"].(map[string]any)))
	}
}

// --- Guard tests for modify/remove/expect (new verbs, ADR-057). ---

// modifyHostsOp builds a modify op on redis_hosts (list of objects). Under
// [ADR-0084] match/patch are ordinary module params, so by merge time they are
// literals — there is no run context left to precompute.
func modifyHostsOp(match string, patch map[string]any, expect config.Expect) render.RenderedOp {
	return render.RenderedOp{
		Verb: config.VerbModify, Field: "redis_hosts",
		Match: match, Patch: patch, Expect: expect,
	}
}

// TestMergeOps_ModifyAllByPredicate — ★ modify of ALL elements
// matching the predicate (3 replicas role→standby) → all 3 changed, primary untouched.
func TestMergeOps_ModifyAllByPredicate(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
		map[string]any{"sid": "host-b", "role": "replica"},
		map[string]any{"sid": "host-c", "role": "replica"},
		map[string]any{"sid": "host-d", "role": "replica"},
	}}
	op := modifyHostsOp("elem.role == 'replica'", map[string]any{"role": "${ 'standby' }"}, "")

	after, err := stateop.Merge(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
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
		t.Fatalf("* standby = %d, want 3 (all replicas patched)", standby)
	}
	if hosts[0].(map[string]any)["role"] != "primary" {
		t.Errorf("primary affected: %+v (did not match the predicate)", hosts[0])
	}
	// Original untouched (deep-copy + per-element copy in applyPatch).
	if before["redis_hosts"].([]any)[1].(map[string]any)["role"] != "replica" {
		t.Errorf("before mutated")
	}
}

// TestMergeOps_ModifyEmptyMatch_Noop — empty match → no-op (not an error).
func TestMergeOps_ModifyEmptyMatch_Noop(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	op := modifyHostsOp("elem.role == 'replica'", map[string]any{"role": "${ 'standby' }"}, "")

	after, err := stateop.Merge(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("* empty-match modify should be a no-op, not an error: %v", err)
	}
	if after["redis_hosts"].([]any)[0].(map[string]any)["role"] != "primary" {
		t.Errorf("no-op violated: %+v", after["redis_hosts"])
	}
}

// TestMergeOps_ModifyNestedPatch — ★ a dotted-path patch (config.x) →
// the nested field is updated, SIBLING fields stay intact (merge, not overwrite).
func TestMergeOps_ModifyNestedPatch(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{
			"sid": "host-a", "role": "primary",
			"config": map[string]any{"maxmemory": "256mb", "appendonly": "yes"},
		},
	}}
	op := modifyHostsOp("elem.sid == 'host-a'",
		map[string]any{"config.maxmemory": "${ '512mb' }"}, "")

	after, err := stateop.Merge(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	host := after["redis_hosts"].([]any)[0].(map[string]any)
	cfg := host["config"].(map[string]any)
	if cfg["maxmemory"] != "512mb" {
		t.Fatalf("* config.maxmemory = %v, want 512mb (nested field updated)", cfg["maxmemory"])
	}
	if cfg["appendonly"] != "yes" {
		t.Errorf("* config.appendonly = %v, want yes (sibling field clobbered - patch overwrote the whole entry)", cfg["appendonly"])
	}
	if host["role"] != "primary" {
		t.Errorf("top-level role clobbered: %+v", host)
	}
}

// TestMergeOps_ModifyMapByKey — modify of a map collection
// (redis_users): match sees key/value, patch merges into the entry's value.
func TestMergeOps_ModifyMapByKey(t *testing.T) {
	before := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read", "state": "on"},
		"bob":   map[string]any{"acl": "+@read", "state": "on"},
	}}
	// The literals are what render leaves behind after interpolating
	// `${ input.* }` into the task's params ([ADR-0084]).
	op := render.RenderedOp{
		Verb: config.VerbModify, Field: "redis_users",
		Match: "key == 'alice'",
		Patch: map[string]any{"acl": "${ '+@all' }", "state": "${ 'off' }"},
	}
	after, err := stateop.Merge(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users := after["redis_users"].(map[string]any)
	alice := users["alice"].(map[string]any)
	if alice["acl"] != "+@all" || alice["state"] != "off" {
		t.Errorf("alice was not patched: %+v", alice)
	}
	if users["bob"].(map[string]any)["acl"] != "+@read" {
		t.Errorf("bob affected (did not match key == 'alice'): %+v", users["bob"])
	}
}

// TestMergeOps_RemoveAllByPredicate — remove of all matches; others
// untouched. remove with an empty match → no-op.
func TestMergeOps_RemoveAllByPredicate(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
		map[string]any{"sid": "host-b", "role": "replica"},
		map[string]any{"sid": "host-c", "role": "replica"},
	}}
	op := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.role == 'replica'"}

	after, err := stateop.Merge(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 1 || hosts[0].(map[string]any)["sid"] != "host-a" {
		t.Fatalf("* remove replicas: left %+v, want [host-a]", hosts)
	}

	// empty match (no replicas) → no-op.
	noop, err := stateop.Merge(after, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("* remove empty-match should be a no-op: %v", err)
	}
	if len(noop["redis_hosts"].([]any)) != 1 {
		t.Errorf("empty-match remove changed the collection: %+v", noop["redis_hosts"])
	}
}

// TestMergeOps_RemoveMapByKey — remove from a map collection by a key predicate.
func TestMergeOps_RemoveMapByKey(t *testing.T) {
	before := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read"},
		"bob":   map[string]any{"acl": "+@read"},
	}}
	op := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_users", Match: "key == 'bob'"}

	after, err := stateop.Merge(before, []render.RenderedOp{op}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users := after["redis_users"].(map[string]any)
	if _, ok := users["bob"]; ok {
		t.Errorf("bob was not removed: %+v", users)
	}
	if _, ok := users["alice"]; !ok {
		t.Errorf("alice was removed erroneously: %+v", users)
	}
}

// TestMergeOps_ExpectOne — ★ expect: one matching 2 elements → error
// (state NOT committed); matching 1 → ok.
func TestMergeOps_ExpectOne(t *testing.T) {
	twoReplicas := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-b", "role": "replica"},
		map[string]any{"sid": "host-c", "role": "replica"},
	}}
	tooMany := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.role == 'replica'", Expect: config.ExpectOne}
	if _, err := stateop.Merge(twoReplicas, []render.RenderedOp{tooMany}, redisHostsSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("* expect: one matched 2 - expected an error (error_locked, state not committed)")
	}

	// Matched exactly one → ok.
	one := render.RenderedOp{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == 'host-b'", Expect: config.ExpectOne}
	after, err := stateop.Merge(twoReplicas, []render.RenderedOp{one}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("* expect: one matching 1 should be ok: %v", err)
	}
	if len(after["redis_hosts"].([]any)) != 1 {
		t.Errorf("after removing one, left %+v", after["redis_hosts"])
	}
}

// buildOps crosses the seam [ADR-0084] opened: op construction moved out of
// render into the dispatched module, so a render→merge test has to build the ops
// itself. It goes through [stateop.OpsFromPlan] — the same entry the L0 harness
// uses — rather than reassembling the loop, because a local copy would skip
// [stateop.CheckParams] and green-light a param combination the keeper refuses at
// dispatch. Only the Vault/store work of the real module is missing.
func buildOps(t *testing.T, tasks []*render.RenderedTask) []render.RenderedOp {
	t.Helper()
	ops, err := stateop.OpsFromPlan(tasks)
	if err != nil {
		t.Fatalf("OpsFromPlan: %v", err)
	}
	return ops
}

// TestStateAddTasks_GrowByN — ★ the successor to the deleted `foreach` op
// ([ADR-0084] F-C): a capture is an ordinary task, so N captures are N tasks.
// Render interpolates each one's params on its own, the module turns each into
// one op, and the merge composes them in plan order — the collection grows by N
// and a repeat is idempotent through on_conflict: skip.
//
// ⚠ `loop:` does NOT yet reach here: renderKeeperTask rejects it
// (pipeline.go:1092-1094, pinned by TestRender_LoopOnKeeperTaskRejected). Until that
// lifts, a capture over a runtime-sized collection has to be written out, which
// is the one thing `foreach` could express and this cannot.
func TestStateAddTasks_GrowByN(t *testing.T) {
	capture := func(name, value string) config.Task {
		return config.Task{
			Name: name,
			On:   "keeper",
			Module: &config.ModuleTask{
				Module: "core.state.add",
				Params: map[string]any{
					"field":       "redis_hosts",
					"value":       value,
					"match":       "elem == value",
					"on_conflict": "skip",
				},
			},
		}
	}
	manifest := &config.ScenarioManifest{
		Name: "add_replicas",
		Tasks: []config.Task{
			capture("Record replica 1", "${ input.replicas[0] }"),
			capture("Record replica 2", "${ input.replicas[1] }"),
			capture("Record replica 3", "${ input.replicas[2] }"),
		},
	}
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := render.NewPipeline(nil, eng, nil, nil)
	matchEval, opEval := p.StateOpEvaluators(context.Background(), "")
	in := render.RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"replicas": []any{"r1", "r2", "r3"}},
		Incarnation: render.IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{{SID: "a", Coven: []string{"svc"}}},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	ops := buildOps(t, tasks)

	// list of scalars schema.
	schema := config.InputSchemaMap{
		"redis_hosts": &config.InputSchema{Type: "array", Items: &config.InputSchema{Type: "string"}},
	}
	before := map[string]any{"redis_hosts": []any{"r0"}}

	after, err := stateop.Merge(before, ops, schema, matchEval, opEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 4 {
		t.Fatalf("* redis_hosts len = %d, want 4 (r0 + 3 captures)", len(hosts))
	}

	// Idempotency: repeating the same ops → length doesn't grow (on_conflict: skip).
	again, err := stateop.Merge(after, ops, schema, matchEval, opEval)
	if err != nil {
		t.Fatalf("merge2: %v", err)
	}
	if len(again["redis_hosts"].([]any)) != 4 {
		t.Errorf("* repeated capture is not idempotent: len = %d, want 4", len(again["redis_hosts"].([]any)))
	}
}

// TestStateModifyTasks_PerEntryLiteral — ★ each capture patches ITS OWN entry.
// Render interpolates `${ input.* }` into each task's params separately, so by
// merge time every predicate and patch is a literal that names one element. That
// is what makes taking the run context out of the merge-time scope safe
// ([ADR-0084]): a shared evaluation is the thing that could confuse two entries,
// and there is no longer a shared scope to confuse.
func TestStateModifyTasks_PerEntryLiteral(t *testing.T) {
	patchUser := func(name, keyExpr, aclExpr string) config.Task {
		return config.Task{
			Name: name,
			On:   "keeper",
			Module: &config.ModuleTask{
				Module: "core.state.modify",
				Params: map[string]any{
					"field": "redis_users",
					"match": "key == '" + keyExpr + "'",
					"patch": map[string]any{"acl": aclExpr},
				},
			},
		}
	}
	manifest := &config.ScenarioManifest{
		Name: "update_acl",
		Tasks: []config.Task{
			patchUser("Patch alice", "alice", "${ input.changes.alice.acl }"),
			patchUser("Patch bob", "bob", "${ input.changes.bob.acl }"),
		},
	}
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := render.NewPipeline(nil, eng, nil, nil)
	matchEval, opEval := p.StateOpEvaluators(context.Background(), "")
	in := render.RenderInput{
		Scenario: manifest,
		Input: map[string]any{"changes": map[string]any{
			"alice": map[string]any{"acl": "+@all"},
			"bob":   map[string]any{"acl": "+@write"},
		}},
		Incarnation: render.IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{{SID: "a", Coven: []string{"svc"}}},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	ops := buildOps(t, tasks)

	before := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read", "state": "on"},
		"bob":   map[string]any{"acl": "+@read", "state": "on"},
		"carol": map[string]any{"acl": "+@read", "state": "on"},
	}}
	after, err := stateop.Merge(before, ops, redisHostsSchema, matchEval, opEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	users := after["redis_users"].(map[string]any)
	if users["alice"].(map[string]any)["acl"] != "+@all" {
		t.Errorf("* alice.acl = %v, want +@all (own capture)", users["alice"])
	}
	if users["bob"].(map[string]any)["acl"] != "+@write" {
		t.Errorf("* bob.acl = %v, want +@write (own capture)", users["bob"])
	}
	if users["carol"].(map[string]any)["acl"] != "+@read" {
		t.Errorf("carol affected (no capture names it): %v", users["carol"])
	}
	// state field intact (patch touches only acl, merge not overwrite).
	if users["alice"].(map[string]any)["state"] != "on" {
		t.Errorf("alice.state clobbered by patch: %v", users["alice"])
	}
}

// stateMirrorFixture/stateMirrorOps/stateMirrorExpected — the end-to-end shape
// of one commit: a set and an add over a populated fixture, pinned to an exact
// expected state. It was one half of an anti-drift pair against the trial-side
// duplicate of the merge; the duplicate is gone ([ADR-0084] F-C, trial merges
// through [stateop.Merge]), and what is left is a plain regression pin.
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

// TestMergeOps_MirrorProd — the prod side of the anti-drift check.
func TestMergeOps_MirrorProd(t *testing.T) {
	after, err := stateop.Merge(stateMirrorFixture(), stateMirrorOps(), redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := json.Marshal(after)
	if !equalJSONState(t, string(got), stateMirrorExpectedJSON) {
		t.Errorf("* prod state_after = %s, want %s", got, stateMirrorExpectedJSON)
	}
}

// verbsMirrorFixture/verbsMirrorOps/verbsMirrorExpectedJSON — the same pin for
// the modify/remove verbs. Predicates and patches arrive as literals: render
// interpolated the task's params before the module built the op ([ADR-0084]).
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
	return []render.RenderedOp{
		// modify map by key: alice.acl → +@all (state intact).
		{Verb: config.VerbModify, Field: "redis_users", Match: "key == 'alice'",
			Patch: map[string]any{"acl": "${ '+@all' }"}},
		// remove list by sid: host-c removed (expect: one).
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == 'host-c'",
			Expect: config.ExpectOne},
	}
}

const verbsMirrorExpectedJSON = `{"redis_users":{"alice":{"acl":"+@all","state":"on"},"bob":{"acl":"+@read","state":"on"}},"redis_hosts":[{"sid":"host-a","role":"primary"},{"sid":"host-b","role":"replica"}]}`

// TestMergeVerbsMirror_Prod — the prod side of the new-verbs anti-drift check.
func TestMergeVerbsMirror_Prod(t *testing.T) {
	after, err := stateop.Merge(verbsMirrorFixture(), verbsMirrorOps(), redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := json.Marshal(after)
	if !equalJSONState(t, string(got), verbsMirrorExpectedJSON) {
		t.Errorf("* prod state_after = %s, want %s (modify/remove drift)", got, verbsMirrorExpectedJSON)
	}
}

// --- Guard tests for coverage gaps (ADR-057): composition within a block,
// scalar lists, empty collections, patch clobber. ---

// TestMergeOps_Composition_SetThenAdd — ★ set creates a collection,
// and an add into it in the SAME block sees the intermediate state (ops apply
// in order against the intermediate result, ADR-057 §e). Deterministic order.
func TestMergeOps_Composition_SetThenAdd(t *testing.T) {
	before := map[string]any{} // redis_hosts absent
	ops := []render.RenderedOp{
		{Verb: config.VerbSet, Field: "redis_hosts", Value: []any{}}, // create an empty list
		addRedisHost("host-a", "primary", config.OnConflictSkip),     // add sees the created list
		addRedisHost("host-b", "replica", config.OnConflictSkip),
	}
	after, err := stateop.Merge(before, ops, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("* redis_hosts len = %d, want 2 (add into a set-created collection)", len(hosts))
	}
	if hosts[0].(map[string]any)["sid"] != "host-a" || hosts[1].(map[string]any)["sid"] != "host-b" {
		t.Errorf("* add order violated: %+v", hosts)
	}
}

// TestMergeOps_Composition_AddThenRemove — ★ add X → remove X by match
// within one block: the element ends up absent (remove sees add's result).
// Intermediate state is visible to the following op.
func TestMergeOps_Composition_AddThenRemove(t *testing.T) {
	before := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	ops := []render.RenderedOp{
		addRedisHost("host-b", "replica", config.OnConflictSkip),                       // +host-b
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.sid == 'host-b'"}, // -host-b
	}
	after, err := stateop.Merge(before, ops, redisHostsSchema, matchEvalForTest(t), opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts := after["redis_hosts"].([]any)
	if len(hosts) != 1 || hosts[0].(map[string]any)["sid"] != "host-a" {
		t.Fatalf("* add X -> remove X: expected only host-a, got %+v", hosts)
	}
}

// TestMergeOps_ScalarList_ModifyRemove — modify/remove over a list
// of scalars (elem=scalar): remove works by a predicate over the scalar;
// modify (a dotted-path patch) produces a CLEAR error, not a panic.
func TestMergeOps_ScalarList_ModifyRemove(t *testing.T) {
	scalarSchema := config.InputSchemaMap{
		"tags": &config.InputSchema{Type: "array", Items: &config.InputSchema{Type: "string"}},
	}
	before := func() map[string]any {
		return map[string]any{"tags": []any{"a", "b", "c"}}
	}

	// remove over a scalar list by an elem predicate — works.
	rm := render.RenderedOp{Verb: config.VerbRemove, Field: "tags", Match: "elem == 'b'"}
	after, err := stateop.Merge(before(), []render.RenderedOp{rm}, scalarSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("remove over scalar-list: %v", err)
	}
	tags := after["tags"].([]any)
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "c" {
		t.Fatalf("★ remove scalar 'b': got %+v, want [a c]", tags)
	}

	// modify (dotted-path patch) over a scalar element — a clear error, not a panic.
	mod := render.RenderedOp{Verb: config.VerbModify, Field: "tags", Match: "elem == 'a'",
		Patch: map[string]any{"x": "${ 'y' }"}}
	if _, err := stateop.Merge(before(), []render.RenderedOp{mod}, scalarSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("* modify of a scalar element via a dotted patch should error (patch only applies to an object)")
	}
}

// TestMergeOps_RemoveAll_EmptyNotNil — ★ removing ALL elements yields
// an EMPTY collection ([]any{} / map{}), NOT nil: a following add must see
// the empty collection and materialize into it, not crash on nil.
func TestMergeOps_RemoveAll_EmptyNotNil(t *testing.T) {
	// list: remove-all → []any{} (not nil), then add into it grows by 1.
	beforeList := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "replica"},
		map[string]any{"sid": "host-b", "role": "replica"},
	}}
	ops := []render.RenderedOp{
		{Verb: config.VerbRemove, Field: "redis_hosts", Match: "elem.role == 'replica'"},
		addRedisHost("host-c", "primary", config.OnConflictSkip),
	}
	after, err := stateop.Merge(beforeList, ops, redisHostsSchema, matchEvalForTest(t), opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	hosts, ok := after["redis_hosts"].([]any)
	if !ok {
		t.Fatalf("* redis_hosts = %T, want []any (remove-all leaves an empty list, not nil)", after["redis_hosts"])
	}
	if len(hosts) != 1 || hosts[0].(map[string]any)["sid"] != "host-c" {
		t.Fatalf("* add after remove-all: got %+v, want [host-c]", hosts)
	}

	// map: remove-all → map{} (not nil), add by key grows by 1.
	beforeMap := map[string]any{"redis_users": map[string]any{
		"alice": map[string]any{"acl": "+@read"},
	}}
	opsMap := []render.RenderedOp{
		{Verb: config.VerbRemove, Field: "redis_users", Match: "true == true"},
		{Verb: config.VerbAdd, Field: "redis_users", Key: "bob",
			Value: map[string]any{"acl": "+@all"}, OnConflict: config.OnConflictSkip},
	}
	afterMap, err := stateop.Merge(beforeMap, opsMap, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("merge map: %v", err)
	}
	users, ok := afterMap["redis_users"].(map[string]any)
	if !ok {
		t.Fatalf("* redis_users = %T, want map (remove-all leaves an empty map, not nil)", afterMap["redis_users"])
	}
	if len(users) != 1 {
		t.Fatalf("* add after remove-all map: got %+v, want {bob}", users)
	}
	if _, has := users["bob"]; !has {
		t.Errorf("* bob was not added to the emptied map: %+v", users)
	}
}

// TestMergeOps_PatchClobber_MissingVsExistingScalar — ★ QA
// observation: patching the nested path config.maxmemory.
//   - MISSING intermediate path (no config) → materialize a map (ADR-057 §f);
//   - EXISTING non-map intermediate node (config="string") → ERROR, not a
//     silent clobber (data loss is unsafe).
func TestMergeOps_PatchClobber_MissingVsExistingScalar(t *testing.T) {
	patchOp := func() render.RenderedOp {
		return render.RenderedOp{Verb: config.VerbModify, Field: "redis_hosts",
			Match: "elem.sid == 'host-a'", Patch: map[string]any{"config.maxmemory": "${ '512mb' }"}}
	}

	// missing → materialize config as a map.
	beforeMissing := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"}, // config absent
	}}
	after, err := stateop.Merge(beforeMissing, []render.RenderedOp{patchOp()}, redisHostsSchema, noMatch, opEvalForTest(t))
	if err != nil {
		t.Fatalf("* a missing intermediate path should materialize, not error: %v", err)
	}
	cfg := after["redis_hosts"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if cfg["maxmemory"] != "512mb" {
		t.Errorf("* config.maxmemory = %v, want 512mb (config materialized)", cfg["maxmemory"])
	}

	// existing-scalar → ERROR (config is a string; descending the nested path = clobber).
	beforeScalar := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary", "config": "some-string-value"},
	}}
	if _, err := stateop.Merge(beforeScalar, []render.RenderedOp{patchOp()}, redisHostsSchema, noMatch, opEvalForTest(t)); err == nil {
		t.Fatal("* patch config.maxmemory over config=\"string\" should error (silent clobber is unsafe), not silently overwrite")
	}
}

// TestMergeOps_AddConflictReason_NoSecretLeak — ★ BUG-3 (security): add
// into a map with key=a resolved secret + on_conflict:error. The error
// reason (which ends up unmasked in incarnation.status_details.error —
// audit.MaskSecrets only catches `vault:` refs, not plaintext values) must
// NOT contain the key's value — only the collection field name. Same for a
// list add-conflict (resolved value/elem).
func TestMergeOps_AddConflictReason_NoSecretLeak(t *testing.T) {
	const secret = "s3cr3t-vault-resolved-value"

	// map add-conflict: key is already resolved to a secret (as after render `${ vault(...) }`).
	beforeMap := map[string]any{"redis_users": map[string]any{
		secret: map[string]any{"acl": "+@read"},
	}}
	mapOp := render.RenderedOp{Verb: config.VerbAdd, Field: "redis_users", Key: secret,
		Value: map[string]any{"acl": "+@all"}, OnConflict: config.OnConflictError}
	_, err := stateop.Merge(beforeMap, []render.RenderedOp{mapOp}, redisHostsSchema, noMatch, noOpEval)
	if err == nil {
		t.Fatal("expected an error (on_conflict=error on an existing key)")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("* secret-LEAK: reason contains the plaintext key: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redis_users") {
		t.Errorf("reason should name the field redis_users: %q", err.Error())
	}

	// list add-conflict: value carries a secret, the element already exists (deep-equal).
	beforeList := map[string]any{"redis_hosts": []any{secret}}
	listOp := render.RenderedOp{Verb: config.VerbAdd, Field: "redis_hosts",
		Value: secret, OnConflict: config.OnConflictError}
	_, err = stateop.Merge(beforeList, []render.RenderedOp{listOp}, redisHostsSchema, matchEvalForTest(t), noOpEval)
	if err == nil {
		t.Fatal("expected an error (on_conflict=error on an existing element)")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("* secret-LEAK: list-reason contains the plaintext value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redis_hosts") {
		t.Errorf("list-reason should name the field redis_hosts: %q", err.Error())
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
		t.Errorf("host-2.probe_b should not exist (task without register:)")
	}
	if len(got["host-2"]) != 1 {
		t.Errorf("host-2 register keys = %d, want 1", len(got["host-2"]))
	}
}

// EVERY task with a register: is accumulated, including one whose module
// returns a secret. The `no_log:` exclusion that used to drop such a task's row
// wholesale went with the key ([ADR-0083] §8) and deliberately has no per-field
// successor here: this same fold feeds the NEXT Passage's render
// (loadRegisterByHostUpToPassage), so dropping the row would break the register
// chain rather than protect it — the following task would read nothing where
// the previous one wrote.
//
// The protection moved earlier and got narrower. A declared secret never travels
// through a register as plaintext: §1 keeps it out of incarnation.state and §6
// turns the value a keeper task writes into a `vault:` ref, sealing the register
// name so any state cell reading it is masked on the way out ([ADR-010] §7.4).
// A reversal that reinstated a drop here would look like extra safety and would
// actually break the chain.
func TestBuildRegisterByHost_SecretCarryingTaskIsAccumulated(t *testing.T) {
	tasks := []*render.RenderedTask{
		{Index: 0, Register: "plain"},
		{Index: 1, Register: "secret_probe", SecretOutput: []string{"data"}},
	}
	rows := []applyrun.TaskRegister{
		{ApplyID: "a", SID: "host-1", PlanIndex: 0, TaskIdx: 0, RegisterData: map[string]any{"stdout": "ok"}},
		{ApplyID: "a", SID: "host-1", PlanIndex: 1, TaskIdx: 1, RegisterData: map[string]any{"data": "vault:secret/x#value"}},
	}

	got := buildRegisterByHost(rows, tasks)

	if v := got["host-1"]["plain"].(map[string]any)["stdout"]; v != "ok" {
		t.Errorf("host-1.plain.stdout = %v, want ok", v)
	}
	probe, ok := got["host-1"]["secret_probe"].(map[string]any)
	if !ok {
		t.Fatalf("host-1.secret_probe missing — the next Passage would read nothing where this task wrote")
	}
	if probe["data"] != "vault:secret/x#value" {
		t.Errorf("host-1.secret_probe.data = %v, want the ref stored intact", probe["data"])
	}
	if len(got["host-1"]) != 2 {
		t.Errorf("host-1 register keys = %d, want 2", len(got["host-1"]))
	}
}

func TestBuildRegisterByHost_EmptyRows(t *testing.T) {
	got := buildRegisterByHost(nil, []*render.RenderedTask{{Index: 0, Register: "p"}})
	if got == nil || len(got) != 0 {
		t.Errorf("got = %v, want empty map", got)
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
		t.Fatalf("* register X missing - probe-register clobbered by task_idx collision (bug)")
	}
	if v := x.(map[string]any)["stdout"]; v != "probe-A-value" {
		t.Errorf("* register X.stdout = %v, want probe-A-value (name resolves by global plan_index)", v)
	}
	// Y resolves to its own value (plan_index 2 → Index 2 → name Y).
	if v := got["host-1"]["Y"].(map[string]any)["stdout"]; v != "action-Y-value" {
		t.Errorf("register Y.stdout = %v, want action-Y-value", v)
	}
	if len(got["host-1"]) != 2 {
		t.Errorf("host-1 register keys = %d, want 2 (X and Y)", len(got["host-1"]))
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
		t.Errorf("* host-A.R.stdout = %v, want A-R (local task_idx=1)", v)
	}
	if v := got["host-B"]["R"].(map[string]any)["stdout"]; v != "B-R" {
		t.Errorf("* host-B.R.stdout = %v, want B-R (local task_idx=0, same plan_index=1)", v)
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
		t.Fatal("keeperRegisterBucket returned nil - previous Passage keeper-register lost (Slice 1 gap)")
	}
	// keeper register is available in flat form under the name provision →
	// keeperVars will see register.provision.* on the active Passage's keeper task.
	prov, ok := bucket["provision"].(map[string]any)
	if !ok {
		t.Fatalf("bucket[provision] = %T, want map (keeper task register)", bucket["provision"])
	}
	if prov["ip"] != "10.0.0.5" {
		t.Errorf("bucket[provision].ip = %v, want 10.0.0.5", prov["ip"])
	}
	// The host register (probe under host-1) does NOT end up in the keeper
	// bucket: the flattening into Register carries ONLY keeper tasks, the
	// host bucket stays in RegisterByHost[sid] (the host path reads it
	// per-host, not from the flat Register).
	if _, leaked := bucket["probe"]; leaked {
		t.Error("bucket[probe] is present - host-register leaked into the keeper-bucket")
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
		t.Errorf("keeperRegisterBucket = %v, want nil (no register under KeeperTargetSID)", bucket)
	}
	// Empty/nil map → nil.
	if bucket := keeperRegisterBucket(nil); bucket != nil {
		t.Errorf("keeperRegisterBucket(nil) = %v, want nil", bucket)
	}
	if bucket := keeperRegisterBucket(map[string]map[string]any{}); bucket != nil {
		t.Errorf("keeperRegisterBucket(empty) = %v, want nil", bucket)
	}
}

// ★ Guard for [ADR-0083] §4 — a DECLARED secret (`type: secret`) never reaches
// the merged state record. The trial preview runs the same merge, so it cannot
// show a password the real commit does not store.
//
// The `secret: true` field next to it is the OTHER marker ([ADR-010] §7.4) — that
// value LIVES in state and is masked on the way out. It must survive untouched;
// stripping it would silently delete an operator's data.
func secretStripSchema() config.InputSchemaMap {
	return config.InputSchemaMap{
		"admin_password": {Type: config.SecretTypeName},
		"tls_key":        {Type: "string", Secret: true},
		"redis_users": {
			Type: "array",
			Items: &config.InputSchema{
				Type: "object",
				Properties: config.InputSchemaMap{
					"name":     {Type: "string"},
					"perms":    {Type: "string"},
					"password": {Type: config.SecretTypeName, Key: "name"},
				},
			},
		},
	}
}

func TestMergeOps_StripsDeclaredSecrets_Prod(t *testing.T) {
	before := map[string]any{"tls_key": "KEEP-ME"}
	ops := []render.RenderedOp{
		{Verb: config.VerbSet, Field: "admin_password", Value: "PLAINTEXT-ADMIN"},
		{Verb: config.VerbSet, Field: "redis_users", Value: []any{
			map[string]any{"name": "alice", "perms": "+@read", "password": "PLAINTEXT-ALICE"},
		}},
	}

	out, err := stateop.Merge(before, ops, secretStripSchema(), nil, nil)
	if err != nil {
		t.Fatalf("stateop.Merge: %v", err)
	}
	if _, ok := out["admin_password"]; ok {
		t.Errorf("scalar declared secret survived the merge: %v", out)
	}
	users, _ := out["redis_users"].([]any)
	if len(users) != 1 {
		t.Fatalf("redis_users = %v", out["redis_users"])
	}
	el, _ := users[0].(map[string]any)
	if _, ok := el["password"]; ok {
		t.Errorf("collection declared secret survived the merge: %v", el)
	}
	if el["name"] != "alice" || el["perms"] != "+@read" {
		t.Errorf("the addressing properties were damaged: %v", el)
	}
	if out["tls_key"] != "KEEP-ME" {
		t.Errorf("tls_key = %v, want the ADR-010 `secret: true` value left in state", out["tls_key"])
	}
	if blob, _ := json.Marshal(out); strings.Contains(string(blob), "PLAINTEXT") {
		t.Errorf("a plaintext secret is in the committed record: %s", blob)
	}
}
