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
		t.Fatalf("display = %q, want local %q", display, want)
	}
	if !strings.Contains(string(data), "local") {
		t.Fatalf("data = %q, want content of the local file", data)
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
		t.Skip("chmod 000 does not block reading under the current uid (root?) - case not reproducible")
	}

	resolve := scenarioIncludeResolver(artifact.NewServiceLoader(t.TempDir(), nil), newServiceArtifactAt(root), "deploy")
	data, display, err := resolve("lib.yml")
	if err == nil {
		t.Fatalf("expected I/O error reading local file, got silent read display=%q data=%q (fallback masked permission denied)", display, data)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error treated as fs.ErrNotExist, want permission denied: %v", err)
	}
	if strings.Contains(string(data), "service-bait") {
		t.Fatalf("resolver returned the service-level decoy instead of an error: %q", data)
	}
}

// TestScenarioIncludeResolver_SharedSubdirectory is the NIM-694 guard for the
// shared-bodies layout: the common bodies of a scenario family live in
// `scenario/_create/`, so `include: _create/provision.yml` written in
// `scenario/create/main.yml` must resolve at the SERVICE level (one directory up
// from the scenario), while a subdirectory of the scenario itself keeps
// resolving locally and shadowing it.
func TestScenarioIncludeResolver_SharedSubdirectory(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "scenario", "_create", "provision.yml"), "- include: shared-body\n")
	mustWrite(t, filepath.Join(root, "scenario", "create", "parts", "extra.yml"), "- include: local-part\n")
	// Shadowing still holds one level down: a local `_create/deploy.yml` wins
	// over the service-level file of the same relative path.
	mustWrite(t, filepath.Join(root, "scenario", "create", "_create", "deploy.yml"), "- include: local-wins\n")
	mustWrite(t, filepath.Join(root, "scenario", "_create", "deploy.yml"), "- include: service-loses\n")

	resolve := scenarioIncludeResolver(artifact.NewServiceLoader(t.TempDir(), nil), newServiceArtifactAt(root), "create")

	for _, tc := range []struct {
		name        string
		wantDisplay string
		wantBody    string
	}{
		{"_create/provision.yml", "scenario/_create/provision.yml", "shared-body"},
		{"parts/extra.yml", "scenario/create/parts/extra.yml", "local-part"},
		{"_create/deploy.yml", "scenario/create/_create/deploy.yml", "local-wins"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, display, err := resolve(tc.name)
			if err != nil {
				t.Fatalf("resolve(%q): %v", tc.name, err)
			}
			if display != tc.wantDisplay {
				t.Errorf("display = %q, want %q", display, tc.wantDisplay)
			}
			if !strings.Contains(string(data), tc.wantBody) {
				t.Errorf("data = %q, want body %q", data, tc.wantBody)
			}
		})
	}
}

// TestScenarioIncludeResolver_TraversalClamped proves the second line of defence
// (NIM-694): the include grammar cannot express `..` at all, but if a target ever
// reaches the resolver anyway, securejoin inside ServiceLoader.ReadFile keeps it
// inside the snapshot. The witness is a real file one level ABOVE the snapshot
// root with the exact name the traversal aims at — a plain filepath.Join would
// return its content on the service-level tier.
func TestScenarioIncludeResolver_TraversalClamped(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "service")
	mustWrite(t, filepath.Join(root, "scenario", "create", "main.yml"), "name: create\n")
	mustWrite(t, filepath.Join(base, "etc", "passwd"), "- include: outside-the-snapshot\n")

	resolve := scenarioIncludeResolver(artifact.NewServiceLoader(t.TempDir(), nil), newServiceArtifactAt(root), "create")
	for _, name := range []string{"../../etc/passwd", "_create/../../../etc/passwd", "/etc/passwd"} {
		t.Run(name, func(t *testing.T) {
			data, display, err := resolve(name)
			if err == nil {
				t.Fatalf("resolve(%q) escaped the snapshot: display=%q data=%q", name, display, data)
			}
			if strings.Contains(string(data), "outside-the-snapshot") {
				t.Fatalf("resolve(%q) returned content from outside the snapshot: %q", name, data)
			}
		})
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
