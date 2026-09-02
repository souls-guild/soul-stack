package statemigrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/config"
)

// fixtureDir is the path to real consolidated Redis fixtures (authoritative
// over docs examples). The first step is a DSL grammar demo (rename + set +
// foreach + delete), moving an invariant from the previous redis-cluster.
const fixtureDir = "../../../examples/service/redis/migrations"

// firstStepDir is the bottom rung of that ladder — the step leading to version 2.
const firstStepDir = "002_acl_users_list_to_map"

func mustEvaluator(t *testing.T) Evaluator {
	t.Helper()
	ev, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev
}

// mustParseStep reads one step of the fixture ladder by its directory name,
// `<NNN>_<slug>`, and takes the version it leads to from that <NNN> — the same
// derivation the loader makes, so a fixture that breaks the naming rule fails here
// rather than parsing as version 0.
func mustParseStep(t *testing.T, stepDir string) *Migration {
	t.Helper()
	path := filepath.Join(fixtureDir, stepDir, config.MigrationStepFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m, err := Parse(data, stepVersion(t, stepDir), path)
	if err != nil {
		t.Fatalf("Parse %s: %v", path, err)
	}
	return m
}

// stepVersion pulls the version off a `<NNN>_<slug>` directory name.
func stepVersion(t *testing.T, stepDir string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.SplitN(stepDir, "_", 2)[0])
	if err != nil {
		t.Fatalf("step directory %q does not start with <NNN>: %v", stepDir, err)
	}
	return n
}

// migrationTestCase is the format of tests/<case>.yml (state_before → migration →
// assert state_after).
type migrationTestCase struct {
	Name        string         `yaml:"name"`
	StateBefore map[string]any `yaml:"state_before"`
	StateAfter  map[string]any `yaml:"state_after"`
}

// TestApply_AllFixtures runs generic traversal of all Redis service migration fixtures.
// For each step directory "<NNN>_<slug>/ holding main.yml + tests/*.yml" it executes
// that ONE step (<NNN>-1 → <NNN>) on state_before and compares with state_after.
// This gates against silent regression: any existing fixture (incl. 4 of 006_acl_users_map_to_array)
// runs without manual path hardcoding. Authoritative over docs examples.
//
// Each case lives inside the step it exercises (`<NNN>_<slug>/tests/<case>.yml`), and
// the step's version comes from that directory's <NNN> — never from the document.
// Therefore a one-step Chain suffices — multi-step chains are covered by step-snapshot
// tests below.
func TestApply_AllFixtures(t *testing.T) {
	ev := mustEvaluator(t)

	stepDocs, err := filepath.Glob(filepath.Join(fixtureDir, "*", config.MigrationStepFile))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(stepDocs) == 0 {
		t.Fatalf("no migration steps found in %s", fixtureDir)
	}

	var totalCases int
	for _, stepDoc := range stepDocs {
		stepName := filepath.Base(filepath.Dir(stepDoc)) // e.g. 006_acl_users_map_to_array
		caseFiles, err := filepath.Glob(filepath.Join(fixtureDir, stepName, "tests", "*.yml"))
		if err != nil {
			t.Fatalf("glob cases %s: %v", stepName, err)
		}
		if len(caseFiles) == 0 {
			// Migration without fixtures is a potential coverage gap but not a failure
			// (some steps may be trivial). Mark explicitly.
			t.Logf("migration %s: no test fixtures", stepName)
			continue
		}

		mig := mustParseStep(t, stepName)
		for _, caseFile := range caseFiles {
			caseName := stepName + "/" + strings.TrimSuffix(filepath.Base(caseFile), ".yml")
			t.Run(caseName, func(t *testing.T) {
				data, err := os.ReadFile(caseFile)
				if err != nil {
					t.Fatalf("read case: %v", err)
				}
				var tc migrationTestCase
				if err := yaml.Unmarshal(data, &tc); err != nil {
					t.Fatalf("unmarshal case: %v", err)
				}

				res, err := Apply(context.Background(), tc.StateBefore, Chain{mig}, ev)
				if err != nil {
					t.Fatalf("Apply %s: %v", caseName, err)
				}
				assertDeepEqualJSON(t, res.FinalState, tc.StateAfter)
			})
			totalCases++
		}
	}

	t.Logf("migrations run: %d, test cases: %d", len(stepDocs), totalCases)
}

