package rbac

import (
	"strings"
	"testing"
)

// ADR-047 S2b — soulprint-ключ селектора: CEL-предикат по фактам хоста
// (`soulprint.self.*`, ADR-018 каноническая форма). TDD-first: тесты фиксируют
// контракт ДО реализации (red), затем зеленеют.
//
// Граница S2b (как regex в S2a): soulprint добавляется в грамматику селектора +
// Purview.SoulprintExprs + least-privilege subset (string-equality fail-closed).
// Matches в S2b — fail-closed: текущий context (map[string]string) НЕ несёт
// nested soulprint-факты, поэтому soulprint-предикат всегда deny; РЕАЛЬНЫЙ
// CEL-eval против фактов хоста — слайсы S3/S4 (list-видимость/target), там
// резолвер list/target подаёт факты. Standalone-eval (EvalSoulprintExpr) готов и
// протестирован под S3.

// --- Парсинг quoted soulprint-значения ---

// soulprint='soulprint.self.os.family == "debian"' парсится в
// Selector{soulprint:[…]} (CEL-предикат без внешних кавычек).
func TestParseSelector_Soulprint_Simple(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on soulprint='soulprint.self.os.family == "debian"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["soulprint"]
	want := `soulprint.self.os.family == "debian"`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[soulprint] = %v, want [%q]", got, want)
	}
}

// Внутренние двойные кавычки и пробелы CEL-предиката не рвут value-list:
// одинарные кавычки снаружи защищают от `,`-разделителя и пробелов.
func TestParseSelector_Soulprint_QuotesAndSpaces(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on soulprint='soulprint.self.os.family == "debian" && soulprint.self.os.arch == "amd64"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["soulprint"]
	want := `soulprint.self.os.family == "debian" && soulprint.self.os.arch == "amd64"`
	if len(got) != 1 || got[0] != want {
		t.Errorf("Selector[soulprint] = %v, want [%q]", got, want)
	}
}

// Битый CEL → ошибка load (parseSelector валидирует компиляцию через shared/cel).
func TestParseSelector_Soulprint_BrokenRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on soulprint='soulprint.self.os.family =='`, // незавершённое выражение
		`incarnation.run on soulprint='soulprint.self.os.family && '`,
		`incarnation.run on soulprint='('`, // несбалансированная скобка
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParsePermission(in)
			if err == nil {
				t.Fatalf("ParsePermission(%q): want compile error, got nil", in)
			}
			if !strings.Contains(err.Error(), "soulprint") {
				t.Errorf("err = %v, want substring \"soulprint\"", err)
			}
		})
	}
}

// CEL-предикат, обращающийся к запрещённому в sandbox-е корню/функции, отвергается
// на load — soulprint-scope = чистая функция от фактов хоста (FlowControl-
// песочница: vault()/now() guard-ом, state — необъявлен). register/input/essence
// в FlowControl ОБЪЯВЛЕНЫ (общий контекст-набор) и компилируются, но для
// scope-предиката бессмысленны (всегда no-such-key → deny); это безвредный
// footgun (deny + string-equality subset), не load-fail (см. observations).
func TestParseSelector_Soulprint_SandboxRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on soulprint='vault("secret/x") == "y"'`,
		`incarnation.run on soulprint='now() > timestamp("2020-01-01T00:00:00Z")'`,
		`incarnation.run on soulprint='state.x == 1'`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePermission(in); err == nil {
				t.Fatalf("ParsePermission(%q): want sandbox/compile error, got nil", in)
			}
		})
	}
}

// soulprint.hosts/soulprint.where — host-аксессор прогона, недоступен в
// scope-предикате (изоляция allowHosts=false в FlowControl-песочнице) → load-fail.
func TestParseSelector_Soulprint_HostsAccessorRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on soulprint='soulprint.hosts.exists(h, h.sid == "x")'`)
	if err == nil {
		t.Fatal("ParsePermission(soulprint.hosts ...): want isolation error, got nil")
	}
}

// Незакавыченное soulprint-значение запрещено: предикат с пробелами/кавычками
// неотличим от value-list без quoted-формы.
func TestParseSelector_Soulprint_RequiresQuotes(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on soulprint=soulprint.self.os.family`)
	if err == nil {
		t.Fatal("ParsePermission(unquoted soulprint): want error (must be quoted), got nil")
	}
}

