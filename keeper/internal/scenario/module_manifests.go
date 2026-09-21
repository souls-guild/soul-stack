package scenario

import (
	"context"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// moduleManifests takes a fresh snapshot of the allow-listed plugin manifests
// for one parse (NIM-228).
//
// Per parse rather than per process: a plugin allow-listed a minute ago must be
// visible to the next render, and a revoked one must stop being. The cost is one
// read next to the several a render already does.
//
// Never fatal. A nil source or a failed read yields a nil resolver, which is not
// a silent pass — every plugin module in the definition then reports
// `plugin_params_unchecked`. Refusing to render because the allow-list was
// briefly unreadable would be the worse failure: the definition is fine, and the
// Soul-side gate (ADR-0076(t)) still stands behind it.
func (r *Runner) moduleManifests(ctx context.Context) config.ModuleManifestResolver {
	return artifact.SnapshotModuleManifests(ctx, r.deps.ModuleManifests)
}
