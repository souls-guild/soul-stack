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
	want := ModuleManifestMap{"community.redis": {Namespace: "community", Name: "redis"}}
	got := SnapshotModuleManifests(t.Context(), stubSource{r: want})
	if got == nil {
		t.Fatal("a working source produced no resolver")
	}
	if _, ok := got.ResolveModule("community", "redis"); !ok {
		t.Error("the snapshot lost the module the source carried")
	}
}

func TestModuleManifestMap_ResolveModule(t *testing.T) {
	m := ModuleManifestMap{"community.redis": &plugin.Manifest{Namespace: "community", Name: "redis"}}
	if _, ok := m.ResolveModule("community", "redis"); !ok {
		t.Error("a present module did not resolve")
	}
	// A miss is an ordinary answer, not an error: the caller reports it as
	// plugin_params_unchecked.
	if _, ok := m.ResolveModule("community", "mongo"); ok {
		t.Error("an absent module resolved")
	}
	// The key is the whole address. A namespace collision must not let
	// `other.redis` resolve against `community.redis`'s contract.
	if _, ok := m.ResolveModule("other", "redis"); ok {
		t.Error("a module resolved across namespaces")
	}
}
