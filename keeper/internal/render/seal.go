package render

import (
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// seal / sealed-paths ([ADR-010] §7.4) — render-time provenance/taint. The
// pipeline marks a params cell path SEALED when its RAW (pre vault-resolve+CEL)
// value is a string with a `${ … }` expression reading a secret source: a
// secret input of the active pass schema, vault(), or transitively sealed
// vars/compute. Detection is AST-based, via [cel.Engine.DetectSealed]
// (shared/cel/seal.go).
//
// SealedSet accumulates the found paths (dot/idx form, matching renderValue
// path EXACTLY) for a SINGLE Render pass. The caller (scenario.run) creates it,
// puts it in [RenderInput.Sealed], and after Render uses the paths for
// seal-aware masking (audit.MaskSecretsSealed) at write points
// (error_summary/status_details). nil → collection disabled (push/trial/Acolyte
// have no seal need).
type SealedSet struct {
	paths map[string]bool
}

// NewSealedSet — an empty accumulator of sealed paths for one Render pass.
func NewSealedSet() *SealedSet { return &SealedSet{paths: map[string]bool{}} }

// add marks a path sealed. nil receiver — no-op (collection disabled).
func (s *SealedSet) add(path string) {
	if s == nil {
		return
	}
	s.paths[path] = true
}

// Paths returns the sealed-path set for audit.MaskSecretsSealed (the map is
// copied — the caller can't mutate internal state). nil receiver → nil.
func (s *SealedSet) Paths() map[string]bool {
	if s == nil || len(s.paths) == 0 {
		return nil
	}
	out := make(map[string]bool, len(s.paths))
	for k := range s.paths {
		out[k] = true
	}
	return out
}

// scenarioSealSources builds [cel.SealSources] for a scenario pass: the
// secret-input set of the active scenario schema. vars/compute transitivity
// isn't precomputed in the pilot (vars resolve per-task; secret provenance via
// vars is still caught because the vars value itself goes through
// DetectSealed — an extension of this). nil schema → empty set (the detector
// only catches vault()).
func scenarioSealSources(in RenderInput) cel.SealSources {
	return cel.SealSources{SecretInputs: secretInputNames(in.Scenario)}
}

// secretInputNames — names of input parameters declared secret:true in the
// pass schema (scenario.Input / destiny.Input — both config.InputSchemaMap),
// plus fields with a non-empty vault_scope (by config-validator contract that's
// only applicable to secret:true, but we check explicitly — defense in depth,
// a secret's name shouldn't depend on another validator's invariant). nil →
// empty set.
func secretInputNames(scn *config.ScenarioManifest) map[string]bool {
	if scn == nil || len(scn.Input) == 0 {
		return nil
	}
	out := make(map[string]bool, len(scn.Input))
	for name, s := range scn.Input {
		if s != nil && (s.Secret || s.VaultScope != "") {
			out[name] = true
		}
	}
	return out
}

// renderContextInputPrefix — the cell-path prefix for
// render_context.input.<field> (Variant B, ADR-010 §7.4 S-1). render_context
// lives in params under the render_context key (paramRenderContext), input is
// a subsection of the §3.2 root; the cell path of a given input field for
// seal masking is render_context.input.<field>.
const renderContextInputPrefix = paramRenderContext + ".input."

// sealRenderContextInput marks sealed paths render_context.input.<secret> for
// every secret-input of the active pass schema (ADR-010 §7.4, mechanism S-1,
// Variant B). Closes the seal gap left by dropping the `params.vars`
// passthrough: a raw `${ input.secret }` no longer appears in params →
// collectSealed/DetectSealed can't catch it, so provenance is restored
// DECLARATIVELY — BY SCHEMA (the list of secret names), not by expression
// presence.
//
// ★CONDITIONAL (injectInput): the caller must call this ONLY when
// render_context.input is actually injected (the same gate as
// buildRenderContext). Otherwise the secret never lands in params, and its
// seal paths would just produce dead entries for a nonexistent cell — the gate
// keeps the seal set in sync with the real render_context contents.
//
// The list's source is secretInputNames(in.Scenario): in the pilot, a destiny
// pass doesn't propagate the destiny-input schema (set is empty — destiny's
// vault() provenance is caught without a schema). set nil → no-op. Called once
// per task (the path is host-invariant).
func sealRenderContextInput(set *SealedSet, in RenderInput) {
	if set == nil {
		return
	}
	for name := range secretInputNames(in.Scenario) {
		set.add(renderContextInputPrefix + name)
	}
}

// collectSealed walks RAW params (pre vault-resolve+CEL) with the same path
// walk as renderValue, and marks in set the path of every string cell whose
// `${ … }` expression reads a secret source (engine.DetectSealed). set nil →
// no-op. Called once per task (not per host): a cell's provenance is
// host-invariant (the secret source is the same on every host).
func collectSealed(engine *cel.Engine, set *SealedSet, params map[string]any, sources cel.SealSources, base string) {
	if set == nil {
		return
	}
	walkSealed(engine, set, params, sources, base)
}

func walkSealed(engine *cel.Engine, set *SealedSet, v any, sources cel.SealSources, path string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			walkSealed(engine, set, val, sources, joinKey(path, k))
		}
	case []any:
		for i, val := range t {
			walkSealed(engine, set, val, sources, joinIdx(path, i))
		}
	case string:
		if engine.DetectSealed(t, sources) {
			set.add(path)
		}
	}
}
