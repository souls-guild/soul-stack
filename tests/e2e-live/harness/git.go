//go:build e2e_live

package harness

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// L3b service fixture: a per-test working-tree git repo under $TMP +
// registration in the service registry via the Operator API (POST
// /v1/services). Keeper's service loader clones the file://-URL as a
// regular remote (go-git PlainCloneContext, ref=main);
// SOUL_STACK_ALLOW_FILE_REPOS=1 is already set by NewStack on keeper
// init/run (see stack.go::runKeeperInit/startKeeperRun).
//
// Why: NewStack brings up PG/Redis/Vault/keeper + soul containers, but
// without a service_registry entry POST /v1/incarnations responds 422
// "service <name> is not registered" (ADR-029,
// incarnation_typed.go::ErrServiceNotRegistered). registerExampleService
// closes that gap BEFORE CreateIncarnation in the test flow.
//
// Universal: materializes cfg.ExamplePath under the name cfg.ServiceName —
// no hardcoded nginx (drift/redis-cluster-live/a custom node-exporter are
// all handled by the same path).
//
// Pattern (working-tree repo, not bare) and the deterministic fixture env
// are a verbatim port of L3a (tests/e2e/harness/git.go): go-git clones from
// a working-tree repo just fine, bare isn't required; a fixed author/date
// gives a stable commit SHA -> keeper reuses the snapshot cache instead of
// spawning orphans on every run.

// gitFixtureEnv — fixed git commit environment (deterministic SHA, parity
// with dev/provision.sh and the L3a harness).
var gitFixtureEnv = []string{
	"GIT_AUTHOR_NAME=soul-stack-e2e",
	"GIT_AUTHOR_EMAIL=e2e@soul-stack.local",
	"GIT_COMMITTER_NAME=soul-stack-e2e",
	"GIT_COMMITTER_EMAIL=e2e@soul-stack.local",
	"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
	"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
}

// registerExampleService materializes cfg.ExamplePath into a per-test git
// repo under $TMP and registers it in Keeper's service registry under
// cfg.ServiceName on the `main` branch. Called by NewStack after
// keeper-HTTP is ready, before soul containers are spawned (the order
// doesn't matter for CreateIncarnation — a soul isn't needed for
// registration). No-op if ServiceName/ExamplePath is empty.
func (s *Stack) registerExampleService(t *testing.T) {
	t.Helper()
	if s.cfg.ServiceName == "" || s.cfg.ExamplePath == "" {
		return
	}

	gitURL := s.materializeServiceRepo(t, s.cfg.ServiceName, s.cfg.ExamplePath)

	c := s.opClient(t)
	resp, status, err := c.post(context.Background(), "/v1/services", map[string]any{
		"name": s.cfg.ServiceName,
		"git":  gitURL,
		"ref":  "main",
	})
	if err != nil {
		t.Fatalf("registerExampleService %s: http: %v", s.cfg.ServiceName, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("registerExampleService %s: status %d, body=%s", s.cfg.ServiceName, status, string(resp))
	}
	var out struct {
		Name string `json:"name"`
		Git  string `json:"git"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("registerExampleService %s: decode: %v (body=%s)", s.cfg.ServiceName, err, string(resp))
	}
	t.Logf("registerExampleService: registered name=%s git=%s ref=%s (status=%d)", out.Name, gitURL, out.Ref, status)
}

// materializeServiceRepo copies the example directory into a per-test git
// repo under $TMP and makes one deterministic commit on the main branch.
// Returns the file://-URL.
func (s *Stack) materializeServiceRepo(t *testing.T, serviceName, relativePath string) string {
	t.Helper()

	srcDir := filepath.Join(repoRoot(t), relativePath)
	if _, err := os.Stat(srcDir); err != nil {
		t.Fatalf("materializeServiceRepo %s: source %s: %v", serviceName, srcDir, err)
	}

	repoDir := filepath.Join(s.tmpDir, "repos", serviceName)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("materializeServiceRepo %s: mkdir %s: %v", serviceName, repoDir, err)
	}

	// Init a working-tree repo on the main branch, copy the example
	// contents (without the root directory, parity with provision.sh
	// `cp -R src/. dest/`), commit with one deterministic commit.
	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	copyTree(t, srcDir, repoDir)
	runGit(t, repoDir, "add", "-A")
	// commit.gpgsign=false is local to the call: the operator's global
	// ~/.gitconfig may require a signature (gpg/ssh key) that isn't
	// available in the run environment — the fixture must be hermetic
	// and not depend on host settings.
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "e2e-live service snapshot from "+relativePath)

	return "file://" + repoDir
}

// runGit runs a git command in dir (empty dir -> cwd from args) with the
// deterministic fixture env. Fatal on error (with stdout+stderr).
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), gitFixtureEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\nOUTPUT:\n%s", args, err, out)
	}
}

// copyTree recursively copies the contents of src into dst (without src's
// root directory and without .git). Symmetric with `cp -R src/. dst/`; there
// are no symlinks in example services, this is enough for small example
// directories.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("copyTree: readdir %s: %v", src, err)
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dstPath, 0o755); err != nil {
				t.Fatalf("copyTree: mkdir %s: %v", dstPath, err)
			}
			copyTree(t, srcPath, dstPath)
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			t.Fatalf("copyTree: read %s: %v", srcPath, err)
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			t.Fatalf("copyTree: write %s: %v", dstPath, err)
		}
	}
}

// repoRoot returns the repository root (tests/e2e-live/<test>.go ->
// wd/../..), symmetric with locateKeeperBinary.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("repoRoot: getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}
