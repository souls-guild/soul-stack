package config

import "testing"

// plugins.soul_modules[] — the SoulModule plugin catalog (core.module.installed epic,
// S1): the same PluginCatalogEntry as cloud_drivers/ssh_providers; source/ref are
// validated at resolve (plugingit), config phase — parse + unknown_key.
func TestPluginsCatalog_SoulModulesParsed(t *testing.T) {
	src := keeperBaseRequired + `plugins:
  soul_modules:
    - { name: redis, source: "ssh://git@example.com/community-redis.git", ref: "v1.2.0" }
`
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatal("plugins.soul_modules should be a known schema key")
	}
	if cfg.Plugins == nil || len(cfg.Plugins.SoulModules) != 1 {
		t.Fatalf("SoulModules: want 1 entry, got %+v", cfg.Plugins)
	}
	e := cfg.Plugins.SoulModules[0]
	if e.Name != "redis" || e.Source != "ssh://git@example.com/community-redis.git" || e.Ref != "v1.2.0" {
		t.Fatalf("SoulModules[0] = %+v", e)
	}
}
