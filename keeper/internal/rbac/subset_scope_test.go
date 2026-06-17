package rbac

import (
	"errors"
	"testing"
)

// mustScope парсит default_scope-строку в селектор (nil при пустой строке).
func mustScope(t *testing.T, raw string) map[string][]string {
	t.Helper()
	sc, err := ParseDefaultScope(raw)
	if err != nil {
		t.Fatalf("ParseDefaultScope(%q): %v", raw, err)
	}
	return sc
}

// TestSubset_DefaultScope_Escalation — least-privilege subset-check С учётом
// role default_scope обеих сторон (ADR-047 S1, security-fix privilege-escalation).
//
// Дыра: subset сравнивал СЫРЫЕ permission-строки без применения default_scope.
// Caller с ролью default_scope=coven=prod + bare `incarnation.run` имеет
// эффективный scope = prod, но при выдаче `incarnation.run on coven=staging`
// сырая bare-perm (Selector==nil) покрывала ЛЮБОЙ coven → грант на staging
// проходил (эскалация). Фикс: сравниваются ЭФФЕКТИВНЫЕ (resource,action,coven)-
// точки — bare-perm caller-а наследует его default_scope, bare-perm гранящейся
// роли — её default_scope.
func TestSubset_DefaultScope_Escalation(t *testing.T) {
	tests := []struct {
		name string
		// caller: его permissions + default_scope роли, через которую они выданы.
		callerRaws  []string
		callerScope string
		// granted: permissions гранящейся роли + её default_scope.
		grantedRaws  []string
		grantedScope string
		wantHeld     bool // true → ErrPermissionNotHeld (выдача запрещена)
	}{
		{
			name:         "ЭСКАЛАЦИЯ: caller scope=prod + bare → выдать coven=staging запрещено",
			callerRaws:   []string{"incarnation.run"},
			callerScope:  "coven=prod",
			grantedRaws:  []string{"incarnation.run on coven=staging"},
			grantedScope: "",
			wantHeld:     true,
		},
		{
			name:         "caller scope=prod + bare → выдать coven=prod ок (в его scope)",
			callerRaws:   []string{"incarnation.run"},
			callerScope:  "coven=prod",
			grantedRaws:  []string{"incarnation.run on coven=prod"},
			grantedScope: "",
			wantHeld:     false,
		},
		{
			name:         "caller scope=prod → роль scope=prod + bare ок (эффективно тот же scope)",
			callerRaws:   []string{"incarnation.run"},
			callerScope:  "coven=prod",
			grantedRaws:  []string{"incarnation.run"},
			grantedScope: "coven=prod",
			wantHeld:     false,
		},
		{
			name:         "caller scope=prod → роль scope=staging запрещена (шире/иной scope)",
			callerRaws:   []string{"incarnation.run"},
			callerScope:  "coven=prod",
			grantedRaws:  []string{"incarnation.run"},
			grantedScope: "coven=staging",
			wantHeld:     true,
		},
		{
			name:         "caller scope=prod → роль scope=prod,staging запрещена (staging вне scope)",
			callerRaws:   []string{"incarnation.run"},
			callerScope:  "coven=prod",
			grantedRaws:  []string{"incarnation.run"},
			grantedScope: "coven=prod,staging",
			wantHeld:     true,
		},
		{
			name:         "cluster-admin (*) → может выдать любой scope",
			callerRaws:   []string{"*"},
			callerScope:  "",
			grantedRaws:  []string{"incarnation.run on coven=staging"},
			grantedScope: "",
			wantHeld:     false,
		},
		{
			name:         "backcompat: caller БЕЗ default_scope + bare → может выдать любой scope",
			callerRaws:   []string{"incarnation.run"},
			callerScope:  "",
			grantedRaws:  []string{"incarnation.run on coven=staging"},
			grantedScope: "",
			wantHeld:     false,
		},
		{
			name:         "backcompat: caller БЕЗ scope + bare → выдать роль с любым scope ок",
			callerRaws:   []string{"incarnation.run"},
			callerScope:  "",
			grantedRaws:  []string{"incarnation.run"},
			grantedScope: "coven=prod,staging,dev",
			wantHeld:     false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caller := effectivePermissions(mustParse(t, tc.callerRaws...), mustScope(t, tc.callerScope))
			required := effectivePermissions(mustParse(t, tc.grantedRaws...), mustScope(t, tc.grantedScope))
			err := assertCallerCovers(caller, required)
			gotHeld := errors.Is(err, ErrPermissionNotHeld)
			if gotHeld != tc.wantHeld {
				t.Fatalf("assertCallerCovers err = %v; ErrPermissionNotHeld=%v, want %v", err, gotHeld, tc.wantHeld)
			}
		})
	}
}
