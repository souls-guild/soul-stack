package api

// Guard-сверка huma-enum типа Herald ↔ КАНОНИЧЕСКИЙ herald.AllHeraldTypes
// (единый источник, ADR-052 amendment). huma-тег `enum:"…"` принимает только
// строковый ЛИТЕРАЛ (нельзя сослаться на []herald.HeraldType) — значит литерал
// и рантайм-реестр синхронятся вручную. Этот тест извлекает enum-набор из тега
// РЕФЛЕКСИЕЙ по тем же op-input-структурам, что huma компилирует в спеку, и
// сверяет с herald.AllHeraldTypes. Красный тест = huma-enum разошёлся с
// реестром типов (третье место списка после дескриптора и PG-CHECK).

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
				t.Errorf("huma enum %v != herald.AllHeraldTypes %v — три места списка типов разошлись", got, want)
			}
		})
	}
}

// enumTagValues извлекает значения `enum:"a,b"` тега поля по пути fieldPath.
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
