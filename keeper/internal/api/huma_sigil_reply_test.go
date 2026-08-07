// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO SIGIL domain (handler-native T5d). sigil
// no longer depends on the legacy generator — golden compares json native values against a pinned
// reference string. Both revoked_at branches are covered (nil/non-nil; omitempty-nil → key
// omitted). Mutating the native-struct shape reddens the case.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

func goldenSigilWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_SigilReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	sha := "deadbeef0123456789abcdef"
	const src = "https://example.com/soul-mod-redis.git"

	// --- PluginSigilAllowReply ---
	goldenSigilWire(t, "AllowReply",
		PluginSigilAllowReply{Alias: "redis", Ref: "v1.2.0", SHA256: sha, Source: src},
		`{"alias":"redis","ref":"v1.2.0","sha256":"deadbeef0123456789abcdef","source":"https://example.com/soul-mod-redis.git"}`)

	// --- PluginSigilView (nested): revoked_at omitempty — both branches ---
	goldenSigilWire(t, "PluginSigilView/active",
		PluginSigilView{Alias: "redis", AllowedAt: ts, AllowedByAID: "archon-alice", Ref: "v1.2.0", Source: src, RevokedAt: nil, SHA256: sha},
		`{"alias":"redis","allowed_at":"2026-06-14T12:34:56.789012345Z","allowed_by_aid":"archon-alice","ref":"v1.2.0","source":"https://example.com/soul-mod-redis.git","sha256":"deadbeef0123456789abcdef"}`)
	goldenSigilWire(t, "PluginSigilView/revoked",
		PluginSigilView{Alias: "redis", AllowedAt: ts, AllowedByAID: "archon-alice", Ref: "v1.2.0", Source: src, RevokedAt: &ts2, SHA256: sha},
		`{"alias":"redis","allowed_at":"2026-06-14T12:34:56.789012345Z","allowed_by_aid":"archon-alice","ref":"v1.2.0","source":"https://example.com/soul-mod-redis.git","revoked_at":"2026-06-13T01:02:03.456789012Z","sha256":"deadbeef0123456789abcdef"}`)
}

// TestGoldenWire_SigilProjection verifies that the projection of domain handlers.Sigil* results
// → native keeps byte-exact wire against the pinned reference. Catches regressions in field
// mapping (incl. list items[]).
func TestGoldenWire_SigilProjection(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 123456789, time.UTC)
	sha := "feedface"
	const src = "https://example.com/n.git"

	allowV := handlers.SigilAllowView{Alias: "n", Source: src, Ref: "v1", SHA256: sha}
	goldenSigilWire(t, "proj/AllowReply", newPluginSigilAllowReply(allowV),
		`{"alias":"n","ref":"v1","sha256":"feedface","source":"https://example.com/n.git"}`)

	viewV := handlers.SigilView{Alias: "n", AllowedAt: ts, AllowedByAID: "archon-bob", Source: src, Ref: "v1", RevokedAt: nil, SHA256: sha}
	goldenSigilWire(t, "proj/PluginSigilView", newPluginSigilView(viewV),
		`{"alias":"n","allowed_at":"2026-06-14T12:00:00.123456789Z","allowed_by_aid":"archon-bob","ref":"v1","source":"https://example.com/n.git","sha256":"feedface"}`)

	pageV := handlers.SigilListPage{Items: []handlers.SigilView{viewV}}
	goldenSigilWire(t, "proj/PluginSigilListReply", newPluginSigilListReply(pageV),
		`{"items":[{"alias":"n","allowed_at":"2026-06-14T12:00:00.123456789Z","allowed_by_aid":"archon-bob","ref":"v1","source":"https://example.com/n.git","sha256":"feedface"}]}`)
	// handler returns make([]., 0): items=`[]` (non-nil), NOT null
	pageEmpty := handlers.SigilListPage{Items: []handlers.SigilView{}}
	goldenSigilWire(t, "proj/PluginSigilListReply/empty", newPluginSigilListReply(pageEmpty),
		`{"items":[]}`)
}
