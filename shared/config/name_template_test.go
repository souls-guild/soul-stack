package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestRenderNameTemplate_Composes is the primary guard of ADR-0079: the template
// from the ticket assembles the expected name out of separate input components.
func TestRenderNameTemplate_Composes(t *testing.T) {
	got, err := RenderNameTemplate(
		"${input.name}-${input.project}-${input.subproject}-redis-${input.service_type}",
		map[string]any{
			"name":         "cache",
			"project":      "billing",
			"subproject":   "invoices",
			"service_type": "sentinel",
		})
	if err != nil {
		t.Fatalf("RenderNameTemplate: %v", err)
	}
	if want := "cache-billing-invoices-redis-sentinel"; got != want {
		t.Fatalf("composed %q, want %q", got, want)
	}
}

// TestRenderNameTemplate_NonStringComponents proves scalars other than strings
// still compose (an int shard number, a bool flag): a name is a string, so every
// block is stringified rather than returned natively (unlike ADR-010 §5(a)).
func TestRenderNameTemplate_NonStringComponents(t *testing.T) {
	got, err := RenderNameTemplate("svc-${input.shard}-${input.tls}", map[string]any{
		"shard": 7,
		"tls":   true,
	})
	if err != nil {
		t.Fatalf("RenderNameTemplate: %v", err)
	}
	if want := "svc-7-true"; got != want {
		t.Fatalf("composed %q, want %q", got, want)
	}
}

// TestRenderNameTemplate_ListComponentRejected — a list/map has no meaningful
// string form inside a name; rendering must fail loudly instead of emitting
// something like `[a b]`.
func TestRenderNameTemplate_ListComponentRejected(t *testing.T) {
	_, err := RenderNameTemplate("svc-${input.nodes}", map[string]any{"nodes": []any{"a", "b"}})
	if !errors.Is(err, ErrNameTemplateRender) {
		t.Fatalf("expected ErrNameTemplateRender for a list component, got %v", err)
	}
}

// TestRenderNameTemplate_InputOnlySandbox proves the sandbox barrier is
// structural: a template reaching for vars/soulprint/vault does not compile,
// because those names are simply undeclared in the env (same as required_when).
func TestRenderNameTemplate_InputOnlySandbox(t *testing.T) {
	for _, tmpl := range []string{
		"${vars.cluster}",
		"${soulprint.self.hostname}",
		"${vault('secret/x').y}",
		"${register.probe.stdout}",
	} {
		if _, err := RenderNameTemplate(tmpl, map[string]any{}); !errors.Is(err, ErrNameTemplateRender) {
			t.Errorf("template %q must not render outside the input sandbox, got err=%v", tmpl, err)
		}
	}
}

// TestRenderNameTemplate_EscapedMarker — `\${` stays literal text (ADR-010 §9.1).
func TestRenderNameTemplate_EscapedMarker(t *testing.T) {
	got, err := RenderNameTemplate(`\${literal}-${input.name}`, map[string]any{"name": "x"})
	if err != nil {
		t.Fatalf("RenderNameTemplate: %v", err)
	}
	if want := "${literal}-x"; got != want {
		t.Fatalf("composed %q, want %q", got, want)
	}
}

// TestNameTemplateInputRefs collects referenced components AST-wise: text outside
// a block and a CEL string literal are NOT references.
func TestNameTemplateInputRefs(t *testing.T) {
	refs, err := NameTemplateInputRefs(`input.decoy-${input.a}-${"input.also_decoy"}-${input.b + input.a}`)
	if err != nil {
		t.Fatalf("NameTemplateInputRefs: %v", err)
	}
	if strings.Join(refs, ",") != "a,b" {
		t.Fatalf("refs = %v, want [a b]", refs)
	}
}

// TestNameTemplateInputRefs_IndexFormRejected — the index form hides the component
// name from static analysis, so it is rejected deterministically (mirror of
// shared/cel.ErrVarIndexForm for vars).
func TestNameTemplateInputRefs_IndexFormRejected(t *testing.T) {
	if _, err := NameTemplateInputRefs(`${input['project']}`); !errors.Is(err, ErrNameTemplateIndexForm) {
		t.Fatalf("expected ErrNameTemplateIndexForm, got %v", err)
	}
}

