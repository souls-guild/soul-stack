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
//  1. harness builds redis and publishes it into per-test
//     git repo (dist/ holding the stamped artifact + schema.json, tag v1.0.0);
//  2. keeper starts with `plugins.soul_modules[]` registering that repo under the
//     alias `redis` and materializes slot `<cache_root>/redis/current/`
//     on startup;
//  3. AllowSoulModule (POST /v1/plugins/sigils) allows that alias on (source, ref);
//  4. ASSERT: allow entry is visible in GET /v1/plugins/sigils; the FS slot holds
//     the executable artifact and NOTHING else; slot byte sha256 == allow sha256
//     (content-addressed authority ADR-065(b)).
//
// The alias is `redis` since NIM-766: the artifact declares SIX modules, one per
// OBJECT it manages (examples/module/redis/schema.json), the registry key
// is `<alias>.<module>`, and every scenario addresses `redis.<object>.<action>`.
// The old alias `community` named where the plugin came from rather than what it
// manages, which is the grouping level ADR-020's 2026-09-02 amendment removed.
func TestL3bPluginChannel_CatalogAndAllow(t *testing.T) {
	repoURL := harness.BuildRedisPlugin(t)

	stack := harness.NewStack(t, harness.Config{
		Souls: 0,
		SoulModules: []harness.SoulModuleEntry{
			{Name: harness.RedisAlias, Source: repoURL, Ref: harness.RedisPluginRef},
		},
	})
	defer stack.Cleanup()

	sha := stack.AllowSoulModule(t, harness.RedisAlias, repoURL, harness.RedisPluginRef)

	// (a) Allow entry is visible through Operator API.
	items := stack.ListPluginSigils(t)
	found := false
	for _, it := range items {
		if it.Alias == harness.RedisAlias && it.Source == repoURL && it.Ref == harness.RedisPluginRef {
			found = true
			if it.SHA256() != sha {
				t.Errorf("list sha256 = %q, allow returned %q", it.SHA256(), sha)
			}
		}
	}
	if !found {
		t.Fatalf("allow entry %s (%s@%s) is not visible in GET /v1/plugins/sigils: %+v",
			harness.RedisAlias, repoURL, harness.RedisPluginRef, items)
	}

	// (b) Slot is materialized in cache_root (ADR-065(b)/(g), R-nested layout).
	// The slot is named by the ALIAS, and so is the artifact inside it: NIM-377
	// removed the filename convention, so the resolver renames whatever single
	// executable `dist/` holds to the registration alias.
	slotDir := filepath.Join(stack.PluginCacheRoot, harness.RedisAlias, "current")
	binPath := filepath.Join(slotDir, harness.RedisAlias)
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
	if len(entries) != 1 || entries[0].Name() != harness.RedisAlias {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("slot holds %v, want only the artifact %q", names, harness.RedisAlias)
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
