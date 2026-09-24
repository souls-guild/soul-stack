package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestRenderIDTemplate_Composes is the primary guard of ADR-0079: the template
// from the ticket assembles the expected name out of separate input components.
func TestRenderIDTemplate_Composes(t *testing.T) {
	got, err := RenderIDTemplate(
		"${input.name}-${input.project}-${input.subproject}-redis-${input.service_type}",
		map[string]any{
			"name":         "cache",
			"project":      "billing",
			"subproject":   "invoices",
			"service_type": "sentinel",
		})
	if err != nil {
		t.Fatalf("RenderIDTemplate: %v", err)
	}
	if want := "cache-billing-invoices-redis-sentinel"; got != want {
		t.Fatalf("composed %q, want %q", got, want)
	}
}

// TestRenderIDTemplate_NonStringComponents proves scalars other than strings
// still compose (an int shard number, a bool flag): a name is a string, so every
// block is stringified rather than returned natively (unlike ADR-010 §5(a)).
func TestRenderIDTemplate_NonStringComponents(t *testing.T) {
	got, err := RenderIDTemplate("svc-${input.shard}-${input.tls}", map[string]any{
		"shard": 7,
		"tls":   true,
	})
	if err != nil {
		t.Fatalf("RenderIDTemplate: %v", err)
	}
	if want := "svc-7-true"; got != want {
		t.Fatalf("composed %q, want %q", got, want)
	}
}

// TestRenderIDTemplate_ListComponentRejected — a list/map has no meaningful
// string form inside a name; rendering must fail loudly instead of emitting
// something like `[a b]`.
func TestRenderIDTemplate_ListComponentRejected(t *testing.T) {
	_, err := RenderIDTemplate("svc-${input.nodes}", map[string]any{"nodes": []any{"a", "b"}})
	if !errors.Is(err, ErrIDTemplateRender) {
		t.Fatalf("expected ErrIDTemplateRender for a list component, got %v", err)
	}
}

// TestRenderIDTemplate_InputOnlySandbox proves the sandbox barrier is
// structural: a template reaching for vars/soulprint/vault does not compile,
// because those names are simply undeclared in the env (same as required_when).
func TestRenderIDTemplate_InputOnlySandbox(t *testing.T) {
	for _, tmpl := range []string{
		"${vars.cluster}",
		"${soulprint.self.hostname}",
		"${vault('secret/x').y}",
		"${register.probe.stdout}",
	} {
		if _, err := RenderIDTemplate(tmpl, map[string]any{}); !errors.Is(err, ErrIDTemplateRender) {
			t.Errorf("template %q must not render outside the input sandbox, got err=%v", tmpl, err)
		}
	}
}

// TestRenderIDTemplate_EscapedMarker — `\${` stays literal text (ADR-010 §9.1).
func TestRenderIDTemplate_EscapedMarker(t *testing.T) {
	got, err := RenderIDTemplate(`\${literal}-${input.name}`, map[string]any{"name": "x"})
	if err != nil {
		t.Fatalf("RenderIDTemplate: %v", err)
	}
	if want := "${literal}-x"; got != want {
		t.Fatalf("composed %q, want %q", got, want)
	}
}

// TestIDTemplateInputRefs collects referenced components AST-wise: text outside
// a block and a CEL string literal are NOT references.
func TestIDTemplateInputRefs(t *testing.T) {
	refs, err := IDTemplateInputRefs(`input.decoy-${input.a}-${"input.also_decoy"}-${input.b + input.a}`)
	if err != nil {
		t.Fatalf("IDTemplateInputRefs: %v", err)
	}
	if strings.Join(refs, ",") != "a,b" {
		t.Fatalf("refs = %v, want [a b]", refs)
	}
}

// TestIDTemplateInputRefs_IndexFormRejected — the index form hides the component
// name from static analysis, so it is rejected deterministically (mirror of
// shared/cel.ErrVarIndexForm for vars).
func TestIDTemplateInputRefs_IndexFormRejected(t *testing.T) {
	if _, err := IDTemplateInputRefs(`${input['project']}`); !errors.Is(err, ErrIDTemplateIndexForm) {
		t.Fatalf("expected ErrIDTemplateIndexForm, got %v", err)
	}
}

// scenarioWithIDTemplate parses a scenario manifest and returns its diagnostics.
func scenarioWithIDTemplate(t *testing.T, body string) []diag.Diagnostic {
	t.Helper()
	_, _, diags, err := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	return diags
}

// TestIDTemplate_UnknownInputRef is the lint guard the ticket asks for: a
// `${input.X}` with X undeclared in `input:` is an ERROR — the create would fail
// for every operator, so it must never reach a service repo.
func TestIDTemplate_UnknownInputRef(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}-${input.nope}"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "id_template_input_unknown") {
		t.Fatalf("expected id_template_input_unknown; diags=%v", diags)
	}
	if !diag.HasErrors(diags) {
		t.Fatal("an undeclared component reference must be an ERROR, not a warning")
	}
}

