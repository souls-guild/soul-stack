package render

// THE INVARIANT, guarded where the CEL root `incarnation.*` is built
// ([ADR-0085], NIM-728): an incarnation's `label` is not in it. `incarnation.id`
// is the identifier, and `incarnation.label` does not resolve at all.
//
// Why the guard is here and not only in a doc comment: `incarnation` is declared
// `cel.DynType` in all three CEL environments, so a DynType root does not
// type-check its fields. Nothing about this is a compile error — a wrong root is
// a no-such-key at EVALUATION. One of the three environments is flow-control,
// evaluated on the HOST, so an expression reading a caption would fail mid-run,
// after earlier tasks had already applied. There is no static catcher on either
// side of the wire; this test is it.
//
// The second reason is subtler and is why the caption must be absent rather than
// merely unused. A caption is MUTABLE. If `incarnation.label` resolved, a
// scenario could branch on it, and editing a screen caption would silently
// change what a run does on a host — the exact class of surprise the split
// exists to remove.
//
// HOW TO BREAK IT ON PURPOSE (the two mutations this file catches):
//  1. add `Label string` to IncarnationMeta and `m["label"] = in.Incarnation.Label`
//     to incarnationVars — TestIncarnationLabel_NotACELRoot goes red.
//  2. substitute the caption for the identifier: `"id": in.Incarnation.Label`
//     — TestIncarnationLabel_CELIDIsTheIdentifier goes red.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
)

const (
	guardIncarnationID    = "redis-billing"
	guardIncarnationLabel = "redis-billing-prod" // a VALID identifier too — see below
)

func guardRenderInput() RenderInput {
	return RenderInput{
		Incarnation: IncarnationMeta{
			ID:             guardIncarnationID,
			Service:        "redis",
			ServiceVersion: "v1.2.3",
		},
	}
}

// TestIncarnationLabel_NotACELRoot pins the key set of the `incarnation` CEL root
// as the real builder produces it, and the structure that makes a caption
// unexpressible there.
func TestIncarnationLabel_NotACELRoot(t *testing.T) {
	// (a) IncarnationMeta is the whole vocabulary the render pass has about an
	// incarnation. A caption cannot reach CEL without first being added here, so
	// this assertion is the cheapest place to stop it.
	metaT := reflect.TypeOf(IncarnationMeta{})
	for i := range metaT.NumField() {
		if strings.EqualFold(metaT.Field(i).Name, "label") {
			t.Errorf("IncarnationMeta gained a %q field.\n"+
				"ADR-0085: a caption participates in nothing derived, and `incarnation.*` in CEL "+
				"is one of the four surfaces named. IncarnationMeta is deliberately the identifier "+
				"and its service coordinates — nothing mutable, because a scenario branching on a "+
				"mutable caption turns editing a screen into changing what runs on a host.",
				metaT.Field(i).Name)
		}
	}

	// (b) The map the root is actually built from, through the real function.
	vars := incarnationVars(guardRenderInput(), 3)
	if _, ok := vars["label"]; ok {
		t.Errorf("incarnationVars produced a %q key — `incarnation.label` would resolve in CEL.\n"+
			"ADR-0085: it must not. A caption is mutable; an expression that reads one makes a "+
			"label edit change a run.", "label")
	}
	if got := vars["id"]; got != guardIncarnationID {
		t.Errorf("incarnation.id = %v, want the IDENTIFIER %q", got, guardIncarnationID)
	}
	for k, v := range vars {
		if s, ok := v.(string); ok && s == guardIncarnationLabel {
			t.Errorf("the CAPTION %q reached the CEL root under key %q", guardIncarnationLabel, k)
		}
	}
}

// TestIncarnationLabel_CELIDIsTheIdentifier evaluates through the real engine,
// not the map: what a scenario author writes is `${ incarnation.id }`, and this
// pins what that yields — and that `incarnation.label` is a no-such-key rather
// than an empty string, so a stale expression fails loudly instead of silently
// matching everything.
func TestIncarnationLabel_CELIDIsTheIdentifier(t *testing.T) {
	eng, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	vars := cel.Vars{Incarnation: incarnationVars(guardRenderInput(), 1)}

	got, err := eng.EvalExpression("incarnation.id", vars)
	if err != nil {
		t.Fatalf("eval incarnation.id: %v", err)
	}
	if s, _ := got.Value().(string); s != guardIncarnationID {
		t.Errorf("incarnation.id evaluated to %q, want the IDENTIFIER %q.\n"+
			"ADR-0085: the CEL root is the identifier. It is also segment 3 of every derived "+
			"Vault path and the RBAC incarnation= scope value — one spelling, or the three "+
			"disagree.", s, guardIncarnationID)
	}

	if _, err := eng.EvalExpression("incarnation.label", vars); err == nil {
		t.Error("`incarnation.label` resolved. ADR-0085: the caption is not a CEL root, " +
			"because a scenario that branches on a mutable value makes editing a screen " +
			"caption change what runs on a host.")
	}
}

// TestIncarnationLabel_HostVarsCarryNoCaption walks the wider builder — the one a
// per-host render actually calls — so the guard covers the assembled context and
// not just the one map inside it.
func TestIncarnationLabel_HostVarsCarryNoCaption(t *testing.T) {
	vars := hostVars(guardRenderInput(), host("a.example.test", []string{"prod"}, nil), 1)
	if _, ok := vars.Incarnation["label"]; ok {
		t.Error("hostVars carried an `incarnation.label` key into the per-host CEL context")
	}
	if got := vars.Incarnation["id"]; got != guardIncarnationID {
		t.Errorf("hostVars incarnation.id = %v, want %q", got, guardIncarnationID)
	}
}
