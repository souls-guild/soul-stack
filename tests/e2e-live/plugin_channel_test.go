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
//     git repo (dist/ holding the stamped artifact + schema.json, tag v1.0.0);
//  2. keeper starts with `plugins.soul_modules[]` registering that repo under the
//     alias `community` and materializes slot `<cache_root>/community/current/`
//     on startup;
//  3. AllowSoulModule (POST /v1/plugins/sigils) allows that alias on (source, ref);
//  4. ASSERT: allow entry is visible in GET /v1/plugins/sigils; the FS slot holds
//     the executable artifact and NOTHING else; slot byte sha256 == allow sha256
//     (content-addressed authority ADR-065(b)).
//
// The alias is `community` and not `redis`: the artifact declares ONE module
// named `redis` (examples/module/soul-mod-community-redis/schema.json), the
// registry key is `<alias>.<module>`, and every scenario addresses
// `community.redis.<state>`. Registering it as `redis` would key it
// `redis.redis` and nothing would resolve.
func TestL3bPluginChannel_CatalogAndAllow(t *testing.T) {
	repoURL := harness.BuildCommunityRedisPlugin(t)

	stack := harness.NewStack(t, harness.Config{
		Souls: 0,
		SoulModules: []harness.SoulModuleEntry{
			{Name: harness.CommunityRedisAlias, Source: repoURL, Ref: harness.CommunityRedisPluginRef},
		},
	})
	defer stack.Cleanup()

	sha := stack.AllowSoulModule(t, harness.CommunityRedisAlias, repoURL, harness.CommunityRedisPluginRef)

	// (a) Allow entry is visible through Operator API.
	items := stack.ListPluginSigils(t)
	found := false
	for _, it := range items {
		if it.Alias == harness.CommunityRedisAlias && it.Source == repoURL && it.Ref == harness.CommunityRedisPluginRef {
			found = true
			if it.SHA256 != sha {
				t.Errorf("list sha256 = %q, allow returned %q", it.SHA256, sha)
			}
		}
	}
	if !found {
		t.Fatalf("allow entry %s (%s@%s) is not visible in GET /v1/plugins/sigils: %+v",
			harness.CommunityRedisAlias, repoURL, harness.CommunityRedisPluginRef, items)
	}

	// (b) Slot is materialized in cache_root (ADR-065(b)/(g), R-nested layout).
	// The slot is named by the ALIAS, and so is the artifact inside it: NIM-377
	// removed the filename convention, so the resolver renames whatever single
	// executable `dist/` holds to the registration alias.
	slotDir := filepath.Join(stack.PluginCacheRoot, harness.CommunityRedisAlias, "current")
	binPath := filepath.Join(slotDir, harness.CommunityRedisAlias)
	st, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("artifact is missing from slot: %v", err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("slot artifact is not executable: %v", st.Mode())
	}

	// The slot holds the artifact and NOTHING else - no manifest.yaml (NIM-377
	// removed it), no schema.json beside the bytes (a second copy could only ever
	// disagree with the signed trailer). Parity with the resolver's own guard,
	// keeper/internal/plugingit/resolver_test.go::TestResolveEntry.
	entries, err := os.ReadDir(slotDir)
	if err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != harness.CommunityRedisAlias {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("slot holds %v, want only the artifact %q", names, harness.CommunityRedisAlias)
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
