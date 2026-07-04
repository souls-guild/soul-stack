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

// Plugin-канал SoulModule (NIM-32 S1, ADR-065(b)/(f)/(g)): helpers доставки
// плагина `community.redis` на стенд ШТАТНЫМ путём — сборка бинаря →
// per-test git-репо в layout-е plugingit-резолвера → каталог
// `plugins.soul_modules[]` (Config.SoulModules) → Sigil-allow через Operator
// API. Trust-механизм не изобретается: allow — keeper-side seal (Signer
// подписывает бинарь из слота cache_root при POST /v1/plugins/sigils),
// seal-артефакт в git-репо плагина НЕ нужен.

// communityRedisPluginDir — исходники плагина относительно корня репо.
const communityRedisPluginDir = "examples/module/soul-mod-community-redis"

// CommunityRedisPluginRef — тег, под которым harness публикует плагин в
// per-test git-репо; ref для записи каталога и Sigil-allow.
const CommunityRedisPluginRef = "v1.0.0"

// communityRedisBinaryName — конвенция kind=soul_module для manifest.name=redis
// (shared/plugin::Manifest.BinaryName).
const communityRedisBinaryName = "soul-mod-redis"

// Кэш сборки — раз на процесс (go build плагина небыстрый; build-cache Go
// делает повторные процессы дешёвыми, но в одном прогоне не пересобираем).
var (
	communityRedisBuildOnce sync.Once
	communityRedisBinPath   string
	communityRedisBuildErr  error
)

// BuildCommunityRedisPlugin собирает soul-mod-community-redis (linux/amd64,
// кэш пер-процесс) и материализует per-test git-репо в layout-е, который
// ожидает plugingit-резолвер (ADR-026(g) F-fetch, parity fixtureRepo из
// keeper/internal/plugingit/resolver_test.go): manifest.yaml в корне +
// dist/soul-mod-redis, один коммит на main, тег [CommunityRedisPluginRef].
//
// Возвращает file://-URL репо для Config.SoulModules[].Source — file://-scheme
// у plugingit работает под SOUL_STACK_ALLOW_FILE_REPOS=1, который NewStack уже
// выставляет keeper-процессам (stack.go::runKeeperInit/startKeeperRun).
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
	// tag.gpgsign=false локально к вызову (как commit.gpgsign выше): глобальный
	// tag.gpgsign=true превращает lightweight-тег в annotated и требует message.
	runGit(t, repoDir, "-c", "tag.gpgsign=false", "tag", CommunityRedisPluginRef)

	return "file://" + repoDir
}

// buildCommunityRedisBinary — `GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64
// go build` исходников плагина (replace-директивы go.mod резолвятся внутри
// репо). Выход — вне репо (MkdirTemp), путь кэшируется на процесс.
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

// AllowSoulModule допускает плагин (namespace, name, ref) через Operator API
// POST /v1/plugins/sigils (ADR-026 S4a): keeper читает бинарь+manifest из
// слота `<cache_root>/<ns>-<name>/current/`, подписывает Signer-ом и пишет
// допуск в plugin_sigils. Возвращает sha256 допущенного бинаря из 201-ответа.
//
// Слот обязан быть материализован ДО вызова — штатно это делает `keeper run`
// на старте (plugingit.ResolveCatalog по `plugins.soul_modules[]`), т.е.
// достаточно передать запись в Config.SoulModules.
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
		t.Fatalf("AllowSoulModule %s/%s/%s: пустой sha256 в 201 body=%s", namespace, name, ref, string(resp))
	}
	return out.SHA256
}

// PluginSigilItem — элемент items[] GET /v1/plugins/sigils (подмножество
// wire-полей PluginSigilView, нужное assert-ам).
type PluginSigilItem struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ref       string `json:"ref"`
	SHA256    string `json:"sha256"`
}

// ListPluginSigils возвращает активные Sigil-допуски через Operator API
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
