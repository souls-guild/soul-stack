package runtime

// Param-level strictness before Apply (ADR-0076): the last silent-ignore mode.
//
// The capability gate (NIM-161) closes "this binary does not have the module at
// all". One level down stays open: the module IS here, but a param of it is not.
// A Soul reads params by key, so a key it does not know is never read — the
// module does its old job and reports OK/CHANGED while what the author asked for
// never happened.
//
// The contract enforced is the one compiled into THIS binary, never keeper's:
// a newer keeper sending a param this Soul does not implement gets a loud FAILED
// instead of a false success, while an older keeper sending a subset passes
// untouched, so the only-add discipline of ADR-012 keeps holding.

import (
	"fmt"
	"log/slog"
	"sort"

	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// ParamStrictness — what a layer's input declaration is worth at runtime. The
// two manifest homes carry different guarantees: a core manifest is EMBEDDED
// (go:embed), so declaration and implementation cannot drift and it is enforced;
// a custom module's manifest sits beside its binary, was never enforced before,
// and under-declares in practice, so it only warns (ADR-0076 amendment (q)).
type ParamStrictness int

const (
	// ParamsUnchecked — no enforceable contract here: the layer carries no
	// manifest for the module (core.augur), or the state is unknown to it. The
	// task dispatches unchecked, exactly as before.
	ParamsUnchecked ParamStrictness = iota
	// ParamsAdvisory — a declaration exists but was not authored under
	// enforcement: an unknown param is logged, the task still runs.
	ParamsAdvisory
	// ParamsEnforced — the declaration is complete by construction: an unknown
	// param FAILS the task with module.unknown_param.
	ParamsEnforced
)

// ParamSchema is the optional half of [Registry]: a layer that can state the
// input contract of the modules it serves. A layer that does not implement it
// (a hand-built fake, a store with no manifests) yields no contract and its
// modules dispatch unchecked.
type ParamSchema interface {
	// StateInput returns the declared input of module+state and how far that
	// declaration may be trusted.
	StateInput(module, state string) (map[string]plugin.InputParamDef, ParamStrictness)
}

// NewCoreParamSchema decorates the static core layer with the manifest contract
// embedded in THIS binary (shared/coremanifest) — the very declaration keeper
// validates the author's text against, so the two gates can never disagree
// about what a core module accepts. Enforced: go:embed puts the manifest and
// the implementation in the same artifact, so the declaration cannot lag behind
// the module the way an on-disk plugin manifest can.
//
// A core module with no embedded manifest (core.augur) yields ParamsUnchecked
// rather than "accepts nothing" — absence of a declaration is not a declaration
// of absence.
func NewCoreParamSchema(reg Registry) Registry { return coreParamSchema{Registry: reg} }

type coreParamSchema struct{ Registry }

func (coreParamSchema) StateInput(module, state string) (map[string]plugin.InputParamDef, ParamStrictness) {
	def, ok := coremanifest.Default().State(module, state)
	if !ok {
		return nil, ParamsUnchecked
	}
	return def.Input, ParamsEnforced
}

// transportParams — engine-internal keys keeper substitutes into a task at
// render, deliberately absent from the author-facing manifest input: for
// `core.file.rendered` the author writes `template:`/`vars:` and the wire
// carries `template_content`/`render_context` instead (ADR-012(d) variant A1,
// keeper/internal/render). Keyed by the FULL address including state, so a
// transport key is tolerated only on the state that owns it.
var transportParams = map[string]map[string]struct{}{
	"core.file.rendered": {"template_content": {}, "render_context": {}},
}

// checkParams rejects a param that this binary's own manifest does not declare
// (ADR-0076). Returns nil when the task is clean, when no contract exists for
// module+state, or when the contract is advisory (the unknown param is logged
// instead).
//
// Only the UNKNOWN direction is enforced. A missing required param is left to
// keeper's static check (shared/config.validateModuleParams) and to the module
// itself: keeper validates the author's text before a run exists, where the
// diagnostic carries a line and column, and a param may legitimately arrive
// from a manifest default. Re-deriving that here would duplicate the rule in a
// place with worse errors and no upside.
func (r *ApplyRunner) checkParams(module, state, taskName string, params *structpb.Struct) *keeperv1.TaskError {
	schema, ok := r.registry.(ParamSchema)
	if !ok {
		return nil
	}
	declared, strictness := schema.StateInput(module, state)
	if strictness == ParamsUnchecked {
		return nil
	}

	addr := module + "." + state
	transport := transportParams[addr]
	var unknown []string
	for name := range params.GetFields() {
		if p, known := declared[name]; known {
			if p.Deprecated != nil {
				// The author's surface is keeper's static check; this line is
				// for the operator reading Soul logs during a rollout.
				slog.Default().Warn("runtime: "+p.Deprecated.Notice(name), slog.String("module", addr))
			}
			continue
		}
		if _, isTransport := transport[name]; isTransport {
			continue
		}
		unknown = append(unknown, name)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	if strictness == ParamsAdvisory {
		slog.Default().Warn("runtime: task carries params this module does not declare",
			slog.String("module", addr),
			slog.String("task", taskName),
			slog.Any("params", unknown),
			slog.String("hint", "the module will ignore them - declare them in its manifest input (ADR-0076)"))
		return nil
	}

	return &keeperv1.TaskError{
		Code:   "module.unknown_param",
		Module: module,
		Message: fmt.Sprintf(
			"%s does not accept %s (task %q) - this soul implements %s; the task was NOT applied, upgrade the soul binary or drop the param (ADR-0076)",
			addr, quoteList(unknown), taskName, quoteList(sortedKeys(declared))),
	}
}

func sortedKeys(m map[string]plugin.InputParamDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// quoteList renders names as `"a", "b"`; an empty set reads as `no params` (a
// state that declares no input accepts none).
func quoteList(names []string) string {
	if len(names) == 0 {
		return "no params"
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", n)
	}
	return out
}
