package validate

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestOnIncarnationNameDiagnostics pins the offline `on:`-target rule
// (ADR-008 amendment/NIM-124): a bare `${ incarnation.name }` element is an
// error; a derived value or a non-name target is not. Mirrors the keeper
// render resolver (resolveCovenList) at the literal level.
func TestOnIncarnationNameDiagnostics(t *testing.T) {
	cases := []struct {
		name    string
		on      any
		wantErr bool
	}{
		{name: "bare name interp", on: []any{"${ incarnation.name }"}, wantErr: true},
		{name: "bare name no spaces", on: []any{"${incarnation.name}"}, wantErr: true},
		{name: "name among covens", on: []any{"prod", "${ incarnation.name }"}, wantErr: true},
		{name: "string slice form", on: []string{"${ incarnation.name }"}, wantErr: true},
		{name: "real coven", on: []any{"prod"}, wantErr: false},
		{name: "derived from name (prefix)", on: []any{"env-${ incarnation.name }"}, wantErr: false},
		{name: "derived from name (cel suffix)", on: []any{"${ incarnation.name + '-x' }"}, wantErr: false},
		{name: "keeper scalar", on: "keeper", wantErr: false},
		{name: "omitted", on: nil, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks := []config.Task{{Name: "t", On: tc.on}}
			diags := onIncarnationNameDiagnostics("scn.yml", tasks)
			got := false
			for _, d := range diags {
				if d.Code == "on_incarnation_name" {
					got = true
				}
			}
			if got != tc.wantErr {
				t.Fatalf("on=%v: got error=%v, want %v (diags=%v)", tc.on, got, tc.wantErr, diags)
			}
		})
	}
}

// TestOnIncarnationNameDiagnostics_NestedBlock confirms the walk recurses into
// block: children (an `on:` on a block child is still flagged).
func TestOnIncarnationNameDiagnostics_NestedBlock(t *testing.T) {
	tasks := []config.Task{{
		Name: "group",
		Block: &config.BlockTask{Block: []config.Task{
			{Name: "child", On: []any{"${ incarnation.name }"}},
		}},
	}}
	diags := onIncarnationNameDiagnostics("scn.yml", tasks)
	if len(diags) != 1 || diags[0].Code != "on_incarnation_name" {
		t.Fatalf("expected one on_incarnation_name from a block child, got %v", diags)
	}
}
