package trial

// Drift-guard "generate ≡ read" for redis-create secrets (qa-gap 2026-06-28).
// Paired with redis_create_secrets_passage_test.go: that watches ORDER
// (generate STRICTLY before any vault()-read), this one — watches PATH COVERAGE.
//
// Invariant: EVERY actually-read-by-deploy redis-secret-path
// (secret/redis/<inc> and secret/redis/<inc>/users/<name>, password field) MUST
// be in targets of core.vault.kv-present step, i.e., read-set ⊆ generated-set.
// Otherwise on fresh Vault, deploy render fails on vault_resolve for missing path:
// generate did not create it. L0-cases pre-seed ALL secrets in fixtures.vault, so
// by themselves they DO NOT catch this drift (read passes on pre-seeded) — this
// guard is needed, verifying actually-read paths against what the step generates.
//
// How read-set is collected: render-plan create runs through ONE render pass
// (like RunCase) with tracking-KVReader over fixtureVault — it intercepts each
// ReadKV (= vault() argument without #field, ADR-010/shared.cel.splitVaultField). vault()
// in one pass resolves for all active (non-group-drop) tasks of the mode, so
// ALL readable paths of the specific mode are intercepted (sentinel OR cluster — branches
// diverge via include-when). From them only redis-secret-paths are taken (see
// isRedisSecretPath): TLS-PEM-paths live under operator-convention secret/ops/...
// (tls-essence-refs/case.yml) and do not fall under generation.
//
// generated-set — paths from targets of rendered kv-present task (actual plan,
// not re-evaluated CEL). Verification by PATH: both targets and read use
// password field, path discriminator is sufficient (ReadKV doesn't see field anyway).
//
// What catches regression: new system/operator user reading
// vault('secret/redis/<inc>/users/<new>#password') in redis-deploy-*.yml, whose path is
// not in union of targets kv-present (desync between generate and read formulas).

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	createClusterAclCase  = "../../../examples/service/redis/scenario/create/tests/cluster-acl-users/case.yml"
	createSentinelAclCase = "../../../examples/service/redis/scenario/create/tests/sentinel-acl-users/case.yml"
	kvPresentModuleAddr   = "core.vault.kv-present"
)

// trackingVault wraps fixtureVault and records the set of paths actually
// accessed by deploy via ReadKV (= vault() argument without #field). Delegates
// reading to base fixture-reader, so render proceeds exactly like in RunCase
// (values are not substituted — only access is intercepted).
type trackingVault struct {
	base render.KVReader
	read map[string]struct{}
}

func newTrackingVault(base render.KVReader) *trackingVault {
	return &trackingVault{base: base, read: make(map[string]struct{})}
}

func (t *trackingVault) ReadKV(ctx context.Context, path string) (map[string]any, error) {
	t.read[path] = struct{}{}
	return t.base.ReadKV(ctx, path)
}

