package pluginsource

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// recordingProvider answers for one kind and remembers what it was asked to resolve.
type recordingProvider struct {
	kind string
	seen []string
	err  error
}

func (p *recordingProvider) Kind() string { return p.kind }

func (p *recordingProvider) Resolve(_ context.Context, e config.PluginCatalogEntry) (Resolved, error) {
	p.seen = append(p.seen, e.Name)
	if p.err != nil {
		return Resolved{}, p.err
	}
	return Resolved{Alias: e.Name, Kind: p.kind, Source: e.SourceURL(), Ref: e.Ref}, nil
}

func newProviders() (*recordingProvider, *recordingProvider) {
	return &recordingProvider{kind: sharedplugin.SourceKindGit},
		&recordingProvider{kind: sharedplugin.SourceKindArtifact}
}

// Each entry reaches the provider for its kind and no other. An entry with no `kind:`
// is a git entry, which is what keeps every catalog written before NIM-793 meaning
// what it meant.
func TestResolveCatalog_DispatchesByKind(t *testing.T) {
	git, artifact := newProviders()
	c, err := NewCatalog(nil, git, artifact)
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}

	plugins := &config.KeeperPlugins{
		SSHProviders: []config.PluginCatalogEntry{
			{Name: "teleport", Source: "https://example.com/soul-ssh-teleport.git", Ref: "v1"},
		},
		SoulModules: []config.PluginCatalogEntry{
			{Name: "redis", Kind: sharedplugin.SourceKindArtifact,
				BaseURL: "https://nexus.internal/plugins/redis", Ref: "v1.4.0"},
			{Name: "pkg", Source: "https://example.com/soul-mod-pkg.git", Ref: "v2"},
		},
	}

	slots, warns, err := c.ResolveCatalog(context.Background(), plugins)
	if err != nil {
		t.Fatalf("ResolveCatalog: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("warns = %v, want none", warns)
	}
	if len(slots) != 3 {
		t.Fatalf("slots = %d, want 3", len(slots))
	}
	if strings.Join(git.seen, ",") != "teleport,pkg" {
		t.Errorf("git provider saw %v, want [teleport pkg]", git.seen)
	}
	if strings.Join(artifact.seen, ",") != "redis" {
		t.Errorf("artifact provider saw %v, want [redis]", artifact.seen)
	}
}

// One unreachable source must not stop a Keeper from starting with the plugins it can
// resolve — but the entry that failed says so, with enough of its identity in the text
// for an operator to find it.
func TestResolveCatalog_PerEntryFailureIsAWarning(t *testing.T) {
	git, artifact := newProviders()
	artifact.err = errors.New("nexus unreachable")
	c, _ := NewCatalog(nil, git, artifact)

	slots, warns, err := c.ResolveCatalog(context.Background(), &config.KeeperPlugins{
		SoulModules: []config.PluginCatalogEntry{
			{Name: "redis", Kind: sharedplugin.SourceKindArtifact,
				BaseURL: "https://nexus.internal/plugins/redis", Ref: "v1.4.0"},
			{Name: "pkg", Source: "https://example.com/soul-mod-pkg.git", Ref: "v2"},
		},
	})
	if err != nil {
		t.Fatalf("a per-entry failure must not be fatal: %v", err)
	}
	if len(slots) != 1 || slots[0].Alias != "pkg" {
		t.Fatalf("slots = %+v, want the one entry that resolved", slots)
	}
	if len(warns) != 1 {
		t.Fatalf("warns = %v, want 1", warns)
	}
	for _, want := range []string{"redis", "artifact", "https://nexus.internal/plugins/redis", "v1.4.0", "nexus unreachable"} {
		if !strings.Contains(warns[0], want) {
			t.Errorf("warning %q does not mention %q", warns[0], want)
		}
	}
}

// A kind nothing is wired for is an error, not a skip: "nobody resolves this" must not
// read like "resolved" in a startup log, because the second is what an operator will
// assume from silence.
func TestResolveCatalog_UnknownKindIsReported(t *testing.T) {
	git, _ := newProviders()
	c, _ := NewCatalog(nil, git)

	slots, warns, err := c.ResolveCatalog(context.Background(), &config.KeeperPlugins{
		SoulModules: []config.PluginCatalogEntry{
			{Name: "redis", Kind: sharedplugin.SourceKindArtifact,
				BaseURL: "https://nexus.internal/plugins/redis", Ref: "v1.4.0"},
		},
	})
	if err != nil {
		t.Fatalf("ResolveCatalog: %v", err)
	}
	if len(slots) != 0 || len(warns) != 1 {
		t.Fatalf("slots=%v warns=%v, want no slot and one warning", slots, warns)
	}
	if !strings.Contains(warns[0], "artifact") {
		t.Errorf("warning %q does not name the unresolvable kind", warns[0])
	}
}

// The alias is checked before dispatch and therefore before a byte of egress: a name
// that cannot be registered is not worth reaching a network for, and the rule is the
// same for a git remote and a published release.
func TestResolveEntry_AliasCheckedBeforeAnyProvider(t *testing.T) {
	git, artifact := newProviders()
	c, _ := NewCatalog(nil, git, artifact)

	for _, alias := range []string{"", "core", "Redis", "redis.acl", "../escape"} {
		_, err := c.ResolveEntry(context.Background(), config.PluginCatalogEntry{
			Name: alias, Kind: sharedplugin.SourceKindArtifact,
			BaseURL: "https://nexus.internal/x", Ref: "v1",
		})
		if !errors.Is(err, ErrAliasInvalid) {
			t.Errorf("alias %q: err = %v, want ErrAliasInvalid", alias, err)
		}
	}
	if len(git.seen) != 0 || len(artifact.seen) != 0 {
		t.Errorf("a provider was reached despite an invalid alias: git=%v artifact=%v", git.seen, artifact.seen)
	}
}

// Two providers for one kind is a wire-up error rather than last-one-wins: otherwise
// the catalog's meaning would depend on the order the daemon happened to pass them.
func TestNewCatalog_RefusesAmbiguousWiring(t *testing.T) {
	git, _ := newProviders()
	other := &recordingProvider{kind: sharedplugin.SourceKindGit}
	if _, err := NewCatalog(nil, git, other); err == nil {
		t.Fatal("NewCatalog accepted two providers for one kind")
	}
	if _, err := NewCatalog(nil, &recordingProvider{kind: "torrent"}); err == nil {
		t.Fatal("NewCatalog accepted a provider for an unknown kind")
	}
	if _, err := NewCatalog(nil, nil); err == nil {
		t.Fatal("NewCatalog accepted a nil provider")
	}
}

func TestResolveCatalog_NilPluginsIsEmpty(t *testing.T) {
	git, artifact := newProviders()
	c, _ := NewCatalog(nil, git, artifact)
	slots, warns, err := c.ResolveCatalog(context.Background(), nil)
	if err != nil || slots != nil || warns != nil {
		t.Errorf("nil plugins: slots=%v warns=%v err=%v, want all empty", slots, warns, err)
	}
}
