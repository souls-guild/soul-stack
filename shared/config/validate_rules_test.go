package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestLoadScenarioManifest_ValidateBlock — a valid top-level `validate:` section
// (ADR-009 amendment, DSL wave 2): a list of {that, message} rules parses into
// ScenarioManifest.Validate.
func TestLoadScenarioManifest_ValidateBlock(t *testing.T) {
	src := `name: x
input:
  tls: { type: boolean, default: false }
  port: { type: integer, default: 0 }
validate:
  - that: "input.tls || input.port > 0"
    message: "either enable tls or set a positive port"
  - that: "input.port < 65536"
    message: "port must be below 65536"
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors on valid validate block")
	}
	if len(cfg.Validate) != 2 {
		t.Fatalf("Validate = %#v, want 2 rules", cfg.Validate)
	}
	if cfg.Validate[0].That != "input.tls || input.port > 0" {
		t.Errorf("Validate[0].That = %q", cfg.Validate[0].That)
	}
	if cfg.Validate[0].Message != "either enable tls or set a positive port" {
		t.Errorf("Validate[0].Message = %q", cfg.Validate[0].Message)
	}
}

// TestLoadScenarioManifest_ValidateEmpty — an empty `validate: []` → empty_value
// (a rule-block with no rules misleads the author, rejected explicitly).
func TestLoadScenarioManifest_ValidateEmpty(t *testing.T) {
	src := `name: x
validate: []
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "empty_value") {
		dump(t, diags)
		t.Fatalf("expected empty_value on validate: []")
	}
}

// TestLoadScenarioManifest_ValidateMissingThat — a rule without that → required.
func TestLoadScenarioManifest_ValidateMissingThat(t *testing.T) {
	src := `name: x
validate:
  - message: "no predicate"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.validate[0].that") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on validate[0].that")
	}
}

// TestLoadScenarioManifest_ValidateMissingMessage — a rule without message → required
// (without message a 422 failure is anonymous; unlike assert.message, here it's required).
func TestLoadScenarioManifest_ValidateMissingMessage(t *testing.T) {
	src := `name: x
validate:
  - that: "input.port > 0"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.validate[0].message") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on validate[0].message")
	}
}

// TestLoadScenarioManifest_ValidateBrokenCEL — a syntactically broken that →
// validate_rule_invalid (CEL doesn't compile).
func TestLoadScenarioManifest_ValidateBrokenCEL(t *testing.T) {
	src := `name: x
validate:
  - that: "input.port >>> 0"
    message: "bad"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "validate_rule_invalid") {
		dump(t, diags)
		t.Fatalf("expected validate_rule_invalid on broken CEL in that")
	}
}

// TestLoadScenarioManifest_ValidateInputOnlyBarrier — a structural input-only
// barrier: an essence/soulprint/register reference in that → validate_rule_invalid
// (undeclared reference), NOT a value. The barrier comes from inputEnv's
// undeclaration, not a text guard.
func TestLoadScenarioManifest_ValidateInputOnlyBarrier(t *testing.T) {
	forbidden := []string{
		`soulprint.self.os.family == 'debian'`,
		`essence.redis_port > 0`,
		`register.probe.changed`,
		`size(soulprint.hosts) == 3`,
	}
	for _, expr := range forbidden {
		src := "name: x\nvalidate:\n  - that: \"" + expr + "\"\n    message: \"m\"\ntasks: []\n"
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if !hasCode(diags, "validate_rule_invalid") {
			dump(t, diags)
			t.Fatalf("expected validate_rule_invalid for non-input reference %q", expr)
		}
	}
}

// TestLoadScenarioManifest_ValidateUnknownKey — an extra key inside a rule → unknown_key.
func TestLoadScenarioManifest_ValidateUnknownKey(t *testing.T) {
	src := `name: x
validate:
  - that: "input.port > 0"
    message: "m"
    severity: error
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_key", "$.validate[0].severity") {
		dump(t, diags)
		t.Fatalf("expected unknown_key on validate[0].severity")
	}
}

