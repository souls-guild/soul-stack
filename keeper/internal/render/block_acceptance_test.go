package render

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// realRedisDestinyResolver — a DestinyResolver that loads the REAL `redis` destiny
// (manifest + tasks/main.yml + .tmpl) from disk at examples/destiny/redis/. The
// acceptance test below gates on the destiny's own tasks (deploy_redis skipping the
// data plane), so a synthetic one-step stand-in would assert nothing.
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
		Incarnation: IncarnationMeta{ID: "redis", Service: "redis"},
		Hosts:       hosts,
		Destiny:     realRedisDestinyResolver{dir: filepath.FromSlash("../../../examples/destiny/redis")},
	}

	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render (acceptance destiny redis sentinel_only: deploy_redis=false): %v", err)
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
		t.Errorf("redis.conf task is present in the plan - include server.yml must be group-dropped when deploy_redis=false (physical absence, not a placeholder)")
	}
	if redisRunning != nil {
		t.Errorf("core.service.running redis-server is present in the plan - include server.yml must be group-dropped when deploy_redis=false")
	}
	if sentinelConf == nil {
		t.Fatal("sentinel.conf task (core.file.rendered) not found in the plan")
	}
	if sentinelConf.Params == nil {
		t.Errorf("sentinel.conf placeholder-skip (Params == nil) - sentinel daemon is not deployed in sentinel_only")
	}
	if pkgInstall == nil {
		t.Fatal("core.pkg.installed redis-server not found in the plan")
	}
	if pkgInstall.Params == nil {
		t.Errorf("redis package is NOT installed in sentinel_only (Params == nil) - the package must always be installed (it carries the sentinel daemon)")
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
	return nil, errors.New("stubKV: no secret " + path)
}
