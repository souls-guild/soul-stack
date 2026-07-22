package config

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestResolveInputValues_PrefillFromStateNotLeaked — GUARD (invariant a):
// `prefill_from_state` does NOT take part in resolving the effective input. A field
// with this key but WITHOUT a default and WITHOUT a passed value is absent from the
// effective input (prefill is an operational UI hint, NOT a create default;
// incarnation.state must not leak into input resolution). Catches the regression
// where "prefill_from_state silently became a default".
func TestResolveInputValues_PrefillFromStateNotLeaked(t *testing.T) {
	schema := schemaFromInput(t, `redis_version:
  type: string
  prefill_from_state: state.redis_version
`)
	// Nothing passed: prefill_from_state doesn't supply a value (unlike default),
	// and required is not raised (the key doesn't make the field required).
	got, err := ResolveInputValues(schema, map[string]any{})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	if _, present := got["redis_version"]; present {
		t.Fatalf("prefill_from_state leaked into the effective input: redis_version=%#v (expected absence)", got["redis_version"])
	}
	if len(got) != 0 {
		t.Fatalf("effective input is not empty: %#v", got)
	}
}

// TestResolveInputValues_PrefillFromStateCoexistsWithDefault — `default` and
// `prefill_from_state` coexist: default stays the create default (in merge),
// prefill_from_state takes no part in resolution (visible only to the form-prefill endpoint).
func TestResolveInputValues_PrefillFromStateCoexistsWithDefault(t *testing.T) {
	schema := schemaFromInput(t, `tls_enabled:
  type: boolean
  default: false
  prefill_from_state: state.tls_enabled
`)
	got, err := ResolveInputValues(schema, map[string]any{})
	if err != nil {
		t.Fatalf("ResolveInputValues: %v", err)
	}
	want := map[string]any{"tls_enabled": false} // default merged; prefill has no effect
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
}

// TestValidatePrefillFromState_Accepts — valid `state.<path>` paths pass schema
// validation with no diagnostics. Applies to any type.
func TestValidatePrefillFromState_Accepts(t *testing.T) {
	cases := []string{
		`f:
  type: string
  prefill_from_state: state.redis_version
`,
		`f:
  type: object
  properties:
    x: { type: string }
  prefill_from_state: state.config.nested_field
`,
		`f:
  type: array
  items: { type: string }
  prefill_from_state: state.redis_users
`,
	}
	for _, c := range cases {
		diags := diagsForInput(t, c)
		for _, d := range diags {
			if d.Code == "input_prefill_from_state_invalid" {
				t.Fatalf("valid prefill_from_state rejected: %s\n---\n%s", d.Message, c)
			}
		}
	}
}

// TestValidatePrefillFromState_Rejects — invalid path forms raise
// input_prefill_from_state_invalid (no state root / empty / broken segment).
func TestValidatePrefillFromState_Rejects(t *testing.T) {
	cases := []string{
		`f:
  type: string
  prefill_from_state: redis_version
`, // no state root.
		`f:
  type: string
  prefill_from_state: state
`, // root only, no segment
		`f:
  type: string
  prefill_from_state: state.
`, // empty segment
		`f:
  type: string
  prefill_from_state: incarnation.state.redis_version
`, // foreign root (not state)
		`f:
  type: string
  prefill_from_state: state.Bad-Segment
`, // not a snake_case segment
	}
	for _, c := range cases {
		if !hasDiagCode(diagsForInput(t, c), "input_prefill_from_state_invalid") {
			t.Fatalf("invalid prefill_from_state NOT rejected:\n---\n%s", c)
		}
	}
}

// TestPrefillFromState_KnownKey — `prefill_from_state` doesn't raise unknown_key
// (registered in inputSchemaKnownKeys).
func TestPrefillFromState_KnownKey(t *testing.T) {
	c := `f:
  type: string
  prefill_from_state: state.x
`
	for _, d := range diagsForInput(t, c) {
		if d.Code == "unknown_key" {
			t.Fatalf("prefill_from_state raised unknown_key: %s", d.Message)
		}
	}
}

// diagsForInput parses a scenario with an input block and returns ALL diagnostics
// (including errors — the test decides what to expect). Unlike schemaFromInput,
// which fails on any error diagnostic.
func diagsForInput(t *testing.T, inputYAML string) []diag.Diagnostic {
	t.Helper()
	body := "name: t\ndescription: d\nstate_changes: {}\ntasks: []\ninput:\n" + indentBlock(inputYAML, "  ")
	_, _, diags, err := LoadScenarioManifestFromBytes("t.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v\n---\n%s", err, body)
	}
	return diags
}

func hasDiagCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}