// renderCreateReadSet runs create-case through one render pass (mirror of
// renderCase/RunCase) with tracking-reader and returns: the set of actually
// read redis-secret-paths (read-set) and the set of paths from targets of
// kv-present step (generated-set). Both are normalized by trimming the default
// mount-prefix secret/ (like fixtureVault), so verification is independent of logical/relative form.
func renderCreateReadSet(t *testing.T, caseFile string) (readSet, generatedSet map[string]struct{}) {
	t.Helper()
	ctx := context.Background()

	c, file, err := LoadCase(caseFile)
	if err != nil {
		t.Fatalf("LoadCase(%s): %v", caseFile, err)
	}

	// Single load+covenant-resolve (harness.go::loadResolvedScenario) — same as
	// renderCase: redis create — is a covenant-scenario (compute.install/data_dir/
	// sentinel_directives in covenant.yml), without CEL resolve fails "no such key:
	// compute.install".
	scn, _, err := loadResolvedScenario(file)
	if err != nil {
		t.Fatalf("%v", err)
	}
	scnPath := scenarioPathFor(file)
	expanded, iDiags := config.ExpandIncludes(scn.Tasks, fixtureScenarioIncludeResolver(scnPath))
	if hasErrors(iDiags) {
		t.Fatalf("expand includes: %s", formatDiags(iDiags))
	}
	scn.Tasks = expanded

	tv := newTrackingVault(newFixtureVault(c.Fixtures.Vault))
	engine, err := cel.New(cel.WithVault(tv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	pipeline := render.NewPipeline(tv, engine, nil, nil)

	deps, err := loadServiceDestinyDeps(file)
	if err != nil {
		t.Fatalf("destiny deps: %v", err)
	}
	destiny := newFixtureDestinyResolver(serviceRootFor(file), c.Fixtures.DefaultDestinySource, deps)

	effectiveInput, err := config.ResolveInputValues(scn.Input, c.Fixtures.Input)
	if err != nil {
		t.Fatalf("resolve input: %v", err)
	}
	if fail, evErr := config.EvalValidateRules(scn.Validate, effectiveInput); evErr != nil {
		t.Fatalf("validate err: %v", evErr)
	} else if fail != nil {
		t.Fatalf("validate fail: %s", fail.Error())
	}

	svcRoot := serviceRootFor(file)
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return readWithin(svcRoot, rel) },
		"scenario/"+scn.Name,
	)

	in := render.RenderInput{
		Scenario:    scn,
		Essence:     orEmptyMap(c.Fixtures.Essence),
		Input:       effectiveInput,
		Register:    orEmptyMap(c.Mocks.Register),
		Incarnation: render.IncarnationMeta{Name: scn.Name},
		Hosts:       fixtureHosts(scn.Name, c.Fixtures),
		Destiny:     destiny,
		Templates:   templates,
		State:       c.Fixtures.State,
		Ctx:         ctx,
	}

	tasks, _, rerr := pipeline.Render(ctx, in)
	if rerr != nil {
		t.Fatalf("%s: render failed: %v", caseFile, rerr)
	}

	generatedSet = kvPresentTargetPaths(t, caseFile, tasks)

	readSet = make(map[string]struct{})
	for p := range tv.read {
		if isRedisSecretPath(p) {
			readSet[normalizeVaultKey(p)] = struct{}{}
		}
	}
	return readSet, generatedSet
}

// kvPresentTargetPaths extracts the set of path from targets of the ONLY
// kv-present task in rendered plan. Multiple such tasks or their absence
// is Fatal: plan is ambiguous / generate-step is missing (passage-guard would catch this too,
// but here we're specifically checking targets, so we verify explicitly).
func kvPresentTargetPaths(t *testing.T, caseFile string, tasks []*render.RenderedTask) map[string]struct{} {
	t.Helper()
	out := make(map[string]struct{})
	found := 0
	for _, rt := range tasks {
		if rt.Module != kvPresentModuleAddr || rt.Params == nil {
			continue
		}
		found++
		raw, ok := rt.Params.AsMap()["targets"].([]any)
		if !ok {
			t.Fatalf("%s: kv-present targets not a list: %T", caseFile, rt.Params.AsMap()["targets"])
		}
		for i, item := range raw {
			obj, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("%s: kv-present targets[%d] not an object: %T", caseFile, i, item)
			}
			path, ok := obj["path"].(string)
			if !ok || path == "" {
				t.Fatalf("%s: kv-present targets[%d].path empty/not a string: %v", caseFile, i, obj["path"])
			}
			out[normalizeVaultKey(path)] = struct{}{}
		}
	}
	if found == 0 {
		t.Fatalf("%s: kv-present task (%s) MISSING from plan — generate-step for secrets is gone", caseFile, kvPresentModuleAddr)
	}
	if found > 1 {
		t.Fatalf("%s: found %d kv-present tasks — plan is ambiguous for targets verification", caseFile, found)
	}
	return out
}

// isRedisSecretPath checks if path belongs to secrets of redis-incarnation under generation:
// everything under secret/redis/<inc> (master password secret/redis/<inc> + per-user
// secret/redis/<inc>/users/...). TLS-PEM-paths under operator-convention
// (secret/ops/..., see tls-essence-refs/case.yml) do NOT fall here — they are not
// generated by kv-present, operator provides PEM manually. Normalize mount-prefix before
// check so that logical ('secret/redis/...') and relative ('redis/...') match;
// incarnation name is not hardcoded — prefix redis/ is sufficient, no other redis-paths
// (except incarnation passwords) exist in the plan.
func isRedisSecretPath(path string) bool {
	return strings.HasPrefix(normalizeVaultKey(path), "redis/")
}

