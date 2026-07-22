package trial

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ★ Anti-drift verification: trial-side of prod-merge mirror. Fixtures/operations/
// expectation MUST match byte-for-byte with scenario.TestMergeStateChanges_MirrorProd
// (state_test.go). If mergeStateChanges bodies (state.go vs diff.go) diverge —
// one of two tests will fail on the same input.
//
// Mirror values are duplicated here intentionally (scenario/trial packages
// are isolated; shared fixture would require exported helper in render — over-
// engineering for one test).

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
		add("host-b", "replica"), // new → grows
		add("host-a", "primary"), // existing → no-op
	}
}

// mirrorExpectedJSON — must match scenario.stateMirrorExpectedJSON.
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
		t.Errorf("★ trial state_after = %s, want %s (duplicate mergeStateChanges drift)", got, mirrorExpectedJSON)
	}
}

// --- Anti-drift for NEW verbs modify/remove. Fixtures/operations/expectation
// MUST match byte-for-byte with scenario.TestMergeVerbsMirror_Prod (state_test.go).
// Divergence in bodies applyModifyOp/applyRemoveOp/applyPatch would split Trial from prod.

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

// verbsMirrorExpectedJSON — must match scenario.verbsMirrorExpectedJSON.
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
		t.Errorf("★ trial state_after = %s, want %s (modify/remove duplicate drift)", got, verbsMirrorExpectedJSON)
	}
}

// TestPatchClobber_Trial — ★ trial-side patch-clobber (synchronous with
// scenario.TestMergeStateChanges_PatchClobber_MissingVsExistingScalar): missing
// intermediate path is materialized, existing non-map node → error. Divergence in
// setNestedPath behavior would split Trial from prod.
func TestPatchClobber_Trial(t *testing.T) {
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	pl := render.NewPipeline(nil, eng, nil, nil)
	ctx := map[string]any{"input": map[string]any{"mem": "512mb"}}
	patchOp := render.RenderedOp{Verb: config.VerbModify, Field: "redis_hosts",
		Match: "elem.sid == 'host-a'", Patch: map[string]any{"config.maxmemory": "${ input.mem }"}, Context: ctx}

	// missing → materialize.
	beforeMissing := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary"},
	}}
	after, err := mergeStateChanges(beforeMissing, []render.RenderedOp{patchOp}, mirrorSchema(), pl.EvalStateMatch, pl.EvalStateOpExpr)
	if err != nil {
		t.Fatalf("★ trial: missing intermediate path must materialize: %v", err)
	}
	cfg := after["redis_hosts"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if cfg["maxmemory"] != "512mb" {
		t.Errorf("★ trial config.maxmemory = %v, want 512mb", cfg["maxmemory"])
	}

	// existing-scalar → error.
	beforeScalar := map[string]any{"redis_hosts": []any{
		map[string]any{"sid": "host-a", "role": "primary", "config": "some-string-value"},
	}}
	if _, err := mergeStateChanges(beforeScalar, []render.RenderedOp{patchOp}, mirrorSchema(), pl.EvalStateMatch, pl.EvalStateOpExpr); err == nil {
		t.Fatal("★ trial: patch over config=\"string\" must error (synchronized with prod branch)")
	}
}

// TestSetNestedPath_NoSilentClobber — unit-guard directly on setNestedPath: missing
// segment is created, existing non-map segment → error WITHOUT mutation.
func TestSetNestedPath_NoSilentClobber(t *testing.T) {
	// missing → create nested map.
	m := map[string]any{}
	if err := setNestedPath(m, "config.maxmemory", "256mb"); err != nil {
		t.Fatalf("setNestedPath missing: %v", err)
	}
	if m["config"].(map[string]any)["maxmemory"] != "256mb" {
		t.Errorf("setNestedPath did not materialize config: %+v", m)
	}

	// existing-scalar → error, original value NOT clobbered.
	m2 := map[string]any{"config": "scalar"}
	if err := setNestedPath(m2, "config.maxmemory", "256mb"); err == nil {
		t.Fatal("★ setNestedPath over config=\"scalar\" must return error, not clobber")
	}
	if m2["config"] != "scalar" {
		t.Errorf("★ original scalar value clobbered: %+v (silent-clobber)", m2)
	}
}
