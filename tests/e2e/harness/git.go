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

// Service-fixture: per-test bare git-repo в $TMP + регистрация в реестре
// сервисов через Operator API (POST /v1/services). Service-loader Keeper-а
// читает file://-URL как обычный remote (SOUL_STACK_ALLOW_FILE_REPOS=1 уже
// выставлен NewStack-ом на keeper init/run, см. stack.go).
//
// Зачем: NewStack поднимает PG/Redis/Vault/keeper, но НЕ регистрирует ни одного
// сервиса. Без записи в service_registry POST /v1/incarnations отвечает 422
// «service <name> is not registered» (incarnation/upgrade_prepare.go::
// ErrServiceNotRegistered). RegisterService закрывает этот разрыв для apply-e2e.
//
// Контракт детерминированности — как dev/provision.sh::provision_git_repo:
// фиксированные author/committer/date → стабильный commit-SHA при неизменном
// содержимом example-каталога → keeper переиспользует снапшот в snapshot-cache,
// а не плодит сироты на каждый прогон.

// gitFixtureEnv — фиксированное окружение git-commit-а (детерминированный SHA,
// parity с dev/provision.sh).
var gitFixtureEnv = []string{
	"GIT_AUTHOR_NAME=soul-stack-e2e",
	"GIT_AUTHOR_EMAIL=e2e@soul-stack.local",
	"GIT_COMMITTER_NAME=soul-stack-e2e",
	"GIT_COMMITTER_EMAIL=e2e@soul-stack.local",
	"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
	"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
}

// RegisterService материализует service-каталог из repo (relativePath, например
// "examples/service/smoke-nginx") в per-test git-репо под $TMP и регистрирует
// его в реестре сервисов Keeper-а под именем serviceName на ветке `main`.
//
// Шаги:
//  1. git init -b main + add -A + commit (детерминированный SHA) в
//     $TMP/repos/<serviceName>.git (рабочее дерево, не bare — file://-clone
//     keeper-а читает и checked-out-репо).
//  2. POST /v1/services {name, git: file://..., ref: main}; 201 → запись в
//     service_registry, видна CreateIncarnation/RunScenario.
//
// relativePath резолвится от repo-root (как locateKeeperBinary). Любой не-201 —
// t.Fatal с телом ответа. Возвращает file://-URL репо (для диагностики/повторного
// использования).
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

// materializeServiceRepo копирует example-каталог в per-test git-репо под $TMP и
// делает один детерминированный commit на ветке main. Возвращает file://-URL.
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

	// Копируем содержимое srcDir/* в repoDir (без корневого каталога), parity с
	// provision.sh `cp -R "${src}/." "${dest}/"`.
	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	copyTree(t, srcDir, repoDir)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-q", "-m", "e2e service snapshot from "+relativePath)

	return "file://" + repoDir
}

// runGit выполняет git-команду в dir (пустой dir → cwd по аргументам) с
// детерминированным fixture-env. Fatal при ошибке (с stdout+stderr).
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

// copyTree рекурсивно копирует содержимое src в dst (без корневого каталога src,
// без .git). Симметрично `cp -R src/. dst/`. Достаточно для маленьких
// example-каталогов; symlink-ов в example-сервисах нет.
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

// repoRoot возвращает корень репозитория (tests/e2e/<test>.go → wd/../..),
// симметрично locateKeeperBinary.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("repoRoot: getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

var _ = fmt.Sprintf // зарезервировано под диагностику будущих helper-ов
