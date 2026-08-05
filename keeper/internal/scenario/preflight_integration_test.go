//go:build integration

// Integration tests for the pre-flight assert gate (ADR-009/ADR-027 amendment
// 2026-06-23, form A): [Runner.PreflightAssert] evaluates a create scenario's
// assert predicates ON RUN CREATION (request path), BEFORE the incarnation is
// committed. Use case — redis cluster topology size-guard (connected-souls
// count must match shards*(1+replicas_per_shard)).
//
// Via testcontainers PG (shared harness in integration_test.go): seed a roster
// on the incarnation + a local-fs service repo with a cluster size-guard
// assert. A non-matching roster → render.ErrAssertFailed (caller handler →
// 422); a matching one → nil (create proceeds).
//
// WHAT THESE TESTS COVER SINCE NIM-124. The seeded-roster tests
// ([seedIncarnationRoster]) exercise the assert side of pre-flight — evaluation
// against the resolved roster, include expansion, `when:` gating — on an
// incarnation whose row and membership already exist.
//
// The bootstrap-create ordering is covered separately, and by
// [ResolveCreatePlan] rather than PreflightAssert alone, since the handler's
// entry point is what decides whether the gate runs at all. Since NIM-124 the
// roster resolves through `incarnation_membership`, which FKs the incarnation,
// so a roster CANNOT exist before Create — which made every create carrying a
// topology assert resolve an empty roster and 422 unconditionally (NIM-235).
// Those asserts are now deferred to the render fail-safe; the asserts that read
// only input/vars keep their pre-flight 422.

package scenario

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// clusterAssertServiceRepo is a service repo with a create scenario carrying a
// cluster topology size-guard via assert: (same invariant as
// examples/service/redis/scenario/create/cluster.yml). input.shards /
// input.replicas_per_shard are ints with defaults; assert is active only when
// redis_type==cluster (gated by when:, as in prod).
func clusterAssertServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: redis-cluster-guard
state_schema_version: 1
description: cluster topology pre-flight assert test service
state_schema:
  type: object
  properties: {}
`)
	write("scenario/create/main.yml", `name: create
description: cluster topology size-guard via assert
state_changes: {}
input:
  redis_type:
    type: string
    default: cluster
  shards:
    type: integer
    default: 3
  replicas_per_shard:
    type: integer
    default: 1
tasks:
  - name: cluster topology matches shards*(1+replicas)
    when: "input.redis_type == 'cluster'"
    assert:
      that:
        - "size(soulprint.hosts) == int(input.shards) * (1 + int(input.replicas_per_shard))"
      message: "topology mismatch: hosts != shards*(1+replicas_per_shard)"
  - name: Echo on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init cluster-guard", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_PreflightAssert_TopologyMismatch_Fails — the roster does NOT
