package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// assertTask builds an assert task (ADR-009 amendment 2026-06-23) with a
// roster-size predicate — the same invariant as the cluster size-guard in
// redis create.
func assertTask(when, pred, message string) config.Task {
	return config.Task{
		Name:   "topology guard",
		When:   when,
		Assert: &config.AssertSpec{That: []string{pred}, Message: message},
	}
}

// TestAssert_PassEmitsNoTask — assert predicate true → render succeeds, no
// task is emitted into the plan (assert is a check, not a task): the plan
// stays empty for it, the index doesn't advance. A regular module task next
// to it checks that its Index doesn't shift under the assert.
func TestAssert_PassEmitsNoTask(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("", "size(soulprint.hosts) == 3", "want 3 hosts"),
		{
			Name:   "install",
			Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis-server"}},
		},
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
			host("c.example.com", []string{"svc"}, nil),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: assert-pass не должен ронять render, got %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (assert не эмитит задачу, остаётся только install)", len(tasks))
	}
	if tasks[0].Index != 0 {
		t.Errorf("install Index = %d, want 0 (assert не резервирует индекс)", tasks[0].Index)
	}
	if tasks[0].Module != "core.pkg.installed" {
		t.Errorf("Module = %q, want core.pkg.installed", tasks[0].Module)
	}
}

// TestAssert_FailAbortsRenderWithMessage — assert predicate false → render
// ABORTS with ErrAssertFailed (no tasks returned, the run never starts); the
// error carries the author's message + the index/text of the failed
// predicate.
func TestAssert_FailAbortsRenderWithMessage(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("", "size(soulprint.hosts) == 3", "topology mismatch: want 3 hosts"),
		{
			Name:   "install",
			Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis-server"}},
		},
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts: []*topology.HostFacts{ // 2 hosts against the expected 3 → assert false.
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
		},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatalf("Render: assert-fail должен оборвать render, got tasks=%d err=nil", len(tasks))
	}
	if !errors.Is(err, ErrAssertFailed) {
		t.Errorf("err не ErrAssertFailed: %v", err)
	}
	if !strings.Contains(err.Error(), "topology mismatch: want 3 hosts") {
		t.Errorf("ошибка не несёт message автора: %v", err)
	}
	if !strings.Contains(err.Error(), "that[0]") {
		t.Errorf("ошибка не указывает индекс непрошедшего предиката: %v", err)
	}
	if tasks != nil {
		t.Errorf("tasks = %v, want nil (ни одной задачи на dispatch при abort)", tasks)
	}
}

// TestAssert_DefaultMessageOnEmpty — message omitted → error carries a
// default built from the task name (not an empty string).
func TestAssert_DefaultMessageOnEmpty(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("", "size(soulprint.hosts) == 5", ""),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: assert-fail должен оборвать render")
	}
	if !strings.Contains(err.Error(), "topology guard") {
		t.Errorf("дефолт-сообщение не несёт имя задачи: %v", err)
	}
}

// TestAssert_StaticWhenFalseSkips — assert is gated by `when:`: a
// statically-false when (input-only predicate, inactive mode) → assert is NOT
// evaluated, render succeeds even if that[] would have been false. Mirrors
// the cluster-assert on a standalone run.
func TestAssert_StaticWhenFalseSkips(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		// when: mode != cluster → assert inactive; that[] would evaluate to
		// false (1 host != 99), but it's never reached.
		assertTask("input.redis_type == 'cluster'", "size(soulprint.hosts) == 99", "never reached"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "standalone"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: static-when:false должен пропустить assert без вычисления, got %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("len(tasks) = %d, want 0 (assert не эмитит задачу, when:false → не вычислялся)", len(tasks))
	}
}

// TestAssert_StaticWhenTrueEvaluates — reverse check: a statically-true when →
// assert is evaluated; a false predicate on an active mode aborts render.
func TestAssert_StaticWhenTrueEvaluates(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("input.redis_type == 'cluster'", "size(soulprint.hosts) == 99", "topology mismatch"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "cluster"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: when:true + false-предикат должен оборвать render")
	}
	if !errors.Is(err, ErrAssertFailed) {
		t.Errorf("err не ErrAssertFailed: %v", err)
	}
}

