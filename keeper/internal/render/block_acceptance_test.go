package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// redisResolver — фикстурный DestinyResolver для apply: destiny: redis в
// acceptance-сценарии restart. Возвращает минимальную destiny (один module-шаг),
// чтобы apply-задачи прогона разворачивались без снапшота.
type redisResolver struct{}

func (redisResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	if name != "redis" {
		return nil, errors.New("unknown destiny " + name)
	}
	return &ResolvedDestiny{
		Name: "redis",
		Tasks: []config.Task{
			{Name: "redis-step", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
		},
		Input: config.InputSchemaMap{"action": {Type: "string"}},
	}, nil
}

// TestAcceptance_RestartBlockFanOut — ★ ПРИЁМКА ТЗ: реальный потребитель
// examples/service/redis/scenario/restart/main.yml (block fan-out + serial:1 +
// унаследованный block.where) рендерится корректно. Рендерим Passage 1 (где живёт
// block) с per-host register probe (Passage 0): хост a — master, b/c — slave.
//
// Доказывает:
//   - block fan-out: 2 потомка блока (Restart + Wait) разворачиваются в 2
//     RenderedTask со сквозными Index;
//   - унаследованный block.where (register.redis_role.stdout == 'slave'):
//     потомки таргетят ТОЛЬКО slave-хосты (b, c), не master (a);
//   - serial:1 наследуется: каждый потомок несёт SerialWidth=1.
func TestAcceptance_RestartBlockFanOut(t *testing.T) {
	path := filepath.FromSlash("../../../examples/service/redis/scenario/restart/main.yml")
	m, _, diags, err := config.LoadScenarioManifest(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostic (%s): %s", d.Code, d.Message)
		}
	}

	plan, err := Stratify(m.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}

	// Health-gate block-потомок (community.redis.replica-synced) рендерит params с
	// vault('secret/redis/redis-prod#password') — движок с фикстурным KVReader-ом
	// (паттерн TestAcceptance_SentinelReplicaExcludesMaster). essence не задан →
	// essence.tls_enable отсутствует → plaintext-ветка (default false).
	engine, err := cel.New(cel.WithVault(stubKV{"secret/redis/redis-prod": {"password": "fixture-redis-pass-16+"}}))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	p := NewPipeline(stubKV{}, engine, nil, nil)
	in := RenderInput{
		Scenario:    m,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{Name: "redis-prod", Service: "redis"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"redis-prod"}, nil),
			host("b.example.com", []string{"redis-prod"}, nil),
			host("c.example.com", []string{"redis-prod"}, nil),
		},
		TaskPassage:   plan.TaskPassage,
		ActivePassage: 1, // Passage 1 — где живёт block (passage_plan [0 1 1]: probe→block+restart-master).
		// Per-host register Passage 0 (probe redis_role через community.redis.role):
		// a=master, b/c=slave. Поле register.redis_role.role (Output плагина), НЕ
		// .stdout (shell-probe заменён на community.redis.role, go-redis INFO replication).
		RegisterByHost: map[string]map[string]any{
			"a.example.com": {"redis_role": map[string]any{"role": "master"}},
			"b.example.com": {"redis_role": map[string]any{"role": "slave"}},
			"c.example.com": {"redis_role": map[string]any{"role": "slave"}},
		},
		Destiny: redisResolver{},
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (приёмка restart/main.yml): %v", err)
	}

	// Собираем block-потомков по имени: 2 шага блока (Restart + Wait).
	blockChildren := map[string]*RenderedTask{}
	blockPlans := map[string]*DispatchPlan{}
	byIndex := map[int]*RenderedTask{}
	for _, tk := range tasks {
		byIndex[tk.Index] = tk
	}
	for i := range plans {
		tk := byIndex[plans[i].TaskIndex]
		if tk == nil {
			continue
		}
		switch tk.Name {
		case "Restart redis-server", "Wait until replica resynced with master":
			blockChildren[tk.Name] = tk
			blockPlans[tk.Name] = &plans[i]
		}
	}
	if len(blockChildren) != 2 {
		t.Fatalf("block fan-out дал %d потомков, want 2 (Restart + Wait)", len(blockChildren))
	}

	for name, pl := range blockPlans {
		// Унаследованный block.where (slave) → таргет ТОЛЬКО slave-хосты b,c.
		if len(pl.TargetSIDs) != 2 {
			t.Errorf("block-потомок %q таргетит %v, want [b c] (унаследованный where: slave)", name, pl.TargetSIDs)
			continue
		}
		for _, sid := range pl.TargetSIDs {
			if sid == "a.example.com" {
				t.Errorf("block-потомок %q таргетит master a — унаследованный where: slave не применился", name)
			}
		}
		// serial:1 наследуется всеми потомками.
		if pl.SerialWidth != 1 {
			t.Errorf("block-потомок %q SerialWidth = %d, want 1 (унаследован block.serial:1)", name, pl.SerialWidth)
		}
	}
}

