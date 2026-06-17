package rbac

import (
	"strings"
	"testing"
)

// ADR-047 S2c — state-ключ селектора: CEL-предикат по incarnation.state.
// TDD-first: тесты фиксируют контракт ДО реализации (red), затем зеленеют.
//
// Параллель regex (S2a) / soulprint (S2b), но валидация/eval делегированы
// keeper/internal/statepredicate (Compile/Matches уже есть, sandbox migration-CEL
// корень `state`) — RBAC НЕ дублирует CEL-движок state-предикатов.
//
// Граница S2c (как soulprint в S2b): state добавляется в грамматику селектора +
// Purview.StateExprs + least-privilege subset (string-equality fail-closed).
// Matches активен, когда incarnation.state есть в context (S3b его подаст); пока
// context (map[string]string) не несёт nested state — fail-closed deny.

// --- Парсинг quoted state-значения ---

// state='state.redis_version == "8.0"' парсится в Selector{state:[…]}
// (CEL-предикат без внешних кавычек).
func TestParseSelector_State_Simple(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on state='state.redis_version == "8.0"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["state"]
	want := `state.redis_version == "8.0"`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[state] = %v, want [%q]", got, want)
	}
}

// Внутренние двойные кавычки и пробелы CEL-предиката не рвут value-list:
// одинарные кавычки снаружи защищают от `,`-разделителя и пробелов.
func TestParseSelector_State_QuotesAndSpaces(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on state='state.redis_version == "8.0" && state.replicas == 3'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["state"]
	want := `state.redis_version == "8.0" && state.replicas == 3`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[state] = %v, want [%q]", got, want)
	}
}

// Битый CEL → ошибка load (parseSelector валидирует компиляцию через
// statepredicate.Compile).
func TestParseSelector_State_BrokenRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on state='state.redis_version =='`, // незавершённое выражение
		`incarnation.run on state='state.redis_version && '`,
		`incarnation.run on state='('`, // несбалансированная скобка
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParsePermission(in)
			if err == nil {
				t.Fatalf("ParsePermission(%q): want compile error, got nil", in)
			}
			if !strings.Contains(err.Error(), "state") {
				t.Errorf("err = %v, want substring \"state\"", err)
			}
		})
	}
}

// CEL-предикат, обращающийся к запрещённому в migration-sandbox корню/функции,
// отвергается на load — state-scope = чистая функция от state (запрещены
// vault/now/register/soulprint/input/essence; объявлен только `state.*`).
func TestParseSelector_State_SandboxRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on state='vault("secret/x") == "y"'`,
		`incarnation.run on state='now() > timestamp("2020-01-01T00:00:00Z")'`,
		`incarnation.run on state='soulprint.self.os.family == "debian"'`,
		`incarnation.run on state='register.self.rc == 0'`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePermission(in); err == nil {
				t.Fatalf("ParsePermission(%q): want sandbox/compile error, got nil", in)
			}
		})
	}
}

// Незакавыченное state-значение запрещено: предикат с пробелами/кавычками
// неотличим от value-list без quoted-формы.
func TestParseSelector_State_RequiresQuotes(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on state=state.redis_version`)
	if err == nil {
		t.Fatal("ParsePermission(unquoted state): want error (must be quoted), got nil")
	}
}

// Пустой state (state=”) отвергается.
func TestParseSelector_State_EmptyRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on state=''`)
	if err == nil {
		t.Fatal("ParsePermission(state=''): want error for empty predicate, got nil")
	}
}

// Слишком длинный предикат отвергается на load (length-cap).
func TestParseSelector_State_LengthCapped(t *testing.T) {
	long := `state.redis_version == "` + strings.Repeat("a", maxStateExprLen) + `"`
	_, err := ParsePermission(`incarnation.run on state='` + long + `'`)
	if err == nil {
		t.Fatal("ParsePermission(over-long state): want length-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too long") && !strings.Contains(err.Error(), "length") {
		t.Errorf("err = %v, want length-cap message", err)
	}
}

// --- Matches: активен при наличии incarnation.state в context, иначе fail-closed ---

// Без state в context — soulprint-подобный fail-closed deny: текущий
// map[string]string-context не несёт nested state (S3b его подаст).
func TestMatches_State_FailClosedWithoutState(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on state='state.redis_version == "8.0"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Matches("incarnation", "run", map[string]string{"incarnation": "redis-prod", "coven": "prod"}) {
		t.Errorf("state-perm без state-в-context должна давать deny (S2c fail-closed)")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Errorf("state-perm с nil-context должна давать deny")
	}
}

