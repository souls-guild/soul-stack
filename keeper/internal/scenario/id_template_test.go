package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

// idTemplateSnapshot writes a create scenario whose id is composed from four
// input components (the ADR-0079 shape from NIM-177) and returns a loader over it.
//
// key is the spelling to write the template under — `id_template`, or the retired
// `name_template` of the ADR-0085 compatibility window. Every guard below runs on
// both, which is what makes "the old spelling still composes" a test rather than
// a claim.
func idTemplateSnapshot(t *testing.T, extra string) *fakeCreateLoader {
	t.Helper()
	return idTemplateSnapshotKeyed(t, "id_template", extra)
}

func idTemplateSnapshotKeyed(t *testing.T, key, extra string) *fakeCreateLoader {
	t.Helper()
	root := t.TempDir()
	writeScenarioFile(t, root, "create", `name: create
create: true
`+key+`: "${input.name}-${input.project}-${input.subproject}-redis-${input.service_type}"
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

// TestResolveCreatePlan_ComposesID is the end-to-end guard of the feature at the
// shared helper both surfaces go through: the id comes back composed, and it is
// built over the RESOLVED input (service_type is not supplied — its schema default
// `sentinel` lands in the id).
func TestResolveCreatePlan_ComposesID(t *testing.T) {
	plan, err := ResolveCreatePlan(context.Background(), idTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a")
	if err != nil {
		t.Fatalf("ResolveCreatePlan: %v", err)
	}
	if want := "cache-billing-inv-redis-sentinel"; plan.ComposedID != want {
		t.Fatalf("ComposedID = %q, want %q", plan.ComposedID, want)
	}
	if got := plan.EffectiveName(""); got != plan.ComposedID {
		t.Fatalf("EffectiveName should return the composed id, got %q", got)
	}
}

// TestResolveCreatePlan_ComposedIDOverCeiling — the 63-character ceiling is the
// main operational risk of composing ids. It must produce an explicit refusal
// that names the offending string and its length, NOT a silently truncated id
// (the id is the immutable primary key — truncating creates a different identity).
func TestResolveCreatePlan_ComposedIDOverCeiling(t *testing.T) {
	long := strings.Repeat("x", 30)
	_, err := ResolveCreatePlan(context.Background(), idTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput(long, long, long), "archon-a")
	if !errors.Is(err, ErrComposedIDInvalid) {
		t.Fatalf("expected ErrComposedIDInvalid, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"characters", "63", long} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must state what overflowed and by how much; %q missing from %q", want, msg)
		}
	}
}

// TestResolveCreatePlan_ComposedIDNotTruncated pins the anti-truncation
// invariant directly: nothing in the pipeline may hand back a 63-char prefix.
func TestResolveCreatePlan_ComposedIDNotTruncated(t *testing.T) {
	long := strings.Repeat("x", 30)
	plan, err := ResolveCreatePlan(context.Background(), idTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput(long, long, long), "archon-a")
	if err == nil {
		t.Fatalf("an over-long composed id must fail; got ComposedID=%q", plan.ComposedID)
	}
	if plan.ComposedID != "" {
		t.Fatalf("failed composition must not leak a name, got %q", plan.ComposedID)
	}
}

// TestResolveCreatePlan_ComposedIDBadGrammar — components with characters
// outside kebab-case are refused with the pattern in the message, not silently
// sanitized.
func TestResolveCreatePlan_ComposedIDBadGrammar(t *testing.T) {
	_, err := ResolveCreatePlan(context.Background(), idTemplateSnapshot(t, ""), nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("Cache", "billing", "inv"), "archon-a")
	if !errors.Is(err, ErrComposedIDInvalid) {
		t.Fatalf("expected ErrComposedIDInvalid for an uppercase component, got %v", err)
	}
}

// TestResolveCreatePlan_ExplicitIDRejected — with a template the id is derived,
// not negotiated. Accepting both would let the RBAC `incarnation=<id>` dimension
// be checked against one id while a different one is inserted.
func TestResolveCreatePlan_ExplicitIDRejected(t *testing.T) {
	_, err := ResolveCreatePlan(context.Background(), idTemplateSnapshot(t, ""), nil,
		"my-own-name", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a")
	if !errors.Is(err, ErrIDNotComposable) {
		t.Fatalf("expected ErrIDNotComposable when both an explicit id and id_template are present, got %v", err)
	}
}

// TestResolveCreatePlan_NoTemplateUnchanged is the back-compat guard: a create
// scenario without the key behaves exactly as before — nothing is composed and the
// operator's id stands.
func TestResolveCreatePlan_NoTemplateUnchanged(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", "name: create\ncreate: true\ntasks: []\n")

	plan, err := ResolveCreatePlan(context.Background(), &fakeCreateLoader{localDir: root}, nil,
		"legacy-name", artifact.ServiceRef{Name: "svc"}, "create", nil, "archon-a")
	if err != nil {
		t.Fatalf("ResolveCreatePlan: %v", err)
	}
	if plan.ComposedID != "" {
		t.Fatalf("a scenario without a template must compose nothing, got %q", plan.ComposedID)
	}
	if got := plan.EffectiveName("legacy-name"); got != "legacy-name" {
		t.Fatalf("EffectiveName = %q, want the operator-supplied id", got)
	}
}

// TestResolveCreatePlan_TemplateRenderFailure — a component that cannot be part of
// an id (a list) surfaces as the render sentinel, which both handlers map to 422
// rather than 500.
func TestResolveCreatePlan_TemplateRenderFailure(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", `name: create
create: true
id_template: "svc-${input.nodes}"
input:
  nodes:
    type: array
    items:
      type: string
tasks: []
`)
	_, err := ResolveCreatePlan(context.Background(), &fakeCreateLoader{localDir: root}, nil,
		"", artifact.ServiceRef{Name: "svc"}, "create", map[string]any{"nodes": []any{"a", "b"}}, "archon-a")
	if !errors.Is(err, config.ErrIDTemplateRender) {
		t.Fatalf("expected config.ErrIDTemplateRender, got %v", err)
	}
}

// TestResolveCreatePlan_InputGateRunsFirst — input validation still precedes id
// composition: a missing required component must report the input problem, not a
// confusing "composed id is invalid".
func TestResolveCreatePlan_InputGateRunsFirst(t *testing.T) {
	loader := idTemplateSnapshot(t, `validate:
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
	plan, err := ResolveCreatePlan(context.Background(), idTemplateSnapshot(t, ""), rec,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a")
	if err != nil {
		t.Fatalf("ResolveCreatePlan: %v", err)
	}
	if rec.spec.IncarnationName != plan.ComposedID {
		t.Fatalf("pre-flight saw %q, want the composed %q", rec.spec.IncarnationName, plan.ComposedID)
	}
}

// TestResolveCreatePlan_LegacyKeyStillComposes is the create-path half of the
// [ADR-0085] compatibility window (NIM-730): a service repository still writing
// `name_template:` composes exactly what the migrated spelling composes, over the
// same resolved input, and refuses an explicit id for the same reason.
//
// The window is not a lint concession — the key is READ, or every unmigrated
// service stops being creatable the day this lands. That is what this pins, and
// nothing above it does: every other guard here writes the new spelling.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: delete the
// `m.IDTemplate = m.LegacyNameTemplate` line from
// config.ScenarioManifest.normalizeIDTemplate. Nothing stops compiling, the
// warning still fires, and this test goes red on an empty ComposedID — which is
// what an operator would have seen as "this create no longer composes anything".
func TestResolveCreatePlan_LegacyKeyStillComposes(t *testing.T) {
	loader := idTemplateSnapshotKeyed(t, "name_template", "")
	plan, err := ResolveCreatePlan(context.Background(), loader, nil,
		"", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a")
	if err != nil {
		t.Fatalf("ResolveCreatePlan over the retired key: %v", err)
	}
	if want := "cache-billing-inv-redis-sentinel"; plan.ComposedID != want {
		t.Fatalf("ComposedID = %q, want %q — the retired key must compose while the window is open",
			plan.ComposedID, want)
	}

	if _, err := ResolveCreatePlan(context.Background(), idTemplateSnapshotKeyed(t, "name_template", ""), nil,
		"my-own-id", artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"), "archon-a"); !errors.Is(err, ErrIDNotComposable) {
		t.Fatalf("the retired key must refuse an explicit id exactly as the new one does, got %v", err)
	}
}

// TestPreviewID_LegacyKeyPreviews — the live preview reads the same manifest
// field, so a form over an unmigrated service must still show the id it would
// create rather than falling back to "type one yourself".
func TestPreviewID_LegacyKeyPreviews(t *testing.T) {
	preview, err := PreviewID(context.Background(), idTemplateSnapshotKeyed(t, "name_template", ""),
		artifact.ServiceRef{Name: "redis"}, "create", composeInput("cache", "billing", "inv"))
	if err != nil {
		t.Fatalf("PreviewID: %v", err)
	}
	if !preview.Composes {
		t.Fatal("Composes = false over the retired key — the form would show an id field for a create that refuses one")
	}
	if want := "cache-billing-inv-redis-sentinel"; preview.ID != want {
		t.Fatalf("ID = %q, want %q", preview.ID, want)
	}
}