// scenarioWithNameTemplate parses a scenario manifest and returns its diagnostics.
func scenarioWithNameTemplate(t *testing.T, body string) []diag.Diagnostic {
	t.Helper()
	_, _, diags, err := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	return diags
}

// TestNameTemplate_UnknownInputRef is the lint guard the ticket asks for: a
// `${input.X}` with X undeclared in `input:` is an ERROR — the create would fail
// for every operator, so it must never reach a service repo.
func TestNameTemplate_UnknownInputRef(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: create
create: true
name_template: "${input.name}-${input.nope}"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "name_template_input_unknown") {
		t.Fatalf("expected name_template_input_unknown; diags=%v", diags)
	}
	if !diag.HasErrors(diags) {
		t.Fatal("an undeclared component reference must be an ERROR, not a warning")
	}
}

// TestNameTemplate_DeclaredRefsClean — the same template with every component
// declared produces no name_template diagnostics at all.
func TestNameTemplate_DeclaredRefsClean(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: create
create: true
name_template: "${input.name}-${input.project}-redis-${input.service_type}"
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
		if strings.HasPrefix(d.Code, "name_template") {
			t.Fatalf("unexpected diagnostic on a valid template: %+v", d)
		}
	}
}

// TestNameTemplate_LiteralSkeletonTooLong — literal text alone over the 63-char
// ceiling can never produce a valid name, whatever the operator types.
func TestNameTemplate_LiteralSkeletonTooLong(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: create
create: true
name_template: "`+strings.Repeat("a", 70)+`-${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "name_template_too_long") {
		t.Fatalf("expected name_template_too_long; diags=%v", diags)
	}
}

// TestNameTemplate_OutsideSandboxRejected — a template referencing a name outside
// `input` is caught at lint time, not at create time.
func TestNameTemplate_OutsideSandboxRejected(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: create
create: true
name_template: "${vars.cluster}-${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasCode(diags, "name_template_invalid") {
		t.Fatalf("expected name_template_invalid; diags=%v", diags)
	}
}

// TestNameTemplate_NotACreateScenario — the key is only read on the create path;
// on an operational scenario it is dead config → WARNING (not an error: it breaks
// nothing, it just does nothing).
func TestNameTemplate_NotACreateScenario(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: add_user
name_template: "${input.name}"
input:
  name:
    type: string
tasks: []
`)
	if !hasWarn(diags, "name_template_ignored") {
		t.Fatalf("expected name_template_ignored WARNING; diags=%v", diags)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("name_template on a non-create scenario must not be an ERROR; diags=%v", diags)
	}
}

// TestNameTemplate_ConstantWarns — a template with no block composes the same name
// for every incarnation, so the second create always collides.
func TestNameTemplate_ConstantWarns(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: create
create: true
name_template: "always-the-same"
tasks: []
`)
	if !hasWarn(diags, "name_template_constant") {
		t.Fatalf("expected name_template_constant WARNING; diags=%v", diags)
	}
}

// TestNameTemplate_Empty — an empty value is a footgun (silently never composes),
// rejected explicitly like an empty required_when/validate.that.
func TestNameTemplate_Empty(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: create
create: true
name_template: ""
tasks: []
`)
	if !hasCode(diags, "empty_value") {
		t.Fatalf("expected empty_value; diags=%v", diags)
	}
}

// TestNameTemplate_AbsentIsSilent — the whole feature is opt-in: a scenario
// without the key produces no name_template diagnostics whatsoever.
func TestNameTemplate_AbsentIsSilent(t *testing.T) {
	diags := scenarioWithNameTemplate(t, `name: create
create: true
input:
  name:
    type: string
tasks: []
`)
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "name_template") {
			t.Fatalf("a scenario without name_template must produce no name_template diagnostics: %+v", d)
		}
	}
}