// match (4 members vs. the expected shards=1*(1+1)=2) → PreflightAssert →
// render.ErrAssertFailed with message "topology mismatch".
func TestIntegration_PreflightAssert_TopologyMismatch_Fails(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationRoster(t, "redis-new",
		"a.example.com", "b.example.com", "c.example.com", "d.example.com")
	gitURL := clusterAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-new",
		ServiceRef:      artifact.ServiceRef{Name: "redis-cluster-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // expects 2, roster 4
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("PreflightAssert: a non-converging topology must give an error, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err is not ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "topology mismatch") {
		t.Errorf("error does not carry the author message: %v", err)
	}
}

// TestIntegration_PreflightAssert_TopologyMatches_Passes — the roster matches
// (2 members == shards=1*(1+1)) → PreflightAssert → nil (create proceeds).
// Also the no-false-positive counterpart of the mismatch test: it proves the
// assert reads the ACTUAL roster size, not a constant — the mismatch case would
// pass on an empty roster too.
func TestIntegration_PreflightAssert_TopologyMatches_Passes(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationRoster(t, "redis-ok", "a.example.com", "b.example.com")
	gitURL := clusterAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-ok",
		ServiceRef:      artifact.ServiceRef{Name: "redis-cluster-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // expects 2, roster 2
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: a converging topology must pass, got %v", err)
	}
}

// TestIntegration_PreflightAssert_StandaloneSkipsClusterGuard — when: gate: on
// a standalone run (redis_type != cluster) the cluster-assert is NOT
// evaluated, even if the roster wouldn't match the cluster invariant. Mirrors
// the placeholder-skip of an inactive mode (ADR-012(d)).
func TestIntegration_PreflightAssert_StandaloneSkipsClusterGuard(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationRoster(t, "redis-standalone", "a.example.com")
	gitURL := clusterAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-standalone",
		ServiceRef:      artifact.ServiceRef{Name: "redis-cluster-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"redis_type": "standalone", "shards": 9, "replicas_per_shard": 9},
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: standalone mode must not compute cluster-assert, got %v", err)
	}
}

// dispatcherAssertServiceRepo is a service repo MIRRORING the redis dispatcher:
// top-level main.yml is a guard task + `include: branch.yml`, while the assert
// (size-guard) lives INSIDE the included branch.yml, not at top level.
// Regression guard for a bug (caught live 2026-06-23): hasAssertTask on the
// unexpanded main.yml didn't find the assert → a false no-op pre-flight →
// render-assert failed during applying → error_locked instead of a
// synchronous 422. After the fix, includes are expanded BEFORE hasAssertTask.
//
// branch.yml falls back to service-level (scenario/create/branch.yml doesn't
// exist → scenario/branch.yml), but we place it locally
// (scenario/create/branch.yml), like the redis branches. assert gated by
// when: redis_type=='cluster' (as in prod).
func dispatcherAssertServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: redis-dispatch-guard
state_schema_version: 1
description: dispatcher-with-include pre-flight assert test service
state_schema:
  type: object
  properties: {}
`)
	// main.yml is the DISPATCHER: top-level = mode-guard + branch include. No assert here.
	write("scenario/create/main.yml", `name: create
description: dispatcher main.yml — guard + include branch (assert lives in branch)
state_changes: {}
input:
  redis_type:
    type: string
    default: cluster
  shards:
    type: integer
    default: 3
  replicas_per_shard:
    type: integer
    default: 1
tasks:
  - name: Guard redis_type is an implemented mode
    run_once: true
    module: core.cmd.shell
    changed_when: "false"
    params:
      cmd: "test '${ input.redis_type }' = 'cluster' || exit 1"
  - include: branch.yml
`)
	// branch.yml is the BRANCH: a flat task sequence (like redis cluster.yml,
	// WITHOUT the tasks: wrapper). The size-guard assert lives here and only here.
	write("scenario/create/branch.yml", `- name: cluster topology matches shards*(1+replicas)
  when: "input.redis_type == 'cluster'"
  assert:
    that:
      - "size(soulprint.hosts) == int(input.shards) * (1 + int(input.replicas_per_shard))"
    message: "topology mismatch: hosts != shards*(1+replicas_per_shard)"
- name: Echo on every host
  module: core.exec.run
  params:
    cmd: echo
    args: ["hello"]
  changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init dispatch-guard", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_PreflightAssert_AssertInIncludeBranch_Fails — REGRESSION
// GUARD for a live bug: assert lives INSIDE the include branch (redis
// dispatcher pattern), top-level main.yml carries only guard + include.
// roster=4 vs. the expected shards=1*(1+1)=2. Before the fix (hasAssertTask
// BEFORE ExpandIncludes) pre-flight silently returned nil → this test would
// have been RED (expects ErrAssertFailed, got nil). After the fix, includes
// expand first → assert is found → ErrAssertFailed. Mirrors render: render
// also expands includes and evaluates assert on the expanded list (single-source).
func TestIntegration_PreflightAssert_AssertInIncludeBranch_Fails(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationRoster(t, "redis-dispatch-fail",
		"a.example.com", "b.example.com", "c.example.com", "d.example.com")
	gitURL := dispatcherAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-dispatch-fail",
		ServiceRef:      artifact.ServiceRef{Name: "redis-dispatch-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // expects 2, roster 4
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("PreflightAssert: assert in the include branch must fire, got nil (REGRESSION: include not expanded before hasAssertTask)")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err is not ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "topology mismatch") {
		t.Errorf("error does not carry the author message: %v", err)
	}
}

// TestIntegration_PreflightAssert_AssertInIncludeBranch_Passes — no-false-positive:
// the assert in the include branch MATCHES (roster=2 == shards=1*(1+1)) → pre-flight nil.
func TestIntegration_PreflightAssert_AssertInIncludeBranch_Passes(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationRoster(t, "redis-dispatch-ok", "a.example.com", "b.example.com")
	gitURL := dispatcherAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-dispatch-ok",
		ServiceRef:      artifact.ServiceRef{Name: "redis-dispatch-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // expects 2, roster 2
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: a converging topology in the include branch must pass, got %v", err)
	}
}

// TestIntegration_PreflightAssert_AssertInIncludeBranch_NoIncarnation_Defers —
// NIM-235. With no incarnation row the roster is not empty by circumstance but
// IMPOSSIBLE: membership FKs the incarnation (NIM-124, migration 099), and
// pre-flight runs before Create. A topology assert therefore has nothing to
// read, and evaluating it anyway made `size(soulprint.hosts) == N` false for
// EVERY create — an unconditional 422 that no roster could have satisfied.
// The assert is deferred to the render fail-safe, which is the first point
// where the run's roster exists.
//
// This test previously asserted the opposite (0 hosts → ErrAssertFailed), which
// was the bug written down as a contract: it was authored when the roster
// resolved by the root Coven label and could pre-date its incarnation.
//
// This is also where the FORM-A INVARIANT still lives: nothing is seeded for
// this name at all, so the post-condition "pre-flight created no incarnation"
// is a real observation rather than an artifact of the fixture. The other
// tests in this file must seed the incarnation to have a roster (NIM-124), so
// they cannot make that claim.
func TestIntegration_PreflightAssert_AssertInIncludeBranch_NoIncarnation_Defers(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// Neither the incarnation nor a single member — the roster cannot exist.
	gitURL := dispatcherAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-dispatch-empty",
		ServiceRef:      artifact.ServiceRef{Name: "redis-dispatch-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1},
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: a topology assert must be deferred when the incarnation has no row yet, got %v", err)
	}
	if cnt := countIncarnations(t, "redis-dispatch-empty"); cnt != 0 {
		t.Errorf("incarnation created on the pre-flight path (rows=%d), want 0 — pre-flight is read-only until Create", cnt)
	}
}

// createGuardServiceRepo is a service repo shaped like examples/service/dragonfly
// ::create — the shape NIM-235 actually breaks. Two differences from
// [clusterAssertServiceRepo] matter:
//
//   - `create: true`, so the scenario is in the create set and
//     [ResolveCreatePlan] (the handler's real entry point, not PreflightAssert
//     alone) reaches the pre-flight gate rather than resolving `bare`;
//   - the topology assert carries NO `when:` gate. redis hand-writes
//     `when: "!(has(input.provision) && input.provision.enabled)"` on its
//     size-guards to keep them off the provision path; dragonfly does not, and
//     a service author is not required to. An ungated roster assert must not
//     make create unreachable.
//
// The second assert reads input only. It stays evaluable with no roster and is
// what proves the gate was narrowed rather than switched off.
func createGuardServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `name: create-guard
state_schema_version: 1
description: ungated topology size-guard on the create path
state_schema:
  type: object
  properties: {}
`)
	write("scenario/create/main.yml", `name: create
description: ungated size-guard + an input-only guard
create: true
state_changes: {}
input:
  replicas_per_master:
    type: integer
    default: 1
  max_replicas:
    type: integer
    default: 5
tasks:
  - name: Guard roster size matches replicas
    assert:
      that:
        - "size(soulprint.hosts) == 1 + int(input.replicas_per_master)"
      message: "roster size must be exactly 1+replicas_per_master"
  - name: Guard replicas_per_master is within the supported ceiling
    assert:
      that:
        - "int(input.replicas_per_master) <= int(input.max_replicas)"
      message: "replicas_per_master exceeds max_replicas"
  - name: Echo on every host
    module: core.exec.run
    params:
      cmd: echo
      args: ["hello"]
    changed_when: "false"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init create-guard", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// TestIntegration_ResolveCreatePlan_UngatedRosterAssert_Proceeds — NIM-235 on
// the PRODUCT path. [ResolveCreatePlan] is what POST /v1/incarnations and the
// MCP create tool both call; it runs the pre-flight gate between ValidateInput
// and incarnation.Create. With an ungated roster assert this returned
// ErrAssertFailed → 422 for every create of such a service, with no input the
// operator could supply to get past it (a roster cannot be bound before the row
// exists, and the row is inserted by this very call).
func TestIntegration_ResolveCreatePlan_UngatedRosterAssert_Proceeds(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	gitURL := createGuardServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)
	ref := artifact.ServiceRef{Name: "create-guard", Git: gitURL, Ref: "master"}

	plan, err := ResolveCreatePlan(context.Background(), r.deps.Loader, r, "df-new", ref, "create",
		map[string]any{"replicas_per_master": 2}, "archon-alice")
	if err != nil {
		t.Fatalf("ResolveCreatePlan: an ungated roster assert must not reject create before the incarnation exists, got %v", err)
	}
	if plan.CreateScenario != "create" {
		t.Errorf("plan.CreateScenario = %q, want create", plan.CreateScenario)
	}
	if cnt := countIncarnations(t, "df-new"); cnt != 0 {
		t.Errorf("incarnation rows = %d, want 0 — ResolveCreatePlan is read-only, Create is the caller's next step", cnt)
	}
}

// TestIntegration_ResolveCreatePlan_InputAssert_StillFails — the other half of
// the fix: pre-flight was NARROWED to the asserts it cannot evaluate, not
// disabled. An assert reading only `input.*` loses nothing by the incarnation
// row being absent, so it keeps its 422-before-mutation (ADR-009 amendment
// 2026-06-23, form A) — and no row is created.
func TestIntegration_ResolveCreatePlan_InputAssert_StillFails(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	gitURL := createGuardServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)
	ref := artifact.ServiceRef{Name: "create-guard", Git: gitURL, Ref: "master"}

	_, err := ResolveCreatePlan(context.Background(), r.deps.Loader, r, "df-too-many", ref, "create",
		map[string]any{"replicas_per_master": 9, "max_replicas": 5}, "archon-alice")
	if err == nil {
		t.Fatal("ResolveCreatePlan: an input-only assert must still reject on the request path, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err is not ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "exceeds max_replicas") {
		t.Errorf("error does not carry the author message: %v", err)
	}
	if cnt := countIncarnations(t, "df-too-many"); cnt != 0 {
		t.Errorf("incarnation rows = %d, want 0", cnt)
	}
}

// TestIntegration_ResolveCreatePlan_RosterAssert_EvaluatedOnceRowExists — the
// deferral is keyed on "the incarnation has no row", NOT on "this is the create
// path". Once the row and its members exist, the same ungated assert evaluates
// against the real roster and fails on the merits. This is what keeps the
// narrowing from silently swallowing topology guards on every future caller of
// PreflightAssert.
func TestIntegration_ResolveCreatePlan_RosterAssert_EvaluatedOnceRowExists(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnationRoster(t, "df-existing", "a.example.com", "b.example.com", "c.example.com")
	gitURL := createGuardServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)
	ref := artifact.ServiceRef{Name: "create-guard", Git: gitURL, Ref: "master"}

	_, err := ResolveCreatePlan(context.Background(), r.deps.Loader, r, "df-existing", ref, "create",
		map[string]any{"replicas_per_master": 1}, "archon-alice") // wants 2, roster 3
	if err == nil {
		t.Fatal("ResolveCreatePlan: with a real roster the topology assert must be evaluated, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err is not ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "1+replicas_per_master") {
		t.Errorf("error does not carry the author message: %v", err)
	}
}

// countIncarnations returns the number of incarnation rows with the given
// name — checks the invariant "pre-flight didn't create an incarnation".
func countIncarnations(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM incarnation WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("countIncarnations: %v", err)
	}
	return n
}
