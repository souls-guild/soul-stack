package rbac

import (
	"errors"
	"testing"
)

// mustParse is a helper: parses permission strings into []Permission, failing on error.
func mustParse(t *testing.T, raws ...string) []Permission {
	t.Helper()
	out := make([]Permission, 0, len(raws))
	for _, r := range raws {
		p, err := ParsePermission(r)
		if err != nil {
			t.Fatalf("ParsePermission(%q): %v", r, err)
		}
		out = append(out, p)
	}
	return out
}

// TestAssertCallerCovers is the least-privilege subset check (pure coverage
// logic, no DB). Implication semantics come from ParsePermission/Matches.
func TestAssertCallerCovers(t *testing.T) {
	tests := []struct {
		name       string
		callerRaws []string
		required   []string
		wantHeld   bool // true → ErrPermissionNotHeld
	}{
		{
			name:       "caller без * не может выдать *",
			callerRaws: []string{"role.create", "role.grant-operator"},
			required:   []string{"*"},
			wantHeld:   true,
		},
		{
			name:       "caller без права не может выдать его",
			callerRaws: []string{"role.create", "role.grant-operator"},
			required:   []string{"operator.create"},
			wantHeld:   true,
		},
		{
			name:       "caller с правом может выдать его",
			callerRaws: []string{"role.create", "soul.list"},
			required:   []string{"soul.list"},
			wantHeld:   false,
		},
		{
			name:       "cluster-admin (*) покрывает любое право",
			callerRaws: []string{"*"},
			required:   []string{"soul.list", "operator.create", "incarnation.run"},
			wantHeld:   false,
		},
		{
			name:       "cluster-admin (*) покрывает выдачу *",
			callerRaws: []string{"*"},
			required:   []string{"*"},
			wantHeld:   false,
		},
		{
			name:       "одна непокрытая среди покрытых → отказ",
			callerRaws: []string{"soul.list", "incarnation.get"},
			required:   []string{"soul.list", "operator.create"},
			wantHeld:   true,
		},
		{
			name:       "resource-wildcard caller-а покрывает конкретный action",
			callerRaws: []string{"incarnation.*"},
			required:   []string{"incarnation.run"},
			wantHeld:   false,
		},
		{
			name:       "конкретный action caller-а НЕ покрывает resource-wildcard",
			callerRaws: []string{"incarnation.run"},
			required:   []string{"incarnation.*"},
			wantHeld:   true,
		},
		{
			name:       "пустой required → всегда покрыт (нечего проверять)",
			callerRaws: []string{},
			required:   []string{},
			wantHeld:   false,
		},
		{
			name:       "caller без ролей не покрывает ничего",
			callerRaws: []string{},
			required:   []string{"soul.list"},
			wantHeld:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := assertCallerCovers(mustParse(t, tc.callerRaws...), mustParse(t, tc.required...))
			gotHeld := errors.Is(err, ErrPermissionNotHeld)
			if gotHeld != tc.wantHeld {
				t.Fatalf("assertCallerCovers err = %v; ErrPermissionNotHeld=%v, want %v", err, gotHeld, tc.wantHeld)
			}
		})
	}
}

// TestAssertCallerCovers_Selectors covers selector coverage: granting
// `x on coven=a,b` requires the caller to cover BOTH values.
func TestAssertCallerCovers_Selectors(t *testing.T) {
	t.Run("caller с тем же селектором покрывает", func(t *testing.T) {
		caller := mustParse(t, "incarnation.run on coven=prod")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod"))
		if errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want covered", err)
		}
	})

	t.Run("caller без селектора покрывает любой селектор", func(t *testing.T) {
		// `incarnation.run` (no filter) matches any context (Selector=nil).
		caller := mustParse(t, "incarnation.run")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod,stage"))
		if errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want covered (caller без фильтра покрывает всё)", err)
		}
	})

	t.Run("caller с одним значением НЕ покрывает выдачу двух", func(t *testing.T) {
		// Granting `coven=prod,stage` while only holding `coven=prod` is escalation onto stage.
		caller := mustParse(t, "incarnation.run on coven=prod")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod,stage"))
		if !errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want ErrPermissionNotHeld (stage не покрыт)", err)
		}
	})

	t.Run("caller с обоими значениями покрывает", func(t *testing.T) {
		caller := mustParse(t, "incarnation.run on coven=prod,stage")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod,stage"))
		if errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want covered", err)
		}
	})
}

// TestAddedPermissions is the diff for UpdateRolePermissions (the subset
// check applies only to added permissions; removal is unrestricted).
func TestAddedPermissions(t *testing.T) {
	tests := []struct {
		name string
		old  []string
		new  []string
		want []string
	}{
		{name: "ничего не добавлено", old: []string{"soul.list"}, new: []string{"soul.list"}, want: nil},
		{name: "одно добавлено", old: []string{"soul.list"}, new: []string{"soul.list", "*"}, want: []string{"*"}},
		{name: "удаление не считается добавлением", old: []string{"soul.list", "*"}, new: []string{"soul.list"}, want: nil},
		{name: "полная замена", old: []string{"soul.list"}, new: []string{"operator.create"}, want: []string{"operator.create"}},
		{name: "дубли схлопываются", old: nil, new: []string{"soul.list", "soul.list"}, want: []string{"soul.list"}},
		{name: "пустой новый набор", old: []string{"*"}, new: nil, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := addedPermissions(tc.old, tc.new)
			if !equalStrings(got, tc.want) {
				t.Fatalf("addedPermissions = %v, want %v", got, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
