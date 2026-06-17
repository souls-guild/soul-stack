package pluginhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, sharedplugin.FileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func writeFakeBinary(t *testing.T, dir, name string, executable bool) {
	t.Helper()
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatalf("write bin: %v", err)
	}
}

func TestDiscoverAllKinds(t *testing.T) {
	root := t.TempDir()

	// soul_module
	d1 := filepath.Join(root, "wb-redis-failover")
	if err := os.Mkdir(d1, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeManifest(t, d1, `kind: soul_module
protocol_version: 1
namespace: wb
name: redis-failover
spec: { states: { promoted: {} } }
`)
	writeFakeBinary(t, d1, "soul-mod-redis-failover", true)

	// cloud_driver
	d2 := filepath.Join(root, "soulstack-aws")
	if err := os.Mkdir(d2, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeManifest(t, d2, `kind: cloud_driver
protocol_version: 1
namespace: soulstack
name: aws
spec: { provider_kind: aws, profile_schema: { type: object } }
`)
	writeFakeBinary(t, d2, "soul-cloud-aws", true)

	// ssh_provider
	d3 := filepath.Join(root, "soulstack-vault-ssh")
	if err := os.Mkdir(d3, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeManifest(t, d3, `kind: ssh_provider
protocol_version: 1
namespace: soulstack
name: vault-ssh
spec: { provider_kind: vault_ssh_ca }
`)
	writeFakeBinary(t, d3, "soul-ssh-vault-ssh", true)

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if len(found) != 3 {
		t.Fatalf("found = %d, want 3", len(found))
	}

	// FilterByKinds: soul-side keeps только soul_module.
	soulOnly, w := FilterByKinds(found, []string{sharedplugin.KindSoulModule})
	if len(soulOnly) != 1 || soulOnly[0].Manifest.Name != "redis-failover" {
		t.Errorf("soul filter: %v", soulOnly)
	}
	if len(w) != 2 {
		t.Errorf("expected 2 warnings (cloud, ssh skipped), got %d: %v", len(w), w)
	}

	// FilterByKinds: keeper-side keeps cloud + ssh.
	keeperOnly, _ := FilterByKinds(found, []string{sharedplugin.KindCloudDriver, sharedplugin.KindSSHProvider})
	if len(keeperOnly) != 2 {
		t.Errorf("keeper filter: want 2 got %d", len(keeperOnly))
	}
}

func TestDiscoverSkipsBadEntries(t *testing.T) {
	root := t.TempDir()

	// 1. Невалидный manifest.
	d1 := filepath.Join(root, "broken")
	_ = os.Mkdir(d1, 0o755)
	writeManifest(t, d1, `not: a valid manifest`)

	// 2. Manifest есть, бинаря нет.
	d2 := filepath.Join(root, "wb-noop")
	_ = os.Mkdir(d2, 0o755)
	writeManifest(t, d2, `kind: soul_module
protocol_version: 1
namespace: wb
name: noop
spec: { states: { run: {} } }
`)

	// 3. Бинарь без +x.
	d3 := filepath.Join(root, "wb-nx")
	_ = os.Mkdir(d3, 0o755)
	writeManifest(t, d3, `kind: soul_module
protocol_version: 1
namespace: wb
name: nx
spec: { states: { run: {} } }
`)
	writeFakeBinary(t, d3, "soul-mod-nx", false)

	// 4. Файл в корне (не директория) — игнорируется без warning.
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 5. Валидный плагин — должен найтись.
	d5 := filepath.Join(root, "wb-ok")
	_ = os.Mkdir(d5, 0o755)
	writeManifest(t, d5, `kind: soul_module
protocol_version: 1
namespace: wb
name: ok
spec: { states: { run: {} } }
`)
	writeFakeBinary(t, d5, "soul-mod-ok", true)

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found = %d, want 1", len(found))
	}
	if found[0].Manifest.Name != "ok" {
		t.Errorf("found name = %q", found[0].Manifest.Name)
	}
	if len(warns) != 3 {
		t.Errorf("warns = %d, want 3 (broken, missing-bin, non-exec):\n%v", len(warns), warns)
	}
}

func TestDiscoverRootMissing(t *testing.T) {
	_, _, err := Discover(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestFilterByKindsEmptyAllowedReturnsAll(t *testing.T) {
	input := []Discovered{
		{Manifest: &sharedplugin.Manifest{Kind: sharedplugin.KindCloudDriver, Name: "aws"}},
		{Manifest: &sharedplugin.Manifest{Kind: sharedplugin.KindSSHProvider, Name: "ssh"}},
	}
	out, warns := FilterByKinds(input, nil)
	if len(out) != len(input) {
		t.Errorf("expected passthrough with nil allowedKinds, got %d", len(out))
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings, got %v", warns)
	}
}

func TestFilterByKindsWarningMentionsKind(t *testing.T) {
	input := []Discovered{
		{Dir: "/some/dir", Manifest: &sharedplugin.Manifest{Kind: sharedplugin.KindCloudDriver, Name: "aws"}},
	}
	_, warns := FilterByKinds(input, []string{sharedplugin.KindSoulModule})
	if len(warns) != 1 || !strings.Contains(warns[0], "cloud_driver") {
		t.Errorf("warns = %v", warns)
	}
}