// assertReadSubsetOfGenerated performs the key check: read-set ⊆ generated-set. Any
// redis-secret-path read but not in targets kv-present is a failure (on fresh Vault,
// deploy render would fail on this path). Empty read-set is also a failure: guard lost
// its subject (deploy stopped reading secrets?).
func assertReadSubsetOfGenerated(t *testing.T, caseFile string) {
	t.Helper()
	readSet, generatedSet := renderCreateReadSet(t, caseFile)

	if len(readSet) == 0 {
		t.Fatalf("%s: no redis-secret paths were read — guard lost its subject (deploy stopped reading secrets?)", caseFile)
	}

	var missing []string
	for p := range readSet {
		if _, ok := generatedSet[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%s: readable secret-paths NOT covered by targets kv-present (generate won't create them → deploy render fails on fresh Vault):\n  missing: %v\n  generated: %v\n  read: %v",
			caseFile, missing, sortedSetKeys(generatedSet), sortedSetKeys(readSet))
	}
	t.Logf("%s: read-set (%d paths) ⊆ generated-set (%d targets)", caseFile, len(readSet), len(generatedSet))
}

func sortedSetKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRedisCreate_GeneratedSecretsCoverVaultReads_Sentinel — sentinel-mode, without
// operator-extra: read-set ⊆ targets kv-present.
func TestRedisCreate_GeneratedSecretsCoverVaultReads_Sentinel(t *testing.T) {
	assertReadSubsetOfGenerated(t, createSentinelCase)
}

// TestRedisCreate_GeneratedSecretsCoverVaultReads_Cluster — cluster-mode with
// operator-extra (cluster-acl-users carries input.users zeta/alpha).
func TestRedisCreate_GeneratedSecretsCoverVaultReads_Cluster(t *testing.T) {
	assertReadSubsetOfGenerated(t, createClusterAclCase)
}

// TestRedisCreate_GeneratedSecretsCoverVaultReads_SentinelOperatorExtra —
// sentinel-mode WITH operator-extra (sentinel-acl-users): covers drift for sentinel +
// input.users with a permanent case.
func TestRedisCreate_GeneratedSecretsCoverVaultReads_SentinelOperatorExtra(t *testing.T) {
	assertReadSubsetOfGenerated(t, createSentinelAclCase)
}

// TestRedisCreate_ReadSetContainsExpectedPaths — sanity check on the interception itself: in
// sentinel+operator-mode, read-set MUST contain auth-path default_admin and per-user
// path from operator-extra. Without this, guard could pass on empty read-set with
// broken interception (false positive). Check lower bound of the set.
//
// ★ REDESIGN default_admin (2026-06-30): previous master path redis/create (requirepass/
// replica-auth/sentinel auth_pass) in sentinel-branch NO LONGER READ — all intra-cluster
// AUTH (REPLICAOF masterauth+masteruser, SENTINEL MONITOR/connection-AUTH, health-PING)
// moved to secret/redis/<inc>/users/default_admin#password. Master path remains auth-path
// ONLY in cluster-branch (redis-deploy-cluster.yml still uses it — separate incomplete
// redesign), so sanity check on master path would move to cluster-case; here sentinel —
// we expect default_admin.
func TestRedisCreate_ReadSetContainsExpectedPaths(t *testing.T) {
	readSet, _ := renderCreateReadSet(t, createSentinelAclCase)
	for _, want := range []string{
		"redis/create/users/default_admin", // intra-cluster AUTH (replica/sentinel/health)
		"redis/create/users/zeta",          // operator-extra
		"redis/create/users/alpha",         // operator-extra
	} {
		if _, ok := readSet[want]; !ok {
			t.Errorf("expected readable path %q in read-set, only have: %v", want, sortedSetKeys(readSet))
		}
	}
}
