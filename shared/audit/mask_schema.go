package audit

import (
	"log/slog"
	"reflect"
)

// Declarative secret masking ([ADR-010] §7.4, four AUGMENT/OR layers):
//
//  1. schema — a field is masked if its path is declared secret:true in the
//     active source schema (input_schema/state_schema/manifest InputParamDef.Secret).
//     Declarative-primary: the operator/author explicitly marks a field secret;
//     masking does not depend on guessing by key name.
//  2. vault origin — a string value contains `vault:<mount>/` ([vaultRefRe],
//     mask.go). Masking by CONTENT (vault ref in the value), not by name.
//  3. seal (render-time provenance) — the cell path is marked sealed during the
//     render phase (a CEL expression read a secret source: secret-input/vault()/
//     transitively vars/compute). The seal set comes from the render pass
//     (keeper/internal/render), passed here as the dot-path set [SealOpts.Sealed].
//  4. regex-LAST-RESORT — [sensitiveKeyRe]/[isSensitiveKey] (mask.go). Catches
//     the sensitive-by-name class not covered by the declarative layers (internal
//     bootstrap_token/jwt/creds without a schema). Firing of THIS layer alone
//     (schema/vault/seal all silent) → the [SealOpts.RegexFallback] alarm + warn
//     log: a signal of a declarative gap, so the class is closed structurally
//     rather than relying on the key name.
//
// The layers are OR-combined: any hit → the cell is masked. The old [MaskSecrets]
// (mask.go) — vault+regex layers without schema/seal — is retained (additive,
// called from ~46 sites); [MaskSecretsWithSchema] adds the schema layer for the
// read path, [MaskSecretsSealed] the seal layer for render write points.

// SecretSchema is the narrow "is this path secret" surface: declarative layer
// (1). Implemented keeper-side over config.InputSchemaMap (input/scenario schema)
// and over a flat state_schema (`properties.<field>.secret: true`). The dot-path
// is the human-readable render cell path (`acl[0].password`, `config.tls_key`).
//
// shared/audit does not import shared/config (layering): the caller builds a
// SecretSchema from its own schema. The simplest implementation is [SecretPathSet].
type SecretSchema interface {
	// IsSecret reports whether the field path (dot/idx form, like payload keys)
	// is declared secret.
	IsSecret(path string) bool
}

// SecretPathSet is the set of dot-paths declared secret. The simplest
// [SecretSchema]: keeper builds it once by walking its schema. Slice indices are
// normalized — `acl[0].password` is checked against both the exact
// `acl[0].password` AND the generalized `acl[].password` (the schema describes an
// array element without a concrete index), see [normalizeIdx].
type SecretPathSet map[string]bool

// IsSecret reports whether the path is in the set (exact form or with
// generalized `[]` indices).
func (s SecretPathSet) IsSecret(path string) bool {
	if s[path] {
		return true
	}
	return s[normalizeIdx(path)]
}

// SealOpts holds the layered-masking parameters on top of [MaskSecrets].
type SealOpts struct {
	// Schema is the declarative layer (1); nil → layer off.
	Schema SecretSchema

	// Sealed is the seal layer (3): the set of cell dot-paths marked sealed at
	// the render phase. nil/empty → layer off. Checked exactly by cell path AND
	// by the generalized idx form (symmetric with SecretPathSet).
	Sealed map[string]bool

	// RegexFallback is the alarm (4): called when a cell was caught by the
	// regex-last-resort ALONE (schema/vault/seal silent for this path). nil → not
	// called (keeper wires the metric/log via DefaultSealHooks). Need not be
	// idempotent: called once per such cell.
	RegexFallback func(path string)

	// Logger is the warn-log channel for the regex fallback. nil → log suppressed
	// (the alarm metric still fires via RegexFallback).
	Logger *slog.Logger
}

// MaskSecretsWithSchema is the read-path variant ([ADR-010] §7.4): layered
// masking of the payload by the source schema (layer 1) on top of the vault+regex
// layers of [MaskSecrets]. nil schema → degrades to [MaskSecrets] byte-for-byte
// (schema layer off). The regex-fallback alarm comes from [DefaultSealHooks]
// (keeper wires the metric/log; nil → no-op in tests/offline).
//
// payload is not mutated; the result shape is as for [MaskSecrets].
func MaskSecretsWithSchema(payload map[string]any, schema SecretSchema) map[string]any {
	return MaskSecretsSealed(payload, SealOpts{
		Schema:        schema,
		RegexFallback: DefaultSealHooks.RegexFallback,
		Logger:        DefaultSealHooks.Logger,
	})
}