// redisSentinelResolver — минимальный DestinyResolver для apply:destiny: redis в
// sentinel-acceptance: принимает apply:input sentinel-ветки (version/password/
// config/users/sentinel_enabled/sentinel) через permissive-схему. Тело destiny
// неважно (один module-шаг) — assert на ТАРГЕТ top-level REPLICAOF-задачи
// sentinel.yml, а не на destiny-задачи.
type redisSentinelResolver struct{}

func (redisSentinelResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	if name != "redis" {
		return nil, errors.New("unknown destiny " + name)
	}
	return &ResolvedDestiny{
		Name: "redis",
		Tasks: []config.Task{
			{Name: "redis-step", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
		},
		Input: config.InputSchemaMap{
			"version":          {Type: "string"},
			"password":         {Type: "string"},
			"config":           {Type: "object", AdditionalProperties: true},
			"users":            {Type: "object", AdditionalProperties: true},
			"sentinel_enabled": {Type: "boolean"},
			"sentinel":         {Type: "object", AdditionalProperties: true},
		},
	}, nil
}

// TestAcceptance_SentinelReplicaExcludesMaster — ★ P0-РЕГРЕСС (master НЕ
// реплицирует сам себя). Реальный потребитель
// examples/service/redis/scenario/create/main.yml в режиме sentinel на multi-host
// roster-е (1 master + 2 replica). Раньше REPLICAOF-задача шла на ВСЕХ хостах
// (addr=127.0.0.1:6379, master_addr=primary_ip ⇒ на master-узле addr!=master_addr
// ⇒ плагин-guard НЕ срабатывал ⇒ REPLICAOF на самом master-е). Фикс — scenario
// `where: soulprint.self.sid != soulprint.hosts[0].sid` на REPLICAOF-задаче.
//
// Доказывает на РЕАЛЬНОЙ комбинации (это маскировал unit-тест с addr==master_addr,
// которой в prod нет): отрендеренная задача community.redis.replica таргетит
// ТОЛЬКО реплики (node-2/node-3), а на выбранном master (node-1, первый по SID)
// ОТСУТСТВУЕТ — DispatchPlan.TargetSIDs её не содержит.
func TestAcceptance_SentinelReplicaExcludesMaster(t *testing.T) {
	path := filepath.FromSlash("../../../examples/service/redis/scenario/create/main.yml")
	m, doc, diags, err := config.LoadScenarioManifest(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest: %v", err)
	}
	// create/main.yml несёт `extends: covenant` (R3): секции input/compute/state_changes/
	// validate уехали в examples/service/redis/covenant.yml. Без covenant-merge CEL
	// apply.input.install падает «no such key: install» (compute.install объявлен в
	// covenant). Резолвим ЗЕРКАЛОМ прода (artifact.LoadScenarioManifestResolved) / trial
	// (harness.loadResolvedScenario) / soul-lint — единым config.ResolveScenarioCovenant.
	// serviceRoot — корень снапшота сервиса (сиблинг covenant.yml/scenario/), для пути
	// .../redis/scenario/create/main.yml это .../redis.
	serviceRoot := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	diags = append(diags, config.ResolveScenarioCovenant(m, doc, serviceRoot)...)
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostic (%s): %s", d.Code, d.Message)
		}
	}

	// include: cluster.yml / sentinel.yml (локальные create/) + redis-provision.yml /
	// redis-deploy-{sentinel,cluster}.yml (хоистнуты на service-level scenario/, S2) —
	// раскрытие в плоский список ДО render (как прод/трайл). Резолвер ДВУХУРОВНЕВЫЙ
	// (зеркало scenario.scenarioIncludeResolver, orchestration.md §6): сначала локально
	// scenario/create/<file>, при отсутствии — service-level fallback scenario/<file>.
	// Узкий локальный резолвер скрывал бы хоисты (trial зелёный из-за fallback, go-test
	// красный) — поэтому повторяем прод-семантику.
	scenarioDir := filepath.Dir(path)               // .../scenario/create
	serviceScenarioDir := filepath.Dir(scenarioDir) // .../scenario
	expanded, idiags := config.ExpandIncludes(m.Tasks, twoLevelIncludeResolver(scenarioDir, serviceScenarioDir))
	for _, d := range idiags {
		if d.Level == diag.LevelError {
			t.Fatalf("ExpandIncludes diagnostic (%s): %s", d.Code, d.Message)
		}
	}
	m.Tasks = expanded

	// Roster sentinel-режима: 3 хоста (1 master + 2 replica), В ПОРЯДКЕ ПО SID —
	// зеркало прод-roster (topology.LoadIncarnationHosts: ORDER BY sid ASC).
	// soulprint.hosts проецирует in.Hosts КАК ЕСТЬ (не сортирует), поэтому
	// master-election soulprint.hosts[0] = node-1 опирается именно на этот порядок.
	node := func(sid, ip string) *topology.HostFacts {
		return host(sid, []string{"redis"}, map[string]any{"network": map[string]any{"primary_ip": ip}})
	}
	hosts := []*topology.HostFacts{
		node("node-1.example.com", "10.0.0.1"),
		node("node-2.example.com", "10.0.0.2"),
		node("node-3.example.com", "10.0.0.3"),
	}

	// apply:input sentinel-ветки тянет vault('secret/redis/redis#password') —
	// движок собираем с фикстурным KVReader-ом (паттерн trial.fixtureVault).
	engine, err := cel.New(cel.WithVault(stubKV{"secret/redis/redis": {"password": "fixture-redis-pass-16+"}}))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	// Зеркало прода (scenario.run §4.5) / trial (harness.go:120): эффективный input
	// материализуется через ResolveInputValues ДО сборки RenderInput — дефолты схемы
	// (persistence=rdb) подставляются, required enforce-ятся. version — required без
	// default → задаём явно в фикстуре. replicas_per_master=2 — UNIFIED-поле d1 (2026-
	// 06-25, бывшее `replicas`): size-guard sentinel-ветки сверяет size(hosts)==1+
	// replicas_per_master (3==1+2). sentinel_quorum/sentinel_master_name из контракта
	// убраны (quorum АВТО size/2+1, master_name — essence) — больше не задаём.
	effectiveInput, err := config.ResolveInputValues(m.Input, map[string]any{
		"redis_type":          "sentinel",
		"version":             "7.4.1",
		"replicas_per_master": 2,
	})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	p := NewPipeline(stubKV{}, engine, nil, nil)
	in := RenderInput{
		Scenario:    m,
		Input:       effectiveInput,
		Essence:     redisSentinelEssence(),
		Incarnation: IncarnationMeta{Name: "redis", Service: "redis"},
		Hosts:       hosts,
		Destiny:     redisSentinelResolver{},
	}

	tasks, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (приёмка create/main.yml sentinel): %v", err)
	}

	byIndex := map[int]*RenderedTask{}
	for _, tk := range tasks {
		byIndex[tk.Index] = tk
	}
	var replicaPlan *DispatchPlan
	for i := range plans {
		if tk := byIndex[plans[i].TaskIndex]; tk != nil && tk.Module == "community.redis.replica" {
			replicaPlan = &plans[i]
		}
	}
	if replicaPlan == nil {
		t.Fatal("REPLICAOF-задача (community.redis.replica) не найдена в отрендеренном плане")
	}

	// master НЕ в таргете: where исключил node-1 (первый по SID).
	const masterSID = "node-1.example.com"
	for _, sid := range replicaPlan.TargetSIDs {
		if sid == masterSID {
			t.Errorf("REPLICAOF таргетит master %s — where (self.sid != hosts[0].sid) не исключил master (P0: master реплицирует сам себя)", masterSID)
		}
	}
	// Ровно две реплики в таргете.
	if len(replicaPlan.TargetSIDs) != 2 {
		t.Errorf("REPLICAOF таргетит %v, want ровно 2 реплики (node-2, node-3) без master", replicaPlan.TargetSIDs)
	}
	for _, want := range []string{"node-2.example.com", "node-3.example.com"} {
		found := false
		for _, sid := range replicaPlan.TargetSIDs {
			if sid == want {
				found = true
			}
		}
		if !found {
			t.Errorf("REPLICAOF не таргетит реплику %s: TargetSIDs=%v", want, replicaPlan.TargetSIDs)
		}
	}
}

