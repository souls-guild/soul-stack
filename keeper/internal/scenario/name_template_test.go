package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// nameTemplateSnapshot writes a create scenario whose name is composed from four
// input components (the ADR-0079 shape from NIM-177) and returns a loader over it.
func nameTemplateSnapshot(t *testing.T, extra string) *fakeCreateLoader {
	t.Helper()
	root := t.TempDir()
	writeScenarioFile(t, root, "create", `name: create
create: true
name_template: "${input.name}-${input.project}-${input.subproject}-redis-${input.service_type}"
input:
  name:
    type: string
  project:
    type: string
  subproject:
    type: string
  service_type:
    type: string
    default: sentinel
`+extra+`tasks: []
`)
	return &fakeCreateLoader{localDir: root}
}

func composeInput(name, project, subproject string) map[string]any {
	return map[string]any{"name": name, "project": project, "subproject": subproject}
}

// TestResolveCreatePlan_ComposesName is the end-to-end guard of the feature at the
// shared helper both surfaces go through: the name comes back composed, and it is
// built over the RESOLVED input (service_type is not supplied — its schema default
// `sentinel` lands in the name).
func TestResolveCreatePlan_ComposesName(t *testing.T) {
	plan, err := ResolveCreatePlan(context.Background(), nameTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a")
	if err != nil {
		t.Fatalf("ResolveCreatePlan: %v", err)
	}
	if want := "cache-billing-inv-redis-sentinel"; plan.ComposedName != want {
		t.Fatalf("ComposedName = %q, want %q", plan.ComposedName, want)
	}
	if got := plan.EffectiveName(""); got != plan.ComposedName {
		t.Fatalf("EffectiveName should return the composed name, got %q", got)
	}
}

// TestResolveCreatePlan_ComposedNameOverCeiling — the 63-character ceiling is the
// main operational risk of composing names. It must produce an explicit refusal
// that names the offending string and its length, NOT a silently truncated name
// (the name is the immutable primary key — truncating creates a different identity).
func TestResolveCreatePlan_ComposedNameOverCeiling(t *testing.T) {
	long := strings.Repeat("x", 30)
	_, err := ResolveCreatePlan(context.Background(), nameTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput(long, long, long), "archon-a")
	if !errors.Is(err, ErrComposedNameInvalid) {
		t.Fatalf("expected ErrComposedNameInvalid, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"characters", "63", long} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must state what overflowed and by how much; %q missing from %q", want, msg)
		}
	}
}

// TestResolveCreatePlan_ComposedNameNotTruncated pins the anti-truncation
// invariant directly: nothing in the pipeline may hand back a 63-char prefix.
func TestResolveCreatePlan_ComposedNameNotTruncated(t *testing.T) {
	long := strings.Repeat("x", 30)
	plan, err := ResolveCreatePlan(context.Background(), nameTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput(long, long, long), "archon-a")
	if err == nil {
		t.Fatalf("an over-long composed name must fail; got ComposedName=%q", plan.ComposedName)
	}
	if plan.ComposedName != "" {
		t.Fatalf("failed composition must not leak a name, got %q", plan.ComposedName)
	}
}

// TestResolveCreatePlan_ComposedNameBadGrammar — components with characters
// outside kebab-case are refused with the pattern in the message, not silently
// sanitized.
func TestResolveCreatePlan_ComposedNameBadGrammar(t *testing.T) {
	_, err := ResolveCreatePlan(context.Background(), nameTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("Cache", "billing", "inv"), "archon-a")
	if !errors.Is(err, ErrComposedNameInvalid) {
		t.Fatalf("expected ErrComposedNameInvalid for an uppercase component, got %v", err)
	}
}

// TestResolveCreatePlan_ExplicitNameRejected — with a template the name is derived,
// not negotiated. Accepting both would let the RBAC `incarnation=<name>` dimension
// be checked against one name while a different one is inserted.
func TestResolveCreatePlan_ExplicitNameRejected(t *testing.T) {
	_, err := ResolveCreatePlan(context.Background(), nameTemplateSnapshot(t, ""), nil,
		"my-own-name", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a")
	if !errors.Is(err, ErrNameNotComposable) {
		t.Fatalf("expected ErrNameNotComposable when both name and name_template are present, got %v", err)
	}
}

// TestResolveCreatePlan_NoTemplateUnchanged is the back-compat guard: a create
// scenario without the key behaves exactly as before — nothing is composed and the
// operator's name stands.
func TestResolveCreatePlan_NoTemplateUnchanged(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")

	plan, err := ResolveCreatePlan(context.Background(), &fakeCreateLoader{localDir: root}, nil,
		"legacy-name", artifact.ServiceRef{Name: "svc"}, "create", nil, "archon-a")
	if err != nil {
		t.Fatalf("ResolveCreatePlan: %v", err)
	}
	if plan.ComposedName != "" {
		t.Fatalf("a scenario without name_template must compose nothing, got %q", plan.ComposedName)
	}
	if got := plan.EffectiveName("legacy-name"); got != "legacy-name" {
		t.Fatalf("EffectiveName = %q, want the operator-supplied name", got)
	}
}

// TestResolveCreatePlan_TemplateRenderFailure — a component that cannot be part of
// a name (a list) surfaces as the render sentinel, which both handlers map to 422
// rather than 500.
func TestResolveCreatePlan_TemplateRenderFailure(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", `name: create
create: true
name_template: "svc-${input.nodes}"
input:
  nodes:
    type: array
    items:
      type: string
tasks: []
`)
	_, err := ResolveCreatePlan(context.Background(), &fakeCreateLoader{localDir: root}, nil,
		"", artifact.ServiceRef{Name: "svc"}, "create", map[string]any{"nodes": []any{"a", "b"}}, "archon-a")
	if !errors.Is(err, config.ErrNameTemplateRender) {
		t.Fatalf("expected config.ErrNameTemplateRender, got %v", err)
	}
}

// TestResolveCreatePlan_InputGateRunsFirst — input validation still precedes name
// composition: a missing required component must report the input problem, not a
// confusing "composed name is invalid".
func TestResolveCreatePlan_InputGateRunsFirst(t *testing.T) {
	loader := nameTemplateSnapshot(t, `validate:
  - that: "input.project != 'forbidden'"
    message: project forbidden
`)
	_, err := ResolveCreatePlan(context.Background(), loader, nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "forbidden", "inv"), "archon-a")
	if !errors.Is(err, ErrValidateFailed) {
		t.Fatalf("expected the input gate to fail first with ErrValidateFailed, got %v", err)
	}
}

// preflightRecorder captures the RunSpec the pre-flight assert gate receives.
type preflightRecorder struct{ spec RunSpec }

func (p *preflightRecorder) PreflightAssert(_ context.Context, spec RunSpec) error {
	p.spec = spec
	return nil
}

// TestResolveCreatePlan_PreflightSeesComposedName — composition happens BEFORE the
// assert gate, so `assert:` predicates and the bootstrap run target the real name.
func TestResolveCreatePlan_PreflightSeesComposedName(t *testing.T) {
	rec := &preflightRecorder{}
	plan, err := ResolveCreatePlan(context.Background(), nameTemplateSnapshot(t, ""), rec,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a")
	if err != nil {
		t.Fatalf("ResolveCreatePlan: %v", err)
	}
	if rec.spec.IncarnationName != plan.ComposedName {
		t.Fatalf("pre-flight saw %q, want the composed %q", rec.spec.IncarnationName, plan.ComposedName)
	}
}
