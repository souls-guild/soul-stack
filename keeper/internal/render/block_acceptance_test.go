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

// redisResolver — fixture DestinyResolver for apply: destiny: redis in the
// restart acceptance scenario. Returns a minimal destiny (one module step) so
// apply tasks expand without a snapshot.
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

// TestAcceptance_RestartBlockFanOut — ★ ACCEPTANCE: the real consumer
// examples/service/redis/scenario/restart/main.yml (block fan-out + serial:1 +
// inherited block.where) renders correctly. Renders Passage 1 (where the block
// lives) with a per-host register probe (Passage 0): host a is master, b/c are slaves.
//
// Proves:
//   - block fan-out: 2 block children (Restart + Wait) expand into 2
//     RenderedTask with contiguous Index;
//   - inherited block.where (register.redis_role.stdout == 'slave'):
//     children target ONLY slave hosts (b, c), not the master (a);
//   - serial:1 is inherited: each child carries SerialWidth=1.
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

	// The health-gate block child (community.redis.replica-synced) + restart-master
	// render params with vault('secret/redis/redis-prod/users/default_admin#password')
	// (★ default_admin REDESIGN 2026-06-30: restart/main.yml:117,156 — auth under
	// the system default_admin) — engine built with a fixture KVReader (same pattern as
	// TestAcceptance_SentinelReplicaExcludesMaster). essence isn't set →
	// essence.tls_enable is absent → plaintext branch (default false), so
	// vault(incarnation.state.tls.ca_ref) under compute.tls_on is NOT invoked.
	engine, err := cel.New(cel.WithVault(stubKV{
		"secret/redis/redis-prod":                     {"password": "fixture-redis-pass-16+"},
		"secret/redis/redis-prod/users/default_admin": {"password": "fixture-admin-pass-16+"},
	}))
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
		ActivePassage: 1, // Passage 1 — where the block lives (passage_plan [0 1 1]: probe→block+restart-master).
		// Per-host register from Passage 0 (probe redis_role via community.redis.role):
		// a=master, b/c=slave. Field register.redis_role.role (plugin Output), NOT
		// .stdout (the shell probe was replaced by community.redis.role, go-redis INFO replication).
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

	// Collect block children by name: 2 block steps (Restart + Wait).
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
		// Inherited block.where (slave) → target is ONLY slave hosts b,c.
		if len(pl.TargetSIDs) != 2 {
			t.Errorf("block-потомок %q таргетит %v, want [b c] (унаследованный where: slave)", name, pl.TargetSIDs)
			continue
		}
		for _, sid := range pl.TargetSIDs {
			if sid == "a.example.com" {
				t.Errorf("block-потомок %q таргетит master a — унаследованный where: slave не применился", name)
			}
		}
		// serial:1 is inherited by all children.
		if pl.SerialWidth != 1 {
			t.Errorf("block-потомок %q SerialWidth = %d, want 1 (унаследован block.serial:1)", name, pl.SerialWidth)
		}
	}
}

// redisSentinelResolver — minimal DestinyResolver for apply:destiny in the
// sentinel acceptance test. Accepts apply:input for redis's sentinel branch (version/password/
// config/users/sentinel_enabled/sentinel) via a permissive schema, plus the MANDATORY
// data-plane monitoring (Slice I): create UNCONDITIONALLY (no when gate) appends
// apply:destiny node-exporter/redis-exporter at the end → the resolver must know them, or
// render fails with `unknown destiny node-exporter`. The destiny body doesn't matter (one synthetic
// module step) — asserts the TARGET of the top-level REPLICAOF task in sentinel.yml, not destiny tasks.
type redisSentinelResolver struct{}

// syntheticDestiny — minimal ResolvedDestiny with a permissive Input schema and one
// module step. The body doesn't matter for the sentinel-acceptance assertions; what
// matters is that apply:destiny expands without an on-disk snapshot.
func syntheticDestiny(name string, input config.InputSchemaMap) *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: name,
		Tasks: []config.Task{
			{Name: name + "-step", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}}},
		},
		Input: input,
	}
}

