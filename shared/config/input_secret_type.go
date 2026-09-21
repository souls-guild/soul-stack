package config

// The INPUT side of `type: secret` ([ADR-0086] §5, NIM-751).
//
// A `type: secret` property is a STATE-side declaration: the platform mints the
// value, it lives in Vault, and it never travels through operator input
// ([ADR-0083] §1). Since [ADR-0086] §5 a shared type in `types.yml` may carry such
// a property, and one shared type feeds `state_schema` and `input:` alike — so the
// declaration now reaches an operator form through `$type`, where it means nothing
// an operator can act on.
//
// The ADR wrote the rule down — "a property with `type: secret` in a shared type is
// not asked for on input" — and recorded in the same breath that the engine did not
// enforce it. This is that enforcement, and it is three refusals rather than one,
// because "not asked for" has three separate spellings here:
//
//  1. it is not ON THE FORM — the projection drops it
//     (keeper/internal/artifact/scenario_types.go: stripFormSecrets, the raw-map
//     twin of this walk, on the one code path that hands a schema to an operator);
//  2. it is never REQUIRED — [requireInputValues] and [validateObjectFields] skip
//     it, so a `required: true` written inside the shared type cannot make an
//     operator supply a value they have no way to produce. The state-side refusal
//     of that key ([ADR-0086] §7) runs off `state_schema`, so a type reached only
//     from `input:` never passes it — requiredness has to fail closed here too;
//  3. a value supplied ANYWAY is refused, not ignored — [ErrSecretTypeNotWritable].
//
// (3) is [ADR-0086] §5's candidate 1, chosen by the user 2026-09-02 over silently
// dropping the value. A silent drop is worse in the only case that matters: the
// caller is told the request was accepted, so the operator believes the password
// they typed is in force while the platform mints a different one. It is also what
// closes the window [ADR-0083] §6 declares closed — without it the value transits
// `apply_task_register` into `params.value` of `core.state.set`, and
// [StripDeclaredSecrets] only strips on the way into Postgres, i.e. after.
//
// Writing `type: secret` DIRECTLY in an `input:` block is a different refusal and
// was already positional — `input_type_invalid` at the offending `type:`, with a
// hint naming the `secret: true` modifier instead (see [schemaDialect.typeEnum]).
// Only the `$type` route was open.

import (
	"errors"
	"fmt"
)

// SecretTypeNotWritableCode — the refusal of an operator value landing on a
// `type: secret` property. The name is [ADR-0086] §5's, reserved there and spent
// here.
//
// The value path carries plain errors rather than [diag.Diagnostic] (the schema is
// already valid by the time it runs; what fails is the request), so the code is the
// sentinel's message and a test pins [ErrSecretTypeNotWritable] rather than prose.
const SecretTypeNotWritableCode = "input_secret_type_not_writable"

// ErrSecretTypeNotWritable — sentinel behind every refusal of an
// operator-supplied value for a declared secret. Wrapped, so a caller matches it
// with errors.Is while the operator reads the path that offended.
var ErrSecretTypeNotWritable = errors.New(SecretTypeNotWritableCode)

// isDeclaredSecret reports whether this node is a declared secret — the `type:
// secret` marker, NOT the `secret: true` modifier. The two say opposite things
// about who produces the value, which is why one predicate must never stand in for
// the other ([ADR-0086] §5).
func (s *InputSchema) isDeclaredSecret() bool {
	return s != nil && s.Type == SecretTypeName
}

