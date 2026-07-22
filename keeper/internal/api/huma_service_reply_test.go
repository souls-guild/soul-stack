// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the SERVICE domain (handler-native T5d).
// service no longer depends on the legacy generator (0 legacy generator in service files), so golden
// verifies the native JSON values against a PINNED reference string. Both pointer/slice branches are
// covered: omitempty (created_by_aid/refresh/updated_by_aid/schema/git/is_default), nil-vs-empty
// slice (items/refs/migrations/destiny/modules) and the GitRefType enum.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenServiceWire compares json.Marshal(native) byte-for-byte with the pinned reference.
func goldenServiceWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_ServiceReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	aid := "archon-alice"
	refresh := "5m"
	isDefault := true
	gitURL := "https://git.example/redis-dep.git"
	schemaMap := map[string]interface{}{"type": "object"}

	// --- ServiceView: created_by_aid/refresh/updated_by_aid omitempty (both branches) ---
	goldenServiceWire(t, "ServiceView/full",
		ServiceView{CreatedAt: ts, CreatedByAID: &aid, Git: "https://git/r.git", Name: "redis", Ref: "v2.0.0", Refresh: &refresh, UpdatedAt: ts2, UpdatedByAID: &aid},
		`{"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","git":"https://git/r.git","name":"redis","ref":"v2.0.0","refresh":"5m","updated_at":"2026-06-13T01:02:03.456789012Z","updated_by_aid":"archon-alice"}`)
	goldenServiceWire(t, "ServiceView/nil_optionals",
		ServiceView{CreatedAt: ts, CreatedByAID: nil, Git: "https://git/r.git", Name: "redis", Ref: "main", Refresh: nil, UpdatedAt: ts2, UpdatedByAID: nil},
		`{"created_at":"2026-06-14T12:34:56.789012345Z","git":"https://git/r.git","name":"redis","ref":"main","updated_at":"2026-06-13T01:02:03.456789012Z"}`)

	// --- ServiceListReply: items populated / empty array / nil ---
	sv := ServiceView{CreatedAt: ts, Git: "g", Name: "redis", Ref: "v1", UpdatedAt: ts}
	goldenServiceWire(t, "ServiceListReply/items",
		ServiceListReply{Items: []ServiceView{sv}},
		`{"items":[{"created_at":"2026-06-14T12:34:56.789012345Z","git":"g","name":"redis","ref":"v1","updated_at":"2026-06-14T12:34:56.789012345Z"}]}`)
	goldenServiceWire(t, "ServiceListReply/empty",
		ServiceListReply{Items: []ServiceView{}},
		`{"items":[]}`)
	goldenServiceWire(t, "ServiceListReply/nil",
		ServiceListReply{Items: nil},
		`{"items":null}`)

	// --- GitRef (nested): is_default omitempty + enum type ---
	goldenServiceWire(t, "GitRef/tag",
		GitRef{Commit: "abc123", IsDefault: nil, Name: "v2.0.0", Type: GitRefType("tag")},
		`{"commit":"abc123","name":"v2.0.0","type":"tag"}`)
	goldenServiceWire(t, "GitRef/default_branch",
		GitRef{Commit: "def456", IsDefault: &isDefault, Name: "main", Type: GitRefType("branch")},
		`{"commit":"def456","is_default":true,"name":"main","type":"branch"}`)

	// --- ServiceRefsListReply: refs populated / nil ---
	goldenServiceWire(t, "ServiceRefsListReply/full",
		ServiceRefsListReply{Refs: []GitRef{{Commit: "abc", Name: "v1", Type: GitRefType("tag")}}, Service: "redis"},
		`{"refs":[{"commit":"abc","name":"v1","type":"tag"}],"service":"redis"}`)
	goldenServiceWire(t, "ServiceRefsListReply/nil_refs",
		ServiceRefsListReply{Refs: nil, Service: "redis"},
		`{"refs":null,"service":"redis"}`)

	// --- StateSchemaMigration (nested) ---
	goldenServiceWire(t, "StateSchemaMigration",
		StateSchemaMigration{From: 1, Path: "migrations/001_to_002.yml", To: 2},
		`{"from":1,"path":"migrations/001_to_002.yml","to":2}`)

	// --- ServiceStateSchemaReply: schema omitempty (both branches) + migrations nil/non-empty ---
	goldenServiceWire(t, "ServiceStateSchemaReply/full",
		ServiceStateSchemaReply{Migrations: []StateSchemaMigration{{From: 1, Path: "p", To: 2}}, Ref: "v2", Schema: &schemaMap, Service: "redis", StateSchemaVersion: 2},
		`{"migrations":[{"from":1,"path":"p","to":2}],"ref":"v2","schema":{"type":"object"},"service":"redis","state_schema_version":2}`)
	goldenServiceWire(t, "ServiceStateSchemaReply/nil_schema_nil_migrations",
		ServiceStateSchemaReply{Migrations: nil, Ref: "v1", Schema: nil, Service: "redis", StateSchemaVersion: 1},
		`{"migrations":null,"ref":"v1","service":"redis","state_schema_version":1}`)

	// --- ServiceDependency (nested): git omitempty (both branches) ---
	goldenServiceWire(t, "ServiceDependency/with_git",
		ServiceDependency{Git: &gitURL, Name: "redis", Ref: "v2.0.0"},
		`{"git":"https://git.example/redis-dep.git","name":"redis","ref":"v2.0.0"}`)
	goldenServiceWire(t, "ServiceDependency/no_git",
		ServiceDependency{Git: nil, Name: "acme.failover", Ref: "main"},
		`{"name":"acme.failover","ref":"main"}`)

	// --- ServiceDependenciesReply: destiny/modules populated / nil ---
	goldenServiceWire(t, "ServiceDependenciesReply/full",
		ServiceDependenciesReply{Destiny: []ServiceDependency{{Name: "redis", Ref: "v2"}}, Modules: []ServiceDependency{{Git: &gitURL, Name: "acme.x", Ref: "main"}}, Ref: "v1", Service: "redis"},
		`{"destiny":[{"name":"redis","ref":"v2"}],"modules":[{"git":"https://git.example/redis-dep.git","name":"acme.x","ref":"main"}],"ref":"v1","service":"redis"}`)
	goldenServiceWire(t, "ServiceDependenciesReply/nil_lists",
		ServiceDependenciesReply{Destiny: nil, Modules: nil, Ref: "v1", Service: "redis"},
		`{"destiny":null,"modules":null,"ref":"v1","service":"redis"}`)
}
