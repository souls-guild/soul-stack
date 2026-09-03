// Package servicevars assembles a service's own variables — the values its
// author nailed down in the service repository, read in CEL as `vars.*`
// (ADR-0082, see architecture.md → "Service vars").
//
// They are the BOTTOM of one flat `vars.*` namespace: a scenario's `vars:`, a
// `block:`'s and a task's are layered over them, outermost first. Nothing
// outside the service repository overrides them — there is no
// `incarnation.spec.essence` and no successor to it; a fleet that needs
// different defaults forks the service repo and re-pins its ServiceRef
// (ADR-007). What an operator supplies is `input:`.
//
// Assembly is one layer: the files of `<ServiceDir>/vars/`, deep-merged in
// lexical order. Each next file wins on a collision — maps merge recursively,
// scalars and lists are replaced wholesale.
package servicevars

import (
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Layout of a service's vars in the service repository. All paths are relative
// to `<ServiceDir>/vars/` (docs/service/manifest.md, architecture.md →
// "Repository layout"): vars live in a subdirectory of the service, NOT at its
// root.
const (
	// varsDir — the vars root directory inside the service snapshot. A missing
	// directory is fine (a service carrying no vars of its own).
	varsDir = "vars"
	// stackFile — the optional declarative build pipeline (NIM-413). Excluded
	// from the lexical scan: it declares the order, it is not a layer in it.
	stackFile = config.StackFileName
	// retiredDir — the pre-ADR-0082 directory name. Never read; its presence
	// alongside a missing `vars/` is the signature of a half-finished migration,
	// and that is worth an error rather than an empty map.
	retiredDir = "essence"
	// strategyKey — the reserved top-level key by which a LAYER FILE declares how
	// its own keys merge, without a `_stack.yaml` to say it for them. Two forms:
	//
	//	_strategy: replace                    # every key of this file replaces
	//	_strategy: { install_package: replace }  # only the named keys
	//
	// The file is where the intent lives — `10-mirror.yaml` KNOWS it is an
	// override — and in lexical mode it is the only voice there is. Most specific
	// wins: a per-key declaration beats a whole-file one, which beats the step's
	// `strategy:`. The key never reaches the resolved map.
	strategyKey = "_strategy"
	// stackFileMisspelt — the `.yml` spelling, which is NOT accepted and is an
	// error rather than a shrug. Layer files take either extension, so left alone
	// this name would fail twice over: the pipeline would not run AND its own
	// manifest would be deep-merged in as data, putting a `stack:` key into the
	// service's vars. Neither failure says anything.
	stackFileMisspelt = config.StackFileMisspelt
)

// ResolveInput — input for assembling one service's vars.
type ResolveInput struct {
	// ServiceDir — service snapshot root (artifact.ServiceArtifact.LocalDir).
	ServiceDir string
	// Incarnation — the incarnation fields a `vars/_stack.yaml` step reads under
	// `incarnation.*`. Read ONLY by a stack step; the lexical mode has nothing
	// to evaluate, and a zero value there costs nothing.
	Incarnation IncarnationContext
}

// IncarnationContext — the incarnation's own fields as a `vars/_stack.yaml`
// step sees them.
//
// This env is the resolver's own, NOT the scenario's CEL context: it is
// evaluated BEFORE the render, and it carries `covens` — which the scenario
// context does not, and which is how a step reaches the labels an operator put
// on the incarnation now that the hard-wired `coven/<label>.yaml` overlay
// is gone (ADR-0082). The labels are the incarnation's own and select overlays of
// its own vars — they say nothing about its member hosts (NIM-281). It carries no
// `host_count` and no `state` snapshot: neither exists yet at this point in a run.
// As with [render.IncarnationMeta], the FIELD NAMES mirror the CEL keys rather
// than the registry columns: the column is `id` since [ADR-0085] / NIM-729,
// while the CEL root stays `incarnation.name` until NIM-730 opens its
// compatibility window. Callers fill `Name` from the renamed identifier.
type IncarnationContext struct {
	Name           string
	Service        string
	ServiceVersion string
	Covens         []string
	Traits         map[string]any
}

// celMap renders the context for the CEL activation. An absent field yields the
// normal no-such-key rather than an empty string, so a step cannot silently
// compare against "" and look like it matched something.
func (c IncarnationContext) celMap() map[string]any {
	m := map[string]any{}
	if c.Name != "" {
		m["name"] = c.Name
	}
	if c.Service != "" {
		m["service"] = c.Service
	}
	if c.ServiceVersion != "" {
		m["service_version"] = c.ServiceVersion
	}
	// covens is ALWAYS present, empty list included: `foreach: ${ incarnation.covens }`
	// over an untagged incarnation must iterate zero times, not fail to compile.
	covens := make([]any, len(c.Covens))
	for i, v := range c.Covens {
		covens[i] = v
	}
	m["covens"] = covens
	if c.Traits != nil {
		m["traits"] = c.Traits
	}
	return m
}

// Resolver assembles the vars map from the service's `vars/` directory.
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
