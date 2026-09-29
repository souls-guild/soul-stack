package render

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// resolveCompute evaluates the scenario-level `compute:` block (ADR-009
// amendment 2026-06-23) once per run, returning name→value for
// [RenderInput.Compute]. Entries resolve in declaration order; an
// already-computed value is visible to later ones as `compute.<name>`
// (accumulated in acc).
//
// ★ Isolation barrier #2 (compute is host-invariant): the resolve context is
// run-level only (input/register/incarnation/vars + state from
// incarnationVars) — no soulprint.self or soulprint.hosts (AllowHosts=false).
// Since NIM-908 a `soulprint.self` reference here is refused by NAME
// ([cel.HostFreeCompute]) instead of hitting the no-such-key the omitted map used
// to produce: compute is host-independent by construction, which is exactly why
// the same value can feed a per-host task and an `on: keeper` one without drift.
//
// ★ That invariance is the namespace's own, and it stopped being a claim about
// `apply: input:` in NIM-908. apply.input renders PER HOST now
// ([Pipeline.resolveApplyInput]); a compute value reaching it is still the same on
// every host, but the sentence that used to stand here — "apply.input is resolved
// on targeted[0]" — described the defect, not a guarantee.
//
// Resolved once: an already-computed in.Compute (from a caller or previous
// pass) is returned as-is — idempotent across repeated calls in staged
// render. Empty/absent block → nil (`compute.<name>` is a plain
// no-such-key, backward-compat bit-for-bit).
//
// Non-string values (number/bool/collection) pass through as literals — the
// CEL phase only touches strings (mirrors resolveTaskVars/renderValue).
func (p *Pipeline) resolveCompute(in RenderInput) (map[string]any, error) {
	if in.Compute != nil {
		return in.Compute, nil
	}
	block := in.Scenario.Compute
	if len(block) == 0 {
		return nil, nil
	}

	acc := make(map[string]any, len(block))
	// Run-level context, no soulprint. Compute accumulates in acc and is
	// re-attached each iteration → compute[i] sees compute[j<i].
	base := cel.Vars{
		Input:       in.Input,
		Register:    in.Register,
		Incarnation: incarnationVars(in, len(in.Hosts)),
		Vars:        in.ServiceVars,
		Ctx:         in.Ctx,
		// The block resolves INSIDE its own namespace: entry i reads entries j<i as
		// `compute.<name>` (base.Compute = acc below), so the namespace is in scope
		// here even on the first entry — where a reference is a genuine no-such-key
		// (a forward reference), not an absent namespace.
		ComputeScope: cel.ComputeAvailable,
		// Host-invariance is the property this namespace rests on, so the barrier
		// says so out loud since NIM-908 rather than relying on an omitted soulprint
		// map. HostVariantInputs is in.hostVariantInputs for symmetry with the other
		// host-free builders and is empty in practice: resolveCompute runs on the
		// scenario pass only, and the destiny pass has no compute: block of its own.
		HostScope:         cel.HostFreeCompute,
		HostVariantInputs: in.hostVariantInputs,
		HostVariantVars:   in.hostVariantVars,
	}
	for _, cv := range block {
		s, ok := cv.Value.(string)
		if !ok {
			acc[cv.Name] = cv.Value // literal — passes through
			continue
		}
		base.Compute = acc
		val, err := p.cel.EvalInterpolation(s, base)
		if err != nil {
			return nil, fmt.Errorf("render: compute.%s: %w", cv.Name, err)
		}
		acc[cv.Name] = val
	}
	return acc, nil
}
