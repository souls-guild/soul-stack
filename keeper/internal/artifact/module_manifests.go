package artifact

import (
	"context"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// PluginManifestSource yields a point-in-time resolver over the plugin module
// schemas this cluster has allow-listed, so a definition's `params:` can be
// checked against them while it is parsed (NIM-228).
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
// keyed by `<alias>.<module>` — the first two levels of a task's address.
//
// The key is one MODULE of one registration, not one artifact: an artifact serves
// several modules (`acl`, `config`, `info`) and a task addresses exactly one of
// them. Level 1 is the alias the operator registered, which the artifact never
// knew, so the key can only be assembled here — from the grant that carries the
// alias and the schema document that carries the module.
type ModuleManifestMap map[string]plugin.ModuleDef

// ResolveModule implements [config.ModuleManifestResolver]. Its first argument is
// address level 1, which is now a registration alias rather than a publisher
// namespace; the parameter name follows the interface.
func (m ModuleManifestMap) ResolveModule(alias, module string) (plugin.ModuleDef, bool) {
	def, ok := m[alias+"."+module]
	return def, ok
}