// MaskSecretsSealed is the full layered masking: schema (opts.Schema) + vault +
// seal (opts.Sealed) + regex-last-resort with an alarm. All layers OR. Used by
// render write points (error_summary/status_details/dispatch) that have the
// pass's seal set. Zero-value opts → equivalent to [MaskSecrets] with the alarm
// off.
//
// payload is not mutated; nil input → nil output.
func MaskSecretsSealed(payload map[string]any, opts SealOpts) map[string]any {
	if payload == nil {
		return nil
	}
	return maskMapLayered(payload, "", opts)
}

// maskMapLayered is the layered walk of one map level. path is the dot-path to
// this map (empty at the root). Layer order on the KEY:
//
//	schema(1) ∨ seal(3)  — by cell PATH (declarative/taint, not key name);
//	regex-last-resort(4) — by key NAME (isSensitiveKey).
//
// If a key was caught by regex ALONE (schema/seal silent for the path), the value
// is a string and NOT a vault ref (layer 2 would also be silent) — this is a pure
// regex fallback: alarm (metric+warn-log), a declarative-gap signal. Layer 2
// (vault) works by the string value's content inside maskValueLayered.
func maskMapLayered(m map[string]any, path string, opts SealOpts) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		cell := joinPath(path, k)

		declarative := pathIsSecret(cell, opts) // layers 1 (schema) + 3 (seal)
		if declarative {
			out[k] = maskedValue
			continue
		}

		// Layer 4: regex-last-resort by key name. Fires ONLY when the
		// declarative layers were silent — otherwise it is not the "sole hit".
		if isSensitiveKey(k) {
			alarmRegexFallback(cell, v, opts)
			out[k] = maskedValue
			continue
		}

		out[k] = maskValueLayered(v, cell, opts)
	}
	return out
}

// alarmRegexFallback fires the alarm (metric+warn-log) when a key was caught by
// the regex-last-resort ALONE and the value is NOT a vault ref (layer 2 would
// catch it itself — then regex is not the "sole" hit). A vault-ref value under a
// sensitive key is masked by both layers, so no alarm is needed (no declarative
// gap: vault origin is a structural signal). The alarm records exactly the
// sensitive-by-name class without schema/seal/vault (the expected class ii —
// internal bootstrap_token/jwt/creds), to surface what the declarative layers do
// not yet cover.
func alarmRegexFallback(cell string, v any, opts SealOpts) {
	if s, ok := v.(string); ok && vaultRefRe.MatchString(s) {
		return // vault layer would catch it — regex is not the sole hit
	}
	if opts.RegexFallback != nil {
		opts.RegexFallback(cell)
	}
	if opts.Logger != nil {
		// The cell path is NOT a secret (a field name); the value is NOT logged.
		opts.Logger.Warn("audit: secret caught by regex-last-resort, declarative (schema/seal/vault) stayed silent - declarative gap",
			slog.String("path", cell))
	}
}

// maskValueLayered is the layered walk of a value at cell path cell. string →
// layer 2 (vault) and regex-last-resort (4, with alarm); containers → recursive
// layered walk.
func maskValueLayered(v any, cell string, opts SealOpts) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return maskStringLayered(x, cell, opts)
	case map[string]any:
		return maskMapLayered(x, cell, opts)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskValueLayered(el, joinIdx(cell, i), opts)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, el := range x {
			sub := joinPath(cell, k)
			if pathIsSecret(sub, opts) {
				out[k] = maskedValue
				continue
			}
			if isSensitiveKey(k) {
				alarmRegexFallback(sub, el, opts)
				out[k] = maskedValue
				continue
			}
			out[k] = maskStringLayered(el, sub, opts)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = maskStringLayered(el, joinIdx(cell, i), opts)
		}
		return out
	default:
		// Typed containers (struct, other map/slice/ptr) — reflect walk WITHOUT
		// the schema/seal layers (their paths are not expressible as reflect
		// struct-field names in render-path terms). vault+regex by value/key as in
		// MaskSecrets. Cold path (payload here is map[string]any).
		return maskReflect(reflect.ValueOf(v))
	}
}

// maskStringLayered masks a string by layers 2 (vault) and 4 (regex-last-resort).
// schema/seal were already checked by path above (pathIsSecret). The vault layer
// works by content. A pure regex fallback by string CONTENT does not apply
// (sensitiveKeyRe is by key NAME, not value); the value-level regex fallback is
// decided at the key level in the key match (no separate maskKeyLayered branch is
// needed — the key match runs in maskMapLayered, where schema/seal take priority).
// Only vault-by-content here.
func maskStringLayered(s, _ string, _ SealOpts) any {
	if vaultRefRe.MatchString(s) {
		return maskedValue
	}
	return s
}

// pathIsSecret reports whether layer 1 (schema) OR layer 3 (seal) marked the path secret.
func pathIsSecret(path string, opts SealOpts) bool {
	if opts.Schema != nil && opts.Schema.IsSecret(path) {
		return true
	}
	if len(opts.Sealed) > 0 {
		if opts.Sealed[path] || opts.Sealed[normalizeIdx(path)] {
			return true
		}
	}
	return false
}
