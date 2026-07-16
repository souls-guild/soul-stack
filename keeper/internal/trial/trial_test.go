package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

// writeScenarioTree creates a temporary scenario/<name>/{main.yml,
// tests/<case>/case.yml} tree and returns the case directory path.
func writeScenarioTree(t *testing.T, mainYML, caseYML string) string {
	t.Helper()
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")
	caseDir := filepath.Join(scnDir, "tests", "c1")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(mainYML), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, caseFileName), []byte(caseYML), 0o644); err != nil {
		t.Fatalf("write case.yml: %v", err)
	}
	return caseDir
}

// writeScenarioSibling writes a sibling file into scenario/create/<name> (for
// include targets). Returns the absolute file path.
func writeScenarioSibling(t *testing.T, caseDir, name, content string) {
	t.Helper()
	scnDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/create
	if err := os.WriteFile(filepath.Join(scnDir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write sibling %s: %v", name, err)
	}
}

// writeServiceLevelSibling writes a service-level file (scenario/<name>, the
// shared scenario/ directory for all scenarios, parent of scenario/create/).
func writeServiceLevelSibling(t *testing.T, caseDir, name, content string) {
	t.Helper()
	scnDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/create
	serviceScenarioDir := filepath.Dir(scnDir)    // .../scenario
	if err := os.WriteFile(filepath.Join(serviceScenarioDir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write service-level sibling %s: %v", name, err)
	}
}

// TestRunCase_ScenarioIncludeShadowing checks two-level resolution
// (orchestration.md section 6): a local include file fully shadows a
// same-named service-level file (shadowing, no merge).
func TestRunCase_ScenarioIncludeShadowing(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: greet.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: include shadowing
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/local
        content: hi
`)
	// service-level version (must be shadowed by the local one).
	writeServiceLevelSibling(t, caseDir, "greet.yml", `- name: service-level
  module: core.file.present
  params:
    path: /tmp/service-level
    content: "${ input.greeting }"
`)
	// local version wins.
	writeScenarioSibling(t, caseDir, "greet.yml", `- name: local
  module: core.file.present
  params:
    path: /tmp/local
    content: "${ input.greeting }"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (local shadows service-level), FAIL: %v", results[0].Failures)
	}
}

// TestRunCase_ScenarioIncludeServiceLevelFallback checks that resolution falls
// back to service-level when the local file is missing.
func TestRunCase_ScenarioIncludeServiceLevelFallback(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: shared.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: service-level fallback
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/service-level
        content: hi
`)
	writeServiceLevelSibling(t, caseDir, "shared.yml", `- name: service-level
  module: core.file.present
  params:
    path: /tmp/service-level
    content: "${ input.greeting }"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (service-level fallback), FAIL: %v", results[0].Failures)
	}
}

func TestRunCase_ScenarioInclude(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: greet.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: scenario include splice
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`)
	writeScenarioSibling(t, caseDir, "greet.yml", `- name: write greeting
  module: core.file.present
  params:
    path: /tmp/soul-stack-hello
    content: "${ input.greeting }"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS scenario-include splice, got FAIL: %v", results[0].Failures)
	}
}

func TestRunCase_ScenarioIncludeCycle(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: a.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: include cycle
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)
	writeScenarioSibling(t, caseDir, "a.yml", "- include: b.yml\n")
	writeScenarioSibling(t, caseDir, "b.yml", "- include: a.yml\n")

	_, err := Run(context.Background(), caseDir)
	if err == nil {
		t.Fatal("expected include-cycle error a->b->a, got nil")
	}
	if !strings.Contains(err.Error(), "include_cycle") {
		t.Fatalf("expected include_cycle in error, got: %v", err)
	}
}

const helloMain = `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - name: write greeting
    module: core.file.present
    params:
      path: /tmp/soul-stack-hello
      content: "${ input.greeting }"
`

func TestRunCase_Pass(t *testing.T) {
	caseDir := writeScenarioTree(t, helloMain, `name: hello pass
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS, got FAIL: %v", results[0].Failures)
	}
	// `${ input.greeting }` is one non-bool interpolation and must be covered.
	if got := len(results[0].Coverage.NonBranch) + len(results[0].Coverage.Branches); got == 0 {
		t.Errorf("expected non-zero trial coverage, got 0 expressions")
	}
}

func TestRunCase_FailOnParams(t *testing.T) {
	caseDir := writeScenarioTree(t, helloMain, `name: hello fail
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: WRONG
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL on content mismatch")
	}
	if len(results[0].Failures) == 0 {
		t.Fatalf("expected non-empty mismatch list")
	}
}