// TestIDTemplate_DeclaredRefsClean — the same template with every component
// declared, and a bound inside the platform ceiling, produces no id diagnostics at
// all.
func TestIDTemplate_DeclaredRefsClean(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}-${input.project}-redis-${input.service_type}"
  max_length: 50
input:
  name:
    type: string
  project:
    type: string
  service_type:
    type: string
tasks: []
`)
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "id_template") || strings.HasPrefix(d.Code, "id_max_length") {
			t.Fatalf("unexpected diagnostic on a valid id: block: %+v", d)
		}
	}
}

// TestIDTemplate_LiteralSkeletonTooLong — literal text alone over the 63-char
// platform ceiling can never produce a valid id, whatever the operator types.
func TestIDTemplate_LiteralSkeletonTooLong(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "`+strings.Repeat("a", 70)+`-${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "id_template_too_long") {
		t.Fatalf("expected id_template_too_long; diags=%v", diags)
	}
}

// TestIDTemplate_LiteralSkeletonOverServiceCeiling is what declaring the bound BUYS
// statically: a 45-character skeleton is inside the platform's 63 and cannot fit a
// service capping at 40. The same template WITHOUT the bound must stay silent, or the
// rule is firing on the skeleton rather than on the ceiling.
func TestIDTemplate_LiteralSkeletonOverServiceCeiling(t *testing.T) {
	const skeleton = "-redis-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 45 literal chars
	bounded := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}`+skeleton+`"
  max_length: 40
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(bounded, "id_template_too_long") {
		t.Fatalf("a 45-character skeleton under max_length: 40 was accepted; diags=%v", bounded)
	}

	unbounded := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}`+skeleton+`"
input:
  name:
    type: string
tasks: []
`)
	if hasCode(unbounded, "id_template_too_long") {
		t.Fatalf("the same skeleton is inside the platform ceiling and must pass without a bound; diags=%v", unbounded)
	}
}

// TestIDTemplate_MaxLengthOverPlatformCeiling — a bound wider than the grammar narrows
// nothing while reading as though it did. An ERROR rather than a silent clamp: a
// clamped number in a file gets read as true.
func TestIDTemplate_MaxLengthOverPlatformCeiling(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}-redis"
  max_length: 70
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "id_max_length_over_ceiling") {
		t.Fatalf("expected id_max_length_over_ceiling; diags=%v", diags)
	}
	for _, d := range diags {
		if d.Code == "id_max_length_over_ceiling" && d.YAMLPath != "$.id.max_length" {
			t.Errorf("address = %q, want $.id.max_length", d.YAMLPath)
		}
	}
}

// TestIDTemplate_MaxLengthNonPositive — 0 is why the rule reads the AST: in the decoded
// struct it is indistinguishable from an absent key.
func TestIDTemplate_MaxLengthNonPositive(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}-redis"
  max_length: `+value+`
input:
  name:
    type: string
tasks: []
`)
		if !hasCode(diags, "id_max_length_invalid") {
			t.Fatalf("max_length: %s was accepted; diags=%v", value, diags)
		}
	}
}

func TestIDTemplate_MaxLengthAbsentIsSilent(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}-redis"
input:
  name:
    type: string
tasks: []
`)
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "id_max_length") {
			t.Fatalf("unexpected bound diagnostic with no bound written: %+v", d)
		}
	}
}

// TestIDTemplate_OutsideSandboxRejected — a template referencing a name outside
// `input` is caught at lint time, not at create time.
func TestIDTemplate_OutsideSandboxRejected(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${vars.cluster}-${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "id_template_invalid") {
		t.Fatalf("expected id_template_invalid; diags=%v", diags)
	}
}

// TestIDTemplate_NotACreateScenario — the key is only read on the create path;
// on an operational scenario it is dead config → WARNING (not an error: it breaks
// nothing, it just does nothing).
func TestIDTemplate_NotACreateScenario(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: add_user
id:
  template: "${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasWarn(diags, "id_template_ignored") {
		t.Fatalf("expected id_template_ignored WARNING; diags=%v", diags)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("an id: block on a non-create scenario must not be an ERROR; diags=%v", diags)
	}
}

// TestIDTemplate_ConstantWarns — a template with no block composes the same id
// for every incarnation, so the second create always collides.
func TestIDTemplate_ConstantWarns(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "always-the-same"
tasks: []
`)
	if !hasWarn(diags, "id_template_constant") {
		t.Fatalf("expected id_template_constant WARNING; diags=%v", diags)
	}
}

// TestIDTemplate_Empty — an empty value is a footgun (silently never composes),
// rejected explicitly like an empty required_when/validate.that.
func TestIDTemplate_Empty(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: ""
tasks: []
`)
	if !hasCode(diags, "empty_value") {
		t.Fatalf("expected empty_value; diags=%v", diags)
	}
}

