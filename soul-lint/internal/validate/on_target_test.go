package validate

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestOnIncarnationIDDiagnostics pins the offline `on:`-target rule
// (ADR-008 amendment/NIM-124): a bare `${ incarnation.id }` element is an
// error; a derived value or a non-identifier target is not. Mirrors the keeper
// render resolver (resolveCovenList) at the literal level.
//
// BOTH spellings are cases here, not one. `incarnation.name` still evaluates for
// the length of the [ADR-0085] window, so for that window it is still writable —
// and a rule that knew only the new spelling would hand a stale scenario a way
// past a fail-closed guard.
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md
func TestOnIncarnationIDDiagnostics(t *testing.T) {
	cases := []struct {
		name    string
		on      any
		wantErr bool
	}{
		{name: "bare id interp", on: []any{"${ incarnation.id }"}, wantErr: true},
		{name: "bare id no spaces", on: []any{"${incarnation.id}"}, wantErr: true},
		{name: "id among covens", on: []any{"prod", "${ incarnation.id }"}, wantErr: true},
		{name: "string slice form", on: []string{"${ incarnation.id }"}, wantErr: true},
		{name: "retired spelling, bare", on: []any{"${ incarnation.name }"}, wantErr: true},
		{name: "retired spelling, no spaces", on: []any{"${incarnation.name}"}, wantErr: true},
		{name: "retired spelling among covens", on: []any{"prod", "${ incarnation.name }"}, wantErr: true},
		{name: "real coven", on: []any{"prod"}, wantErr: false},
		{name: "derived from id (prefix)", on: []any{"env-${ incarnation.id }"}, wantErr: false},
		{name: "derived from id (cel suffix)", on: []any{"${ incarnation.id + '-x' }"}, wantErr: false},
		{name: "derived from the retired spelling", on: []any{"env-${ incarnation.name }"}, wantErr: false},
		{name: "keeper scalar", on: "keeper", wantErr: false},
		{name: "omitted", on: nil, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks := []config.Task{{Name: "t", On: tc.on}}
			diags := onIncarnationIDDiagnostics("scn.yml", tasks)
			got := false
			for _, d := range diags {
				if d.Code == "on_incarnation_id" {
					got = true
				}
			}
			if got != tc.wantErr {
				t.Fatalf("on=%v: got error=%v, want %v (diags=%v)", tc.on, got, tc.wantErr, diags)
			}
		})
	}
}

// TestOnIncarnationIDDiagnostics_NestedBlock confirms the walk recurses into
// block: children (an `on:` on a block child is still flagged).
func TestOnIncarnationIDDiagnostics_NestedBlock(t *testing.T) {
	tasks := []config.Task{{
		Name: "group",
		Block: &config.BlockTask{Block: []config.Task{
			{Name: "child", On: []any{"${ incarnation.id }"}},
		}},
	}}
	diags := onIncarnationIDDiagnostics("scn.yml", tasks)
	if len(diags) != 1 || diags[0].Code != "on_incarnation_id" {
		t.Fatalf("expected one on_incarnation_id from a block child, got %v", diags)
	}
}