// TestRunCase_FailOnModule checks a module address mismatch.
func TestRunCase_FailOnModule(t *testing.T) {
	caseDir := writeScenarioTree(t, helloMain, `name: wrong module
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.absent
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL on module mismatch")
	}
}

// whereMain is a scenario with a where: predicate on soulprint.self for checking
// branch coverage. One host means the predicate is evaluated once.
const whereMain = `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - name: write only on linux
    module: core.file.present
    where: "soulprint.self.os.family == 'linux'"
    params:
      path: /tmp/soul-stack-hello
      content: "${ input.greeting }"
`

// TestRunCase_WhereBranchCovered checks that a case with where: that hit the
// truthy branch is included in branch coverage (one branch out of two).
func TestRunCase_WhereBranchCovered(t *testing.T) {
	caseDir := writeScenarioTree(t, whereMain, `name: where truthy
fixtures:
  input:
    greeting: hi
  soulprint:
    os:
      family: linux
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS, got: %v", results[0].Failures)
	}
	covered, total := results[0].Coverage.CoveredBranches()
	if total != 1 {
		t.Fatalf("expected 1 bool expression (where:), got %d", total)
	}
	if covered != 0 {
		t.Fatalf("one truthy branch does not cover the expression fully: covered=%d, expected 0", covered)
	}
}

// optionalStateMain is a scenario that reads optional-without-default input in
// state_changes.sets WITHOUT a has() guard. On the latest path (value not
// provided), sets rendering must fail with "no such key": the class of bugs the
// harness catches by rendering state_changes.
const optionalStateMain = `name: create
input:
  redis_version:
    type: string
    required: false
state_changes:
  sets:
    redis_version: "${ input.redis_version }"
tasks:
  - name: noop
    module: core.file.present
    params:
      path: /tmp/noop
      content: x
`

// TestRunCase_StateChangesRenderError checks that unguarded
// optional-without-default input in state_changes.sets causes a sets render
// error (RunCase returns err), not a silent PASS. This was a harness blind spot
// before expansion: tasks rendered, state_changes did not.
func TestRunCase_StateChangesRenderError(t *testing.T) {
	caseDir := writeScenarioTree(t, optionalStateMain, `name: state_changes render must fail
fixtures:
  input: {}
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)
	_, err := Run(context.Background(), caseDir)
	if err == nil {
		t.Fatal("expected state_changes render error (no such key on unguarded optional input), got nil")
	}
	if !strings.Contains(err.Error(), "state_changes") {
		t.Fatalf("error must point to state_changes, got: %v", err)
	}
}

// guardedStateMain is the same optional input, but with the canonical has()
// guard: sets rendering does not fail, and latest writes "" into state.
const guardedStateMain = `name: create
input:
  redis_version:
    type: string
    required: false
state_changes:
  sets:
    redis_version: "${ has(input.redis_version) ? input.redis_version : '' }"
tasks:
  - name: noop
    module: core.file.present
    params:
      path: /tmp/noop
      content: x
`

// TestRunCase_StateChangesAssertPass checks that has()-guarded optional renders
// as "" and matches assert.state_changes.
func TestRunCase_StateChangesAssertPass(t *testing.T) {
	caseDir := writeScenarioTree(t, guardedStateMain, `name: state_changes assert pass
fixtures:
  input: {}
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
  state_changes:
    redis_version: ""
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (guard -> \"\"), got: %v", results[0].Failures)
	}
}