// TestIDTemplate_BlockWithoutTemplate — a ceiling on nothing, addressed at the `id:`
// key because the leaf it is about is not in the file.
func TestIDTemplate_BlockWithoutTemplate(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  max_length: 40
tasks: []
`)
	var found bool
	for _, d := range diags {
		if d.Code != "empty_value" {
			continue
		}
		found = true
		if d.Line != 3 {
			t.Errorf("Line = %d, want 3 (the `id:` key) — the leaf is absent, so the block is the address", d.Line)
		}
	}
	if !found {
		t.Fatalf("an id: block with no template was accepted; diags=%v", diags)
	}
}

// TestIDTemplate_AbsentIsSilent — the whole feature is opt-in: a scenario
// without the key produces no id diagnostics whatsoever.
func TestIDTemplate_AbsentIsSilent(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
input:
  name:
    type: string
tasks: []
`)
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "id_template") || strings.HasPrefix(d.Code, "id_max_length") {
			t.Fatalf("a scenario without an id: block must produce no id diagnostics: %+v", d)
		}
	}
}

// TestIDSpec_Ceiling — both wrong directions resolve to the platform ceiling: a
// negative bound read literally would refuse every id, one above 63 would claim room
// the grammar does not have.
func TestIDSpec_Ceiling(t *testing.T) {
	for _, tc := range []struct {
		max  int
		want int
	}{
		{0, IncarnationIDMaxLen},
		{-1, IncarnationIDMaxLen},
		{IncarnationIDMaxLen + 1, IncarnationIDMaxLen},
		{IncarnationIDMaxLen, IncarnationIDMaxLen},
		{1, 1},
		{50, 50},
	} {
		if got := (IDSpec{MaxLength: tc.max}).Ceiling(); got != tc.want {
			t.Errorf("IDSpec{MaxLength: %d}.Ceiling() = %d, want %d", tc.max, got, tc.want)
		}
	}
}

func TestIDSpec_CeilingPhrase(t *testing.T) {
	service := (IDSpec{MaxLength: 50}).CeilingPhrase()
	if !strings.Contains(service, "50") || !strings.Contains(service, "id.max_length") {
		t.Errorf("phrase = %q, want the number and the key that set it", service)
	}
	platform := (IDSpec{}).CeilingPhrase()
	if !strings.Contains(platform, "63") || strings.Contains(platform, "id.max_length") {
		t.Errorf("phrase = %q, want the platform ceiling and no key to go change", platform)
	}
}

// TestIDTemplate_HintIsPastableYAML — the hint is read by someone who has just
// mistyped this construct, so it has to parse. Under the block the template is a
// NESTED key.
func TestIDTemplate_HintIsPastableYAML(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"name: create\ncreate: true\nid:\n  template: \"\"\ntasks: []\n", "id:\n  template: "},
		{"name: create\ncreate: true\nid_template: \"\"\ntasks: []\n", "id_template: "},
	} {
		var hint string
		for _, d := range scenarioWithIDTemplate(t, tc.body) {
			if d.Code == "empty_value" {
				hint = d.Hint
			}
		}
		if !strings.Contains(hint, tc.want) {
			t.Errorf("hint = %q, want the %q shape", hint, tc.want)
		}
	}
}

// TestIDTemplate_ScalarIDIsATypeError is the migration's likeliest slip — renaming
// `id_template:` to `id:` and leaving the value. Read as "block absent" it would
// silently stop composing an id the file plainly declares.
func TestIDTemplate_ScalarIDIsATypeError(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id: "${input.name}-redis"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "type_mismatch") {
		t.Fatalf("a scalar id: was accepted as a block; diags=%v", diags)
	}
	for _, d := range diags {
		if d.Code == "type_mismatch" && d.Line != 3 {
			t.Errorf("Line = %d, want 3 (the id: key)", d.Line)
		}
	}
}

// TestIDTemplate_UnknownSubKeyIsRefused — `max_lenght:` would parse, mean nothing and
// leave the id bounded only by the platform. The reflect walker covers this because of
// the field's TYPE, which is not written down anywhere.
func TestIDTemplate_UnknownSubKeyIsRefused(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id:
  template: "${input.name}-redis"
  max_lenght: 40
input:
  name:
    type: string
tasks: []
`)
	var found bool
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.id.max_lenght" {
			found = true
			if d.Line != 5 {
				t.Errorf("Line = %d, want 5", d.Line)
			}
		}
	}
	if !found {
		t.Fatalf("a typo'd sub-key of id: was silently ignored; diags=%v", diags)
	}
}