// NotAskedOfOperator reports whether the form projection drops this node: it IS a
// declared secret, or the strip leaves it with nothing an operator could fill.
//
// It is the typed mirror of `stripFormSecretNode`
// (keeper/internal/artifact/scenario_types.go) and must agree with it EXACTLY, in
// both directions. A field the form drops but the engine still demands produces
// *"input \"toks\" is required but was not provided"* for something the operator was
// never offered; a field the form keeps but the engine stops asking for is the
// mirror-image regression, an input silently going optional. The two walks are
// separate because the form travels as a raw `map[string]any` on a path that never
// builds an [InputSchema] — which is exactly why the agreement needs a test rather
// than a promise, and why this is exported: it is the only way the artifact package
// can compare them (`TestStripFormSecrets_AgreesWithTypedPredicate`).
//
// The survival rule is emptiness, not contagion — see the raw twin's doc comment for
// why each of `items` / `additional_properties` / `properties` behaves as it does.
// An object carrying one minted secret among ordinary properties stays; an object
// whose every property is minted goes.
func (s *InputSchema) NotAskedOfOperator() bool {
	if s == nil {
		return false
	}
	if s.isDeclaredSecret() {
		return true
	}
	// An array's element schema is its whole content.
	if s.Items != nil && s.Items.NotAskedOfOperator() {
		return true
	}

	var hadContent, hasContent bool
	if ap := s.additionalPropertiesSchema(); ap != nil {
		hadContent = true
		if !ap.NotAskedOfOperator() {
			hasContent = true
		}
	}
	if len(s.Properties) > 0 {
		hadContent = true
		for _, p := range s.Properties {
			if !p.NotAskedOfOperator() {
				hasContent = true
				break
			}
		}
	}
	return hadContent && !hasContent
}

// additionalPropertiesSchema returns the schema governing an object's undescribed
// keys, or nil when the node names none (absent, or the bare `false` that forbids
// them rather than describing them).
func (s *InputSchema) additionalPropertiesSchema() *InputSchema {
	ap, _ := s.AdditionalProperties.(*InputSchema)
	return ap
}

// refuseDeclaredSecretValues refuses an operator value that landed on a declared
// secret in a position ordinary value validation does not reach.
//
// That position is `additional_properties`: [validateObjectFields] skips a key the
// object's `properties:` does not describe, because the MVP does not check
// undescribed values in depth. Skipping the CHECKS is a deliberate MVP limit;
// skipping the REFUSAL was a hole — `additional_properties: { $type: AclUser }` put
// every minted password back on the operator's side of the boundary, on the one
// container shape the form strip already handled.
//
// So this walk enforces exactly one rule and validates nothing else: no type match,
// no enum, no pattern. Widening it into real validation of undescribed values would
// be a different decision with its own compatibility cost, and it is not this one.
//
// The walk follows the schema's own structure the way [scanSecretNodes] does, so a
// secret nested inside the additional-properties schema is found rather than only a
// secret that IS it. Ordering is sorted at every level: several offending keys must
// name the same one first on every run.
func refuseDeclaredSecretValues(path string, s *InputSchema, v any) error {
	if s == nil {
		return nil
	}
	// The secret check comes FIRST, before the nil short-circuit: `{"pw": null}` is a
	// key the caller wrote, and [validateValueAt] refuses it on a described property
	// without looking at the value. Two enforcement sites disagreeing about one input
	// is how "nulls are fine here" gets learned.
	if s.isDeclaredSecret() {
		return secretTypeNotWritable(path)
	}
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case []any:
		for i, el := range val {
			if err := refuseDeclaredSecretValues(fmt.Sprintf("%s[%d]", path, i), s.Items, el); err != nil {
				return err
			}
		}
	case map[string]any:
		ap := s.additionalPropertiesSchema()
		for _, k := range sortedMapKeys(val) {
			sub := s.Properties[k]
			if sub == nil {
				sub = ap
			}
			if err := refuseDeclaredSecretValues(path+"."+k, sub, val[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

// secretTypeNotWritable — the refusal for a value at `path`.
//
// The value is NOT echoed, at any masking. Every other refusal in this file's
// neighbour prints the offending literal because the literal is the diagnostic;
// here the diagnostic is the KEY, and the literal is a password the caller sent in
// the clear. A validation error lands in incarnation.StatusDetails and in audit
// (see [literalFor]), so not formatting it is the only way it cannot be stored.
func secretTypeNotWritable(path string) error {
	return fmt.Errorf("input %s declares `type: secret` — the platform issues that value and it is not accepted on input (%w)",
		path, ErrSecretTypeNotWritable)
}