// --- Standalone CEL-eval против incarnation.state (через statepredicate, готов под S3b) ---

func TestEvalStateExpr(t *testing.T) {
	v80 := map[string]any{"redis_version": "8.0", "replicas": int64(3)}
	v81 := map[string]any{"redis_version": "8.1", "replicas": int64(3)}

	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, v80); err != nil || !ok {
		t.Errorf("state 8.0: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, v81); err != nil || ok {
		t.Errorf("state 8.1: ok=%v err=%v, want false,nil", ok, err)
	}
	// Отсутствующий ключ в state → no-match (fail-closed), НЕ ошибка функции.
	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, map[string]any{}); err != nil || ok {
		t.Errorf("пустой state: ok=%v err=%v, want false,nil (no-such-key → no-match)", ok, err)
	}
	// nil-state → no-match.
	if ok, err := EvalStateExpr(`state.redis_version == "8.0"`, nil); err != nil || ok {
		t.Errorf("nil-state: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- Purview.StateExprs ---

// ResolvePurview со state-permission заполняет Purview.StateExprs.
func TestResolvePurview_State(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "redis8-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.run on state='state.redis_version == "8.0"'`},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (state-scoped)")
	}
	want := `state.redis_version == "8.0"`
	if len(p.StateExprs) != 1 || p.StateExprs[0] != want {
		t.Errorf("StateExprs = %v, want [%q]", p.StateExprs, want)
	}
}

// default_scope=state наследуется bare-permission-ом (S1+S2c вместе).
func TestResolvePurview_State_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "redis8-ops", operators: []string{"archon-a"},
		defaultScope: `state='state.redis_version == "8.0"'`,
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare наследует state default_scope)")
	}
	want := `state.redis_version == "8.0"`
	if len(p.StateExprs) != 1 || p.StateExprs[0] != want {
		t.Errorf("StateExprs = %v, want [%q] (наследование default_scope)", p.StateExprs, want)
	}
}

// --- subset: state = string-equality fail-closed ---

func TestSubset_State_StringEquality(t *testing.T) {
	v80 := `incarnation.run on state='state.redis_version == "8.0"'`
	v81 := `incarnation.run on state='state.redis_version == "8.1"'`
	// Логически уже предиката v80 (8.0 И replicas==3), но статически containment
	// CEL неразрешим → string-inequal → DENY.
	v80repl := `incarnation.run on state='state.redis_version == "8.0" && state.replicas == 3'`

	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (выдача запрещена)
	}{
		{
			name:        "идентичный state → выдача ок",
			callerRaws:  []string{v80},
			grantedRaws: []string{v80},
			wantHeld:    false,
		},
		{
			name:        "иной state → DENY (fail-closed, не string-equal)",
			callerRaws:  []string{v80},
			grantedRaws: []string{v81},
			wantHeld:    true,
		},
		{
			name:        "state-сужение недостижимо статически → DENY",
			callerRaws:  []string{v80},
			grantedRaws: []string{v80repl},
			wantHeld:    true,
		},
		{
			name:        "caller с * выдаёт любой state",
			callerRaws:  []string{"*"},
			grantedRaws: []string{v80},
			wantHeld:    false,
		},
		{
			name:        "caller без state-scope (bare) выдаёт state → ок (bare покрывает)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{v80},
			wantHeld:    false,
		},
		{
			name:        "caller со state выдаёт bare → DENY (bare шире state-scope caller-а)",
			callerRaws:  []string{v80},
			grantedRaws: []string{"incarnation.run"},
			wantHeld:    true,
		},
		{
			name:        "caller со state выдаёт coven (иное измерение) → DENY",
			callerRaws:  []string{v80},
			grantedRaws: []string{"incarnation.run on coven=prod"},
			wantHeld:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caller := mustParse(t, tc.callerRaws...)
			required := mustParse(t, tc.grantedRaws...)
			err := assertCallerCovers(caller, required)
			gotHeld := strings.Contains(errString(err), "least-privilege")
			if gotHeld != tc.wantHeld {
				t.Fatalf("assertCallerCovers err = %v; held=%v, want %v", err, gotHeld, tc.wantHeld)
			}
		})
	}
}
