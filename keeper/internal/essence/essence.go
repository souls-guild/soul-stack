// Package essence assembles the effective essence of a service for a
// specific host through a layer hierarchy (see architecture.md → "Essence:
// assembly pipeline"):
//
//	essence/_default.yaml → essence/os/<family>.yaml → essence/coven/<label>.yaml... → incarnation.spec.essence
//
// Convention-based ordering (no `_stack.yaml`): essence is role-agnostic
// (ADR-008 — no role/<Y>.yaml step). Each next layer is deep-merged over the
// previous one: maps merge recursively, scalars and lists are replaced
// wholesale (later wins).
package essence

import "log/slog"

// Essence layer names in the service repository. All paths are relative to
// `<ServiceDir>/essence/` per the documented convention (docs/service/manifest.md,
// architecture.md → "Repository layout"): essence lives in a subdirectory of
// the service, NOT at its root.
const (
	// essenceDir — essence root directory inside the service snapshot.
	essenceDir = "essence"
	// defaultFile — baseline layer shared by all incarnations
	// (`essence/_default.yaml`). Missing file is fine (empty base).
	defaultFile = essenceDir + "/_default.yaml"
	// osDir — per-OS overlay directory (`essence/os/debian.yaml`,
	// `essence/os/rhel.yaml`).
	osDir = essenceDir + "/os"
	// covenDir — per-coven overlay directory (`essence/coven/<label>.yaml`).
	covenDir = essenceDir + "/coven"
)

// ResolveInput — input for assembling one host's essence.
type ResolveInput struct {
	// ServiceDir — service snapshot root (artifact.ServiceArtifact.LocalDir).
	ServiceDir string
	// OSFamily — host's `soulprint.self.os.family` (e.g. "debian"). Empty
	// string → the os layer is skipped.
	OSFamily string
	// Covens — host's Coven labels (souls.coven[]). Layers apply in
	// name-sorted order for determinism.
	Covens []string
	// IncarnationSpec — `incarnation.spec.essence`, the operator override
	// (the strongest layer). May be nil.
	IncarnationSpec map[string]any
}

// Resolver assembles the essence map from layers. Stateless, safe for
// concurrent use.
type Resolver struct {
	logger *slog.Logger
}

// NewResolver creates a Resolver. If logger is nil, slog.Default is used.
func NewResolver(logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{logger: logger}
}
