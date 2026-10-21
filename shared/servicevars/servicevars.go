// Package servicevars assembles a service's own variables — the service rung of
// `vars.*` ([ADR-0082]) — into the layers of a lazy [cel.VarView]: one layer per
// file of `<service>/vars/` in lexical order, or one per selected `_stack.yaml`
// step and `foreach:` binding. Only the stack's selectors are evaluated here. A
// layer's definitions are RAW, and the view evaluates a name when something reads
// it (ADR-0082 amendment 2026-10-15).
//
// It lives in shared/ because two binaries assemble the same rung: keeper, for a
// run, and soul-lint, offline. Files are read through the [fs.FS] the caller
// supplies, and containment is that FS's job: keeper's resolves every path,
// symlinks included, inside the snapshot root.
//
// [ADR-0082]: ../../docs/adr/0082-service-vars.md
package servicevars

import (
	"fmt"
	"sync"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// serviceVarsEngine is the one service-rung engine every layer and stack parse in
// the process shares, and selectorEngine the one every `_stack.yaml` selector is
// evaluated with. An environment costs a cel-go build, and telemetry assembles a
// view on every Soul connect; an Engine is safe for concurrent use.
var (
	serviceVarsEngine = sync.OnceValues(func() (*cel.Engine, error) {
		return cel.NewServiceVars()
	})
	selectorEngine = sync.OnceValues(func() (*cel.Engine, error) {
		return cel.NewServiceVarsSelector()
	})
)

// Layout of a service's vars in the service repository, relative to the
// snapshot root (docs/service/manifest.md).
const (
	// varsDir — the vars root directory. A missing one is a service carrying no
	// vars of its own.
	varsDir = "vars"
	// stackFile — the optional declarative pipeline. Excluded from the lexical
	// scan: it declares the order, it is not a layer in it.
	stackFile = config.StackFileName
	// stackFileMisspelt — the `.yml` spelling, refused rather than ignored. Layer
	// files take either extension, so left alone this name would fail twice over:
	// the pipeline would not run AND its own manifest would be merged in as data.
	stackFileMisspelt = config.StackFileMisspelt
	// retiredDir — the pre-ADR-0082 directory name. Never read; its presence
	// beside a missing `vars/` is a half-finished migration, worth an error rather
	// than no vars.
	retiredDir = "essence"
	// strategyKey — the reserved top-level key by which a LAYER FILE declares how
	// its own keys merge:
	//
	//	_strategy: replace                       # every key of this file replaces
	//	_strategy: { install_package: replace }  # only the named keys
	//
	// The file is where the intent lives, and in lexical mode it is the only voice
	// there is. Most specific wins: a per-key declaration beats a whole-file one,
	// which beats the step's `strategy:`. The key is never a var.
	strategyKey = "_strategy"
)

// IncarnationContext — the incarnation's own fields, as a service definition and
// a `_stack.yaml` selector read them under `incarnation.*`.
//
// This is the service rung's own environment ([cel.NewServiceVars]), NOT the
// scenario's: it carries `covens` — which is how a step reaches the labels an
// operator put on the incarnation now that the hard-wired `coven/<label>.yaml`
// overlay is gone (ADR-0082). The labels are the incarnation's own and select
// overlays of its own vars; they say nothing about its member hosts (NIM-281). It
// carries no `host_count` and no `state` snapshot: the service view is built once
// per run, at run start, before either exists. The FIELD NAMES mirror the CEL
// keys, as keeper's render.IncarnationMeta does, and since NIM-730 they agree with
// the registry column: `id` ([ADR-0085]).
//
// [ADR-0085]: ../../docs/adr/0085-entity-id-and-label.md
type IncarnationContext struct {
	ID             string
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
	if c.ID != "" {
		m["id"] = c.ID
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
	// traits is ALWAYS present too: a run reads them off a NOT NULL DEFAULT '{}'
	// column, so a caller holding none — the create path's pre-flight, with no row
	// yet — must not answer `has(incarnation.traits)` or
	// `default(incarnation.traits.team, …)` otherwise.
	traits := c.Traits
	if traits == nil {
		traits = map[string]any{}
	}
	m["traits"] = traits
	return m
}

// NewLayer is one service-rung layer over definitions the caller already holds —
// an L0 case's `fixtures.vars`, which stands for the whole rung. origin names the
// layer in an error. defs are taken as they are: no `_strategy` is lifted out,
// since a single layer has nothing below it to merge onto.
func NewLayer(origin string, defs map[string]any, inc IncarnationContext) (cel.VarLayer, error) {
	engine, err := serviceVarsEngine()
	if err != nil {
		return cel.VarLayer{}, fmt.Errorf("servicevars: cel engine: %w", err)
	}
	return cel.VarLayer{
		Rung:   cel.RungService,
		Origin: origin,
		Defs:   defs,
		Engine: engine,
		Base:   cel.Vars{Incarnation: inc.celMap()},
	}, nil
}