// TestRunCase_StateChangesAssertFail checks that a mismatch between
// assert.state_changes and the rendered value fails the case.
func TestRunCase_StateChangesAssertFail(t *testing.T) {
	caseDir := writeScenarioTree(t, guardedStateMain, `name: state_changes assert fail
fixtures:
  input:
    redis_version: "7.2.4"
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
  state_changes:
    redis_version: "WRONG"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL on state_changes mismatch")
	}
	if len(results[0].Failures) == 0 {
		t.Fatal("expected non-empty mismatch list")
	}
}

// addUserStateMain is a scenario that accumulates state on top of existing
// state: state_changes.sets adds last_user from input without touching base
// users. Mirror of add_user operations over incarnation.state
// (orchestration.md section 7.1).
const addUserStateMain = `name: add_user
input:
  name:
    type: string
state_changes:
  sets:
    last_user: "${ input.name }"
tasks:
  - name: create user
    module: core.user.present
    params:
      name: "${ input.name }"
`

// TestRunCase_StateAfterPass checks assert.state_after against the FULL final
// state: base fixtures.state (users) + rendered sets (last_user). Mirror of the
// production mergeStateChanges(stateBefore, renderedSets) commit.
func TestRunCase_StateAfterPass(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user accumulates over base state
fixtures:
  input:
    name: bob
  state:
    users:
      - alice
assert:
  rendered_tasks:
    - index: 0
      module: core.user.present
  state_after:
    users:
      - alice
    last_user: bob
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (base.users + sets.last_user), got: %v", results[0].Failures)
	}
}

// TestRunCase_StateAfterFail checks that expected final state differing from
// actual (last_user in state_after does not match rendered value) fails the case.
func TestRunCase_StateAfterFail(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user state_after mismatch
fixtures:
  input:
    name: bob
  state:
    users:
      - alice
assert:
  rendered_tasks:
    - index: 0
      module: core.user.present
  state_after:
    users:
      - alice
    last_user: WRONG
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL on last_user mismatch in state_after")
	}
	if len(results[0].Failures) == 0 {
		t.Fatal("expected non-empty mismatch list")
	}
}

// TestRunCase_StateAfterFullCompare checks that state_after is compared FULLY
// (like L1): an extra key in the actual final state (base users not mentioned in
// expected state_after) is also a mismatch. This differs from partial
// state_changes checks.
func TestRunCase_StateAfterFullCompare(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user state_after must be complete
fixtures:
  input:
    name: bob
  state:
    users:
      - alice
assert:
  rendered_tasks:
    - index: 0
      module: core.user.present
  state_after:
    last_user: bob
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL: base users not mentioned in state_after, full comparison catches extra key")
	}
}

// assertMain is a scenario with an assert task (ADR-009 amendment 2026-06-23):
// render aborts if the roster host count != input.want_hosts.
const assertMain = `name: create
input:
  want_hosts:
    type: integer
    required: true
tasks:
  - name: topology guard
    assert:
      that:
        - "size(soulprint.hosts) == int(input.want_hosts)"
      message: "topology mismatch: hosts != want_hosts"
  - name: write marker
    module: core.file.present
    params:
      path: /tmp/soul-stack-marker
      content: ok
`

// TestRunCase_ExpectRenderError_Match checks that an assert failure aborts
// render; a case with expect_render_error matching the substring passes
// (ADR-023 amendment).
func TestRunCase_ExpectRenderError_Match(t *testing.T) {
	caseDir := writeScenarioTree(t, assertMain, `name: assert aborts render
fixtures:
  input:
    want_hosts: 3
  hosts:
    - { sid: a.example.com, covens: [create] }
    - { sid: b.example.com, covens: [create] }
expect_render_error: "topology mismatch"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (render aborted with substring): %v", results[0].Failures)
	}
}

// TestRunCase_ExpectRenderError_RenderSucceeds checks expect_render_error set
// while render SUCCEEDS (topology matches) -> FAIL (expected abort did not happen).
func TestRunCase_ExpectRenderError_RenderSucceeds(t *testing.T) {
	caseDir := writeScenarioTree(t, assertMain, `name: assert passes but error expected
fixtures:
  input:
    want_hosts: 2
  hosts:
    - { sid: a.example.com, covens: [create] }
    - { sid: b.example.com, covens: [create] }
expect_render_error: "topology mismatch"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL: render succeeded while the case expected an abort")
	}
}

// TestRunCase_ExpectRenderError_WrongSubstring checks render abort with a
// non-matching error substring -> FAIL (catches message substitution).
func TestRunCase_ExpectRenderError_WrongSubstring(t *testing.T) {
	caseDir := writeScenarioTree(t, assertMain, `name: assert aborts but wrong substring
fixtures:
  input:
    want_hosts: 5
  hosts:
    - { sid: a.example.com, covens: [create] }
expect_render_error: "completely different text"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL: render aborted, but substring did not match")
	}
}

