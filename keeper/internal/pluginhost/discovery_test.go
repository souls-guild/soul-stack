package pluginhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
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

// writeNestedSlot создаёт R-nested-слот <root>/<ns>-<name>/<commit>/ +
// current → <commit> с manifest и бинарём (A1-S1 раскладка).
func writeNestedSlot(t *testing.T, root, key, commit, manifest, binName string) {
	t.Helper()
	pluginDir := filepath.Join(root, key)
	slot := filepath.Join(pluginDir, commit)
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatalf("mkdir slot: %v", err)
	}
	writeManifest(t, slot, manifest)
	writeFakeBinary(t, slot, binName, true)
	if err := os.Symlink(commit, filepath.Join(pluginDir, "current")); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
}

func TestDiscoverFiltersKeeperKinds(t *testing.T) {
	// Раскладываем все три kind-а в R-nested-слотах. Keeper-host оставляет
	// только cloud+ssh, soul_module падает в warnings.
	root := t.TempDir()
	const commit = "0123456789abcdef0123456789abcdef01234567"

	writeNestedSlot(t, root, "soulstack-aws", commit, `kind: cloud_driver
protocol_version: 1
namespace: soulstack
name: aws
spec: { provider_kind: aws, profile_schema: { type: object } }
`, "soul-cloud-aws")

	writeNestedSlot(t, root, "soulstack-vault-ssh", commit, `kind: ssh_provider
protocol_version: 1
namespace: soulstack
name: vault-ssh
spec: { provider_kind: vault_ssh_ca }
`, "soul-ssh-vault-ssh")

	writeNestedSlot(t, root, "wb-redis-failover", commit, `kind: soul_module
protocol_version: 1
namespace: wb
name: redis-failover
spec: { states: { promoted: {} } }
`, "soul-mod-redis-failover")

	found, warns, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("found = %d, want 2: %v", len(found), found)
	}
	if len(warns) != 1 {
		t.Errorf("warns = %d, want 1 (soul_module skipped): %v", len(warns), warns)
	}
}

func TestDiscoverRootMissing(t *testing.T) {
	_, _, err := Discover(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestFilterByCatalog(t *testing.T) {
	// Готовим найденные плагины: aws (cloud), gcp (cloud), vault-ssh (ssh),
	// teleport (ssh). В каталоге keeper.yml объявлены только aws и vault-ssh.
	mk := func(kind, name string) Discovered {
		return Discovered{Manifest: &Manifest{Kind: kind, Name: name, Namespace: "soulstack"}}
	}
	found := []Discovered{
		mk(KindCloudDriver, "aws"),
		mk(KindCloudDriver, "gcp"),
		mk(KindSSHProvider, "vault-ssh"),
		mk(KindSSHProvider, "teleport"),
	}
	plugins := &config.KeeperPlugins{
		CloudDrivers: []config.PluginCatalogEntry{
			{Name: "aws", Source: "git@example.com:soul-cloud-aws.git", Ref: "v1.0.0"},
			{Name: "yc", Source: "git@example.com:soul-cloud-yc.git", Ref: "v0.1.0"}, // нет в кеше
		},
		SSHProviders: []config.PluginCatalogEntry{
			{Name: "vault-ssh", Source: "git@example.com:soul-ssh-vault.git", Ref: "v1.0.0"},
		},
	}

	out, warns := FilterByCatalog(found, plugins)
	if len(out) != 2 {
		t.Fatalf("out = %d, want 2: %v", len(out), out)
	}
	names := map[string]bool{}
	for _, d := range out {
		names[d.Manifest.Name] = true
	}
	if !names["aws"] || !names["vault-ssh"] {
		t.Errorf("expected aws+vault-ssh, got %v", names)
	}

	// Должны быть warning-и: gcp/teleport не объявлены; yc объявлен, но не найден.
	var gotGcp, gotTeleport, gotYc bool
	for _, w := range warns {
		switch {
		case strings.Contains(w, "soulstack.gcp"):
			gotGcp = true
		case strings.Contains(w, "soulstack.teleport"):
			gotTeleport = true
		case strings.Contains(w, "name=yc"):
			gotYc = true
		}
	}
	if !gotGcp || !gotTeleport || !gotYc {
		t.Errorf("missing warnings (gcp=%v teleport=%v yc=%v): %v", gotGcp, gotTeleport, gotYc, warns)
	}
}

func TestFilterByCatalogNil(t *testing.T) {
	found := []Discovered{{Manifest: &Manifest{Kind: KindCloudDriver, Name: "aws"}}}
	out, warns := FilterByCatalog(found, nil)
	if len(out) != 0 {
		t.Errorf("expected 0 with nil catalog, got %d", len(out))
	}
	if len(warns) != 0 {
		t.Errorf("expected no warnings with nil catalog, got %v", warns)
	}
}