// Пустой soulprint (soulprint=”) отвергается.
func TestParseSelector_Soulprint_EmptyRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on soulprint=''`)
	if err == nil {
		t.Fatal("ParsePermission(soulprint=''): want error for empty predicate, got nil")
	}
}

// Слишком длинный предикат отвергается на load (length-cap).
func TestParseSelector_Soulprint_LengthCapped(t *testing.T) {
	long := `soulprint.self.os.family == "` + strings.Repeat("a", maxSoulprintExprLen) + `"`
	_, err := ParsePermission(`incarnation.run on soulprint='` + long + `'`)
	if err == nil {
		t.Fatal("ParsePermission(over-long soulprint): want length-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too long") && !strings.Contains(err.Error(), "length") {
		t.Errorf("err = %v, want length-cap message", err)
	}
}

// --- Matches: fail-closed в S2b (context не несёт soulprint-факты) ---

// soulprint-предикат в S2b всегда даёт deny через Matches: текущий
// map[string]string-context не несёт nested facts. Реальный eval — S3/S4.
func TestMatches_Soulprint_FailClosed(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on soulprint='soulprint.self.os.family == "debian"'`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	// Никакой context (включая coven/host/sid) не активирует soulprint-предикат
	// в S2b — фактов в map[string]string нет.
	if p.Matches("incarnation", "run", map[string]string{"host": "web-01", "coven": "prod"}) {
		t.Errorf("soulprint-perm должна давать deny в Matches (S2b fail-closed)")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Errorf("soulprint-perm с nil-context должна давать deny")
	}
}

// --- Standalone CEL-eval против фактов хоста (готов под S3) ---

func TestEvalSoulprintExpr(t *testing.T) {
	debian := map[string]any{"os": map[string]any{"family": "debian", "arch": "amd64"}}
	rhel := map[string]any{"os": map[string]any{"family": "rhel", "arch": "amd64"}}

	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, debian); err != nil || !ok {
		t.Errorf("debian-хост: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, rhel); err != nil || ok {
		t.Errorf("rhel-хост: ok=%v err=%v, want false,nil", ok, err)
	}
	// Отсутствующий ключ в фактах → no-match (default-deny), НЕ ошибка функции.
	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, map[string]any{}); err != nil || ok {
		t.Errorf("пустые факты: ok=%v err=%v, want false,nil (no-such-key → no-match)", ok, err)
	}
	// nil-факты → no-match.
	if ok, err := EvalSoulprintExpr(`soulprint.self.os.family == "debian"`, nil); err != nil || ok {
		t.Errorf("nil-факты: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- Purview.SoulprintExprs ---

// ResolvePurview с soulprint-permission заполняет Purview.SoulprintExprs.
func TestResolvePurview_Soulprint(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "deb-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.run on soulprint='soulprint.self.os.family == "debian"'`},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (soulprint-scoped)")
	}
	want := `soulprint.self.os.family == "debian"`
	if len(p.SoulprintExprs) != 1 || p.SoulprintExprs[0] != want {
		t.Errorf("SoulprintExprs = %v, want [%q]", p.SoulprintExprs, want)
	}
}

// default_scope=soulprint наследуется bare-permission-ом (S1+S2b вместе).
func TestResolvePurview_Soulprint_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "deb-ops", operators: []string{"archon-a"},
		defaultScope: `soulprint='soulprint.self.os.family == "debian"'`,
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare наследует soulprint default_scope)")
	}
	want := `soulprint.self.os.family == "debian"`
	if len(p.SoulprintExprs) != 1 || p.SoulprintExprs[0] != want {
		t.Errorf("SoulprintExprs = %v, want [%q] (наследование default_scope)", p.SoulprintExprs, want)
	}
}

// --- subset: soulprint = string-equality fail-closed ---

func TestSubset_Soulprint_StringEquality(t *testing.T) {
	deb := `incarnation.run on soulprint='soulprint.self.os.family == "debian"'`
	rhel := `incarnation.run on soulprint='soulprint.self.os.family == "rhel"'`
	// Логически уже предиката deb (debian И amd64), но статически containment
	// CEL неразрешим → string-inequal → DENY.
	debArch := `incarnation.run on soulprint='soulprint.self.os.family == "debian" && soulprint.self.os.arch == "amd64"'`

	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (выдача запрещена)
	}{
		{
			name:        "идентичный soulprint → выдача ок",
			callerRaws:  []string{deb},
			grantedRaws: []string{deb},
			wantHeld:    false,
		},
		{
			name:        "иной soulprint → DENY (fail-closed, не string-equal)",
			callerRaws:  []string{deb},
			grantedRaws: []string{rhel},
			wantHeld:    true,
		},
		{
			name:        "соулпринт-сужение недостижимо статически → DENY",
			callerRaws:  []string{deb},
			grantedRaws: []string{debArch},
			wantHeld:    true,
		},
		{
			name:        "caller с * выдаёт любой soulprint",
			callerRaws:  []string{"*"},
			grantedRaws: []string{deb},
			wantHeld:    false,
		},
		{
			name:        "caller без soulprint-scope (bare) выдаёт soulprint → ок (bare покрывает)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{deb},
			wantHeld:    false,
		},
		{
			name:        "caller с soulprint выдаёт bare → DENY (bare шире soulprint-scope caller-а)",
			callerRaws:  []string{deb},
			grantedRaws: []string{"incarnation.run"},
			wantHeld:    true,
		},
		{
			name:        "caller с soulprint выдаёт coven (иное измерение) → DENY",
			callerRaws:  []string{deb},
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
