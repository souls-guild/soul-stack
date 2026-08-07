package artifact

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

type stubSource struct {
	r   config.ModuleManifestResolver
	err error
}

func (s stubSource) ModuleManifests(context.Context) (config.ModuleManifestResolver, error) {
	return s.r, s.err
}

func TestSnapshotModuleManifests_NilSource(t *testing.T) {
	if got := SnapshotModuleManifests(t.Context(), nil); got != nil {
		t.Errorf("a nil source produced a resolver: %v", got)
	}
}

// A source that fails degrades to "unchecked", never to a refused render. The
// allow-list being briefly unreadable says nothing about the definition, and the
// Soul-side gate still stands behind it — refusing here would turn a storage
// blip into a failed run.
func TestSnapshotModuleManifests_FailedReadDegradesToUnchecked(t *testing.T) {
	got := SnapshotModuleManifests(t.Context(), stubSource{err: errors.New("pg down")})
	if got != nil {
		t.Errorf("a failed read produced a resolver: %v", got)
	}
}

func TestSnapshotModuleManifests_PassesThroughResolver(t *testing.T) {
	want := ModuleManifestMap{"redis.acl": {Name: "acl"}}
	got := SnapshotModuleManifests(t.Context(), stubSource{r: want})
	if got == nil {
		t.Fatal("a working source produced no resolver")
	}
	if _, ok := got.ResolveModule("redis", "acl"); !ok {
		t.Error("the snapshot lost the module the source carried")
	}
}

func TestModuleManifestMap_ResolveModule(t *testing.T) {
	// The key is `<alias>.<module>`: level 1 is the operator's registration, level 2
	// the module the artifact declares. Only the grant holds both.
	m := ModuleManifestMap{"redis.acl": plugin.ModuleDef{Name: "acl"}}
	if _, ok := m.ResolveModule("redis", "acl"); !ok {
		t.Error("a present module did not resolve")
	}
	// A miss is an ordinary answer, not an error: the caller reports it as
	// plugin_params_unchecked.
	if _, ok := m.ResolveModule("redis", "config"); ok {
		t.Error("an absent module resolved")
	}
	// The key is the whole address. The same artifact registered under a second alias
	// is a second address space, and `redis-community.acl` must NOT resolve against
	// `redis.acl` just because the module name matches.
	if _, ok := m.ResolveModule("redis-community", "acl"); ok {
		t.Error("a module resolved across aliases")
	}
}
