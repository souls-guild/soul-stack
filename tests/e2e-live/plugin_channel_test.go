//go:build e2e_live

// L3b S1 plugin-канал SoulModule (NIM-32, ADR-065(b)/(f)/(g)): каталог
// `plugins.soul_modules[]` keeper.yml → plugingit-резолв слота в cache_root
// на старте `keeper run` → Sigil-allow через Operator API (keeper-side seal).
//
// Лёгкий стенд: keeper + PG + Redis + Vault, БЕЗ soul-контейнера (Souls: 0) —
// доставка байтов на живой soul (FetchModule + core.module.installed) — S2+.
package e2e_live_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// TestL3bPluginChannel_CatalogAndAllow — smoke S1:
//  1. harness собирает soul-mod-community-redis и публикует его в per-test
//     git-репо (manifest.yaml + dist/soul-mod-redis, тег v1.0.0);
//  2. keeper стартует с `plugins.soul_modules[]` на этот file://-репо и при
//     старте материализует слот `<cache_root>/community-redis/current/`;
//  3. AllowSoulModule (POST /v1/plugins/sigils) допускает community/redis;
//  4. ASSERT: допуск виден в GET /v1/plugins/sigils; слот на ФС несёт
//     manifest + исполняемый бинарь; sha256 байтов слота == sha256 допуска
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

	// (а) допуск виден через Operator API.
	items := stack.ListPluginSigils(t)
	found := false
	for _, it := range items {
		if it.Namespace == "community" && it.Name == "redis" && it.Ref == harness.CommunityRedisPluginRef {
			found = true
			if it.SHA256 != sha {
				t.Errorf("list sha256 = %q, allow вернул %q", it.SHA256, sha)
			}
		}
	}
	if !found {
		t.Fatalf("допуск community/redis/%s не виден в GET /v1/plugins/sigils: %+v",
			harness.CommunityRedisPluginRef, items)
	}

	// (б) слот материализован в cache_root (ADR-065(b)/(g), R-nested layout).
	slotDir := filepath.Join(stack.PluginCacheRoot, "community-redis", "current")
	if _, err := os.Stat(filepath.Join(slotDir, "manifest.yaml")); err != nil {
		t.Fatalf("manifest.yaml в слоте отсутствует: %v", err)
	}
	binPath := filepath.Join(slotDir, "soul-mod-redis")
	st, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("бинарь в слоте отсутствует: %v", err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("бинарь слота не исполняемый: %v", st.Mode())
	}

	// Content-addressed цепочка: байты слота == sha256 активного допуска.
	b, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read слот-бинаря: %v", err)
	}
	digest := sha256.Sum256(b)
	if got := hex.EncodeToString(digest[:]); got != sha {
		t.Errorf("sha256(слот-бинаря) = %s, допуск = %s", got, sha)
	}
}
