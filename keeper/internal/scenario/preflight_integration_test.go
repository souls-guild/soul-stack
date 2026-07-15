//go:build integration

// Integration tests for the pre-flight assert gate (ADR-009/ADR-027 amendment
// 2026-06-23, form A): [Runner.PreflightAssert] evaluates a create scenario's
// assert predicates ON RUN CREATION (request path), BEFORE the incarnation is
// committed. Use case — redis cluster topology size-guard (connected-souls
// count must match shards*(1+replicas_per_shard)).
//
// Via testcontainers PG (shared harness in integration_test.go): seed
// connected souls in the Coven incarnation + a local-fs service repo with a
// cluster size-guard assert. A non-matching roster → render.ErrAssertFailed
// (caller handler → 422); a matching one → nil (create proceeds).

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
// match (4 connected souls vs. the expected shards=1*(1+1)=2) →
// PreflightAssert → render.ErrAssertFailed with message "topology mismatch".
// incarnation was NOT created (pre-flight is on the request path, before
// Create); the test only seeds souls, not the incarnation — the roster
// resolves by the root Coven label (= the future incarnation's name), so no
// incarnation row is required for this (ADR-008).
func TestIntegration_PreflightAssert_TopologyMismatch_Fails(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// 4 connected souls in the Coven incarnation, which doesn't exist YET (create pre-flight).
	for _, sid := range []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com"} {
		seedConnectedSoul(t, sid, []string{"redis-new"})
	}
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
		t.Fatal("PreflightAssert: несходящаяся топология должна дать ошибку, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err не ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "topology mismatch") {
		t.Errorf("ошибка не несёт авторский message: %v", err)
	}

	// FORM-A INVARIANT: incarnation is NOT created (pre-flight writes nothing to PG).
	if cnt := countIncarnations(t, "redis-new"); cnt != 0 {
		t.Errorf("incarnation создана на pre-flight-fail (rows=%d), want 0 — pre-flight read-only до Create", cnt)
	}
}

// TestIntegration_PreflightAssert_TopologyMatches_Passes — the roster matches
// (2 connected souls == shards=1*(1+1)) → PreflightAssert → nil (create proceeds).
func TestIntegration_PreflightAssert_TopologyMatches_Passes(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	for _, sid := range []string{"a.example.com", "b.example.com"} {
		seedConnectedSoul(t, sid, []string{"redis-ok"})
	}
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
		t.Fatalf("PreflightAssert: сходящаяся топология должна пройти, got %v", err)
	}
}

// TestIntegration_PreflightAssert_StandaloneSkipsClusterGuard — when: gate: on
// a standalone run (redis_type != cluster) the cluster-assert is NOT
// evaluated, even if the roster wouldn't match the cluster invariant. Mirrors
// the placeholder-skip of an inactive mode (ADR-012(d)).
func TestIntegration_PreflightAssert_StandaloneSkipsClusterGuard(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedConnectedSoul(t, "a.example.com", []string{"redis-standalone"})
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
		t.Fatalf("PreflightAssert: standalone-режим не должен вычислять cluster-assert, got %v", err)
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
	for _, sid := range []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com"} {
		seedConnectedSoul(t, sid, []string{"redis-dispatch-fail"})
	}
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
		t.Fatal("PreflightAssert: assert в include-ветке должен сработать, got nil (РЕГРЕСС: include не раскрыт до hasAssertTask)")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err не ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "topology mismatch") {
		t.Errorf("ошибка не несёт авторский message: %v", err)
	}
	if cnt := countIncarnations(t, "redis-dispatch-fail"); cnt != 0 {
		t.Errorf("incarnation создана на pre-flight-fail (rows=%d), want 0", cnt)
	}
}

// TestIntegration_PreflightAssert_AssertInIncludeBranch_Passes — no-false-positive:
// the assert in the include branch MATCHES (roster=2 == shards=1*(1+1)) → pre-flight nil.
func TestIntegration_PreflightAssert_AssertInIncludeBranch_Passes(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	for _, sid := range []string{"a.example.com", "b.example.com"} {
		seedConnectedSoul(t, sid, []string{"redis-dispatch-ok"})
	}
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
		t.Fatalf("PreflightAssert: сходящаяся топология в include-ветке должна пройти, got %v", err)
	}
}

// TestIntegration_PreflightAssert_AssertInIncludeBranch_ZeroHosts_Fails —
// preflight.go contract: 0 connected souls → the topology-assert (size==N)
// doesn't match → ErrAssertFailed. The assert here lives in the include
// branch (dispatcher). Ensures that after the fix, 0-hosts is honestly
// rejected rather than returning nil due to an unexpanded include.
func TestIntegration_PreflightAssert_AssertInIncludeBranch_ZeroHosts_Fails(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// NOT A SINGLE connected soul in the Coven — roster is empty.
	gitURL := dispatcherAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-dispatch-empty",
		ServiceRef:      artifact.ServiceRef{Name: "redis-dispatch-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // expects 2, roster 0
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("PreflightAssert: 0 connected souls должны провалить size-assert, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err не ErrAssertFailed: %v", err)
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
