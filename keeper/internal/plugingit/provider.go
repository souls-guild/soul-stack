package plugingit

import (
	"context"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginsource"
)

// Provider presents the git resolver as a [pluginsource.Provider] (NIM-793).
//
// Adapter and nothing else: it changes no git behaviour, adds no check and removes
// none. The git kind resolves exactly as it did before a second kind existed — which
// is the point of introducing the interface around it rather than rewriting it into a
// generalisation of both.
//
// The one thing it decides is how a git slot looks in the shared shape: ONE artifact,
// with no platform and no published path. The repository declares neither — it holds
// one built binary in `dist/` and says nothing about what it was built for — so the
// grant records that absence ([sharedhost.AnyPlatform]) instead of inventing values
// that would then be checked against a host. An unplatformed artifact answers for every
// platform, which is precisely what this kind's single binary did before grants carried
// a list at all.
type Provider struct {
	r *Resolver
}

// NewProvider wraps a resolver. Nil is not accepted — a provider with no resolver
// would report the git kind as handled and fail every entry of it.
func NewProvider(r *Resolver) *Provider {
	if r == nil {
		panic("plugingit.NewProvider: nil Resolver")
	}
	return &Provider{r: r}
}

// Kind implements [pluginsource.Provider].
func (p *Provider) Kind() string { return sharedplugin.SourceKindGit }

// Resolve implements [pluginsource.Provider] over [Resolver.ResolveEntry].
func (p *Provider) Resolve(ctx context.Context, e config.PluginCatalogEntry) (pluginsource.Resolved, error) {
	slot, err := p.r.ResolveEntry(ctx, e)
	if err != nil {
		return pluginsource.Resolved{}, err
	}
	return pluginsource.Resolved{
		Alias:  slot.Alias,
		Kind:   sharedplugin.SourceKindGit,
		Source: slot.Source,
		Ref:    slot.Ref,
		// The resolved commit is this kind's provenance marker: audit-only, outside
		// the signed block, and required — a git resolve that cannot pin a commit
		// fails rather than returning an empty one.
		Origin:  slot.CommitSHA,
		SlotDir: slot.SlotDir,
		Contents: &pluginhost.SlotContents{
			Kind: sharedplugin.SourceKindGit,
			Artifacts: []pluginhost.SlotArtifact{{
				SHA256:     slot.BinarySHA256,
				BinaryPath: slot.BinaryPath,
			}},
			SchemaBytes: slot.SchemaBytes,
			Doc:         slot.Doc,
		},
	}, nil
}
