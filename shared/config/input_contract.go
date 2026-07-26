package config

// The input contract of an artifact that declares `input:` plus the top-level
// `validate:` section — scenario, covenant fragment and destiny (ADR-009
// amendment 2026-07-26). [ResolveInputContract] is the SINGLE composition of the
// two phases, so the scenario pre-flight gate, the destiny render pass and the
// L0 trial harness cannot drift on what is checked or in which order.

import (
	"errors"
	"fmt"
)

// ErrValidateRuleEval marks an INTERNAL failure of the `validate:` phase — the
// predicate did not compile, blew up at runtime or returned a non-bool. This is
// not "the input is bad" (schema validation already rejects such a rule at load
// time): it is a pre-flight malfunction, which callers map to 5xx rather than
// 422. A rule that legitimately evaluated to false is reported as a
// [ValidateRuleFailure] instead.
var ErrValidateRuleEval = errors.New("config: validate rule evaluation failed")

// ResolveInputContract runs the full input gate of an artifact and returns the
// effective input:
//
//  1. [ResolveInputValues] — default merge, required/`required_when`, and value
//     validation against the schema (type/enum/pattern/format/length,
//     recursively into array/object);
//  2. [EvalValidateRules] — the declarative `validate:` invariants over the
//     MERGED input (input-only CEL sandbox).
//
// The order is strict and deliberate: schema first, invariants second — a `that`
// predicate is authored assuming correct types (`input.port > 0` is meaningless
// if port is not a number).
//
// Errors are classified by the caller:
//
//	var fail *ValidateRuleFailure
//	errors.As(err, &fail)                  // a validate: rule evaluated false
//	errors.Is(err, ErrValidateRuleEval)    // internal pre-flight malfunction
//	// otherwise                           // the values violate the input schema
//
// provided is nil-safe; the returned map is new (provided is not mutated).
// vault-refs are NOT resolved here — see [ResolveInputValuesVault] for the
// keeper-side scoped phase.
func ResolveInputContract(schema InputSchemaMap, rules []ValidateRule, provided map[string]any) (map[string]any, error) {
	merged, err := ResolveInputValues(schema, provided)
	if err != nil {
		return nil, err
	}
	fail, evErr := EvalValidateRules(rules, merged)
	if evErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrValidateRuleEval, evErr)
	}
	if fail != nil {
		return nil, fail
	}
	return merged, nil
}
