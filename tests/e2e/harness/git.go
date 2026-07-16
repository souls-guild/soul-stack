//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Service fixture: a per-test bare git repo under $TMP + registration in the
// service registry via the Operator API (POST /v1/services). The Keeper's
// service loader reads the file:// URL as a regular remote
// (SOUL_STACK_ALLOW_FILE_REPOS=1 is already set by NewStack on keeper
// init/run, see stack.go).
//
// Why: NewStack brings up PG/Redis/Vault/keeper, but does NOT register any
// service. Without a service_registry row, POST /v1/incarnations answers 422
// "service <name> is not registered" (incarnation/upgrade_prepare.go::
// ErrServiceNotRegistered). RegisterService closes this gap for apply e2e.
//
// Determinism contract — same as dev/provision.sh::provision_git_repo: fixed
// author/committer/date -> a stable commit SHA for unchanged example-directory
// contents -> the keeper reuses the snapshot from snapshot-cache instead of
// producing an orphan on every run.

// gitFixtureEnv — fixed environment for the git commit (deterministic SHA,
// parity with dev/provision.sh).
var gitFixtureEnv = []string{
	"GIT_AUTHOR_NAME=soul-stack-e2e",
	"GIT_AUTHOR_EMAIL=e2e@soul-stack.local",
	"GIT_COMMITTER_NAME=soul-stack-e2e",
	"GIT_COMMITTER_EMAIL=e2e@soul-stack.local",
	"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
	"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
}

// RegisterService materializes a service directory from repo (relativePath,
// e.g. "examples/service/smoke-nginx") into a per-test git repo under $TMP and
// registers it in the Keeper's service registry under serviceName on the
// `main` branch.
//
// Steps:
//  1. git init -b main + add -A + commit (deterministic SHA) into
//     $TMP/repos/<serviceName>.git (a working tree, not bare — the keeper's
//     file:// clone reads a checked-out repo).
//  2. POST /v1/services {name, git: file://..., ref: main}; 201 -> a row in
//     service_registry, visible to CreateIncarnation/RunScenario.
//
// relativePath is resolved from repo-root (like locateKeeperBinary). Any
// non-201 — t.Fatal with the response body. Returns the repo's file:// URL
// (for diagnostics/reuse).
func (s *Stack) RegisterService(t *testing.T, serviceName, relativePath string) string {
	t.Helper()

	gitURL := s.materializeServiceRepo(t, serviceName, relativePath)

	c := s.opClient(t)
	resp, status, err := c.post(context.Background(), "/v1/services", map[string]any{
		"name": serviceName,
		"git":  gitURL,
		"ref":  "main",
	})
	if err != nil {
		t.Fatalf("RegisterService %s: http: %v", serviceName, err)
	}
	if status != http.StatusCreated {
		t.Fatalf("RegisterService %s: status %d, body=%s", serviceName, status, string(resp))
	}
	var out struct {
		Name string `json:"name"`
		Git  string `json:"git"`
		Ref  string `json:"ref"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("RegisterService %s: decode: %v (body=%s)", serviceName, err, string(resp))
	}
	return gitURL
}

// materializeServiceRepo copies the example directory into a per-test git
// repo under $TMP and makes one deterministic commit on the main branch.
// Returns the file:// URL.
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

	// Copy the contents of srcDir/* into repoDir (without the root directory),
	// parity with provision.sh `cp -R "${src}/." "${dest}/"`.
	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	copyTree(t, srcDir, repoDir)
	runGit(t, repoDir, "add", "-A")
	// commit.gpgsign=false scoped to the call: the operator's global
	// ~/.gitconfig may require a signature (gpg/ssh key) not present in the
	// run environment — the fixture must be hermetic and not depend on host
	// settings (parity with the L3b harness and harness/destiny.go).
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "e2e service snapshot from "+relativePath)

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
// root directory, without .git). Symmetric to `cp -R src/. dst/`. Sufficient
// for small example directories; example services have no symlinks.
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

// repoRoot returns the repository root (tests/e2e/<test>.go -> wd/../..),
// symmetric with locateKeeperBinary.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("repoRoot: getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

var _ = fmt.Sprintf // reserved for diagnostics of future helpers
