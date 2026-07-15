package util

import (
	"fmt"
	"sort"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// ValidateAgainstManifest — runtime validation of a ValidateRequest against a
// core module's embedded manifest (shared/coremanifest). Single source of
// truth for per-field checks: known states and required params are declared
// in the module's manifest.yaml, not hardcoded separately in each
// Module.Validate (previously duplicated between the linter and runtime code).
//
// `coreName` is the canonical core-module name (`core.exec`, `core.file`).
// Returns a list of errors (empty = all valid). Runtime value type-checking
// is NOT done here: typed param getters in Apply handle that
// (StringParam/OptStringSliceParam/…) — imperative per-field checks the
// manifest DSL can't express. Cross-field invariants (if a module needs them)
// go into its own Module.Validate on top of this call.
func ValidateAgainstManifest(coreName string, req *pluginv1.ValidateRequest) []string {
	m, ok := coremanifest.Default().Lookup(coreName)
	if !ok {
		// A core module's manifest must exist (otherwise it's a build bug); but
		// we don't panic at runtime — report it as a regular validation error.
		return []string{fmt.Sprintf("internal: no manifest for %q", coreName)}
	}
	def, ok := m.Spec.States[req.State]
	if !ok {
		return []string{fmt.Sprintf("unknown state %q (want one of %v)", req.State, sortedStates(m))}
	}
	var errs []string
	for _, name := range sortedRequiredParams(def) {
		if !paramPresent(req, name) {
			errs = append(errs, fmt.Sprintf("param %q: missing", name))
		}
	}
	return errs
}

func paramPresent(req *pluginv1.ValidateRequest, name string) bool {
	if req == nil {
		return false
	}
	return ParamPresent(req.Params, name)
}

func sortedStates(m *plugin.Manifest) []string {
	out := make([]string, 0, len(m.Spec.States))
	for s := range m.Spec.States {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sortedRequiredParams(def plugin.StateDef) []string {
	var out []string
	for name, p := range def.Input {
		if p.Required {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
