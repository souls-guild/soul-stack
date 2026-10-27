package config

import (
	"errors"
	"strings"
	"testing"
)

// TestResolveInputContract_Table is the guard for the SHARED input gate used by
// both scenario pre-flight and the destiny render pass (NIM-167): every rule
// class must behave identically no matter which layer calls it. A regression
// here means one layer silently stopped enforcing what the other does.
func TestResolveInputContract_Table(t *testing.T) {
	schema := InputSchemaMap{
		"redis_type": {Type: "string", Required: true, Enum: []any{"standalone", "cluster"}},
		"port":       {Type: "integer", RequiredWhen: "input.redis_type == 'standalone'"},
		"nodes":      {Type: "array", Items: &InputSchema{Type: "string"}},
		"name":       {Type: "string", Pattern: "^[a-z][a-z0-9-]*$", MaxLength: intPtr(8)},
		"mode":       {Type: "string", Default: "rdb"},
	}
	rules := []ValidateRule{
		{That: "input.redis_type != 'cluster' || size(input.nodes) >= 3", Message: "cluster needs at least 3 nodes"},
	}

	cases := []struct {
		name string
		in   map[string]any
		// wantErr: substring the message must carry; "" means the input passes.
		wantErr string
		// wantRule: the failure must be a *ValidateRuleFailure, not a schema error.
		wantRule bool
	}{
		{
			name: "valid input passes and gets defaults",
			in:   map[string]any{"redis_type": "standalone", "port": 6379, "name": "redis-01"},
		},
		{
			name:    "required missing",
			in:      map[string]any{"port": 6379},
			wantErr: `input "redis_type" is required`,
		},
		{
			name:    "required_when true without a value is refused",
			in:      map[string]any{"redis_type": "standalone"},
			wantErr: `input "port" is required`,
		},
		{
			name: "required_when false leaves the field optional",
			in:   map[string]any{"redis_type": "cluster", "nodes": []any{"a", "b", "c"}},
		},
		{
			name:    "enum violation names the field",
			in:      map[string]any{"redis_type": "sentinel"},
			wantErr: "input $.redis_type",
		},
		{
			name:    "type mismatch names the field",
			in:      map[string]any{"redis_type": "standalone", "port": "6379"},
			wantErr: `input $.port = "6379" does not match type "integer"`,
		},
		{
			name:    "pattern violation names the field",
			in:      map[string]any{"redis_type": "standalone", "port": 6379, "name": "Redis01"},
			wantErr: `input $.name = "Redis01" does not match pattern`,
		},
		{
			name:    "max_length violation names the field",
			in:      map[string]any{"redis_type": "standalone", "port": 6379, "name": "redis-0123"},
			wantErr: "input $.name",
		},
		{
			name:    "array item type is checked recursively",
			in:      map[string]any{"redis_type": "cluster", "nodes": []any{"a", 7, "c"}},
			wantErr: "input $.nodes[1]",
		},
		{
			name:     "validate rule failure carries its message",
			in:       map[string]any{"redis_type": "cluster", "nodes": []any{"a", "b"}},
			wantErr:  "cluster needs at least 3 nodes",
			wantRule: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := ResolveInputContract(schema, rules, tc.in, ValidateContext{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolveInputContract: unexpected error: %v", err)
				}
				if merged["mode"] != "rdb" {
					t.Errorf("mode = %v, want rdb (default merged in)", merged["mode"])
				}
				return
			}
			if err == nil {
				t.Fatalf("ResolveInputContract: want error %q, got nil (contract not enforced)", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			var fail *ValidateRuleFailure
			if got := errors.As(err, &fail); got != tc.wantRule {
				t.Errorf("errors.As(*ValidateRuleFailure) = %v, want %v", got, tc.wantRule)
			}
		})
	}
}

// TestResolveInputContract_SchemaBeforeRules — the phase order is load-bearing: a
// `that` predicate is authored assuming correct types, so a type violation must
// surface as a schema error, never as a CEL blow-up inside a rule.
func TestResolveInputContract_SchemaBeforeRules(t *testing.T) {
	schema := InputSchemaMap{"port": {Type: "integer", Required: true}}
	rules := []ValidateRule{{That: "input.port > 0", Message: "port must be positive"}}

	_, err := ResolveInputContract(schema, rules, map[string]any{"port": "not-a-number"}, ValidateContext{})
	if err == nil {
		t.Fatal("want an error for a non-integer port")
	}
	var fail *ValidateRuleFailure
	if errors.As(err, &fail) {
		t.Fatalf("got a validate-rule failure %v, want the schema error first", err)
	}
	if !strings.Contains(err.Error(), `input $.port`) {
		t.Errorf("error = %q, want it to name $.port", err)
	}
}

// TestResolveInputContract_EvalFailureIsInternal — a predicate that cannot be
// evaluated is a pre-flight malfunction (ErrValidateRuleEval → 5xx), not "the
// operator passed bad input" (422). Schema validation normally rejects such a
// rule at load time; the classification must still hold if one slips through.
func TestResolveInputContract_EvalFailureIsInternal(t *testing.T) {
	rules := []ValidateRule{{That: "input.port", Message: "not a bool predicate"}}

	_, err := ResolveInputContract(nil, rules, map[string]any{"port": 6379}, ValidateContext{})
	if err == nil {
		t.Fatal("want an error for a non-bool predicate")
	}
	if !errors.Is(err, ErrValidateRuleEval) {
		t.Errorf("error = %v, want it to wrap ErrValidateRuleEval", err)
	}
	var fail *ValidateRuleFailure
	if errors.As(err, &fail) {
		t.Errorf("error = %v, want an internal failure, not a rule failure", err)
	}
}

func intPtr(v int) *int { return &v }

// TestResolveInputContract_EmptyStringIsAbsentAtEveryLevel — the documented
// "empty string == not passed" rule (docs/input.md) applies at ANY nesting depth,
// not only at top level. Before NIM-167 nothing validated nested values at all, so
// the asymmetry was invisible; with the destiny render gate enforcing them, a
// nested "" must not be pattern-checked as if it were a real value.
func TestResolveInputContract_EmptyStringIsAbsentAtEveryLevel(t *testing.T) {
	schema := InputSchemaMap{
		"install": {
			Type: "object",
			Properties: map[string]*InputSchema{
				"method":  {Type: "string", Required: true, Enum: []any{"package", "binary"}},
				"version": {Type: "string", Pattern: `^[0-9]+\.[0-9]+\.[0-9]+$`},
				"marker":  {Type: "string", AllowEmpty: true, Pattern: "^ok$"},
			},
		},
	}

	// version is not applicable for method=package; the caller passes "".
	if _, err := ResolveInputContract(schema, nil, map[string]any{
		"install": map[string]any{"method": "package", "version": ""},
	}, ValidateContext{}); err != nil {
		t.Errorf(`nested "" for an optional string must count as absent, got: %v`, err)
	}

	// allow_empty opts back in to real value checks — "" is then a value.
	if _, err := ResolveInputContract(schema, nil, map[string]any{
		"install": map[string]any{"method": "package", "marker": ""},
	}, ValidateContext{}); err == nil {
		t.Error(`allow_empty field: "" is a real value and must be pattern-checked`)
	}

	// A required property is still missing when passed as "".
	if _, err := ResolveInputContract(schema, nil, map[string]any{
		"install": map[string]any{"method": ""},
	}, ValidateContext{}); err == nil {
		t.Error(`required nested property passed as "" must still be reported missing`)
	}
}

// ★ GUARD (ADR-0091 amendment NIM-993, rule 1: a rescue unit's layout-only destiny
// pass renders only the host-invariant inputs). An input left out with WithoutInputs
// gets no default, no `required:` and no value check, and a rule reading it — or
// reading the namespace by a computed key — is skipped; every other rule still holds,
// at its own index.
//
// Mutations: keep a left-out input in the schema → "addr is required"; evaluate a
// `required_when:` reading one → it fails on input.addr; evaluate a `validate:` rule
// reading one, or one reading the namespace by a computed key → it fails on
// input.addr; skip every rule while inputs are left out → the port rule passes.
func TestResolveInputContract_WithoutInputs(t *testing.T) {
	schema := InputSchemaMap{
		"addr":  {Type: "string", Required: true, Pattern: "^10\\."},
		"alias": {Type: "string", Default: "x"},
		"port":  {Type: "integer", Default: 6379},
		"tls":   {Type: "boolean", RequiredWhen: "input.addr.startsWith('10.')"},
	}
	rules := []ValidateRule{
		{That: "input.addr != ''", Message: "addr reads a left-out input"},
		{That: "input[input.alias] != ''", Message: "a computed key might read a left-out input"},
		{That: "input.port < 1000", Message: "port must stay below 1000"},
	}
	merged, err := ResolveInputContract(schema, rules[:2], map[string]any{}, ValidateContext{}, WithoutInputs([]string{"addr"}))
	if err != nil {
		t.Fatalf("ResolveInputContract without addr: %v", err)
	}
	if _, ok := merged["addr"]; ok {
		t.Errorf("merged = %v, want no addr: a left-out input gets nothing, not even a value from the schema", merged)
	}
	if merged["port"] != 6379 {
		t.Errorf("port = %v, want the default 6379", merged["port"])
	}

	_, err = ResolveInputContract(schema, rules, map[string]any{}, ValidateContext{}, WithoutInputs([]string{"addr"}))
	var fail *ValidateRuleFailure
	if !errors.As(err, &fail) || fail.Index != 2 {
		t.Fatalf("error = %v, want validate[2] to fail: a rule reading only rendered inputs still holds", err)
	}

	if _, err := ResolveInputContract(schema, nil, map[string]any{}, ValidateContext{}); err == nil || !strings.Contains(err.Error(), `"addr" is required`) {
		t.Errorf("without the option: error = %v, want addr required", err)
	}
}
