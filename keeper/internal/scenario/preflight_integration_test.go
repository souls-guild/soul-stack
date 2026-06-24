//go:build integration

// Integration-тесты pre-flight assert-гейта (ADR-009/ADR-027 amendment
// 2026-06-23, форма A): [Runner.PreflightAssert] вычисляет assert-предикаты
// сценария create НА СОЗДАНИИ прогона (request-путь), ДО коммита incarnation.
// Use-case — redis cluster topology size-guard (число connected-souls обязано
// совпасть с shards*(1+replicas_per_shard)).
//
// Через testcontainers PG (общий harness integration_test.go): seed connected
// souls в Coven incarnation + local-fs service-репо с cluster size-guard assert.
// Несходящийся roster → render.ErrAssertFailed (caller-handler → 422); сходящийся
// → nil (create продолжается).

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

// clusterAssertServiceRepo — service-репо с create-сценарием, несущим cluster
// topology size-guard через assert: (тот же инвариант, что
// examples/service/redis/scenario/create/cluster.yml). input.shards /
// input.replicas_per_shard — int с дефолтами; assert активен только на
// redis_type==cluster (гейтится when:, как в проде).
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

// TestIntegration_PreflightAssert_TopologyMismatch_Fails — roster НЕ сходится
// (4 connected souls против ожидаемых shards=1*(1+1)=2) → PreflightAssert →
// render.ErrAssertFailed с message «topology mismatch». incarnation НЕ создавалась
// (pre-flight на request-пути, до Create); тест seed-ит только souls, не
// incarnation — roster резолвится по корневой Coven-метке (= имя будущей
// incarnation), запись incarnation для этого НЕ требуется (ADR-008).
func TestIntegration_PreflightAssert_TopologyMismatch_Fails(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// 4 connected souls в Coven incarnation, которой ещё НЕТ (create pre-flight).
	for _, sid := range []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com"} {
		seedConnectedSoul(t, sid, []string{"redis-new"})
	}
	gitURL := clusterAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-new",
		ServiceRef:      artifact.ServiceRef{Name: "redis-cluster-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // ожидает 2, roster 4
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

	// ИНВАРИАНТ формы A: incarnation НЕ создана (pre-flight не пишет ничего в PG).
	if cnt := countIncarnations(t, "redis-new"); cnt != 0 {
		t.Errorf("incarnation создана на pre-flight-fail (rows=%d), want 0 — pre-flight read-only до Create", cnt)
	}
}

// TestIntegration_PreflightAssert_TopologyMatches_Passes — roster сходится
// (2 connected souls == shards=1*(1+1)) → PreflightAssert → nil (create продолжится).
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
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // ожидает 2, roster 2
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: сходящаяся топология должна пройти, got %v", err)
	}
}

// TestIntegration_PreflightAssert_StandaloneSkipsClusterGuard — when-гейт: на
// standalone-прогоне (redis_type != cluster) cluster-assert НЕ вычисляется, даже
// если roster не сошёлся бы с cluster-инвариантом. Зеркало placeholder-skip
// неактивного режима (ADR-012(d)).
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

// dispatcherAssertServiceRepo — service-репо, ЗЕРКАЛЯЩЕЕ redis-диспетчер: top-level
// main.yml — это guard-задача + `include: branch.yml`, а assert (size-guard) живёт
// ВНУТРИ включаемой ветки branch.yml, НЕ на top-level. Регресс-гард на баг
// (live-пойман 2026-06-23): hasAssertTask на нераскрытом main.yml не находил assert
// → ложный no-op pre-flight → render-assert падал в applying → error_locked вместо
// синхронного 422. После фикса include раскрывается ДО hasAssertTask.
//
// branch.yml фоллбэкается на service-level (scenario/create/branch.yml не существует
// → scenario/branch.yml), но мы кладём его локально (scenario/create/branch.yml),
// как redis-ветки. assert gated when: redis_type=='cluster' (как в проде).
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
	// main.yml — ДИСПЕТЧЕР: top-level = mode-guard + include ветки. assert тут НЕТ.
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
	// branch.yml — ВЕТКА: плоская последовательность задач (как redis cluster.yml,
	// БЕЗ обёртки tasks:). Здесь и только здесь живёт size-guard assert.
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

// TestIntegration_PreflightAssert_AssertInIncludeBranch_Fails — РЕГРЕСС-ГАРД на
// live-баг: assert живёт ВНУТРИ include-ветки (диспетчер-паттерн redis), top-level
// main.yml несёт только guard + include. roster=4 против ожидаемых shards=1*(1+1)=2.
// До фикта (hasAssertTask ДО ExpandIncludes) pre-flight молчал nil → этот тест был
// бы КРАСНЫЙ (ждёт ErrAssertFailed, получил nil). После фикта include раскрывается
// первым → assert найден → ErrAssertFailed. Зеркало render: render тоже раскрывает
// include и вычисляет assert в expanded-списке (single-source).
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
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // ожидает 2, roster 4
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
// assert в include-ветке СХОДИТСЯ (roster=2 == shards=1*(1+1)) → pre-flight nil.
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
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // ожидает 2, roster 2
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: сходящаяся топология в include-ветке должна пройти, got %v", err)
	}
}

// TestIntegration_PreflightAssert_AssertInIncludeBranch_ZeroHosts_Fails — контракт
// preflight.go: 0 connected souls → topology-assert (size==N) не сойдётся →
// ErrAssertFailed. Assert при этом в include-ветке (диспетчер). Гарантирует, что
// после фикта 0-hosts честно режется, а не возвращает nil из-за нераскрытого include.
func TestIntegration_PreflightAssert_AssertInIncludeBranch_ZeroHosts_Fails(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	// НИ ОДНОГО connected soul в Coven — roster пуст.
	gitURL := dispatcherAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "redis-dispatch-empty",
		ServiceRef:      artifact.ServiceRef{Name: "redis-dispatch-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		Input:           map[string]any{"shards": 1, "replicas_per_shard": 1}, // ожидает 2, roster 0
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("PreflightAssert: 0 connected souls должны провалить size-assert, got nil")
	}
	if !errors.Is(err, render.ErrAssertFailed) {
		t.Fatalf("err не ErrAssertFailed: %v", err)
	}
}

// countIncarnations возвращает число строк incarnation с данным именем —
// проверка инварианта «pre-flight не создал incarnation».
func countIncarnations(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM incarnation WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("countIncarnations: %v", err)
	}
	return n
}