// realRedisDestinyResolver — DestinyResolver, загружающий РЕАЛЬНУЮ destiny `redis`
// (manifest + tasks/main.yml + .tmpl) с диска examples/destiny/redis/. В отличие от
// redisSentinelResolver (один синтетический шаг), нужен там, где приёмка проверяет
// именно gating настоящих задач destiny (deploy_redis-skip data-плоскости).
type realRedisDestinyResolver struct{ dir string }

func (r realRedisDestinyResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	if name != "redis" {
		return nil, errors.New("unknown destiny " + name)
	}
	manifest, _, mdiags, err := config.LoadDestinyManifest(filepath.Join(r.dir, "destiny.yml"), config.ValidateOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range mdiags {
		if d.Level == diag.LevelError {
			return nil, errors.New("destiny manifest: " + d.Message)
		}
	}
	tasksPath := filepath.Join(r.dir, "tasks", "main.yml")
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		return nil, err
	}
	tasks, tdiags, err := config.LoadDestinyTasksFromBytes(tasksPath, data, config.ValidateOptions{})
	if err != nil {
		return nil, err
	}
	for _, d := range tdiags {
		if d.Level == diag.LevelError {
			return nil, errors.New("destiny tasks: " + d.Message)
		}
	}
	// within-destiny include (tasks/<sub>.yml) раскрывается ДО render — зеркало
	// прода (artifact.DestinyLoader.parseTasks) и trial (fixtureDestinyResolver):
	// tasks/main.yml destiny redis — только include-список логических групп.
	expanded, idiags := config.ExpandIncludes(tasks, func(name string) ([]byte, string, error) {
		rel := filepath.Join("tasks", name)
		d, rerr := os.ReadFile(filepath.Join(r.dir, rel))
		return d, rel, rerr
	})
	for _, d := range idiags {
		if d.Level == diag.LevelError {
			return nil, errors.New("destiny include: " + d.Message)
		}
	}
	tasks = expanded
	// destiny-локалы vars.yml (docs/destiny/vars.md) — зеркало прода
	// (artifact.DestinyLoader.parseVars) и trial (fixtureDestinyResolver): тот же
	// config.LoadDestinyVars, опционален (нет файла → nil,nil). Без проброса Vars
	// `${ vars.* }` (owner/group redis после выноса в vars.yml) падал no-such-key.
	vars, err := config.LoadDestinyVars(filepath.Join(r.dir, "vars.yml"))
	if err != nil {
		return nil, err
	}
	templates := NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(r.dir, rel)) },
		"",
	)
	return &ResolvedDestiny{
		Name:      manifest.Name,
		Tasks:     tasks,
		Input:     manifest.Input,
		Vars:      vars,
		Templates: templates,
	}, nil
}

