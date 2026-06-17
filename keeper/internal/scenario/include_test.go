package scenario

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// newServiceArtifactAt оборачивает каталог снапшота в ServiceArtifact для
// чтения через ServiceLoader.ReadFile (LocalDir — корень securejoin-резолва).
func newServiceArtifactAt(localDir string) *artifact.ServiceArtifact {
	return &artifact.ServiceArtifact{
		Ref:      artifact.ServiceRef{Name: "test-service"},
		SHA1:     "deadbeef",
		LocalDir: localDir,
	}
}

// TestScenarioIncludeResolver_LocalShadowsService — happy-path двухуровневого
// резолва: локальный `scenario/<name>/<file>` перекрывает service-level
// `scenario/<file>`; display-путь — локальный.
func TestScenarioIncludeResolver_LocalShadowsService(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "scenario", "deploy", "lib.yml"), "- include: local\n")
	mustWrite(t, filepath.Join(root, "scenario", "lib.yml"), "- include: service\n")

	resolve := scenarioIncludeResolver(artifact.NewServiceLoader(t.TempDir(), nil), newServiceArtifactAt(root), "deploy")
	data, display, err := resolve("lib.yml")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.ToSlash(filepath.Join("scenario", "deploy", "lib.yml")); display != want {
		t.Fatalf("display = %q, want локальный %q", display, want)
	}
	if !strings.Contains(string(data), "local") {
		t.Fatalf("data = %q, want содержимое локального файла", data)
	}
}

// TestScenarioIncludeResolver_ServiceFallback — при отсутствии локального файла
// (fs.ErrNotExist) фоллбэк на service-level; display-путь — service-level.
func TestScenarioIncludeResolver_ServiceFallback(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "scenario", "lib.yml"), "- include: service\n")

	resolve := scenarioIncludeResolver(artifact.NewServiceLoader(t.TempDir(), nil), newServiceArtifactAt(root), "deploy")
	_, display, err := resolve("lib.yml")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.ToSlash(filepath.Join("scenario", "lib.yml")); display != want {
		t.Fatalf("display = %q, want service-level %q", display, want)
	}
}

// TestScenarioIncludeResolver_IOErrorNotMasked — критичное защитное поведение
// (orchestration.md §6, include.go): фоллбэк на service-level срабатывает ТОЛЬКО
// при fs.ErrNotExist. Прочая I/O-ошибка локального файла (permission denied)
// должна вернуться сразу, НЕ маскироваться молчаливым выбором service-level-
// файла. Регрессия = чтение НЕ того файла без единого сигнала автору.
//
// Воспроизведение: локальный файл существует, но chmod 000 (нечитаем); рядом
// лежит валидный service-level файл-приманка. Ожидаем ошибку чтения, а НЕ
// содержимое приманки.
func TestScenarioIncludeResolver_IOErrorNotMasked(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "scenario", "deploy", "lib.yml")
	mustWrite(t, local, "- include: local\n")
	// Приманка: если фоллбэк ошибочно сработает на I/O-ошибке — резолвер
	// молча вернёт ЭТОТ файл вместо ошибки.
	mustWrite(t, filepath.Join(root, "scenario", "lib.yml"), "- include: service-bait\n")

	if err := os.Chmod(local, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(local, 0o644) })

	// Под root (или иной обходящей права средой) chmod 000 не даёт permission
	// denied — кейс невоспроизводим, пропускаем, но тест остаётся в коде.
	if _, err := os.ReadFile(local); err == nil {
		t.Skip("chmod 000 не блокирует чтение под текущим uid (root?) — кейс невоспроизводим")
	}

	resolve := scenarioIncludeResolver(artifact.NewServiceLoader(t.TempDir(), nil), newServiceArtifactAt(root), "deploy")
	data, display, err := resolve("lib.yml")
	if err == nil {
		t.Fatalf("ожидалась I/O-ошибка чтения локального файла, получено молчаливое чтение display=%q data=%q (фоллбэк замаскировал permission denied)", display, data)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ошибка трактована как fs.ErrNotExist, want permission denied: %v", err)
	}
	if strings.Contains(string(data), "service-bait") {
		t.Fatalf("резолвер вернул service-level-приманку вместо ошибки: %q", data)
	}
}

func mustWrite(t *testing.T, full, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}
