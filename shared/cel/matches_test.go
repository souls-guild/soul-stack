package cel

import (
	"testing"
)

// TestMatches_StdlibAvailable — smoke-test: standard CEL `matches()` (regex,
// parity SaltStack `-E`) включён в env через cel.StdLib() ([engine.go
// buildEngine]). Подтверждает доступность regex-формы для target.where
// ([ADR-040]) симметрично нашему custom glob().
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
		// Регулярный wildcard-аналог `prod-*`.
		{`input.sid.matches("^prod-.+$")`, Vars{Input: map[string]any{"sid": "prod-web-01"}}, true},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			out, err := e.EvalExpression(tc.expr, tc.vars)
			if err != nil {
				t.Fatalf("EvalExpression: %v", err)
			}
			if got := out.Value(); got != tc.want {
				t.Fatalf("результат = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMatches_CombinedWithGlob — обе функции работают в одном выражении
// (типичный target.where: regex + glob через AND).
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
		t.Fatalf("результат = %v, want true", got)
	}
}