// TestAcceptance_SentinelOnlySkipsRedisServer — ★ ПРИЁМКА DESTINY-capability
// sentinel_only: deploy_redis=false НЕ разворачивает data-плоскость redis-server,
// при этом sentinel-демон поднимается. Это ГИБКОСТЬ кирпича destiny `redis`
// (sentinel_enabled/deploy_redis), переиспользуемая, напр., для DragonFly «только
// sentinel». Сервисный режим redis_type=sentinel_only УБРАН (2026-06-25, сужение
// enum до [sentinel, cluster]) — поэтому вход теста гонится НА DESTINY-УРОВЕНЬ
// напрямую (apply: destiny: redis + destiny-input deploy_redis/sentinel_enabled),
// а НЕ через удалённый сервис-режим. Capability жива в destiny → тест её и упражняет.
//
// ★ Гейт deploy_redis в destiny redis стоит НА include (tasks/main.yml: `include:
// server.yml` `when: default(input.deploy_redis, true)`, conditional-include
// group-drop, ADR-009 amendment) — НЕ внутри файла. Поэтому при deploy_redis=false
// include server.yml дропается ЦЕЛИКОМ: задачи data-плоскости ФИЗИЧЕСКИ ОТСУТСТВУЮТ
// в плане (group-drop, не placeholder-skip — не эмитятся вовсе): byName[...] == nil.
//
// Доказывает на отрендеренном плане destiny:
//   - redis.conf-задача (core.file.rendered) ОТСУТСТВУЕТ (include server.yml
//     group-dropped): deploy_redis=false дропнул всю data-плоскость;
//   - core.service redis-server running ОТСУТСТВУЕТ (та же группа);
//   - sentinel.conf-задача (core.file.rendered) РЕНДЕРИТСЯ (Params != nil):
//     sentinel-демон поднимается (sentinel_enabled=true), мониторя ВНЕШНИЙ master
//     из input.sentinel.master_ip;
//   - пакет redis ставится ВСЕГДА (core.pkg.installed рендерится, Params != nil;
//     install.yml безусловен — несёт redis-sentinel).
func TestAcceptance_SentinelOnlySkipsRedisServer(t *testing.T) {
	node := func(sid, ip string) *topology.HostFacts {
		return host(sid, []string{"redis"}, map[string]any{
			"network": map[string]any{"primary_ip": ip},
			"os":      map[string]any{"arch": "amd64"},
		})
	}
	hosts := []*topology.HostFacts{
		node("node-1.example.com", "10.0.0.1"),
		node("node-2.example.com", "10.0.0.2"),
	}

	// sentinel_only через destiny-input напрямую: deploy_redis=false (skip data-
	// плоскости) + sentinel_enabled=true (поднять демон). master уже резолвлен caller-ом
	// (destiny «глупая» — получает ГОТОВЫЕ значения), поэтому master_ip/auth_pass —
	// литералы в apply.input, без vault() в ячейках destiny. Required destiny-input:
	// version (install.method=package), password (≥16), sentinel.master_ip.
	applyInput := map[string]any{
		"deploy_redis":     false,
		"sentinel_enabled": true,
		"version":          "7.4.1",
		"password":         "fixture-redis-pass-16+",
		"sentinel": map[string]any{
			"master_name": "master",
			"master_ip":   "10.9.9.9",
			"master_port": 6379,
			"quorum":      2,
			"auth_pass":   "fixture-redis-pass-16+",
		},
	}

	p := NewPipeline(stubKV{}, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    applyDestinyScenario("redis", applyInput),
		Incarnation: IncarnationMeta{Name: "redis", Service: "redis"},
		Hosts:       hosts,
		Destiny:     realRedisDestinyResolver{dir: filepath.FromSlash("../../../examples/destiny/redis")},
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (приёмка destiny redis sentinel_only: deploy_redis=false): %v", err)
	}

	// Находим задачи destiny по Name. group-dropped задачи (include server.yml выключен)
	// в плане ОТСУТСТВУЮТ вовсе (не placeholder), поэтому byName[...] == nil — это и есть
	// доказательство дропа. Индекс зависит от смещения ветки в диспетчере, ищем по имени.
	byName := map[string]*RenderedTask{}
	for _, tk := range tasks {
		byName[tk.Name] = tk
	}
	redisConf := byName["Render redis.conf"]
	sentinelConf := byName["Render sentinel.conf"]
	redisRunning := byName["Ensure redis-server is running and enabled at boot"]
	pkgInstall := byName["Install redis-server package"]

	if redisConf != nil {
		t.Errorf("redis.conf-задача присутствует в плане — include server.yml должен быть group-dropped при deploy_redis=false (физическое отсутствие, не placeholder)")
	}
	if redisRunning != nil {
		t.Errorf("core.service.running redis-server присутствует в плане — include server.yml должен быть group-dropped при deploy_redis=false")
	}
	if sentinelConf == nil {
		t.Fatal("sentinel.conf-задача (core.file.rendered) не найдена в плане")
	}
	if sentinelConf.Params == nil {
		t.Errorf("sentinel.conf placeholder-skip (Params == nil) — sentinel-демон не разворачивается в sentinel_only")
	}
	if pkgInstall == nil {
		t.Fatal("core.pkg.installed redis-server не найдена в плане")
	}
	if pkgInstall.Params == nil {
		t.Errorf("пакет redis НЕ ставится в sentinel_only (Params == nil) — пакет обязан ставиться всегда (несёт sentinel-демон)")
	}
}

