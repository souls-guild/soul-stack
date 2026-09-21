package api

// Guard check of the huma enum for Herald type ↔ the CANONICAL herald.AllHeraldTypes
// (single source, ADR-052 amendment). The huma tag `enum:"…"` accepts only a string
// LITERAL (you cannot reference []herald.HeraldType) — so the literal and the
// runtime registry are synced by hand. This test extracts the enum set from the tag
// by REFLECTION over the same op-input structs that huma compiles into the spec, and
// checks it against herald.AllHeraldTypes. A red test = the huma enum diverged from
// the type registry (the third place in the list, after the descriptor and the
// PG-CHECK).

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
)

func TestHumaHeraldTypeEnum_MatchesRegistry(t *testing.T) {
	want := make([]string, 0)
	for _, ty := range herald.AllHeraldTypes() {
		want = append(want, string(ty))
	}
	sort.Strings(want)

	cases := []struct {
		name      string
		structPtr any
	}{
		{"heraldCreateInput.Body.Type", &heraldCreateInput{}},
		{"heraldUpdateInput.Body.Type", &heraldUpdateInput{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := enumTagValues(t, tc.structPtr, "Body", "Type")
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("huma enum %v != herald.AllHeraldTypes %v -- the three type-list locations diverged", got, want)
			}
		})
	}
}

// enumTagValues extracts the values of a field's `enum:"a,b"` tag along fieldPath.
func enumTagValues(t *testing.T, structPtr any, fieldPath ...string) []string {
	t.Helper()
	ty := reflect.TypeOf(structPtr).Elem()
	for i, name := range fieldPath {
		f, ok := ty.FieldByName(name)
		if !ok {
			t.Fatalf("field %q not found in %v", name, ty)
		}
		if i == len(fieldPath)-1 {
			tag := f.Tag.Get("enum")
			if tag == "" {
				t.Fatalf("field %q has no enum tag", strings.Join(fieldPath, "."))
			}
			return strings.Split(tag, ",")
		}
		ty = f.Type
	}
	return nil
}
