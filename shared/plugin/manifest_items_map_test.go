package plugin

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ADR-045 S7-amend: `items` under `type: map` declares the VALUE type
// (`map[string]<items>`) — reusing the same key as list (the element type).
// validateInputParam must accept items under map/object just like under
// list/array, and must NOT emit input_items_invalid_for_type.
func TestValidateInputParam_ItemsUnderMap(t *testing.T) {
	const tmpl = `kind: soul_module
protocol_version: 1
namespace: core
name: probe
spec:
  states:
    s:
      input:
        p:
__PARAM__
`
	cases := []struct {
		name     string
		paramDef string // string with the YAML inline object of param p (indented).
		wantCode string // "" — diagnostics must not contain items errors.
	}{
		{
			name:     "map with value items type string is valid",
			paramDef: "          { type: map, items: { type: string } }\n",
			wantCode: "",
		},
		{
			name:     "map without items is valid (JSON-редактор в UI)",
			paramDef: "          { type: map }\n",
			wantCode: "",
		},
		{
			name:     "object with value items type int is valid",
			paramDef: "          { type: object, items: { type: int } }\n",
			wantCode: "",
		},
		{
			name:     "map items with unknown value type rejected",
			paramDef: "          { type: map, items: { type: frobnicate } }\n",
			wantCode: "input_items_type_unknown",
		},
		{
			name:     "map items without type rejected",
			paramDef: "          { type: map, items: { description: x } }\n",
			wantCode: "input_items_type_missing",
		},
		{
			// Regression: items on a scalar is still forbidden.
			name:     "items on scalar type still rejected",
			paramDef: "          { type: string, items: { type: string } }\n",
			wantCode: "input_items_invalid_for_type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(strings.Replace(tmpl, "__PARAM__", tc.paramDef, 1))
			_, diags := LoadFromBytes("manifest.yaml", src)

			got := itemsRelatedCode(diags)
			if tc.wantCode == "" {
				if got != "" {
					t.Fatalf("ожидали отсутствие items-ошибок, получили %q (diags=%v)", got, diagCodesList(diags))
				}
				return
			}
			if got != tc.wantCode {
				t.Fatalf("ожидали %q, получили %q (diags=%v)", tc.wantCode, got, diagCodesList(diags))
			}
		})
	}
}

// itemsRelatedCode returns the first items-related error code in diagnostics
// (input_items_*), or "" if there are none.
func itemsRelatedCode(diags []diag.Diagnostic) string {
	for _, d := range diags {
		if strings.HasPrefix(d.Code, "input_items_") {
			return d.Code
		}
	}
	return ""
}

func diagCodesList(diags []diag.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}