// twoLevelIncludeResolver — дисковый IncludeResolver с двухуровневым резолвом,
// зеркало прод-семантики scenario.scenarioIncludeResolver (orchestration.md §6):
// сначала локально localDir/<file>, при fs.ErrNotExist — service-level serviceDir/<file>.
// I/O-ошибку (не «нет файла») не маскируем — наружу. Нужен в acceptance-тестах, где
// часть веток create-сценария хоистнута на service-level scenario/ (S2): без fallback
// include redis-provision.yml / redis-deploy-*.yml не резолвится.
func twoLevelIncludeResolver(localDir, serviceDir string) config.IncludeResolver {
	return func(name string) ([]byte, string, error) {
		local := filepath.Join(localDir, name)
		data, err := os.ReadFile(local)
		if err == nil {
			return data, local, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		service := filepath.Join(serviceDir, name)
		data, serr := os.ReadFile(service)
		if serr != nil {
			return nil, "", serr
		}
		return data, service, nil
	}
}

// stubKV — герметичный KVReader (cel.KVReader + render.KVReader) для
// acceptance-теста: статическая карта path→секрет. vault()-функция передаёт path
// без `#field`-фрагмента; ключи здесь хранятся в logical-форме (secret/...).
type stubKV map[string]map[string]any

func (k stubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if v, ok := k[path]; ok {
		return v, nil
	}
	return nil, errors.New("stubKV: нет секрета " + path)
}

// redisSentinelEssence — essence-подложка create-сценария redis (persistence
// presets + reserve + базовый redis_config), нужна для merge() в apply:input.
func redisSentinelEssence() map[string]any {
	return map[string]any{
		"memory_reserve_percent": 75,
		"persistence_presets": map[string]any{
			"rdb": map[string]any{"save": "900 1 300 10 60 10000", "appendonly": "no"},
		},
		"redis_config": map[string]any{
			"maxmemory":        "256mb",
			"maxmemory-policy": "allkeys-lru",
			"maxclients":       10000,
			"timeout":          300,
		},
	}
}
