//go:build e2e_live

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// SoulModule plugin channel (NIM-32 S1, ADR-065(b)/(f)/(g)): helpers to deliver
// the `community.redis` plugin to the stand via the REGULAR path - build the
// binary -> per-test git repo in the layout the plugingit resolver expects ->
// `plugins.soul_modules[]` catalog (Config.SoulModules) -> Sigil-allow via the
// Operator API. No trust mechanism is invented: allow is a keeper-side seal
// (the Signer signs the binary from the cache_root slot on POST
// /v1/plugins/sigils), no seal artifact is needed in the plugin git repo.

// communityRedisPluginDir - plugin sources relative to the repo root.
const communityRedisPluginDir = "examples/module/soul-mod-community-redis"

// CommunityRedisPluginRef - tag under which the harness publishes the plugin
// in the per-test git repo; ref for the catalog entry and Sigil-allow.
const CommunityRedisPluginRef = "v1.0.0"

// communityRedisBinaryName - kind=soul_module convention for manifest.name=redis
// (shared/plugin::Manifest.BinaryName).
const communityRedisBinaryName = "soul-mod-redis"

// Build cache - once per process (go build of the plugin isn't fast; Go's
// build cache makes repeated processes cheap, but we don't rebuild within one run).
var (
	communityRedisBuildOnce sync.Once
	communityRedisBinPath   string
	communityRedisBuildErr  error
)

// BuildCommunityRedisPlugin builds soul-mod-community-redis (linux/amd64,
// per-process cache) and materializes a per-test git repo in the layout the
// plugingit resolver expects (ADR-026(g) F-fetch, parity with fixtureRepo in
// keeper/internal/plugingit/resolver_test.go): manifest.yaml at the root +
// dist/soul-mod-redis, one commit on main, tag [CommunityRedisPluginRef].
//
// Returns the file:// URL of the repo for Config.SoulModules[].Source - the
// file:// scheme works for plugingit under SOUL_STACK_ALLOW_FILE_REPOS=1, which
// NewStack already sets for keeper processes (stack.go::runKeeperInit/startKeeperRun).
func BuildCommunityRedisPlugin(t *testing.T) string {
	t.Helper()
	bin := buildCommunityRedisBinary(t)

	repoDir := filepath.Join(t.TempDir(), "soul-mod-community-redis-repo")
	distDir := filepath.Join(repoDir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: mkdir %s: %v", distDir, err)
	}

	manifest, err := os.ReadFile(filepath.Join(repoRoot(t), communityRedisPluginDir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: read manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "manifest.yaml"), manifest, 0o644); err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: write manifest: %v", err)
	}
	binary, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: read built binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(distDir, communityRedisBinaryName), binary, 0o755); err != nil {
		t.Fatalf("BuildCommunityRedisPlugin: write dist binary: %v", err)
	}

	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "community.redis plugin snapshot")
	// tag.gpgsign=false is scoped to this call (like commit.gpgsign above): a
	// global tag.gpgsign=true would turn the lightweight tag into annotated and
	// require a message.
	runGit(t, repoDir, "-c", "tag.gpgsign=false", "tag", CommunityRedisPluginRef)

	return "file://" + repoDir
}

// buildCommunityRedisBinary - `GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64
// go build` of the plugin sources (go.mod replace directives resolve inside the
// repo). Output goes outside the repo (MkdirTemp), path is cached per process.
func buildCommunityRedisBinary(t *testing.T) string {
	t.Helper()
	communityRedisBuildOnce.Do(func() {
		outDir, err := os.MkdirTemp("", "soul-mod-build-")
		if err != nil {
			communityRedisBuildErr = fmt.Errorf("mkdtemp: %w", err)
			return
		}
		out := filepath.Join(outDir, communityRedisBinaryName)
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = filepath.Join(repoRoot(t), communityRedisPluginDir)
		cmd.Env = append(os.Environ(),
			"GOWORK=off", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
		if output, err := cmd.CombinedOutput(); err != nil {
			communityRedisBuildErr = fmt.Errorf("go build %s: %w\nOUTPUT:\n%s",
				communityRedisPluginDir, err, output)
			return
		}
		communityRedisBinPath = out
	})
	if communityRedisBuildErr != nil {
		t.Fatalf("buildCommunityRedisBinary: %v", communityRedisBuildErr)
	}
	return communityRedisBinPath
}

// AllowSoulModule allows a plugin (namespace, name, ref) via Operator API
// POST /v1/plugins/sigils (ADR-026 S4a): keeper reads the binary+manifest from
// the `<cache_root>/<ns>-<name>/current/` slot, signs it with the Signer, and
// writes the allow entry to plugin_sigils. Returns the sha256 of the allowed
// binary from the 201 response.
//
// The slot must be materialized BEFORE this call - normally `keeper run` does
// this at startup (plugingit.ResolveCatalog over `plugins.soul_modules[]`), so
// it's enough to pass the entry in Config.SoulModules.
func (s *Stack) AllowSoulModule(t *testing.T, namespace, name, ref string) string {
	t.Helper()
	c := s.opClient(t)
	resp, status, err := c.post(context.Background(), "/v1/plugins/sigils", map[string]any{
		"namespace": namespace,
		"name":      name,
		"ref":       ref,
	})
	if err != nil {
		t.Fatalf("AllowSoulModule %s/%s/%s: http: %v", namespace, name, ref, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("AllowSoulModule %s/%s/%s: status %d, body=%s", namespace, name, ref, status, string(resp))
	}
	var out struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("AllowSoulModule %s/%s/%s: decode: %v (body=%s)", namespace, name, ref, err, string(resp))
	}
	if out.SHA256 == "" {
		t.Fatalf("AllowSoulModule %s/%s/%s: empty sha256 in 201 body=%s", namespace, name, ref, string(resp))
	}
	return out.SHA256
}

// PluginSigilItem - an items[] element of GET /v1/plugins/sigils (subset of
// PluginSigilView wire fields needed by the asserts).
type PluginSigilItem struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
	SHA256    string `json:"sha256"`
}

// ListPluginSigils returns active Sigil allows via Operator API
// GET /v1/plugins/sigils.
func (s *Stack) ListPluginSigils(t *testing.T) []PluginSigilItem {
	t.Helper()
	c := s.opClient(t)
	resp, status, err := c.get(context.Background(), "/v1/plugins/sigils")
	if err != nil {
		t.Fatalf("ListPluginSigils: http: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("ListPluginSigils: status %d, body=%s", status, string(resp))
	}
	var out struct {
		Items []PluginSigilItem `json:"items"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("ListPluginSigils: decode: %v (body=%s)", err, string(resp))
	}
	return out.Items
}
