package artifact

import (
	"context"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// PluginManifestSource yields a point-in-time resolver over the plugin manifests
// this cluster has allow-listed, so a definition's `params:` can be checked
// against them while it is parsed (NIM-228).
//
// A snapshot rather than a live lookup: [config.ModuleManifestResolver] answers
// synchronously with no ctx and no error, because it is consulted inside a YAML
// walk. Taking one read per parse also makes the answer coherent — every task in
// a definition is judged against the same catalog, not against whatever the
// allow-list happened to be a few microseconds apart.
//
// Optional everywhere. A nil source means plugin params are not checked and each
// such module is reported as `plugin_params_unchecked`, exactly as offline
// soul-lint does without `--modules`.
type PluginManifestSource interface {
	ModuleManifests(ctx context.Context) (config.ModuleManifestResolver, error)
}

// SnapshotModuleManifests takes a snapshot, tolerating a nil source.
//
// A source that FAILS yields a nil resolver rather than an error: an
// unreachable allow-list must not turn into a refused render. The consequence is
// visible, not silent — every plugin module in the definition then reports
// `plugin_params_unchecked`, which is the honest statement of what happened.
func SnapshotModuleManifests(ctx context.Context, src PluginManifestSource) config.ModuleManifestResolver {
	if src == nil {
		return nil
	}
	r, err := src.ModuleManifests(ctx)
	if err != nil {
		return nil
	}
	return r
}

// ModuleManifestMap is the plain in-memory resolver a snapshot resolves to,
// keyed by `<namespace>.<name>`.
type ModuleManifestMap map[string]*plugin.Manifest

// ResolveModule implements [config.ModuleManifestResolver].
func (m ModuleManifestMap) ResolveModule(namespace, name string) (*plugin.Manifest, bool) {
	man, ok := m[namespace+"."+name]
	return man, ok
}
