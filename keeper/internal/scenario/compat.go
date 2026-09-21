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
	// Name is the REGISTERED name, always. It used to be overridden by the
	// manifest's own `name:` when that was set, which meant the message blamed an
	// entity under a word no operator could look up — nothing ever compared the two
	// (NIM-726, which removed the manifest field).
	e := config.CompatEntity{Kind: config.CompatEntityService, Ref: art.Ref.Ref, Name: art.Ref.Name}
	if art.Manifest != nil {
		e.Window = art.Manifest.Compat.KeeperWindow()
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

// warnCompatFloorTooLow reports a declaration that promises more than the body
// can deliver: the declared `min` sits below the floor the body actually needs
// (ADR-0076(k), `compat_floor_too_low`).
//
// A log, never an abort — deliberately. This instance evidently understands the
// feature, since it just parsed it, so rejecting a run that would succeed is the
// worse failure. It still matters here rather than only in soul-lint: the cluster
// is rolling-upgraded (ADR-0076(f)), so the next run of this same definition may
// land on an OLDER instance — exactly the one the stale declaration promised
// would work, and there it fails with the opaque error this axis exists to
// replace.
//
// Called AFTER the body is parsed, which is why it is a separate pass and not
// part of checkKeeperCompat: the window gate must stay ahead of the parse.
func warnCompatFloorTooLow(entity config.CompatEntity, used []config.KeeperFeature, log *slog.Logger) {
	if log == nil {
		return
	}
	d := config.CompatFloorDiagnostic(entity.Window, config.InferKeeperFloor(used))
	if d == nil {
		return
	}
	log.Warn("compat: declared window is below the floor this definition needs (ADR-0076)",
		slog.String("code", d.Code),
		slog.String("entity_kind", entity.Kind),
		slog.String("entity", entity.Name),
		slog.String("ref", entity.Ref),
		slog.String("detail", d.Message),
		slog.String("hint", d.Hint),
	)
}
