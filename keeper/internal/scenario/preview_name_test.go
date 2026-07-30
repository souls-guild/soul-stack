package scenario

import (
	"context"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// TestPreviewName_AgreesWithCreate is THE guard of the whole preview feature: on
// one input, what the operator is SHOWN and what the create would MAKE must be the
// same string.
//
// It is not a formality. The name is the immutable primary key with no rename, so
// a preview that drifts by one character does not mislead — it hands the operator a
// different identity than the one they approved, and the only repair is destroy and
// re-create. The two paths differ deliberately in their input gate (the preview
// merges defaults without the required phase), and this is what pins that
// difference to "rejects less", never to "composes differently".
//
// The table walks the axes where a divergent second implementation would show up
// first: a schema default landing in the name, a bool and a number stringified by
// cel-go rules, and a component the operator typed over the default.
func TestPreviewName_AgreesWithCreate(t *testing.T) {
	textual := nameTemplateSnapshot(t, "")
	scalars := scalarNameTemplateSnapshot(t)

	cases := []struct {
		name   string
		loader *fakeCreateLoader
		input  map[string]any
	}{
		{"default fills the unsupplied component", textual, composeInput("cache", "billing", "inv")},
		{"every component supplied", textual, map[string]any{
			"name": "cache", "project": "billing", "subproject": "inv", "service_type": "cluster",
		}},
		// Non-string scalars are where a second evaluator diverges first: cel-go's
		// ConvertToType decides how an int and a bool become name text, and a
		// client-side reimplementation would pick its own rendering.
		{"integer and boolean components", scalars, map[string]any{
			"name": "cache", "shards": 7, "tls": true,
		}},
		{"integer and boolean defaults", scalars, map[string]any{"name": "cache"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preview, err := PreviewName(context.Background(), tc.loader,
				artifact.ServiceRef{Name: "redis"}, "create", tc.input)
			if err != nil {
				t.Fatalf("PreviewName: %v", err)
			}
			if !preview.Composes {
				t.Fatalf("scenario declares name_template — preview must report Composes")
			}
			if !preview.Valid {
				t.Fatalf("preview invalid for input the create accepts: %s", preview.Reason)
			}

			plan, err := ResolveCreatePlan(context.Background(), tc.loader, nil,
				"", artifact.ServiceRef{Name: "redis"}, "create", tc.input, "archon-a")
			if err != nil {
				t.Fatalf("ResolveCreatePlan: %v", err)
			}
			if preview.Name != plan.ComposedName {
				t.Fatalf("preview shows %q, create makes %q — the operator would approve one identity and get another",
					preview.Name, plan.ComposedName)
			}
		})
	}
}

// scalarNameTemplateSnapshot writes a create scenario whose name draws on
// NON-string components. Stringifying those is where a second CEL implementation
// would diverge, so the agreement test needs a schema that actually accepts them.
func scalarNameTemplateSnapshot(t *testing.T) *fakeCreateLoader {
	t.Helper()
	root := t.TempDir()
	writeScenarioFile(t, root, "create", `name: create
create: true
name_template: "${input.name}-${input.shards}-tls${input.tls}"
input:
  name:
    type: string
  shards:
    type: integer
    default: 3
  tls:
    type: boolean
    default: false
tasks: []
`)
	return &fakeCreateLoader{localDir: root}
}

// TestPreviewName_IncompleteInputIsPreviewedNotRejected — the operator is still
// typing, which is the NORMAL state of a live preview. A missing required component
// must come back as an explained non-result, not as the error the create path
// rightly returns for the same input: a form that 422s on every keystroke has no
// preview at all.
func TestPreviewName_IncompleteInputIsPreviewedNotRejected(t *testing.T) {
	loader := nameTemplateSnapshot(t, "")

	preview, err := PreviewName(context.Background(), loader,
		artifact.ServiceRef{Name: "redis"}, "create", map[string]any{"name": "cache"})
	if err != nil {
		t.Fatalf("an unfinished input is not an infrastructure failure: %v", err)
	}
	if preview.Valid {
		t.Fatalf("preview must not claim a name while components are missing, got %q", preview.Name)
	}
	if preview.Reason == "" {
		t.Fatal("a blank preview with no reason is the exact failure this endpoint removes")
	}
	if !strings.Contains(preview.Reason, "project") {
		t.Errorf("reason must name the component that is missing; got %q", preview.Reason)
	}
}

// TestPreviewName_OverCeilingShowsTheOffendingName — the 63-character ceiling is
// the failure this feature exists to pre-empt, so the preview has to show WHAT
// overflowed. Returning valid=false with an empty name would leave the operator
// with a blank box and four fields to guess between; the character count is the
// whole point.
func TestPreviewName_OverCeilingShowsTheOffendingName(t *testing.T) {
	long := strings.Repeat("x", 30)

	preview, err := PreviewName(context.Background(), nameTemplateSnapshot(t, ""),
		artifact.ServiceRef{Name: "redis"}, "create", composeInput(long, long, long))
	if err != nil {
		t.Fatalf("PreviewName: %v", err)
	}
	if preview.Valid {
		t.Fatal("a name over the ceiling must not preview as valid")
	}
	if len(preview.Name) <= 63 {
		t.Fatalf("the over-long name must come back whole (not truncated, not blank), got %q", preview.Name)
	}
	if !strings.Contains(preview.Reason, "63") {
		t.Errorf("reason must state the ceiling; got %q", preview.Reason)
	}
}

// TestPreviewName_NoTemplateIsNotAnError — a scenario without `name_template`
// keeps the free-text name field. The form asks the same endpoint either way, so
// "this one does not compose" has to be an answer rather than a failure.
func TestPreviewName_NoTemplateIsNotAnError(t *testing.T) {
	root := t.TempDir()
	writeScenarioFile(t, root, "create", `name: create
create: true
input:
  size:
    type: string
tasks: []
`)

	preview, err := PreviewName(context.Background(), &fakeCreateLoader{localDir: root},
		artifact.ServiceRef{Name: "redis"}, "create", map[string]any{})
	if err != nil {
		t.Fatalf("PreviewName: %v", err)
	}
	if preview.Composes || preview.Name != "" {
		t.Fatalf("a scenario without name_template composes nothing, got %+v", preview)
	}
}