// TestLoadScenarioManifest_ValidateAndAssertCoexist — validate: and assert:
// coexist: both parse, no conflict (validate is top-level, assert is per-task).
func TestLoadScenarioManifest_ValidateAndAssertCoexist(t *testing.T) {
	src := `name: x
input:
  shards: { type: integer, default: 3 }
validate:
  - that: "input.shards >= 1"
    message: "shards must be at least 1"
tasks:
  - name: topology guard
    assert:
      that:
        - "size(soulprint.hosts) == int(input.shards)"
      message: "topology mismatch"
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("validate: and assert: must coexist without error")
	}
	if len(cfg.Validate) != 1 {
		t.Fatalf("Validate = %#v, want 1 rule", cfg.Validate)
	}
	if cfg.Tasks[0].Assert == nil {
		t.Fatalf("assert task not parsed alongside validate")
	}
}

// TestEvalValidateRules_AllPass — all rules true → nil failure, nil err.
func TestEvalValidateRules_AllPass(t *testing.T) {
	rules := []ValidateRule{
		{That: "input.port > 0", Message: "port positive"},
		{That: "input.port < 65536", Message: "port in range"},
	}
	fail, err := EvalValidateRules(rules, map[string]any{"port": 6379})
	if err != nil {
		t.Fatalf("eval err: %v", err)
	}
	if fail != nil {
		t.Fatalf("expected no failure, got %+v", fail)
	}
}

// TestEvalValidateRules_FirstFalseWins — the first false wins: the FIRST violated
// rule is returned with its message (short-circuit in order).
func TestEvalValidateRules_FirstFalseWins(t *testing.T) {
	rules := []ValidateRule{
		{That: "input.port > 0", Message: "first rule"},
		{That: "input.name != ''", Message: "second rule"},
	}
	// port=0 fails the first; name is also empty (would fail the second) — but the first wins.
	fail, err := EvalValidateRules(rules, map[string]any{"port": 0, "name": ""})
	if err != nil {
		t.Fatalf("eval err: %v", err)
	}
	if fail == nil {
		t.Fatalf("expected failure on first rule")
	}
	if fail.Index != 0 || fail.Message != "first rule" {
		t.Fatalf("expected first rule failure, got %+v", fail)
	}
	if !strings.Contains(fail.Error(), "first rule") {
		t.Errorf("Error() = %q, want it to carry message", fail.Error())
	}
}

// TestEvalValidateRules_CrossField — a cross-field invariant (the reason validate:
// exists): "port is required if tls is off".
func TestEvalValidateRules_CrossField(t *testing.T) {
	rules := []ValidateRule{
		{That: "input.tls || input.port > 0", Message: "set port or enable tls"},
	}
	// tls=false, port=0 → rule false.
	fail, err := EvalValidateRules(rules, map[string]any{"tls": false, "port": 0})
	if err != nil {
		t.Fatalf("eval err: %v", err)
	}
	if fail == nil {
		t.Fatalf("expected cross-field failure")
	}
	// tls=true covers the missing port → passes.
	fail, err = EvalValidateRules(rules, map[string]any{"tls": true, "port": 0})
	if err != nil {
		t.Fatalf("eval err: %v", err)
	}
	if fail != nil {
		t.Fatalf("expected pass when tls enabled, got %+v", fail)
	}
}

// TestEvalValidateRules_NonBool — that evaluated to non-bool → internal error (not
// "passed"/"failed"): the caller maps it to 500, not 422.
func TestEvalValidateRules_NonBool(t *testing.T) {
	rules := []ValidateRule{{That: "input.port", Message: "m"}}
	_, err := EvalValidateRules(rules, map[string]any{"port": 6379})
	if err == nil {
		t.Fatalf("expected error for non-bool predicate")
	}
}

// TestEvalValidateRules_Empty — empty list / nil merged → no-op.
func TestEvalValidateRules_Empty(t *testing.T) {
	fail, err := EvalValidateRules(nil, nil)
	if err != nil || fail != nil {
		t.Fatalf("empty rules must be no-op, got fail=%+v err=%v", fail, err)
	}
}
