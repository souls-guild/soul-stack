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
// predicate did not compile, read an incarnation fact this path does not have
// ([ErrIncarnationNotInScope]), blew up at runtime or returned a non-bool. This is
// not "the input is bad" (schema validation already rejects such a rule at load
// time): it is a pre-flight malfunction, which callers map to 5xx rather than
// 422. A rule that legitimately evaluated to false is reported as a
// [ValidateRuleFailure] instead.
//
// A scope refusal belongs in THIS bucket and not in the 422 one on purpose: the
// operator's input is not what is wrong, so telling them their request failed
// validation would send them to fix a value that is correct. The scenario is what
// needs fixing, and the message names the rule, the field and the context.
var ErrValidateRuleEval = errors.New("config: validate rule evaluation failed")

// ResolveInputContract runs the full input gate of an artifact and returns the
// effective input:
//
//  1. [ResolveInputValues] — default merge, required/`required_when`, and value
//     validation against the schema (type/enum/pattern/format/length,
//     recursively into array/object);
//  2. [EvalValidateRules] — the declarative `validate:` invariants over the
//     MERGED input and the incarnation facts inc says this path knows
//     (validate_scope.go).
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
// provided is nil-safe; the returned map is new (provided is not mutated). The
// zero inc is the input-only context this gate had before NIM-833 — every caller
// states its stance, and one that inherits the zero value gets a refusal on any
// `incarnation.*` rule rather than a rule that passes over nothing.
//
// vault-refs are NOT resolved here — see [ResolveInputValuesVault] for the
// keeper-side scoped phase.
func ResolveInputContract(schema InputSchemaMap, rules []ValidateRule, provided map[string]any, inc ValidateContext, opts ...InputContractOption) (map[string]any, error) {
	var scope inputContractScope
	for _, opt := range opts {
		opt(&scope)
	}
	merged, err := ResolveInputValues(scope.narrow(schema), provided)
	if err != nil {
		return nil, err
	}
	fail, evErr := evalValidateRules(rules, merged, inc, scope.reads)
	if evErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrValidateRuleEval, evErr)
	}
	if fail != nil {
		return nil, fail
	}
	return merged, nil
}

// InputContractOption adjusts one [ResolveInputContract] call.
type InputContractOption func(*inputContractScope)

type inputContractScope struct {
	absent map[string]bool
}

// WithoutInputs runs the contract for a caller that holds values for only part of
// the input: render's layout-only destiny pass, which leaves the per-host names
// unrendered (ADR-0091 amendment NIM-993, rule 1). The named inputs get no default,
// no `required:` and no value check, and a `required_when:` or `validate:` rule
// that reads one is skipped — so is a rule reading the namespace whole or by a
// computed key, since it might. A skipped `validate:` rule keeps its index for the
// rules after it.
func WithoutInputs(names []string) InputContractOption {
	return func(s *inputContractScope) {
		for _, name := range names {
			if s.absent == nil {
				s.absent = make(map[string]bool, len(names))
			}
			s.absent[name] = true
		}
	}
}

// narrow is schema without the absent inputs, and without any `required_when:`
// that reads one.
func (s inputContractScope) narrow(schema InputSchemaMap) InputSchemaMap {
	if len(s.absent) == 0 {
		return schema
	}
	out := make(InputSchemaMap, len(schema))
	for name, field := range schema {
		if s.absent[name] {
			continue
		}
		if field != nil && field.RequiredWhen != "" && s.reads(field.RequiredWhen) {
			cp := *field
			cp.RequiredWhen = ""
			field = &cp
		}
		out[name] = field
	}
	return out
}

// reads reports whether predicate expr may read an absent input: it names one, or
// reads the namespace whole or by a computed key. One that does not compile is left
// to the evaluator, which refuses it as it does with every input rendered.
func (s inputContractScope) reads(expr string) bool {
	if len(s.absent) == 0 {
		return false
	}
	refs, readable := ValidateRuleInputRefs(expr)
	if !readable {
		return false
	}
	for _, ref := range refs {
		if len(ref) == 0 || ref[0].Element || s.absent[ref[0].Key] {
			return true
		}
	}
	return false
}
