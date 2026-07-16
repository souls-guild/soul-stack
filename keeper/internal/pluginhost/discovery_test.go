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

// writeNestedSlot creates R-nested slot <root>/<ns>-<name>/<commit>/ +
// current → <commit> with manifest and binary (A1-S1 layout).
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
	// Lay out all three kinds in R-nested slots. Keeper-host discovers
	// cloud+ssh+soul_module (S1 epic core.module.installed: keeper is registry
	// of SoulModule plugins for distribution to Souls).
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
	if len(found) != 3 {
		t.Fatalf("found = %d, want 3 (cloud+ssh+soul_module): %v", len(found), found)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %d, want 0: %v", len(warns), warns)
	}
	kinds := map[string]bool{}
	for _, d := range found {
		kinds[d.Manifest.Kind] = true
	}
	if !kinds[KindSoulModule] {
		t.Errorf("soul_module not discovered: %v", kinds)
	}
}

func TestDiscoverRootMissing(t *testing.T) {
	_, _, err := Discover(filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestFilterByCatalog(t *testing.T) {
	// Prepare discovered plugins of three kinds; keeper.yml catalog declares
	// only aws (cloud), vault-ssh (ssh), and redis (soul_module).
	mk := func(kind, name string) Discovered {
		return Discovered{Manifest: &Manifest{Kind: kind, Name: name, Namespace: "soulstack"}}
	}
	found := []Discovered{
		mk(KindCloudDriver, "aws"),
		mk(KindCloudDriver, "gcp"),
		mk(KindSSHProvider, "vault-ssh"),
		mk(KindSSHProvider, "teleport"),
		mk(KindSoulModule, "redis"),
		mk(KindSoulModule, "postgres"),
	}
	plugins := &config.KeeperPlugins{
		CloudDrivers: []config.PluginCatalogEntry{
			{Name: "aws", Source: "git@example.com:soul-cloud-aws.git", Ref: "v1.0.0"},
			{Name: "yc", Source: "git@example.com:soul-cloud-yc.git", Ref: "v0.1.0"}, // not in cache
		},
		SSHProviders: []config.PluginCatalogEntry{
			{Name: "vault-ssh", Source: "git@example.com:soul-ssh-vault.git", Ref: "v1.0.0"},
		},
		SoulModules: []config.PluginCatalogEntry{
			{Name: "redis", Source: "git@example.com:community-redis.git", Ref: "v1.0.0"},
			{Name: "mongo", Source: "git@example.com:community-mongo.git", Ref: "v0.1.0"}, // not in cache
		},
	}

	out, warns := FilterByCatalog(found, plugins)
	if len(out) != 3 {
		t.Fatalf("out = %d, want 3: %v", len(out), out)
	}
	names := map[string]bool{}
	for _, d := range out {
		names[d.Manifest.Name] = true
	}
	if !names["aws"] || !names["vault-ssh"] || !names["redis"] {
		t.Errorf("expected aws+vault-ssh+redis, got %v", names)
	}

	// Warnings: gcp/teleport/postgres not declared; yc/mongo declared but not discovered.
	var gotGcp, gotTeleport, gotYc, gotPostgres, gotMongo bool
	for _, w := range warns {
		switch {
		case strings.Contains(w, "soulstack.gcp"):
			gotGcp = true
		case strings.Contains(w, "soulstack.teleport"):
			gotTeleport = true
		case strings.Contains(w, "name=yc"):
			gotYc = true
		case strings.Contains(w, "soulstack.postgres"):
			gotPostgres = true
		case strings.Contains(w, "name=mongo"):
			gotMongo = true
		}
	}
	if !gotGcp || !gotTeleport || !gotYc || !gotPostgres || !gotMongo {
		t.Errorf("missing warnings (gcp=%v teleport=%v yc=%v postgres=%v mongo=%v): %v",
			gotGcp, gotTeleport, gotYc, gotPostgres, gotMongo, warns)
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
