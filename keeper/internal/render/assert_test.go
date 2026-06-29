package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// assertTask строит assert-задачу (ADR-009 amendment 2026-06-23) с предикатом по
// размеру roster-а — тот же инвариант, что cluster size-guard в redis create.
func assertTask(when, pred, message string) config.Task {
	return config.Task{
		Name:   "topology guard",
		When:   when,
		Assert: &config.AssertSpec{That: []string{pred}, Message: message},
	}
}

// TestAssert_PassEmitsNoTask — assert-предикат true → render проходит, задача в
// план НЕ эмитится (assert — проверка, не задача): план остаётся пустым, индекс не
// растёт. Рядом обычная module-задача проверяет, что её Index не съезжает под assert.
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

// TestAssert_FailAbortsRenderWithMessage — assert-предикат false → render
// ОБРЫВАЕТСЯ ErrAssertFailed (ни одной задачи не возвращается, прогон не стартует),
// ошибка несёт авторский message + индекс/текст непрошедшего предиката.
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
		Hosts: []*topology.HostFacts{ // 2 хоста против ожидаемых 3 → assert false.
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

// TestAssert_DefaultMessageOnEmpty — message опущен → ошибка несёт дефолт по имени
// задачи (а не пустую строку).
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

// TestAssert_StaticWhenFalseSkips — assert гейтится `when:`: статически-false when
// (input-only предикат, неактивный режим) → assert НЕ вычисляется, render проходит,
// даже если предикат that[] был бы false. Зеркало cluster-assert на standalone-прогоне.
func TestAssert_StaticWhenFalseSkips(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "svc", Tasks: []config.Task{
		// when: режим != cluster → assert неактивен; that[] вычислился бы в false
		// (1 хост != 99), но до него дело не доходит.
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

// TestAssert_StaticWhenTrueEvaluates — обратный контроль: статически-true when →
// assert вычисляется; false-предикат на активном режиме обрывает render.
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

// TestEvalAsserts_SameSourceAsRender — REUSE-ИНВАРИАНТ (ADR-009 amendment,
// двухточечная eval): pre-flight (EvalAsserts) и render-ветка (Render →
// evalAssertTask) вычисляют assert ОДНИМ источником (evalAssertTask), без
// диалекта. Тест прогоняет ОДИН и тот же input через обе точки и сверяет, что
// вердикт совпадает (оба проходят / оба роняют с тем же message и тем же
// ErrAssertFailed). Расхождение = диалект (баг).
func TestEvalAsserts_SameSourceAsRender(t *testing.T) {
	cases := []struct {
		name      string
		predicate string
		hosts     int // число хостов roster-а
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

			// Оба либо nil, либо оба ErrAssertFailed — вердикт обязан совпасть.
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

// TestEvalAsserts_NoAssertNoOp — сценарий без assert-задач → EvalAsserts no-op
// (nil), даже с module-задачами в плане: pre-flight не должен ломать сценарии без
// assert (большинство).
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

// TestEvalAsserts_StaticWhenFalseSkips — when-гейт соблюдён в pre-flight тоже:
// статически-false when → assert не вычисляется (зеркало render-ветки и
// TestAssert_StaticWhenFalseSkips).
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

// assertInIncludeGroup строит assert-задачу, какой её оставляет config.ExpandIncludes
// для conditionally-included файла: IncludeWhen/IncludeGroupID проставлены (group-drop-
// несущая форма), собственного when у assert нет. Предикат намеренно ссылается на ключ,
// которого нет на несовпадающем режиме (input.shards отсутствует у sentinel) — чтобы
// отличить «assert дропнут группой» (ожидаемо) от «assert вычислен и упал CEL
// no-such-key» (баг live-vs-trial).
func assertInIncludeGroup(includeWhen, pred string) config.Task {
	return config.Task{
		Name:           "cluster roster guard",
		IncludeWhen:    includeWhen,
		IncludeGroupID: 1,
		Assert:         &config.AssertSpec{That: []string{pred}, Message: "cluster roster mismatch"},
	}
}

// TestEvalAsserts_IncludeGroupDropSkipsAssert — ГАРД на live-vs-trial расхождение:
// assert из conditionally-included файла (redis cluster.yml, `include: cluster.yml
// when: input.redis_type=='cluster'`) НЕ должен вычисляться pre-flight-ом на
// несовпадающем режиме. До фикса EvalAsserts игнорировал group-drop и звал
// evalAssertTask для cluster-assert на sentinel-прогоне → CEL «no such key: shards».
// Render/Trial эту группу дропают; pre-flight обязан вести себя так же.
func TestEvalAsserts_IncludeGroupDropSkipsAssert(t *testing.T) {
	manifest := &config.ScenarioManifest{Name: "redis", Tasks: []config.Task{
		assertInIncludeGroup(
			"input.redis_type == 'cluster'",
			// Зеркало cluster.yml size-guard: ссылается на input.shards, которого нет
			// у sentinel-input → если бы assert вычислялся, упал бы no-such-key.
			"size(soulprint.hosts) == input.shards * (input.replicas + 1)",
		),
	}}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"redis_type": "sentinel"}, // НЕТ shards/replicas.
		Incarnation: IncarnationMeta{Name: "redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)},
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: sentinel-прогон не должен вычислять cluster-assert (group-drop), got %v", err)
	}
}

// TestEvalAsserts_IncludeGroupKeepEvaluatesAssert — обратный контроль: на СОВПАДАЮЩЕМ
// режиме (cluster) include-группа НЕ дропается, cluster-assert вычисляется. Roster (1
// хост) не сходится с топологией (shards=3, replicas=1 → ожидается 6) → assert падает
// ErrAssertFailed. Доказывает, что group-drop не «проглатывает» assert на активном режиме.
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

// clusterSizeGuardWhen — провижн-гейт size-asserts деплой-веток redis (ADR-061
// amendment 2026-06-29, create/cluster.yml + sentinel.yml + migrate_cluster/cluster.yml):
// инверсия include-when provision-тела. STATIC (чистый input, без register.*/
// soulprint.self → isStaticWhen) → pre-flight вычислит его сам.
const clusterSizeGuardWhen = "!(has(input.provision) && input.provision.enabled)"

// clusterSizeGuardPred — зеркало cluster size-guard create/cluster.yml (без
// cluster_topology-ветки, она для предиката-гейта неважна): roster обязан быть ровно
// shards*(1+replicas).
const clusterSizeGuardPred = "size(soulprint.hosts) == int(input.shards) * (1 + int(input.replicas_per_master))"

// TestEvalAsserts_ProvisionGateSkipsSizeGuard — ПРЯМОЙ РЕПРО разблокированного блокера
// (ADR-061 amendment 2026-06-29): create redis cluster С provision.enabled=true при
// ПУСТОМ roster-е (souls ещё не созданы — VM поднимутся позже шагами redis-provision.yml:
// core.cloud.created→await_online→refresh_soulprint) НЕ должен падать pre-flight size-
// guard-ом. Гейт `when: !provision.enabled` СТАТИЧЕН и при provision вычисляется в false
// → assert placeholder-skip (НЕ ErrAssertFailed). До фикса size(soulprint.hosts)==N
// падал на пустом roster-е ДО того, как кластер вообще существует (422 на create).
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
		Hosts:       nil, // ПУСТОЙ roster — VM ещё не созданы (provision поднимет их позже).
	}
	if err := p.EvalAsserts(context.Background(), in); err != nil {
		t.Fatalf("EvalAsserts: при provision.enabled size-guard должен быть пропущен (when:false), got %v", err)
	}
}

// TestEvalAsserts_NoProvisionStillEnforcesSizeGuard — РЕВЕРС-КОНТРОЛЬ: provision опущен
// (штатный путь по существующему roster-у) + roster не сходится с топологией
// (shards=3,replicas=1 → ожидается 6, дан 1 хост) → size-guard АКТИВЕН и падает
// ErrAssertFailed. Доказывает, что провижн-гейт НЕ ослабил проверку для НЕ-provision
// прогонов (when=!(has(provision)&&...)=true → assert вычисляется как раньше).
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
			// provision НЕ задан → has(input.provision) false → when=true → assert активен.
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