// TestLoadCase_ExpectRenderErrorConflictsWithRenderedTasks checks
// expect_render_error and assert.rendered_tasks in one case: strict validation
// error (opposite outcomes).
func TestLoadCase_ExpectRenderErrorConflictsWithRenderedTasks(t *testing.T) {
	caseDir := writeScenarioTree(t, assertMain, `name: conflict
fixtures:
  input:
    want_hosts: 2
expect_render_error: "topology mismatch"
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)
	_, _, err := LoadCase(caseDir)
	if err == nil {
		t.Fatal("expected validation error: expect_render_error XOR assert.rendered_tasks")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error is not about mutual exclusion: %v", err)
	}
}

// TestRun_MixedL0L2Tree recursively runs a tree with L0 and L2 cases: L0
// executes, L2 (stand:/verify: marker) is skipped with Skipped, and strict decode
// of the L2 case does not crash. Regression for a mixed examples/ tree.
func TestRun_MixedL0L2Tree(t *testing.T) {
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")

	// L0 case: executed by the regular L0 pipeline.
	l0Dir := filepath.Join(scnDir, "tests", "l0")
	if err := os.MkdirAll(l0Dir, 0o755); err != nil {
		t.Fatalf("mkdir l0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(helloMain), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(l0Dir, caseFileName), []byte(`name: l0 pass
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`), 0o644); err != nil {
		t.Fatalf("write l0 case: %v", err)
	}

	// L2 case: carries stand:/verify: plus fields that would make L0 strict
	// decode fail (description, expect_idempotent). Must be recognized as L2 and
	// skipped.
	l2Dir := filepath.Join(scnDir, "tests", "l2")
	if err := os.MkdirAll(l2Dir, 0o755); err != nil {
		t.Fatalf("mkdir l2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(l2Dir, caseFileName), []byte(`name: l2 stand
description: |
  This case runs on the stand; the MVP harness does not run it.
stand:
  driver: docker
  image: ubuntu:24.04
input:
  action: apply
expect_idempotent: true
verify:
  - name: ping
    expect:
      stdout: PONG
`), 0o644); err != nil {
		t.Fatalf("write l2 case: %v", err)
	}

	results, err := Run(context.Background(), root)
	if err != nil {
		t.Fatalf("Run on mixed tree must not fail on L2 case: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (L0 + L2), got %d", len(results))
	}

	var sawL0Pass, sawL2Skip bool
	for _, r := range results {
		switch {
		case r.Skipped:
			sawL2Skip = true
			if !strings.Contains(r.Case, "l2") {
				t.Errorf("expected skipped result to be the L2 case, got %q", r.Case)
			}
		default:
			sawL0Pass = r.Pass
			if !r.Pass {
				t.Errorf("L0 case must pass, FAIL: %v", r.Failures)
			}
		}
	}
	if !sawL0Pass {
		t.Fatal("expected executed (not skipped) L0 case")
	}
	if !sawL2Skip {
		t.Fatal("expected skipped L2 case")
	}
}

// TestRun_L2OnlyVerifyMarker checks that a case with only verify: (without
// stand:) is also recognized as L2 and skipped (either marker is enough).
func TestRun_L2OnlyVerifyMarker(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: verify only
verify:
  - name: ping
    expect:
      stdout: PONG
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	results, err := Run(context.Background(), file)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped {
		t.Fatalf("expected 1 skipped L2 case, got %+v", results)
	}
}

// TestRun_L0WithUnknownField_StillErrors is the central L2-skip invariant: an
// L0 case with an unknown field (typo) and WITHOUT stand:/verify: markers passes
// soft pre-parse isL2Case as NOT-L2 and must fail strict decoding in LoadCase,
// rather than silently slipping through as "not-L2 -> strict did not run". The
// run goes strictly through the new Run->isL2Case->LoadCase path: error is
// propagated, and the case is NOT marked Skipped.
func TestRun_L0WithUnknownField_StillErrors(t *testing.T) {
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")
	caseDir := filepath.Join(scnDir, "tests", "l0")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(helloMain), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	// Top-level typo (assertt instead of assert) + no stand:/verify:.
	if err := os.WriteFile(filepath.Join(caseDir, caseFileName), []byte(`name: l0 typo
fixtures:
  input:
    greeting: hi
assertt:
  rendered_tasks:
    - index: 0
      module: core.file.present
`), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}

	results, err := Run(context.Background(), root)
	if err == nil {
		t.Fatal("expected strict-decode error on unknown-field L0 case (without stand/verify), got nil")
	}
	for _, r := range results {
		if r.Skipped {
			t.Fatalf("L0 case with typo must not be skipped as L2: %q", r.Case)
		}
	}
}

// TestRun_NonMapTopLevel_Errors checks case.yml with non-map top-level
// (scalar/list): soft pre-parse isL2Case cannot decode it into a map -> error,
// and Run propagates it instead of silently skipping the case.
func TestRun_NonMapTopLevel_Errors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte("- just a list, not a case map\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	results, err := Run(context.Background(), file)
	if err == nil {
		t.Fatal("expected pre-parse error on non-map top-level, got nil")
	}
	for _, r := range results {
		if r.Skipped {
			t.Fatalf("non-map case must not be skipped: %q", r.Case)
		}
	}
}

func TestLoadCase_RejectsUnknownSection(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: x
fixtures:
  input: {}
assert:
  dispatch:
    - task: 0
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := LoadCase(file); err == nil {
		t.Fatalf("expected strict-decode error on assert.dispatch (TODO section)")
	}
}

func TestLoadCase_RejectsEmptyAssert(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: x
fixtures:
  input: {}
assert:
  rendered_tasks: []
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := LoadCase(file); err == nil {
		t.Fatalf("expected error on empty assert.rendered_tasks")
	}
}

// vaultMain is a scenario with vault:-ref in params to check that fixtures.vault
// feeds the render pipeline hermetically (without starting Vault).
const vaultMain = `name: create
input: {}
tasks:
  - name: write secret-derived content
    module: core.file.present
    params:
      path: /tmp/soul-stack-secret
      content: "vault:secret/app/cfg#token"
`

// TestRunCase_VaultRef checks that fixtures.vault resolves into params through
// fixture-backed KVReader (hermetic, without Vault).
func TestRunCase_VaultRef(t *testing.T) {
	caseDir := writeScenarioTree(t, vaultMain, `name: vault ref
fixtures:
  vault:
    "secret/app/cfg":
      token: abc123
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-secret
        content: abc123
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (vault-ref resolved to abc123), got: %v", results[0].Failures)
	}
}

