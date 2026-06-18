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

// Service-fixture L3b: per-test working-tree git-репо в $TMP + регистрация в
// реестре сервисов через Operator API (POST /v1/services). Service-loader
// Keeper-а клонирует file://-URL как обычный remote (go-git PlainCloneContext,
// ref=main); SOUL_STACK_ALLOW_FILE_REPOS=1 уже выставлен NewStack-ом на
// keeper init/run (см. stack.go::runKeeperInit/startKeeperRun).
//
// Зачем: NewStack поднимает PG/Redis/Vault/keeper + soul-контейнеры, но без
// записи в service_registry POST /v1/incarnations отвечает 422 «service <name>
// is not registered» (ADR-029, incarnation_typed.go::ErrServiceNotRegistered).
// registerExampleService закрывает разрыв ДО CreateIncarnation в потоке теста.
//
// Universal: материализуется cfg.ExamplePath под именем cfg.ServiceName —
// никакого хардкода nginx (drift/redis-cluster-live/пользовательский
// node-exporter подхватываются тем же путём).
//
// Паттерн (working-tree-репо, не bare) и детерминированный fixture-env —
// дословный порт L3a (tests/e2e/harness/git.go): go-git клонирует из
// working-tree-репо штатно, bare не требуется; фиксированные author/date дают
// стабильный commit-SHA → keeper переиспользует snapshot-cache, а не плодит
// сироты на каждый прогон.

// gitFixtureEnv — фиксированное окружение git-commit-а (детерминированный SHA,
// parity с dev/provision.sh и L3a-harness-ом).
var gitFixtureEnv = []string{
	"GIT_AUTHOR_NAME=soul-stack-e2e",
	"GIT_AUTHOR_EMAIL=e2e@soul-stack.local",
	"GIT_COMMITTER_NAME=soul-stack-e2e",
	"GIT_COMMITTER_EMAIL=e2e@soul-stack.local",
	"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
	"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
}

// registerExampleService материализует cfg.ExamplePath в per-test git-репо под
// $TMP и регистрирует его в реестре сервисов Keeper-а под cfg.ServiceName на
// ветке `main`. Вызывается NewStack-ом после готовности keeper-HTTP, до spawn-а
// soul-контейнеров (порядок к CreateIncarnation роли не играет — соул не нужен
// для регистрации). No-op при пустом ServiceName/ExamplePath.
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

// materializeServiceRepo копирует example-каталог в per-test git-репо под $TMP
// и делает один детерминированный commit на ветке main. Возвращает file://-URL.
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

	// init working-tree-репо на ветке main, скопировать содержимое example-а
	// (без корневого каталога, parity с provision.sh `cp -R src/. dest/`),
	// зафиксировать одним детерминированным commit-ом.
	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	copyTree(t, srcDir, repoDir)
	runGit(t, repoDir, "add", "-A")
	// commit.gpgsign=false локально к вызову: глобальный ~/.gitconfig оператора
	// может требовать подпись (gpg/ssh-ключ), которого в среде прогона нет —
	// fixture обязан быть герметичным и не зависеть от настроек хоста.
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "e2e-live service snapshot from "+relativePath)

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

// copyTree рекурсивно копирует содержимое src в dst (без корневого каталога src
// и без .git). Симметрично `cp -R src/. dst/`; symlink-ов в example-сервисах
// нет, достаточно для маленьких example-каталогов.
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

// repoRoot возвращает корень репозитория (tests/e2e-live/<test>.go → wd/../..),
// симметрично locateKeeperBinary.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("repoRoot: getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}
