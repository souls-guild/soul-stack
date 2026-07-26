package scenario

import (
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Engine-compat gate on the render path (ADR-0076(f)). Keeper is a horizontally
// scalable stateless cluster whose instances run DIFFERENT versions during a
// rolling upgrade, so a check performed when the service was registered says
// nothing about the instance that renders a run months later: the rendering
// instance is the authority.
//
// The gate is per entity, not against the intersection — a version is admitted
// by the intersection iff every entity admits it, and blaming the entity that
// actually set the bound is what makes the message actionable (ADR-0076(g)).
// Service manifests are checked here, before render; each destiny is checked as
// the render phase resolves it (see destinyResolver.Resolve).

// serviceCompatEntity — the service manifest's contribution to the effective
// window. Ref is the pinned git ref of the snapshot being rendered (ADR-007).
func serviceCompatEntity(art *artifact.ServiceArtifact) config.CompatEntity {
	e := config.CompatEntity{Kind: config.CompatEntityService, Ref: art.Ref.Ref, Name: art.Ref.Name}
	if art.Manifest != nil {
		e.Window = art.Manifest.Compat.KeeperWindow()
		if art.Manifest.Name != "" {
			e.Name = art.Manifest.Name
		}
	}
	return e
}

// checkKeeperCompat verifies this keeper build against one entity's declared
// window. Returns nil when the entity admits this build, when it declares no
// window (backcompat), or when the build carries no comparable version — in the
// last case the skip is logged rather than silent (ADR-0076(e)).
func checkKeeperCompat(rawVersion string, entity config.CompatEntity, log *slog.Logger) error {
	if !entity.Window.Declared() {
		return nil
	}
	release, ok := config.NormalizeEngineVersion(rawVersion)
	if !ok {
		if log != nil {
			log.Warn("compat: keeper version window not enforced - this build carries no version (ADR-0076)",
				slog.String("entity_kind", entity.Kind),
				slog.String("entity", entity.Name),
				slog.String("declared_window", entity.Window.String()),
				slog.String("keeper_version", rawVersion),
			)
		}
		return nil
	}
	if entity.Window.Contains(release) {
		return nil
	}
	return config.NewKeeperCompatError(entity, rawVersion)
}
