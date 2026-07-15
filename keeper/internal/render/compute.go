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
// run-level only (input/register/incarnation/essence + state from
// incarnationVars) — no soulprint.self or soulprint.hosts (AllowHosts=false).
// A soulprint.* reference in a compute expression hits CEL no-such-key (a
// structural barrier, not a text guard): compute is host-independent by
// construction, so the same value safely feeds both apply.input (resolved on
// targeted[0]) and state_changes (per-run, not per-host) without drift.
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
		Essence:     in.Essence,
		Ctx:         in.Ctx,
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