func (redisSentinelResolver) Resolve(_ context.Context, name string) (*ResolvedDestiny, error) {
	switch name {
	case "redis":
		return syntheticDestiny("redis", config.InputSchemaMap{
			"version":          {Type: "string"},
			"password":         {Type: "string"},
			"config":           {Type: "object", AdditionalProperties: true},
			"users":            {Type: "object", AdditionalProperties: true},
			"sentinel_enabled": {Type: "boolean"},
			"sentinel":         {Type: "object", AdditionalProperties: true},
		}), nil
	case "node-exporter":
		return syntheticDestiny("node-exporter", config.InputSchemaMap{
			"version": {Type: "string"},
			"listen":  {Type: "string"},
		}), nil
	case "redis-exporter":
		return syntheticDestiny("redis-exporter", config.InputSchemaMap{
			"version":        {Type: "string"},
			"sha256":         {Type: "string"},
			"listen":         {Type: "string"},
			"redis_user":     {Type: "string"},
			"redis_password": {Type: "string"},
		}), nil
	case "vector":
		return syntheticDestiny("vector", config.InputSchemaMap{
			"version":       {Type: "string"},
			"sha256":        {Type: "string"},
			"sink_type":     {Type: "string"},
			"sink_endpoint": {Type: "string"},
			"sink_auth_ref": {Type: "string"},
			"log_sources":   {Type: "array"},
		}), nil
	default:
		return nil, errors.New("unknown destiny " + name)
	}
}

