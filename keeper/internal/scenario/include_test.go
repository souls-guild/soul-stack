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

// newServiceArtifactAt wraps a snapshot directory in a ServiceArtifact for
// reading via ServiceLoader.ReadFile (LocalDir is the securejoin-resolve root).
func newServiceArtifactAt(localDir string) *artifact.ServiceArtifact {
	return &artifact.ServiceArtifact{
		Ref:      artifact.ServiceRef{Name: "test-service"},
		SHA1:     "deadbeef",
		LocalDir: localDir,
	}
}

// TestScenarioIncludeResolver_LocalShadowsService — happy path of two-level
// resolution: the local `scenario/<name>/<file>` shadows the service-level
// `scenario/<file>`; the display path is the local one.
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

// TestScenarioIncludeResolver_ServiceFallback — when the local file is absent
// (fs.ErrNotExist), falls back to service-level; the display path is service-level.
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

// TestScenarioIncludeResolver_IOErrorNotMasked — critical protective behavior
// (orchestration.md §6, include.go): the service-level fallback triggers ONLY
// on fs.ErrNotExist. Any other I/O error on the local file (permission denied)
// must be returned immediately, NOT masked by silently picking the service-level
// file. A regression here means reading the WRONG file with zero signal to the author.
//
// Reproduction: the local file exists but is chmod 000 (unreadable); next to it
// sits a valid service-level decoy file. We expect a read error, NOT
// the decoy's content.
func TestScenarioIncludeResolver_IOErrorNotMasked(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "scenario", "deploy", "lib.yml")
	mustWrite(t, local, "- include: local\n")
	// Decoy: if the fallback wrongly triggers on an I/O error, the resolver
	// will silently return THIS file instead of an error.
	mustWrite(t, filepath.Join(root, "scenario", "lib.yml"), "- include: service-bait\n")

	if err := os.Chmod(local, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(local, 0o644) })

	// Under root (or another permission-bypassing environment) chmod 000 doesn't cause permission
	// denied — the case is unreproducible, skip it, but keep the test in the code.
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
