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
id_template: "${input.name}-${input.nope}"
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
// declared produces no id_template diagnostics at all.
func TestIDTemplate_DeclaredRefsClean(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id_template: "${input.name}-${input.project}-redis-${input.service_type}"
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
		if strings.HasPrefix(d.Code, "id_template") {
			t.Fatalf("unexpected diagnostic on a valid template: %+v", d)
		}
	}
}

// TestIDTemplate_LiteralSkeletonTooLong — literal text alone over the 63-char
// ceiling can never produce a valid name, whatever the operator types.
func TestIDTemplate_LiteralSkeletonTooLong(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id_template: "`+strings.Repeat("a", 70)+`-${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "id_template_too_long") {
		t.Fatalf("expected id_template_too_long; diags=%v", diags)
	}
}

// TestIDTemplate_OutsideSandboxRejected — a template referencing a name outside
// `input` is caught at lint time, not at create time.
func TestIDTemplate_OutsideSandboxRejected(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id_template: "${vars.cluster}-${input.name}"
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
id_template: "${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasWarn(diags, "id_template_ignored") {
		t.Fatalf("expected id_template_ignored WARNING; diags=%v", diags)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("id_template on a non-create scenario must not be an ERROR; diags=%v", diags)
	}
}

// TestIDTemplate_ConstantWarns — a template with no block composes the same name
// for every incarnation, so the second create always collides.
func TestIDTemplate_ConstantWarns(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
id_template: "always-the-same"
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
id_template: ""
tasks: []
`)
	if !hasCode(diags, "empty_value") {
		t.Fatalf("expected empty_value; diags=%v", diags)
	}
}

// TestIDTemplate_AbsentIsSilent — the whole feature is opt-in: a scenario
// without the key produces no id_template diagnostics whatsoever.
func TestIDTemplate_AbsentIsSilent(t *testing.T) {
	diags := scenarioWithIDTemplate(t, `name: create
create: true
input:
  name:
    type: string
tasks: []
`)
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "id_template") {
			t.Fatalf("a scenario without id_template must produce no id_template diagnostics: %+v", d)
		}
	}
}