// TestAcceptance_SentinelReplicaExcludesMaster — ★ P0 REGRESSION (master must NOT
// replicate itself). The real consumer
// examples/service/redis/scenario/create/main.yml in sentinel mode on a multi-host
// roster (1 master + 2 replicas). The REPLICAOF task used to go to ALL hosts
// (addr=127.0.0.1:6379, master_addr=primary_ip ⇒ on the master node addr!=master_addr
// ⇒ the plugin guard did NOT trigger ⇒ REPLICAOF ran on the master itself). Fix — scenario
// `where: soulprint.self.sid != soulprint.hosts[0].sid` on the REPLICAOF task.
//
// Proves it on a REAL combination (masked by a unit test with addr==master_addr,
// which doesn't occur in prod): the rendered community.redis.replica task targets
// ONLY the replicas (node-2/node-3), and is ABSENT for the elected master (node-1,
// first by SID) — DispatchPlan.TargetSIDs doesn't contain it.
func TestAcceptance_SentinelReplicaExcludesMaster(t *testing.T) {
	path := filepath.FromSlash("../../../examples/service/redis/scenario/create/main.yml")
	m, doc, diags, err := config.LoadScenarioManifest(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest: %v", err)
	}
	// create/main.yml carries `extends: covenant` (R3): the input/compute/state_changes/
	// validate sections moved to examples/service/redis/covenant.yml. Without the covenant merge, CEL
	// apply.input.install fails with "no such key: install" (compute.install is declared in
	// the covenant). Resolved via a MIRROR of prod (artifact.LoadScenarioManifestResolved) / trial
	// (harness.loadResolvedScenario) / soul-lint — the shared config.ResolveScenarioCovenant.
	// serviceRoot — the service snapshot root (sibling of covenant.yml/scenario/); for the path
	// .../redis/scenario/create/main.yml that's .../redis.
	serviceRoot := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	diags = append(diags, config.ResolveScenarioCovenant(m, doc, serviceRoot)...)
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostic (%s): %s", d.Code, d.Message)
		}
	}

	// include: cluster.yml / sentinel.yml (local create/) + redis-provision.yml /
	// redis-deploy-{sentinel,cluster}.yml (hoisted to service-level scenario/, S2) —
	// expanded to a flat list BEFORE render (like prod/trial). The resolver is TWO-LEVEL
	// (mirrors scenario.scenarioIncludeResolver, orchestration.md §6): local
	// scenario/create/<file> first, falling back to service-level scenario/<file> if absent.
	// A narrow local-only resolver would hide the hoists (trial green due to the fallback, go test
	// red) — so we replicate prod semantics here.
	scenarioDir := filepath.Dir(path)               // .../scenario/create
	serviceScenarioDir := filepath.Dir(scenarioDir) // .../scenario
	expanded, idiags := config.ExpandIncludes(m.Tasks, twoLevelIncludeResolver(scenarioDir, serviceScenarioDir))
	for _, d := range idiags {
		if d.Level == diag.LevelError {
			t.Fatalf("ExpandIncludes diagnostic (%s): %s", d.Code, d.Message)
		}
	}
	m.Tasks = expanded

	// Sentinel-mode roster: 3 hosts (1 master + 2 replicas), IN SID ORDER —
	// mirrors the prod roster (topology.LoadIncarnationHosts: ORDER BY sid ASC).
	// soulprint.hosts projects in.Hosts AS-IS (doesn't sort), so
	// master election via soulprint.hosts[0] = node-1 relies on exactly this order.
	node := func(sid, ip string) *topology.HostFacts {
		return host(sid, []string{"redis"}, map[string]any{"network": map[string]any{"primary_ip": ip}})
	}
	hosts := []*topology.HostFacts{
		node("node-1.example.com", "10.0.0.1"),
		node("node-2.example.com", "10.0.0.2"),
		node("node-3.example.com", "10.0.0.3"),
	}

	// apply:input for the sentinel branch pulls vault secrets — engine built with a fixture
	// KVReader (trial.fixtureVault pattern). ★ default_admin REDESIGN (2026-06-30):
	// step 1 (auth_pass) and step 2 (PING) read secret/redis/redis/users/default_admin
	// (in-cluster AUTH under the system default_admin, requirepass removed); steps 3-5
	// (REPLICAOF/SENTINEL MONITOR/PONG) still read the main secret/redis/redis. Both paths
	// must resolve, or rendering the deploy body fails at vault_resolve.
	engine, err := cel.New(cel.WithVault(stubKV{
		"secret/redis/redis":                     {"password": "fixture-redis-pass-16+"},
		"secret/redis/redis/users/default_admin": {"password": "fixture-admin-pass-16+"},
		// Mandatory monitoring (Slice I): apply redis-exporter reads the password of
		// the monitoring ACL user keeper-side (vault('secret/redis/<inc>/users/monitoring')).
		"secret/redis/redis/users/monitoring": {"password": "fixture-monitoring-pass-16+"},
	}))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	// Mirrors prod (scenario.run §4.5) / trial (harness.go:120): the effective input
	// is materialized via ResolveInputValues BEFORE building RenderInput — schema defaults
	// (persistence=rdb) are applied, required is enforced. version is required with no
	// default → set explicitly in the fixture. replicas_per_master=2 — UNIFIED field d1 (2026-
	// 06-25, formerly `replicas`): the sentinel branch's size guard checks size(hosts)==1+
	// replicas_per_master (3==1+2). sentinel_quorum/sentinel_master_name were removed from the
	// contract (quorum is AUTO size/2+1, master_name comes from essence) — no longer set.
	// provision is EXPLICITLY disabled: create carries input.provision DEFAULT-ON ({enabled: true},
	// decided 2026-06-30) — omitting the section would enable cloud-create + onboarding
	// (core.cloud.created/registered), which this test (sentinel REPLICAOF master-exclusion)
	// doesn't need and which would require essence.provision_*. Passing enabled:false so
	// the merge does NOT apply default-on → the provision body is group-dropped, rendering a clean sentinel branch.
	effectiveInput, err := config.ResolveInputValues(m.Input, map[string]any{
		"redis_type":          "sentinel",
		"version":             "7.4.1",
		"replicas_per_master": 2,
		"provision":           map[string]any{"enabled": false},
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

	// master is NOT in the target: where excluded node-1 (first by SID).
	const masterSID = "node-1.example.com"
	for _, sid := range replicaPlan.TargetSIDs {
		if sid == masterSID {
			t.Errorf("REPLICAOF таргетит master %s — where (self.sid != hosts[0].sid) не исключил master (P0: master реплицирует сам себя)", masterSID)
		}
	}
	// Exactly two replicas in the target.
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

// realRedisDestinyResolver — a DestinyResolver that loads the REAL `redis` destiny
// (manifest + tasks/main.yml + .tmpl) from disk at examples/destiny/redis/. Unlike
// redisSentinelResolver (one synthetic step), needed where the acceptance test checks
// gating of the actual destiny tasks (deploy_redis skip of the data plane).
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
	// within-destiny include (tasks/<sub>.yml) is expanded BEFORE render — mirrors
	// prod (artifact.DestinyLoader.parseTasks) and trial (fixtureDestinyResolver):
	// tasks/main.yml of destiny redis is just an include list of logical groups.
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
	// destiny-local vars.yml (docs/destiny/vars.md) — mirrors prod
	// (artifact.DestinyLoader.parseVars) and trial (fixtureDestinyResolver): the same
	// config.LoadDestinyVars, optional (no file → nil,nil). Without passing Vars through,
	// `${ vars.* }` (owner/group redis after moving to vars.yml) failed with no-such-key.
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

// TestAcceptance_SentinelOnlySkipsRedisServer — ★ ACCEPTANCE for the DESTINY capability
// sentinel_only: deploy_redis=false does NOT expand the redis-server data plane,
// while the sentinel daemon still comes up. This is FLEXIBILITY of the `redis` destiny
// building block (sentinel_enabled/deploy_redis), reusable e.g. for DragonFly's
// "sentinel only" mode. The service-level redis_type=sentinel_only mode was REMOVED
// (2026-06-25, enum narrowed to [sentinel, cluster]) — so the test drives the input at
// the DESTINY LEVEL directly (apply: destiny: redis + destiny-input deploy_redis/sentinel_enabled),
// not through the removed service mode. The capability still lives in destiny → the test exercises it.
//
// ★ The deploy_redis gate in destiny redis sits ON the include (tasks/main.yml: `include:
// server.yml` `when: default(input.deploy_redis, true)`, conditional-include
// group-drop, ADR-009 amendment) — NOT inside the file. So when deploy_redis=false,
// include server.yml is dropped ENTIRELY: data-plane tasks are PHYSICALLY ABSENT
// from the plan (group-drop, not placeholder-skip — not emitted at all): byName[...] == nil.
//
// Proves on the rendered destiny plan:
//   - the redis.conf task (core.file.rendered) is ABSENT (include server.yml
//     group-dropped): deploy_redis=false dropped the whole data plane;
//   - core.service redis-server running is ABSENT (same group);
//   - the sentinel.conf task (core.file.rendered) RENDERS (Params != nil):
//     the sentinel daemon comes up (sentinel_enabled=true), monitoring an EXTERNAL master
//     from input.sentinel.master_ip;
//   - the redis package is installed ALWAYS (core.pkg.installed renders, Params != nil;
//     install.yml is unconditional — it carries redis-sentinel too).
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

	// sentinel_only via destiny-input directly: deploy_redis=false (skip the data
	// plane) + sentinel_enabled=true (bring up the daemon). master is already resolved by the caller
	// (destiny is "dumb" — it receives READY values), so master_ip/auth_pass are
	// literals in apply.input, no vault() in destiny cells. Required destiny-input:
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

	// Find destiny tasks by Name. group-dropped tasks (include server.yml disabled)
	// are ABSENT from the plan entirely (not a placeholder), so byName[...] == nil is
	// itself the proof of the drop. Index depends on dispatcher branch offset, so we look up by name.
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

// twoLevelIncludeResolver — an on-disk IncludeResolver with two-level resolution,
// mirroring prod semantics scenario.scenarioIncludeResolver (orchestration.md §6):
// local localDir/<file> first, service-level serviceDir/<file> on fs.ErrNotExist.
// An I/O error (not "file missing") is not masked — propagated. Needed in acceptance tests where
// part of the create scenario's branches are hoisted to service-level scenario/ (S2): without the fallback,
// include redis-provision.yml / redis-deploy-*.yml wouldn't resolve.
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

// stubKV — hermetic KVReader (cel.KVReader + render.KVReader) for the
// acceptance test: a static path→secret map. The vault() function passes the path
// without the `#field` fragment; keys here are stored in logical form (secret/...).
type stubKV map[string]map[string]any

func (k stubKV) ReadKV(_ context.Context, path string) (map[string]any, error) {
	if v, ok := k[path]; ok {
		return v, nil
	}
	return nil, errors.New("stubKV: нет секрета " + path)
}

// redisSentinelEssence — essence backing for the redis create scenario (persistence
// presets + reserve + a base redis_config), needed for merge() in apply:input.
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
