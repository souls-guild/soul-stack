package cel

import (
	"testing"
)

// TestMatches_StdlibAvailable — smoke-test: standard CEL `matches()` (regex)
// is enabled in the env via cel.StdLib() ([engine.go
// buildEngine]). Confirms the regex form is available for target.where ([ADR-040]),
// symmetric to our custom glob().
func TestMatches_StdlibAvailable(t *testing.T) {
	e := newEngine(t)

	cases := []struct {
		expr string
		vars Vars
		want bool
	}{
		{`input.sid.matches("^db-[0-9]+$")`, Vars{Input: map[string]any{"sid": "db-01"}}, true},
		{`input.sid.matches("^db-[0-9]+$")`, Vars{Input: map[string]any{"sid": "db-xx"}}, false},
		{`input.sid.matches("^db-[0-9]+$")`, Vars{Input: map[string]any{"sid": "web-01"}}, false},
		// Regex wildcard analogue of `prod-*`.
		{`input.sid.matches("^prod-.+$")`, Vars{Input: map[string]any{"sid": "prod-web-01"}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			out, err := e.EvalExpression(tc.expr, tc.vars)
			if err != nil {
				t.Fatalf("EvalExpression: %v", err)
			}
			if got := out.Value(); got != tc.want {
				t.Fatalf("result = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMatches_CombinedWithGlob — both functions work in a single expression
// (typical target.where: regex + glob via AND).
func TestMatches_CombinedWithGlob(t *testing.T) {
	e := newEngine(t)
	vars := Vars{Input: map[string]any{"sid": "db-prod-01"}}

	out, err := e.EvalExpression(
		`input.sid.glob("db-*") && input.sid.matches("^.+-[0-9]+$")`,
		vars,
	)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("result = %v, want true", got)
	}
}
