package servicevars

// THE INVARIANT, guarded at the THIRD builder of the CEL root `incarnation.*`
// ([ADR-0085], NIM-728): a caption is not in it here either.
//
// [ADR-0085] names three CEL environments, and this is the one that is easy to
// forget. The other two are fed by `render.incarnationVars` (the scenario /
// destiny root, and the flow-control context built from it), guarded in
// keeper/internal/render/label_invariant_guard_test.go. This one is separate
// code with its own map builder: `vars/_stack.yaml` is evaluated by
// [Resolver.resolveStack] through `cel.NewServiceVars()` ([ADR-0082]), BEFORE
// the render, and it assembles `incarnation.*` from [IncarnationContext]
// alone.
//
// So a `Label` field added here plus one line in [IncarnationContext.celMap]
// would make `${ incarnation.label }` resolve in a service's `_stack.yaml` while
// the render guard — whose own doc claims to cover "the CEL root
// `incarnation.*`" — stayed green. That is the hole this file closes.
//
// Why a caption must not resolve here specifically: a `_stack.yaml` step selects
// which vars OVERLAY a service gets. A step branching on a mutable caption would
// make renaming a screen label silently change the configuration handed to every
// host of the incarnation — the exact class of surprise the id/label split
// exists to remove, arriving one layer earlier than a scenario would.
//
// HOW TO BREAK IT ON PURPOSE: add `Label string` to IncarnationContext and
// `m["label"] = c.Label` to celMap. Both tests below go red.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
)

const (
	guardStackID    = "redis-billing"
	guardStackLabel = "redis-billing-prod" // a valid identifier too
)

func guardIncarnationContext() IncarnationContext {
	return IncarnationContext{
		ID:             guardStackID,
		Service:        "redis",
		ServiceVersion: "v1.2.3",
		Covens:         []string{"prod"},
	}
}

// TestIncarnationLabel_NotInServiceVarsCELRoot pins both halves: the struct that
// is the whole vocabulary this environment has about an incarnation, and the map
// the real builder produces from it.
func TestIncarnationLabel_NotInServiceVarsCELRoot(t *testing.T) {
	ctxT := reflect.TypeOf(IncarnationContext{})
	for i := range ctxT.NumField() {
		if strings.EqualFold(ctxT.Field(i).Name, "label") {
			t.Errorf("IncarnationContext gained a %q field.\n"+
				"ADR-0085: a caption participates in nothing derived, and `incarnation.*` in CEL "+
				"is one of the four surfaces named — in ALL THREE environments, this one included. "+
				"A `_stack.yaml` step branching on a mutable caption turns renaming a screen label "+
				"into changing the vars every host of the incarnation is handed.",
				ctxT.Field(i).Name)
		}
	}

	m := guardIncarnationContext().celMap()
	if _, ok := m["label"]; ok {
		t.Error("celMap produced a `label` key — `incarnation.label` would resolve in `_stack.yaml`")
	}
	if got := m["id"]; got != guardStackID {
		t.Errorf("incarnation.id = %v, want the IDENTIFIER %q", got, guardStackID)
	}
	for k, v := range m {
		if s, ok := v.(string); ok && s == guardStackLabel {
			t.Errorf("the CAPTION %q reached the service-vars CEL root under key %q", guardStackLabel, k)
		}
	}
}

// TestIncarnationLabel_ServiceVarsCELIDIsTheIdentifier evaluates through the
// real service-vars engine — the same one `_stack.yaml` is evaluated by — so the
// guard pins what an author's expression actually yields, and that
// `incarnation.label` is a no-such-key rather than an empty string.
func TestIncarnationLabel_ServiceVarsCELIDIsTheIdentifier(t *testing.T) {
	eng, err := cel.NewServiceVars()
	if err != nil {
		t.Fatalf("cel.NewServiceVars: %v", err)
	}
	vars := cel.Vars{Incarnation: guardIncarnationContext().celMap()}

	got, err := eng.EvalExpression("incarnation.id", vars)
	if err != nil {
		t.Fatalf("eval incarnation.id: %v", err)
	}
	if s, _ := got.Value().(string); s != guardStackID {
		t.Errorf("incarnation.id evaluated to %q, want the IDENTIFIER %q", s, guardStackID)
	}

	if _, err := eng.EvalExpression("incarnation.label", vars); err == nil {
		t.Error("`incarnation.label` resolved in the service-vars environment. ADR-0085: the " +
			"caption is not a CEL root in any of the three environments.")
	}
}
