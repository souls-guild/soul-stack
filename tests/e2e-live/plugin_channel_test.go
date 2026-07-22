//go:build e2e_live

// L3b S1 SoulModule plugin channel (NIM-32, ADR-065(b)/(f)/(g)): catalog
// `plugins.soul_modules[]` in keeper.yml -> plugingit slot resolve into
// cache_root on `keeper run` startup -> Sigil allow through Operator API
// (keeper-side seal).
//
// Lightweight stand: keeper + PG + Redis + Vault, WITHOUT soul container
// (Souls: 0). Byte delivery to live soul (FetchModule + core.module.installed)
// is S2+.
package e2e_live_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// TestL3bPluginChannel_CatalogAndAllow - smoke S1:
//  1. harness builds soul-mod-community-redis and publishes it into per-test
//     git repo (manifest.yaml + dist/soul-mod-redis, tag v1.0.0);
//  2. keeper starts with `plugins.soul_modules[]` pointing to this file:// repo and
//     materializes slot `<cache_root>/community-redis/current/` on startup;
//  3. AllowSoulModule (POST /v1/plugins/sigils) allows community/redis;
//  4. ASSERT: allow entry is visible in GET /v1/plugins/sigils; FS slot carries
//     manifest + executable binary; slot byte sha256 == allow sha256
//     (content-addressed authority ADR-065(b)).
func TestL3bPluginChannel_CatalogAndAllow(t *testing.T) {
	repoURL := harness.BuildCommunityRedisPlugin(t)

	stack := harness.NewStack(t, harness.Config{
		Souls: 0,
		SoulModules: []harness.SoulModuleEntry{
			{Name: "redis", Source: repoURL, Ref: harness.CommunityRedisPluginRef},
		},
	})
	defer stack.Cleanup()

	sha := stack.AllowSoulModule(t, "community", "redis", harness.CommunityRedisPluginRef)

	// (a) Allow entry is visible through Operator API.
	items := stack.ListPluginSigils(t)
	found := false
	for _, it := range items {
		if it.Namespace == "community" && it.Name == "redis" && it.Ref == harness.CommunityRedisPluginRef {
			found = true
			if it.SHA256 != sha {
				t.Errorf("list sha256 = %q, allow returned %q", it.SHA256, sha)
			}
		}
	}
	if !found {
		t.Fatalf("allow entry community/redis/%s is not visible in GET /v1/plugins/sigils: %+v",
			harness.CommunityRedisPluginRef, items)
	}

	// (b) Slot is materialized in cache_root (ADR-065(b)/(g), R-nested layout).
	slotDir := filepath.Join(stack.PluginCacheRoot, "community-redis", "current")
	if _, err := os.Stat(filepath.Join(slotDir, "manifest.yaml")); err != nil {
		t.Fatalf("manifest.yaml is missing from slot: %v", err)
	}
	binPath := filepath.Join(slotDir, "soul-mod-redis")
	st, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("binary is missing from slot: %v", err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("slot binary is not executable: %v", st.Mode())
	}

	// Content-addressed chain: slot bytes == active allow sha256.
	b, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read slot binary: %v", err)
	}
	digest := sha256.Sum256(b)
	if got := hex.EncodeToString(digest[:]); got != sha {
		t.Errorf("sha256(slot binary) = %s, allow = %s", got, sha)
	}
}
