package cel

import (
	"errors"
	"testing"
)

// TestGlob_HappyPath — basic shell glob: `prod-*` matches `prod-web-01`, does not
// match `dev-web-01`. Member form `sid.glob(...)`.
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
		// Exact match without wildcards.
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

// TestGlob_SoulprintSelf — a real target.where scenario: a predicate against
// `soulprint.self.os.family` ([ADR-040], pre-W-2 examples).
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

// TestGlob_EmptyInputs — an empty pattern matches only the empty string; an empty
// string against a non-empty pattern is false (filepath.Match behavior).
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
		{"", "*", true}, // `*` matches any (incl. empty) sequence.
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

// TestGlob_MalformedPattern — a broken pattern ([filepath.ErrBadPattern]) returns
// false without an error: a per-host target.where predicate must not bring down
// the whole Tide on a single host, syntax validation is done by soul-lint.
func TestGlob_MalformedPattern(t *testing.T) {
	e := newEngine(t)

	// Unclosed char-class `[a-` — filepath.Match returns ErrBadPattern.
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

// TestGlob_CombinedExpression — a real target.where scenario: AND with other
// Soulprint facts ([ADR-040], pre-W-2 examples).
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

// TestGlob_UndeclaredInMigration — migration-CEL ([ADR-019]) is hermetic:
// glob() is NOT registered (see buildEngine). A call → compile error
// no such overload, symmetric to the vault()/now() guard of the migration env.
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

// TestGlob_AvailableInFlowControl — the Soul-side flow-control sandbox
// ([ADR-012(d)]) gets glob(): a pure function, no external context, symmetric to
// scenario predicates.
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
