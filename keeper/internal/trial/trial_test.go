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

// optionalStateMain is a scenario whose capture step reads an
// optional-without-default input WITHOUT a has() guard. On the latest path (the
// value not provided), rendering that step must fail with "no such key".
const optionalStateMain = `name: create
input:
  redis_version:
    type: string
    required: false
tasks:
  - name: noop
    module: core.file.present
    params:
      path: /tmp/noop
      content: x
  - name: record the version
    module: core.state.set
    params:
      field: redis_version
      value: "${ input.redis_version }"
`

// TestRunCase_CaptureRenderError checks that an unguarded
// optional-without-default input in a capture's params: aborts the render
// (RunCase returns err) instead of passing silently. A capture is a task, so
// this is the same guarantee the harness gives any other task's params — the
// blind spot the retired `state_changes:` block had cannot come back.
func TestRunCase_CaptureRenderError(t *testing.T) {
	caseDir := writeScenarioTree(t, optionalStateMain, `name: capture render must fail
fixtures:
  input: {}
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)
	_, err := Run(context.Background(), caseDir)
	if err == nil {
		t.Fatal("expected a render error (no such key on unguarded optional input), got nil")
	}
	if !strings.Contains(err.Error(), "redis_version") {
		t.Fatalf("error must name the missing key, got: %v", err)
	}
}

// guardedStateMain is the same optional input, but with the canonical has()
// guard: sets rendering does not fail, and latest writes "" into state.
const guardedStateMain = `name: create
input:
  redis_version:
    type: string
    required: false
tasks:
  - name: noop
    module: core.file.present
    params:
      path: /tmp/noop
      content: x
  - name: record the version
    module: core.state.set
    params:
      field: redis_version
      value: "${ has(input.redis_version) ? input.redis_version : '' }"
`

// TestRunCase_GuardedOptionalCapturedAsEmpty checks that a has()-guarded optional
// renders as "" and that the capture step commits that "" — the value is asserted
// through the state the run produced ([ADR-0084]), not through a projection of a
// block that no longer exists.
func TestRunCase_GuardedOptionalCapturedAsEmpty(t *testing.T) {
	caseDir := writeScenarioTree(t, guardedStateMain, `name: guarded optional captured as empty
fixtures:
  input: {}
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
  state_after:
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

// TestRunCase_GuardedOptionalMismatchFails — the same scenario with the optional
// PRESENT: the guard passes the input through, so a case naming anything else
// fails.
func TestRunCase_GuardedOptionalMismatchFails(t *testing.T) {
	caseDir := writeScenarioTree(t, guardedStateMain, `name: guarded optional mismatch
fixtures:
  input:
    redis_version: "7.2.4"
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
  state_after:
    redis_version: "WRONG"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL on the captured redis_version")
	}
	if len(results[0].Failures) == 0 {
		t.Fatal("expected non-empty mismatch list")
	}
}

// addUserStateMain is a scenario that accumulates state on top of existing
// state: a capture step records last_user from input without touching base
// users. Mirror of add_user operations over incarnation.state
// (orchestration.md section 7.1).
const addUserStateMain = `name: add_user
input:
  name:
    type: string
tasks:
  - name: create user
    module: core.user.present
    params:
      name: "${ input.name }"
  - name: record the user
    module: core.state.set
    params:
      field: last_user
      value: "${ input.name }"
`

// TestRunCase_StateAfterPass checks assert.state_after against the final state:
// base fixtures.state (users) + what the capture step wrote (last_user). Mirror
// of the production merge, which the harness reaches through the same
// [stateop.Merge] ([ADR-0084] F-C).
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
		t.Fatalf("expected PASS (base.users + the captured last_user), got: %v", results[0].Failures)
	}
}

// TestRunCase_StateAfterFail checks that an expected final state differing from
// the actual one (last_user in state_after does not match the captured value)
// fails the case.
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

// TestRunCase_StateAfterSubset is the C4 semantics of [ADR-0084] F-C: the case
// names ONE field and the run also produces `users`, carried over from
// fixtures.state — and the case still passes. Before C4 this same case failed on
// the unmentioned key.
//
// The point is not leniency. Whole-state equality forces a case to restate
// fields it has no opinion about, and such a case gets updated by pasting in
// whatever the run produced, which asserts nothing. What keeps the subset honest
// is TestRunCase_StateAfterMissingField below: a named field still has to be
// there.
func TestRunCase_StateAfterSubset(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user state_after names one field
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
	if !results[0].Pass {
		t.Fatalf("expected PASS: state_after is a subset, `users` is a field the case does not assert; got: %v", results[0].Failures)
	}
}

// TestRunCase_StateAfterMissingField is the floor under the subset: a field the
// case names but the run never writes is still a failure. Without this the
// subset form would pass a case asserting a field that does not exist at all,
// and every typo in a field name would become a silent green.
func TestRunCase_StateAfterMissingField(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user state_after names an unwritten field
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
    last_operator: bob
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL: state_after names last_operator, which the run never writes")
	}
	if !strings.Contains(strings.Join(results[0].Failures, "\n"), "last_operator") {
		t.Fatalf("expected the failure to name the absent field, got: %v", results[0].Failures)
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

// TestCompareParams_PrintsValues pins the removal of the `no_log` masking branch
// ([ADR-0083] §8): a param diff prints both sides. Trial is offline — the only
// plaintext it can print is a `fixtures.vault` literal from the case file — and a
// diff that names the key but hides both values is unusable for the developer it
// is written for.
func TestCompareParams_PrintsValues(t *testing.T) {
	got, err := structpb.NewStruct(map[string]any{"content": "abc123"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	want := map[string]any{"content": "WRONG"}

	diff := compareParams(0, want, got)
	if diff == "" {
		t.Fatalf("expected diff (values differ)")
	}
	if !strings.Contains(diff, "content") {
		t.Fatalf("diff must name the key: %q", diff)
	}
	for _, v := range []string{"abc123", "WRONG"} {
		if !strings.Contains(diff, v) {
			t.Fatalf("diff must print %q, got: %q", v, diff)
		}
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
		filepath.Join(root, "service.yml"):                    "state_schema: {}\ndestiny:\n  - { name: " + dst + ", ref: v1.0.0 }\n",
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

// captureStateMain is a scenario that writes state the way [ADR-0084] means it
// to be written: `core.state.<verb>` steps, at the step, instead of an
// end-of-run `state_changes` block. `set` overwrites, `present` keeps what is
// already there, `unset` drops the field itself.
const captureStateMain = `name: capture
input:
  name:
    type: string
tasks:
  - name: create user
    module: core.user.present
    params:
      name: "${ input.name }"
  - name: record the last user
    module: core.state.set
    params:
      field: last_user
      value: "${ input.name }"
  - name: keep the owner as first written
    module: core.state.present
    params:
      field: owner
      value: "${ input.name }"
  - name: drop the migration flag
    module: core.state.unset
    params:
      field: migrating
`

// TestRunCase_StateAfterThreadsCaptureSteps — the capture steps of the plan must
// reach `assert.state_after`. Their writes land at their own step ([ADR-0084]),
// nowhere near the retired `state_changes` block this scenario deliberately does
// not have; a harness that only merged that block would report the untouched
// fixture and pass every case that asserts a capture.
//
// `present` over an occupied field is the discriminating one: it proves the
// harness runs the VERB rather than assigning the value.
func TestRunCase_StateAfterThreadsCaptureSteps(t *testing.T) {
	caseDir := writeScenarioTree(t, captureStateMain, `name: capture steps reach state_after
fixtures:
  input:
    name: bob
  state:
    users:
      - alice
    owner: alice
    migrating: true
assert:
  task_present:
    - module: core.state.set
  state_after:
    users:
      - alice
    last_user: bob
    owner: alice
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (set writes last_user, present keeps owner=alice), got: %v", results[0].Failures)
	}
}

// gatedCaptureMain — a multi-action scenario: one plan, and the capture belongs to
// exactly one of the actions. A static `when:` is the supported spelling for that
// ([ADR-0084] F-D), so a case for a DIFFERENT action renders the capture as a skip
// placeholder.
const gatedCaptureMain = `name: create
input:
  action:
    type: string
    required: true
tasks:
  - name: create user
    module: core.user.present
    params:
      name: alice
  - name: record the owner
    module: core.state.set
    when: "input.action == 'create'"
    params:
      field: owner
      value: alice
`

// TestRunCase_StaticallySkippedCaptureIsNotAnOp — ★ a capture the plan statically
// skipped writes nothing, so it is not an op. The skip placeholder keeps the task's
// module address and carries NO params (the render never ran), so a fold that selects
// on the module address alone hands [stateop.CheckParams] an empty map and the case
// dies on `param "field": missing` — a scenario-wide abort, on every case of the
// other action, including the ones asserting no state at all.
func TestRunCase_StaticallySkippedCaptureIsNotAnOp(t *testing.T) {
	t.Run("the gated-off action", func(t *testing.T) {
		caseDir := writeScenarioTree(t, gatedCaptureMain, `name: rotate leaves the capture out
fixtures:
  input:
    action: rotate
assert:
  task_absent:
    - module: core.state.set
`)
		results, err := Run(context.Background(), caseDir)
		if err != nil {
			t.Fatalf("Run: %v -- a skipped capture must not be folded as an op", err)
		}
		if !results[0].Pass {
			t.Fatalf("expected PASS, got: %v", results[0].Failures)
		}
	})

	t.Run("the action that owns it", func(t *testing.T) {
		caseDir := writeScenarioTree(t, gatedCaptureMain, `name: create runs the capture
fixtures:
  input:
    action: create
assert:
  task_present:
    - module: core.state.set
  state_after:
    owner: alice
`)
		results, err := Run(context.Background(), caseDir)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !results[0].Pass {
			t.Fatalf("expected PASS (the capture renders and writes owner), got: %v", results[0].Failures)
		}
	})
}

// TestRunCase_StateAbsentAfterUnset — `core.state.unset` drops the field, and
// `assert.state_absent` is the only section that can say so: state_after is a
// subset, where a field nobody names is a field nobody has an opinion about.
func TestRunCase_StateAbsentAfterUnset(t *testing.T) {
	caseDir := writeScenarioTree(t, captureStateMain, `name: unset removes the field
fixtures:
  input:
    name: bob
  state:
    migrating: true
assert:
  task_present:
    - module: core.state.unset
  state_absent:
    - migrating
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (unset dropped `migrating`), got: %v", results[0].Failures)
	}
}

// TestRunCase_StateAbsentStillPresent — the other direction: a field the run
// leaves in place must FAIL the section, and the message must carry the value,
// or the author cannot tell a field that survived from one that was blanked.
func TestRunCase_StateAbsentStillPresent(t *testing.T) {
	caseDir := writeScenarioTree(t, captureStateMain, `name: state_absent names a field the run keeps
fixtures:
  input:
    name: bob
  state:
    owner: alice
assert:
  task_present:
    - module: core.state.present
  state_absent:
    - owner
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL: `owner` is still in the state, state_absent named it")
	}
	if !strings.Contains(results[0].Failures[0], "state_absent.owner") ||
		!strings.Contains(results[0].Failures[0], "alice") {
		t.Fatalf("the failure must name the field AND the value it still holds, got: %v", results[0].Failures)
	}
}

// TestRunCase_StateAbsentNullIsNotGone — absence is strict KEY absence. A field
// left standing with a null value is a different state from a field that is not
// there, and the section must not conflate them: an author who meant "blanked"
// writes it in state_after, an author who wrote state_absent meant the key.
// Reported WITH the value, or the message reads as if the field were missing and
// the author has nothing to go on.
func TestRunCase_StateAbsentNullIsNotGone(t *testing.T) {
	caseDir := writeScenarioTree(t, captureStateMain, `name: a null field is not an absent field
fixtures:
  input:
    name: bob
  state:
    blanked: null
assert:
  task_present:
    - module: core.state.set
  state_absent:
    - blanked
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("expected FAIL: `blanked` is still a key in the state, holding null -- state_absent means the key is gone")
	}
	if !strings.Contains(results[0].Failures[0], "state_absent.blanked") ||
		!strings.Contains(results[0].Failures[0], "<nil>") {
		t.Fatalf("the failure must name the field AND show the null it still holds, got: %v", results[0].Failures)
	}
}
