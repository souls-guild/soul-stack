package render

import (
	"sort"
	"strings"

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
// secret-input set of the active scenario schema, plus the registers whose
// payload carried a declared secret ([ADR-0083] §6, derived by
// [Pipeline.resolveRegisterSecrets] before any root is built). vars/compute transitivity
// isn't precomputed in the pilot (vars resolve per-task; secret provenance via
// vars is still caught because the vars value itself goes through
// DetectSealed — an extension of this). nil schema → empty set (the detector
// only catches vault()).
func scenarioSealSources(in RenderInput) cel.SealSources {
	return cel.SealSources{
		SecretInputs:    secretInputNames(in.Scenario),
		SealedRegisters: in.sealedRegisters,
	}
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

// SealedValues returns the RESOLVED values a task's params carry at sealed
// paths — the secrets that actually travelled, for a caller who has to keep
// them out of a free-text channel that masking-by-path cannot reach.
//
// [audit.MaskSecretsSealed] answers "what does this payload look like with the
// sealed cells removed" and needs the payload to BE the params tree. A module's
// failure message is not that tree: it is one string that may quote a value out
// of it. The only thing that helps there is knowing the values, which is what
// this returns.
//
// It lives beside [collectSealed] deliberately and walks the same way: the path
// spelling is the whole contract between the two, and a second spelling of it
// elsewhere is how a masker comes to miss exactly the cell the seal marked.
// Sealed paths are collected from the RAW params and looked up here against the
// RENDERED ones — the same cell, before and after the value arrived.
//
// ★ A sealed path is sealed WITH EVERYTHING UNDER IT, and that is not a
// convenience — it is the common case. The seal is collected from the RAW params,
// where a cell is one string; the value that arrives can be a whole subtree. A
// bare `vault:<mount>/<path>` with no `#field` resolves to the entire KV map
// ([readVaultRef]), and a whole-cell `${ … }` yields a native list or map under
// [ADR-010]'s non-string rule. Recording only the string AT the sealed path would
// therefore find nothing in exactly the shape this exists for — a credentials map
// handed to a keeper-side plugin — and mask nothing at all.
//
// ⚠ This is NOT parity with [audit.MaskSecretsSealed], and the gap is real rather
// than a rounding: that masker replaces the cell at a sealed path whatever its
// TYPE, while this one can only return strings. A numeric secret under a sealed
// path is therefore `***MASKED***` in a payload and legible in a message.
// Widening the type switch is not the fix — `87654321` is a plausible substring
// of a byte count or a timestamp, so masking a number out of free text needs a
// decision about text masking rather than one more case arm here.
//
// Empty strings are dropped: redacting "" would blank every position in the
// message. The result is sorted and deduplicated — it feeds an observable
// channel, which must be byte-identical for identical input.
//
// ★ The cost of the subtree rule, stated because it is paid on every message.
// Vault hands back a whole KV map, so a sealed `creds` cell contributes its
// NON-SECRET siblings as well — `user`, `host`, `port`. Those are then redacted
// from every keeper task's message in the run, core modules included, because the
// seal set is RUN-wide and is matched against each task's own params root. An
// operator reading `await timeout for ***` instead of a hostname is the price of
// not leaking the password that sat beside it, and a short leaf (`svc`, `6379`)
// collides with ordinary words. It is the right direction to err in; it is not
// free.
func SealedValues(params map[string]any, sealed map[string]bool) []string {
	if len(params) == 0 || len(sealed) == 0 {
		return nil
	}
	found := map[string]bool{}
	walkSealedValues(params, sealed, "", false, found)
	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for v := range found {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// walkSealedValues carries inSealed down: once a node's path is sealed, every
// string leaf beneath it is a secret, whether or not its own deeper path was
// ever recorded in the set — a sealed subtree has no unsealed interior.
func walkSealedValues(v any, sealed map[string]bool, path string, inSealed bool, found map[string]bool) {
	inSealed = inSealed || sealed[path]
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			walkSealedValues(val, sealed, joinKey(path, k), inSealed, found)
		}
	case []any:
		for i, val := range t {
			walkSealedValues(val, sealed, joinIdx(path, i), inSealed, found)
		}
	case string:
		if t != "" && inSealed {
			found[t] = true
		}
	}
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
		// A literal `vault:<mount>/<path>` cell is replaced by the secret it
		// names in the vault-resolve phase — walkVaultValue keys off the same
		// prefix — so its provenance is known here without an expression to
		// inspect (DetectSealed reads `${ … }` segments and a bare ref has
		// none). Sealing it keeps the declarative layer of
		// audit.MaskSecretsSealed ahead of the regex last resort, which would
		// mask such a cell only when its KEY looked sensitive and would raise a
		// declarative-gap alarm every time it did.
		if strings.HasPrefix(t, vaultRefPrefix) {
			set.add(path)
			return
		}
		if engine.DetectSealed(t, sources) {
			set.add(path)
		}
	}
}