// TestApply_RealFixture_EmptyUsers: empty user list yields empty map.
// The first step explicitly materializes the target key with `set state.redis_users {}`
// before foreach (intent "list became map"), so foreach over [] (no-op) leaves
// redis_users: {} rather than key absence.
func TestApply_RealFixture_EmptyUsers(t *testing.T) {
	mig := mustParseStep(t, firstStepDir)
	ev := mustEvaluator(t)

	in := map[string]any{"redis_users": []any{}, "redis_type": "cluster"}
	res, err := Apply(context.Background(), in, Chain{mig}, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertDeepEqualJSON(t, res.FinalState, map[string]any{
		"redis_users": map[string]any{},
		"redis_type":  "cluster",
	})
}

// TestApply_EmptyForeachNoMaterialize: engine invariant — foreach over empty
// list doesn't create the key by itself (no-op without body). Checked on synthetic
// migration WITHOUT prior set to fix engine behavior independent of fixture intent.
func TestApply_EmptyForeachNoMaterialize(t *testing.T) {
	ev := mustEvaluator(t)
	mig := &Migration{FromVersion: 1, ToVersion: 2, Transform: []Op{
		{Rename: &RenameOp{From: "state.redis_users", To: "state.redis_users_legacy_v1"}},
		{Foreach: &ForeachOp{In: "${ state.redis_users_legacy_v1 }", As: "user_name", Do: []Op{
			{Set: &SetOp{Path: "state.redis_users.${ user_name }", Value: map[string]any{"perms": "x"}}},
		}}},
		{Delete: &DeleteOp{Path: "state.redis_users_legacy_v1"}},
	}}

	in := map[string]any{"redis_users": []any{}, "redis_type": "cluster"}
	res, err := Apply(context.Background(), in, Chain{mig}, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertDeepEqualJSON(t, res.FinalState, map[string]any{"redis_type": "cluster"})
}

// TestApply_DoesNotMutateInput: caller's input state is not mutated.
func TestApply_DoesNotMutateInput(t *testing.T) {
	mig := mustParseStep(t, firstStepDir)
	ev := mustEvaluator(t)

	in := map[string]any{"redis_users": []any{"app"}, "redis_type": "standalone"}
	snapshot := map[string]any{"redis_users": []any{"app"}, "redis_type": "standalone"}

	if _, err := Apply(context.Background(), in, Chain{mig}, ev); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input state mutated: %#v", in)
	}
}

// TestApply_StepSnapshots: before/after snapshot per chain step.
func TestApply_StepSnapshots(t *testing.T) {
	ev := mustEvaluator(t)
	chain := Chain{
		{FromVersion: 1, ToVersion: 2, Transform: []Op{
			{Set: &SetOp{Path: "state.a", Value: 1}},
		}},
		{FromVersion: 2, ToVersion: 3, Transform: []Op{
			{Set: &SetOp{Path: "state.b", Value: 2}},
		}},
	}
	res, err := Apply(context.Background(), map[string]any{}, chain, ev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(res.Steps))
	}
	if _, ok := res.Steps[0].StateBefore["a"]; ok {
		t.Errorf("step0.StateBefore should not contain a")
	}
	if res.Steps[0].StateAfter["a"] != float64(1) && res.Steps[0].StateAfter["a"] != 1 {
		t.Errorf("step0.StateAfter[a] = %v", res.Steps[0].StateAfter["a"])
	}
	if res.Steps[1].FromVersion != 2 || res.Steps[1].ToVersion != 3 {
		t.Errorf("step1 versions = %d->%d", res.Steps[1].FromVersion, res.Steps[1].ToVersion)
	}
}

// TestApply_ChainVersionGap: version gap in chain → error.
func TestApply_ChainVersionGap(t *testing.T) {
	ev := mustEvaluator(t)
	chain := Chain{
		{FromVersion: 1, ToVersion: 2},
		{FromVersion: 3, ToVersion: 4}, // gap: 2 != 3
	}
	_, err := Apply(context.Background(), map[string]any{}, chain, ev)
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassChainVersion {
		t.Fatalf("error = %v, want ClassChainVersion", err)
	}
}

func assertDeepEqualJSON(t *testing.T, got, want map[string]any) {
	t.Helper()
	// Normalize numeric types through JSON round-trip (YAML int vs Apply
	// preserves cel int64 — compare in unified form).
	if !reflect.DeepEqual(normalizeJSON(t, got), normalizeJSON(t, want)) {
		t.Errorf("state mismatch:\n got = %#v\nwant = %#v", got, want)
	}
}

func normalizeJSON(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	return deepCopyMap(m)
}
