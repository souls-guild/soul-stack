package cel

import (
	"errors"
	"testing"
)

// TestGlob_HappyPath — базовый shell-glob: `prod-*` матчит `prod-web-01`,
// не матчит `dev-web-01`. Member-форма `sid.glob(...)`.
func TestGlob_HappyPath(t *testing.T) {
	e := newEngine(t)

	cases := []struct {
		expr string
		vars Vars
		want bool
	}{
		{`input.sid.glob("prod-*")`, Vars{Input: map[string]any{"sid": "prod-web-01"}}, true},
		{`input.sid.glob("prod-*")`, Vars{Input: map[string]any{"sid": "dev-web-01"}}, false},
		{`input.sid.glob("web-0[1-9]")`, Vars{Input: map[string]any{"sid": "web-01"}}, true},
		{`input.sid.glob("web-0[1-9]")`, Vars{Input: map[string]any{"sid": "web-10"}}, false},
		{`input.sid.glob("?eb-01")`, Vars{Input: map[string]any{"sid": "web-01"}}, true},
		{`input.sid.glob("*")`, Vars{Input: map[string]any{"sid": "anything"}}, true},
		// Точное совпадение без wildcard-ов.
		{`input.sid.glob("prod-web-01")`, Vars{Input: map[string]any{"sid": "prod-web-01"}}, true},
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

// TestGlob_SoulprintSelf — реальный target.where-сценарий: предикат против
// `soulprint.self.os.family` ([ADR-040], pre-W-2 примеры).
func TestGlob_SoulprintSelf(t *testing.T) {
	e := newEngine(t)
	vars := Vars{SoulprintSelf: map[string]any{
		"os": map[string]any{"family": "debian"},
	}}

	out, err := e.EvalExpression(`soulprint.self.os.family.glob("deb*")`, vars)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("результат = %v, want true", got)
	}
}

// TestGlob_EmptyInputs — пустой pattern матчит только пустую строку; пустая
// строка против непустого pattern-а — false (поведение filepath.Match).
func TestGlob_EmptyInputs(t *testing.T) {
	e := newEngine(t)

	cases := []struct {
		s       string
		pattern string
		want    bool
	}{
		{"", "", true},
		{"x", "", false},
		{"", "x", false},
		{"", "*", true}, // `*` матчит любую (вкл. пустую) последовательность.
	}

	for _, tc := range cases {
		t.Run(tc.s+"|"+tc.pattern, func(t *testing.T) {
			out, err := e.EvalExpression(`input.s.glob(input.p)`, Vars{
				Input: map[string]any{"s": tc.s, "p": tc.pattern},
			})
			if err != nil {
				t.Fatalf("EvalExpression: %v", err)
			}
			if got := out.Value(); got != tc.want {
				t.Fatalf("glob(%q,%q) = %v, want %v", tc.s, tc.pattern, got, tc.want)
			}
		})
	}
}

// TestGlob_MalformedPattern — битый pattern ([filepath.ErrBadPattern])
// возвращает false без ошибки: per-host предикат target.where не должен
// валить весь Tide на отдельном хосте, валидацию синтаксиса делает soul-lint.
func TestGlob_MalformedPattern(t *testing.T) {
	e := newEngine(t)

	// Незакрытая char-class `[a-` — filepath.Match вернёт ErrBadPattern.
	out, err := e.EvalExpression(`input.sid.glob("[a-")`, Vars{
		Input: map[string]any{"sid": "anything"},
	})
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != false {
		t.Fatalf("малформ-pattern: результат = %v, want false", got)
	}
}

// TestGlob_CombinedExpression — реальный сценарий target.where: AND с другими
// фактами Soulprint ([ADR-040], pre-W-2 examples).
func TestGlob_CombinedExpression(t *testing.T) {
	e := newEngine(t)
	vars := Vars{
		Input: map[string]any{"sid": "web-prod-02"},
		SoulprintSelf: map[string]any{
			"os": map[string]any{"family": "debian"},
		},
	}

	out, err := e.EvalExpression(
		`soulprint.self.os.family == "debian" && input.sid.glob("web-*")`,
		vars,
	)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("результат = %v, want true", got)
	}
}

// TestGlob_UndeclaredInMigration — migration-CEL ([ADR-019]) hermetic:
// glob() НЕ зарегистрирована (см. buildEngine). Вызов → compile-ошибка
// no such overload, симметрично vault()/now()-guard-у миграционного env.
func TestGlob_UndeclaredInMigration(t *testing.T) {
	e := newMigrationEngine(t)

	_, err := e.EvalExpression(`state.key.glob("prefix-*")`, Vars{
		State: map[string]any{"key": "prefix-foo"},
	})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("glob() в migration-env: ошибка = %v, want *ErrCompile (no such overload)", err)
	}
}

// TestGlob_AvailableInFlowControl — Soul-side flow-control sandbox ([ADR-012(d)])
// glob() получает: pure-функция, без внешнего контекста, симметрия с
// scenario-предикатами.
func TestGlob_AvailableInFlowControl(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	out, err := e.EvalExpression(`register.probe.stdout.glob("*running*")`, Vars{
		Register: map[string]any{"probe": map[string]any{"stdout": "service is running fine"}},
	})
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("результат = %v, want true", got)
	}
}
