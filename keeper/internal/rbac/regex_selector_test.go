package rbac

import (
	"strings"
	"testing"
)

// ADR-047 S2a — regex-ключ селектора: матчит по SID/имени хоста (RE2).
// TDD-first: тесты фиксируют контракт ДО реализации (red), затем зеленеют.
//
// Граница S2a: regex добавляется в грамматику селектора + Purview.Regexes +
// учитывается в Matches (host-context) и least-privilege subset (string-equality
// fail-closed). РЕАЛЬНОЕ применение к list-видимости/target-пересечению — S3/S4.

// --- Парсинг quoted regex-значения ---

// regex='^web-' парсится в Selector{regex:[^web-]} (значение без кавычек).
func TestParseSelector_Regex_Simple(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["regex"]
	if len(got) != 1 || got[0] != "^web-" {
		t.Errorf("Selector[regex] = %v, want [^web-]", got)
	}
}

// Запятая ВНУТРИ regex ({1,3}) не рвёт значение — quoted-форма защищает от
// `,`-разделителя value-list.
func TestParseSelector_Regex_CommaInsideQuotes(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^a{1,3}$'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["regex"]
	if len(got) != 1 || got[0] != "^a{1,3}$" {
		t.Errorf("Selector[regex] = %v, want [^a{1,3}$] (запятая внутри regex не рвёт)", got)
	}
}

// Битый regex → ошибка load (parseSelector валидирует regexp.Compile).
func TestParseSelector_Regex_BrokenRejected(t *testing.T) {
	cases := []string{
		"incarnation.run on regex='^web-['",    // незакрытый класс
		"incarnation.run on regex='(unclosed'", // незакрытая группа
		"incarnation.run on regex='*'",         // нет операнда у квантора
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := ParsePermission(in)
			if err == nil {
				t.Fatalf("ParsePermission(%q): want compile error, got nil", in)
			}
			if !strings.Contains(err.Error(), "regex") {
				t.Errorf("err = %v, want substring \"regex\"", err)
			}
		})
	}
}

// Незакавыченное regex-значение запрещено: regex без кавычек неотличимо от
// exact-value и спецсимволы не пройдут reSelValue — требуем quoted-форму явно.
func TestParseSelector_Regex_RequiresQuotes(t *testing.T) {
	_, err := ParsePermission("incarnation.run on regex=^web-")
	if err == nil {
		t.Fatal("ParsePermission(regex=^web-): want error (regex must be quoted), got nil")
	}
}

// Слишком длинный regex отвергается на load (ReDoS-cap длины строки).
func TestParseSelector_Regex_LengthCapped(t *testing.T) {
	long := strings.Repeat("a", maxRegexLen+1)
	_, err := ParsePermission("incarnation.run on regex='" + long + "'")
	if err == nil {
		t.Fatal("ParsePermission(over-long regex): want length-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too long") && !strings.Contains(err.Error(), "length") {
		t.Errorf("err = %v, want length-cap message", err)
	}
}

// Пустой regex (regex=”) отвергается.
func TestParseSelector_Regex_EmptyRejected(t *testing.T) {
	_, err := ParsePermission("incarnation.run on regex=''")
	if err == nil {
		t.Fatal("ParsePermission(regex=''): want error for empty regex, got nil")
	}
}

// --- Matches с host/sid context ---

// permission incarnation.run on regex='^web-' + context{host: web-01} → match.
func TestMatches_Regex_HostContext(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if !p.Matches("incarnation", "run", map[string]string{"host": "web-01"}) {
		t.Errorf("regex=^web- should match host=web-01")
	}
	if p.Matches("incarnation", "run", map[string]string{"host": "db-01"}) {
		t.Errorf("regex=^web- should NOT match host=db-01")
	}
}

// regex также матчит по ключу sid в context-е (часть эндпоинтов кладёт sid).
func TestMatches_Regex_SidContext(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if !p.Matches("incarnation", "run", map[string]string{"sid": "web-01.example.com"}) {
		t.Errorf("regex=^web- should match sid=web-01.example.com")
	}
}

// regex-ключ без host/sid в context → no match (как exact-ключ без своего ключа).
func TestMatches_Regex_NoHostKeyDeny(t *testing.T) {
	p, err := ParsePermission("incarnation.run on regex='^web-'")
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Matches("incarnation", "run", map[string]string{"coven": "prod"}) {
		t.Errorf("regex-perm without host/sid in context должна давать deny")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Errorf("regex-perm с nil-context должна давать deny")
	}
}

// --- Purview.Regexes ---

// ResolvePurview с regex-permission заполняет Purview.Regexes.
func TestResolvePurview_Regex(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "web-ops", operators: []string{"archon-a"},
		permissions: []string{"incarnation.run on regex='^web-'"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (regex-scoped)")
	}
	if len(p.Regexes) != 1 || p.Regexes[0] != "^web-" {
		t.Errorf("Regexes = %v, want [^web-]", p.Regexes)
	}
}

// default_scope=regex наследуется bare-permission-ом (S1+S2a вместе).
func TestResolvePurview_Regex_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "web-ops", operators: []string{"archon-a"},
		defaultScope: "regex='^web-'",
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Errorf("Unrestricted=true, want false (bare наследует regex default_scope)")
	}
	if len(p.Regexes) != 1 || p.Regexes[0] != "^web-" {
		t.Errorf("Regexes = %v, want [^web-] (наследование default_scope)", p.Regexes)
	}
}

// --- subset: regex = string-equality fail-closed ---

func TestSubset_Regex_StringEquality(t *testing.T) {
	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (выдача запрещена)
	}{
		{
			name:        "идентичный regex → выдача ок",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run on regex='^web-'"},
			wantHeld:    false,
		},
		{
			name:        "иной regex → DENY (fail-closed, не string-equal)",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run on regex='^db-'"},
			wantHeld:    true,
		},
		{
			name:        "regex-сужение недостижимо статически → DENY (^web- не покрывает ^web-prod-)",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run on regex='^web-prod-'"},
			wantHeld:    true,
		},
		{
			name:        "caller с * выдаёт любой regex",
			callerRaws:  []string{"*"},
			grantedRaws: []string{"incarnation.run on regex='^web-'"},
			wantHeld:    false,
		},
		{
			name:        "caller без regex-scope (bare) выдаёт regex → ок (bare покрывает)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{"incarnation.run on regex='^web-'"},
			wantHeld:    false,
		},
		{
			name:        "caller с regex выдаёт bare → DENY (bare шире regex-scope caller-а)",
			callerRaws:  []string{"incarnation.run on regex='^web-'"},
			grantedRaws: []string{"incarnation.run"},
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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