// TestEvalAsserts_SameSourceAsRender — REUSE INVARIANT (ADR-009 amendment,
// two-point eval): pre-flight (EvalAsserts) and the render branch (Render →
// evalAssertTask) evaluate asserts from ONE source (evalAssertTask), with no
// dialect drift. The test runs the SAME input through both points and checks
// the verdict matches (both pass / both fail with the same message and the
// same ErrAssertFailed). A mismatch means dialect drift (a bug).
func TestEvalAsserts_SameSourceAsRender(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		hosts     int // roster host count
	}{
		{"pass_3_hosts", "size(soulprint.hosts) == 3", 3},
		{"fail_2_vs_3", "size(soulprint.hosts) == 3", 2},
		{"fail_0_vs_3", "size(soulprint.hosts) == 3", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
				assertTask("", tc.predicate, "topology mismatch"),
			}}
			hosts := make([]*topology.HostFacts, tc.hosts)
			for i := range hosts {
				hosts[i] = host(string(rune('a'+i))+".example.com", []string{"svc"}, nil)
			}
			in := RenderInput{
				Scenario:    manifest,
				Incarnation: IncarnationMeta{Name: "svc"},
				Hosts:       hosts,
			}

			pRender := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, renderErr := pRender.Render(context.Background(), in)

			pPre := NewPipeline(nil, newEngine(t), nil, nil)
			preErr := pPre.EvalAsserts(context.Background(), in)

			// Either both nil or both ErrAssertFailed — the verdict must match.
			if (renderErr == nil) != (preErr == nil) {
				t.Fatalf("вердикт расходится: render=%v, EvalAsserts=%v (диалект)", renderErr, preErr)
			}
			if renderErr != nil {
				if !errors.Is(renderErr, ErrAssertFailed) || !errors.Is(preErr, ErrAssertFailed) {
					t.Fatalf("не ErrAssertFailed на обеих точках: render=%v, pre=%v", renderErr, preErr)
				}
				if renderErr.Error() != preErr.Error() {
					t.Errorf("текст ошибки расходится:\n render=%q\n pre=   %q", renderErr.Error(), preErr.Error())
				}
			}
		})
	}
}

// TestEvalAsserts_NoAssertNoOp — a scenario with no assert tasks → EvalAsserts
// is a no-op (nil), even with module tasks in the plan: pre-flight must not
// break scenarios without asserts (the majority).
func TestEvalAsserts_NoAssertNoOp(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		{Name: "install", Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis-server"}}},
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: сценарий без assert должен быть no-op, got %v", err)
	}
}

// TestEvalAsserts_StaticWhenFalseSkips — the when gate holds in pre-flight
// too: a statically-false when → assert is not evaluated (mirrors the render
// branch and TestAssert_StaticWhenFalseSkips).
func TestEvalAsserts_StaticWhenFalseSkips(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		assertTask("input.redis_type == 'cluster'", "size(soulprint.hosts) == 99", "never reached"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "standalone"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: static-when:false должен пропустить assert, got %v", err)
	}
}

// assertInIncludeGroup builds an assert task the way config.ExpandIncludes
// leaves it for a conditionally-included file: IncludeWhen/IncludeGroupID are
// set (the group-drop-carrying form), the assert has no when of its own. The
// predicate deliberately references a key absent under the non-matching mode
// (input.shards is missing for sentinel) — to distinguish "assert dropped
// with its group" (expected) from "assert evaluated and hit a CEL
// no-such-key" (a live-vs-trial bug).
func assertInIncludeGroup(includeWhen, pred string) config.Task {
	return config.Task{
		Name:           "cluster roster guard",
		IncludeWhen:    includeWhen,
		IncludeGroupID: 1,
		Assert:         &config.AssertSpec{That: []string{pred}, Message: "cluster roster mismatch"},
	}
}

// TestEvalAsserts_IncludeGroupDropSkipsAssert — GUARD against a live-vs-trial
// mismatch: an assert from a conditionally-included file (redis cluster.yml,
// `include: cluster.yml when: input.redis_type=='cluster'`) must NOT be
// evaluated by pre-flight on a non-matching mode. Before the fix, EvalAsserts
// ignored group-drop and called evalAssertTask for the cluster-assert on a
// sentinel run → CEL "no such key: shards". Render/Trial drop that group;
// pre-flight must behave the same way.
func TestEvalAsserts_IncludeGroupDropSkipsAssert(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "redis", Tasks: []config.Task{
		assertInIncludeGroup(
			"input.redis_type == 'cluster'",
			// Mirrors the cluster.yml size-guard: references input.shards,
			// absent from sentinel input → if the assert were evaluated it
			// would hit no-such-key.
			"size(soulprint.hosts) == input.shards * (input.replicas + 1)",
		),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "sentinel"}, // NO shards/replicas.
		Incarnation: IncarnationMeta{Name: "redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)},
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: sentinel-прогон не должен вычислять cluster-assert (group-drop), got %v", err)
	}
}