// celVaultMain is a scenario with CEL function vault() (NOT vault:-ref) in
// params. vault() resolves keeper-side in the CEL render phase through
// fixtureVault (the same hermetic reader as for vault:-ref). Closes a QA gap:
// L0 covered only vault:-ref, not CEL vault().
const celVaultMain = `name: create
input: {}
tasks:
  - name: write secret via CEL vault()
    module: core.file.present
    params:
      path: /tmp/soul-stack-secret
      content: "${ vault('secret/app/cfg#token') }"
`

// TestRunCase_CELVaultFunc checks fixtures.vault resolving through CEL function
// vault() (#field form) deterministically into the real secret value in L0
// (soul-trial).
func TestRunCase_CELVaultFunc(t *testing.T) {
	caseDir := writeScenarioTree(t, celVaultMain, `name: cel vault()
fixtures:
  vault:
    "secret/app/cfg":
      token: abc123
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-secret
        content: abc123
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (CEL vault() resolved to abc123), got: %v", results[0].Failures)
	}
}

// vaultNoLogMain is a scenario with vault-ref in params of a task marked
// no_log: on FAIL the value must not leak into the diff.
const vaultNoLogMain = `name: create
input: {}
tasks:
  - name: write secret-derived content
    module: core.file.present
    no_log: true
    params:
      path: /tmp/soul-stack-secret
      content: "vault:secret/app/cfg#token"
`

// TestRunCase_FailNoLogMasksSecret checks a task with no_log: true and FAIL by
// params: the diff masks values (prints only keys), and the raw secret abc123
// does not appear in Failures.
func TestRunCase_FailNoLogMasksSecret(t *testing.T) {
	caseDir := writeScenarioTree(t, vaultNoLogMain, `name: vault no_log fail
fixtures:
  vault:
    "secret/app/cfg":
      token: abc123
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-secret
        content: WRONG
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL on content mismatch")
	}
	if len(results[0].Failures) == 0 {
		t.Fatalf("expected non-empty mismatch list")
	}
	for _, f := range results[0].Failures {
		if strings.Contains(f, "abc123") {
			t.Fatalf("secret abc123 leaked into no_log task diff: %q", f)
		}
	}
}

