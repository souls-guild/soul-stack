// Package servicevars is keeper's side of a service's own variables — the values
// its author nailed down in the service repository, read in CEL as `vars.*`
// (ADR-0082, see architecture.md → "Service vars"). It reads a service snapshot
// from disk, contained in the snapshot root, and stacks the layers
// `shared/servicevars` assembles into a lazy [cel.VarView]. The assembly itself —
// the lexical order, `_strategy`, `vars/_stack.yaml` — is shared with soul-lint
// and lives there.
//
// They are the BOTTOM of one flat `vars.*` namespace: a scenario's `vars:`, a
// `block:`'s and a task's are layered over them, outermost first. Nothing
// outside the service repository overrides them — there is no
// `incarnation.spec.essence` and no successor to it; a fleet that needs
// different defaults forks the service repo and re-pins its ServiceRef
// (ADR-007). What an operator supplies is `input:`.
package servicevars

import (
	"log/slog"

	assembly "github.com/souls-guild/soul-stack/shared/servicevars"

	"github.com/souls-guild/soul-stack/shared/config"
)

// IncarnationContext — the incarnation fields a service definition and a
// `vars/_stack.yaml` selector read under `incarnation.*`. An alias: the type lives
// with the assembly, which builds the CEL map from it.
type IncarnationContext = assembly.IncarnationContext

// ErrStackStepInvalid — re-exported so a caller matches one sentinel whichever
// side raised it.
var ErrStackStepInvalid = config.ErrStackStepInvalid

// ResolveInput — input for assembling one service's vars.
type ResolveInput struct {
	// ServiceDir — service snapshot root (artifact.ServiceArtifact.LocalDir).
	ServiceDir string
	// Incarnation — what a definition and a `_stack.yaml` selector read under
	// `incarnation.*`. A zero value is an incarnation with no fields and no
	// covens: a definition reading one then fails when it is read.
	Incarnation IncarnationContext
}

// Resolver assembles the service view from a snapshot's `vars/` directory.
// Stateless, safe for concurrent use.
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