// TestEvalAsserts_IncludeGroupKeepEvaluatesAssert — reverse check: on a
// MATCHING mode (cluster) the include group is NOT dropped, the cluster
// assert is evaluated. The roster (1 host) doesn't match the topology
// (shards=3, replicas=1 → expects 6) → assert fails with ErrAssertFailed.
// Proves group-drop doesn't "swallow" an assert on an active mode.
func TestEvalAsserts_IncludeGroupKeepEvaluatesAssert(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "redis", Tasks: []config.Task{
		assertInIncludeGroup(
			"input.redis_type == 'cluster'",
			"size(soulprint.hosts) == input.shards * (input.replicas + 1)",
		),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "cluster", "shards": 3, "replicas": 1},
		Incarnation: IncarnationMeta{Name: "redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)}, // 1 != 6.
	}
	err := p.EvalAsserts(context.Background(), in)
	if err == nil {
		t.Fatal("EvalAsserts: cluster-прогон должен вычислить cluster-assert и упасть (roster не сходится)")
	}
	if !errors.Is(err, ErrAssertFailed) {
		t.Errorf("err не ErrAssertFailed (значит упал не на предикате, а на eval): %v", err)
	}
}

// clusterSizeGuardWhen is the provision gate for redis deploy-branch
// size-asserts (ADR-061 amendment 2026-06-29, create/cluster.yml +
// sentinel.yml + migrate_cluster/cluster.yml): the inverse of the
// provision body's include-when. STATIC (pure input, no register.*/
// soulprint.self → isStaticWhen) → pre-flight evaluates it itself.
const clusterSizeGuardWhen = "!(has(input.provision) && input.provision.enabled)"

// clusterSizeGuardPred mirrors the cluster size-guard in create/cluster.yml
// (minus the cluster_topology branch, irrelevant to the gate predicate):
// the roster must be exactly shards*(1+replicas).
const clusterSizeGuardPred = "size(soulprint.hosts) == int(input.shards) * (1 + int(input.replicas_per_master))"

// TestEvalAsserts_ProvisionGateSkipsSizeGuard — DIRECT REPRO of the unblocked
// blocker (ADR-061 amendment 2026-06-29): creating a redis cluster WITH
// provision.enabled=true on an EMPTY roster (souls not created yet — VMs come
// up later via redis-provision.yml steps: core.cloud.created→await_online→
// refresh_soulprint) must not fail the pre-flight size guard. The
// `when: !provision.enabled` gate is STATIC and evaluates to false under
// provision → assert placeholder-skip (NOT ErrAssertFailed). Before the fix,
// size(soulprint.hosts)==N failed on an empty roster BEFORE the cluster even
// existed (422 on create).
func TestEvalAsserts_ProvisionGateSkipsSizeGuard(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "redis", Tasks: []config.Task{
		assertTask(clusterSizeGuardWhen, clusterSizeGuardPred, "roster size mismatch"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{
			"redis_type":          "cluster",
			"shards":              3,
			"replicas_per_master": 1,
			"provision":           map[string]any{"enabled": true},
		},
		Incarnation: IncarnationMeta{Name: "redis"},
		Hosts:       nil, // EMPTY roster — VMs not created yet (provision brings them up later).
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: при provision.enabled size-guard должен быть пропущен (when:false), got %v", err)
	}
}

// TestEvalAsserts_NoProvisionStillEnforcesSizeGuard — REVERSE CHECK: provision
// omitted (the normal path over an existing roster) + roster doesn't match
// the topology (shards=3,replicas=1 → expects 6, given 1 host) → size-guard
// is ACTIVE and fails with ErrAssertFailed. Proves the provision gate didn't
// weaken the check for non-provision runs (when=!(has(provision)&&...)=true →
// assert evaluates as before).
func TestEvalAsserts_NoProvisionStillEnforcesSizeGuard(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "redis", Tasks: []config.Task{
		assertTask(clusterSizeGuardWhen, clusterSizeGuardPred, "roster size mismatch"),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario: manifest,
		Input: map[string]any{
			"redis_type":          "cluster",
			"shards":              3,
			"replicas_per_master": 1,
			// provision NOT set → has(input.provision) false → when=true → assert active.
		},
		Incarnation: IncarnationMeta{Name: "redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)}, // 1 != 6.
	}
	err := p.EvalAsserts(context.Background(), in)
	if err == nil {
		t.Fatal("EvalAsserts: без provision size-guard должен оставаться активным и упасть (roster 1 != 6)")
	}
	if !errors.Is(err, ErrAssertFailed) {
		t.Errorf("err не ErrAssertFailed (значит упал не на предикате, а на eval): %v", err)
	}
}