// TestCompareParams_NoLogMask is a masking unit test: when noLog=true, diff
// contains no values, only keys (and conversely when noLog=false).
func TestCompareParams_NoLogMask(t *testing.T) {
	got, err := structpb.NewStruct(map[string]any{"content": "abc123"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	want := map[string]any{"content": "WRONG"}

	masked := compareParams(0, want, got, true)
	if masked == "" {
		t.Fatalf("expected diff (values differ)")
	}
	if strings.Contains(masked, "abc123") || strings.Contains(masked, "WRONG") {
		t.Fatalf("no_log diff must not contain values: %q", masked)
	}
	if !strings.Contains(masked, "content") {
		t.Fatalf("no_log diff must print keys: %q", masked)
	}

	open := compareParams(0, want, got, false)
	if !strings.Contains(open, "abc123") {
		t.Fatalf("without no_log, diff must show values: %q", open)
	}
}

// writeApplyDestinyTree builds a hermetic tree for an L0 case with apply:destiny:
//
//	<root>/service.yml                       — declares destiny[] dep
//	<root>/scenario/create/main.yml          — scenario with apply: { destiny: <dst> }
//	<root>/scenario/create/tests/c1/case.yml — case with fixtures.soulprint + default_destiny_source
//	<root>/destiny-<dst>/{destiny.yml,tasks/main.yml} — destiny reading soulprint.self
//
// Returns the case directory path (for Run). serviceRootFor(case.yml) == <root>,
// so service.yml and the destiny directory resolve relative to it.
func writeApplyDestinyTree(t *testing.T, dst, mainYML, caseYML, destinyYML, destinyTasks string) string {
	t.Helper()
	root := t.TempDir()
	caseDir := filepath.Join(root, "scenario", "create", "tests", "c1")
	dstTasksDir := filepath.Join(root, "destiny-"+dst, "tasks")
	for _, d := range []string{caseDir, dstTasksDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "service.yml"):                    "name: arch-svc\nstate_schema_version: 1\nstate_schema:\n  type: object\n  properties: {}\ndestiny:\n  - { name: " + dst + ", ref: v1.0.0 }\n",
		filepath.Join(root, "scenario", "create", "main.yml"): mainYML,
		filepath.Join(caseDir, caseFileName):                  caseYML,
		filepath.Join(root, "destiny-"+dst, "destiny.yml"):    destinyYML,
		filepath.Join(dstTasksDir, "main.yml"):                destinyTasks,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return caseDir
}

// TestRunCase_ApplyDestinySelfArch is an L0 guard for relaxing the invariant
// (ADR-009/010 amendment): destiny rendered through the apply:destiny scenario
// wrapper sees injected fixtures.soulprint.self.os.arch (synthetic arm64).
// Proves that the target host self flows into the destiny pass and into L0 (the
// same renderApplyDestiny as in production): soulprint.self in destiny-CEL
// renders with fixtures.soulprint.
func TestRunCase_ApplyDestinySelfArch(t *testing.T) {
	caseDir := writeApplyDestinyTree(t, "arch-aware",
		`name: create
tasks:
  - name: apply arch-aware destiny
    apply:
      destiny: arch-aware
      input: {}
`,
		`name: apply destiny self.arch
fixtures:
  default_destiny_source: file://destiny-{name}
  soulprint:
    os:
      family: debian
      arch: arm64
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "install --arch arm64"
`,
		"name: arch-aware\n",
		`- name: fetch by arch
  module: core.exec.run
  params:
    cmd: "install --arch ${ soulprint.self.os.arch }"
`,
	)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (soulprint.self.os.arch=arm64 in L0 destiny pass), got: %v", results[0].Failures)
	}
}

// TestFixtureVault checks fixture-vault resolving vault-ref hermetically and
// normalizing logical/relative path forms.
func TestFixtureVault(t *testing.T) {
	fv := newFixtureVault(map[string]map[string]any{
		"secret/db/cred": {"password": "s3cret"},
	})
	for _, path := range []string{"secret/db/cred", "db/cred", "/secret/db/cred"} {
		got, err := fv.ReadKV(context.Background(), path)
		if err != nil {
			t.Fatalf("ReadKV(%q): %v", path, err)
		}
		if got["password"] != "s3cret" {
			t.Errorf("ReadKV(%q) password = %v", path, got["password"])
		}
	}
	if _, err := fv.ReadKV(context.Background(), "secret/missing"); err == nil {
		t.Fatalf("expected ErrVaultKVNotFound for missing secret")
	}
}

// presenceMain is a scenario with TWO tasks of one module (core.file.present)
// with different register/path and one unique task (core.service.running): a
// stress case for the presence matcher where matching by module creates a
// collision disambiguated by id(register)/params_subset.
const presenceMain = `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - name: write alpha
    module: core.file.present
    register: alpha_file
    params:
      path: /tmp/alpha
      content: "${ input.greeting }"
  - name: write beta
    module: core.file.present
    register: beta_file
    params:
      path: /tmp/beta
      content: "${ input.greeting }"
  - name: run service
    module: core.service.running
    params:
      name: redis-server
      enabled: true
`

// TestRunCase_PresencePass is positive: task_present finds tasks by module +
// params_subset (partial subset of params), and task_absent passes on an
// uncalled module. Proves the basic match and coexistence with positional form
// (rendered_tasks is not set here; the plan is asserted only by presence).
func TestRunCase_PresencePass(t *testing.T) {
	caseDir := writeScenarioTree(t, presenceMain, `name: presence pass
fixtures:
  input:
    greeting: hi
assert:
  task_present:
    - module: core.file.present
      id: alpha_file
      params_subset:
        path: /tmp/alpha
    - module: core.service.running
      params_subset:
        name: redis-server
  task_absent:
    - module: core.pkg.installed
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS, got: %v", results[0].Failures)
	}
}

// TestRunCase_PresenceNotFound is negative: task_present by nonexistent
// params_subset (path does not match any task) -> FAIL with "not found".
func TestRunCase_PresenceNotFound(t *testing.T) {
	caseDir := writeScenarioTree(t, presenceMain, `name: presence not found
fixtures:
  input:
    greeting: hi
assert:
  task_present:
    - module: core.file.present
      params_subset:
        path: /tmp/does-not-exist
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL: task_present must not be found")
	}
	if !strings.Contains(strings.Join(results[0].Failures, "\n"), "not found") {
		t.Fatalf("expected \"not found\" mismatch, got: %v", results[0].Failures)
	}
}

// TestRunCase_PresenceAbsentViolated is negative: task_absent on a module that
// EXISTS in the plan and matches by params_subset -> FAIL.
func TestRunCase_PresenceAbsentViolated(t *testing.T) {
	caseDir := writeScenarioTree(t, presenceMain, `name: presence absent violated
fixtures:
  input:
    greeting: hi
assert:
  task_present:
    - module: core.service.running
      params_subset:
        name: redis-server
  task_absent:
    - module: core.file.present
      params_subset:
        path: /tmp/alpha
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL: task_absent violated (task is present)")
	}
	if !strings.Contains(strings.Join(results[0].Failures, "\n"), "expected absence") {
		t.Fatalf("expected \"expected absence\" mismatch, got: %v", results[0].Failures)
	}
}

// TestRunCase_PresenceCollision checks collision: task_present by module without
// disambiguator matches BOTH core.file.present tasks (>1 match) -> FAIL with a
// hint to add id/register or narrow params_subset.
func TestRunCase_PresenceCollision(t *testing.T) {
	caseDir := writeScenarioTree(t, presenceMain, `name: presence collision
fixtures:
  input:
    greeting: hi
assert:
  task_present:
    - module: core.file.present
      params_subset:
        content: hi
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL: collision (2 matches on task_present)")
	}
	if !strings.Contains(strings.Join(results[0].Failures, "\n"), "collision") {
		t.Fatalf("expected \"collision\" mismatch, got: %v", results[0].Failures)
	}
}

// TestRunCase_PresenceCollisionResolved checks the same collision disambiguated
// by id(register) -> exactly one match -> PASS. Proves register union id removes
// the collision.
func TestRunCase_PresenceCollisionResolved(t *testing.T) {
	caseDir := writeScenarioTree(t, presenceMain, `name: presence collision resolved
fixtures:
  input:
    greeting: hi
assert:
  task_present:
    - module: core.file.present
      id: beta_file
      params_subset:
        content: hi
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (collision resolved by id=beta_file), got: %v", results[0].Failures)
	}
}

// skipBranchMain is a scenario with a disableable branch: task
// core.pkg.installed under static-false when (`when: input.enabled`, with
// enabled=false -> static-when skip-placeholder, ADR-012(d) Variant b) plus
// always-active core.service.running. One module (core.pkg.installed) lives ONLY
// in the disabled branch: stress for skip-placeholder matching where bare-module
// presence/absence must not trigger on skip. when is static (no
// register/soulprint), so Keeper emits a placeholder with Params=nil and does
// not render params.
const skipBranchMain = `name: create
input:
  enabled:
    type: boolean
    required: true
tasks:
  - name: install optional pkg
    module: core.pkg.installed
    when: input.enabled
    params:
      name: nginx
  - name: run service
    module: core.service.running
    params:
      name: redis-server
      enabled: true
`

// TestRunCase_PresenceAbsentOnSkippedBranch is a GUARD (MAJOR bug): disabled
// branch (when:false -> skip-placeholder) + task_absent ONLY with that branch's
// module. Skip = "not called" -> NOT presence -> task_absent passes. Before the
// fix, placeholder kept Module with Params=nil, causing false FAIL "task found".
func TestRunCase_PresenceAbsentOnSkippedBranch(t *testing.T) {
	caseDir := writeScenarioTree(t, skipBranchMain, `name: absent on skipped branch
fixtures:
  input:
    enabled: false
assert:
  task_absent:
    - module: core.pkg.installed
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS: skip-placeholder must not count as presence, got: %v", results[0].Failures)
	}
}

// TestRunCase_PresenceNotFoundOnSkippedBranch is a GUARD: same skip +
// task_present with bare-module of the skipped branch -> NOT FOUND (skip is not
// present). Before the fix, bare-module could falsely pass on placeholder.
func TestRunCase_PresenceNotFoundOnSkippedBranch(t *testing.T) {
	caseDir := writeScenarioTree(t, skipBranchMain, `name: present on skipped branch
fixtures:
  input:
    enabled: false
assert:
  task_present:
    - module: core.pkg.installed
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL: skip-placeholder must not match task_present")
	}
	if !strings.Contains(strings.Join(results[0].Failures, "\n"), "not found") {
		t.Fatalf("expected \"not found\" mismatch, got: %v", results[0].Failures)
	}
}

// TestRunCase_PresenceWhenDisambiguatorPositive checks disambiguation via when:
// (only when, no id): with enabled=true the branch is ACTIVE (real task with
// Params), and when-disambiguator addresses exactly it -> one match -> PASS.
func TestRunCase_PresenceWhenDisambiguatorPositive(t *testing.T) {
	caseDir := writeScenarioTree(t, skipBranchMain, `name: when disambiguator positive
fixtures:
  input:
    enabled: true
assert:
  task_present:
    - module: core.pkg.installed
      when: input.enabled
      params_subset:
        name: nginx
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (when-disambiguator addresses active task), got: %v", results[0].Failures)
	}
}

// TestRunCase_PresenceWhenDisambiguatorNegative is a negative case for
// when-disambiguator: assert when does not match task when -> no match -> NOT
// FOUND. Proves when narrows the match (is not ignored).
func TestRunCase_PresenceWhenDisambiguatorNegative(t *testing.T) {
	caseDir := writeScenarioTree(t, skipBranchMain, `name: when disambiguator negative
fixtures:
  input:
    enabled: true
assert:
  task_present:
    - module: core.pkg.installed
      when: input.other
      params_subset:
        name: nginx
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("expected FAIL: assert when does not match task when, match must not fire")
	}
	if !strings.Contains(strings.Join(results[0].Failures, "\n"), "not found") {
		t.Fatalf("expected \"not found\" mismatch, got: %v", results[0].Failures)
	}
}

// TestRunCase_PresencePlusPositional checks presence + positional
// rendered_tasks coexistence in ONE case: both checks are independent and must
// pass. Pins the stated property (compareTaskPresence and compareRenderedTasks
// are appended independently in RunCase). The skip branch is disabled
// (enabled=false): positional check sees skip-placeholder at index 0 (module
// preserved), presence does NOT fire on it (Params=nil): two contracts on one
// plan.
func TestRunCase_PresencePlusPositional(t *testing.T) {
	caseDir := writeScenarioTree(t, skipBranchMain, `name: presence plus positional
fixtures:
  input:
    enabled: false
assert:
  rendered_tasks:
    - index: 0
      module: core.pkg.installed
    - index: 1
      module: core.service.running
      params:
        name: redis-server
  task_present:
    - module: core.service.running
      params_subset:
        name: redis-server
  task_absent:
    - module: core.pkg.installed
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (presence + positional are independent), got: %v", results[0].Failures)
	}
}
