package artifact

// Guards that tie the SHIPPED example to the composition code (ADR-0079): the redis
// service's `create_from_souls` is the corpus's one composing create scenario, and its
// README makes concrete claims about it. `make lint` checks the template's grammar and
// `make trial` renders the scenario, but neither ever composes a name — so a template
// that overflows the 63-character ceiling for ordinary inputs, or a component pattern
// that stopped guarding it, would ship green. These close that gap.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// loadRedisCreateFromSouls loads the example's composing create scenario THROUGH the
// covenant merge (LoadScenarioManifestResolved), which is how the keeper reads it.
func loadRedisCreateFromSouls(t *testing.T) *config.ScenarioManifest {
	t.Helper()
	root := requireRedisExamples(t)
	rel := "scenario/create_from_souls/main.yml"
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Skipf("%s unavailable (%v); guard skipped", rel, err)
	}
	scn, _, diags, err := LoadScenarioManifestResolved(&ServiceArtifact{LocalDir: root}, rel, data, nil)
	if err != nil {
		t.Fatalf("LoadScenarioManifestResolved: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("example scenario is invalid: %v", diagCodes(diags))
	}
	return scn
}

// TestExampleRedis_NameTemplateComposesReadmeName — the example composes exactly the
// name its README advertises, over the RESOLVED input (service_type comes from its
// schema default, not from the request).
func TestExampleRedis_NameTemplateComposesReadmeName(t *testing.T) {
	scn := loadRedisCreateFromSouls(t)
	if scn.NameTemplate == "" {
		t.Fatal("create_from_souls lost its name_template — the corpus no longer covers composition")
	}

	merged, err := config.ResolveInputContract(scn.Input, scn.Validate, map[string]any{
		"name": "cache", "project": "billing", "subproject": "invoices",
		"redis_type": "sentinel", "version": "7.4.1",
	})
	if err != nil {
		t.Fatalf("ResolveInputContract: %v", err)
	}
	got, err := config.RenderNameTemplate(scn.NameTemplate, merged)
	if err != nil {
		t.Fatalf("RenderNameTemplate: %v", err)
	}
	if want := "cache-billing-invoices-redis-cache"; got != want {
		t.Fatalf("composed %q, want %q (README documents this exact name)", got, want)
	}
}

// TestExampleRedis_NameTemplateFitsCeilingAtMaxLength — the load-bearing claim: with
// EVERY component at the maximum its schema permits, the composed name still fits in 63
// characters. Relaxing a max_length or lengthening the literal text without redoing this
// arithmetic would hand operators a 422 they cannot fix without renaming their project.
func TestExampleRedis_NameTemplateFitsCeilingAtMaxLength(t *testing.T) {
	scn := loadRedisCreateFromSouls(t)

	longest := map[string]any{}
	for _, field := range []string{"name", "project", "subproject"} {
		s, ok := scn.Input[field]
		if !ok || s.MaxLength == nil {
			t.Fatalf("input.%s must declare max_length — it is what keeps the composed name legal", field)
		}
		longest[field] = "a" + strings.Repeat("b", *s.MaxLength-1)
	}
	// service_type is an enum: the worst case is its longest member.
	svc, ok := scn.Input["service_type"]
	if !ok || len(svc.Enum) == 0 {
		t.Fatal("input.service_type must be an enum — an unbounded trailing segment breaks the ceiling arithmetic")
	}
	worst := ""
	for _, v := range svc.Enum {
		if s, isStr := v.(string); isStr && len(s) > len(worst) {
			worst = s
		}
	}
	longest["service_type"] = worst

	got, err := config.RenderNameTemplate(scn.NameTemplate, longest)
	if err != nil {
		t.Fatalf("RenderNameTemplate: %v", err)
	}
	if len(got) > config.IncarnationNameMaxLen {
		t.Fatalf("worst-case composed name is %d characters (ceiling %d): %q — shorten a max_length or the literal text",
			len(got), config.IncarnationNameMaxLen, got)
	}
}

// TestExampleRedis_NameComponentsAreKebabGuarded — every component feeding the name
// carries a pattern. Without one an operator's `My_Project` composes an illegal name and
// is rejected only at create, with the whole template quoted back at them.
func TestExampleRedis_NameComponentsAreKebabGuarded(t *testing.T) {
	scn := loadRedisCreateFromSouls(t)
	refs, err := config.NameTemplateInputRefs(scn.NameTemplate)
	if err != nil {
		t.Fatalf("NameTemplateInputRefs: %v", err)
	}
	for _, ref := range refs {
		s, ok := scn.Input[ref]
		if !ok {
			t.Fatalf("name_template references input.%s, which the merged input does not declare", ref)
		}
		if s.Pattern == "" && len(s.Enum) == 0 {
			t.Errorf("input.%s feeds the incarnation name but is neither pattern-guarded nor an enum", ref)
		}
	}
}
