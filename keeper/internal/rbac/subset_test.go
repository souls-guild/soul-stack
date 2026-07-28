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
			name:       "caller without * cannot grant *",
			callerRaws: []string{"role.create", "role.grant-operator"},
			required:   []string{"*"},
			wantHeld:   true,
		},
		{
			name:       "caller without the permission cannot grant it",
			callerRaws: []string{"role.create", "role.grant-operator"},
			required:   []string{"operator.create"},
			wantHeld:   true,
		},
		{
			name:       "caller with the permission can grant it",
			callerRaws: []string{"role.create", "soul.list"},
			required:   []string{"soul.list"},
			wantHeld:   false,
		},
		{
			name:       "cluster-admin (*) covers any permission",
			callerRaws: []string{"*"},
			required:   []string{"soul.list", "operator.create", "incarnation.run"},
			wantHeld:   false,
		},
		{
			name:       "cluster-admin (*) covers granting *",
			callerRaws: []string{"*"},
			required:   []string{"*"},
			wantHeld:   false,
		},
		{
			name:       "one uncovered among covered -> denied",
			callerRaws: []string{"soul.list", "incarnation.get"},
			required:   []string{"soul.list", "operator.create"},
			wantHeld:   true,
		},
		{
			name:       "caller's resource-wildcard covers a specific action",
			callerRaws: []string{"incarnation.*"},
			required:   []string{"incarnation.run"},
			wantHeld:   false,
		},
		{
			name:       "caller's specific action does NOT cover resource-wildcard",
			callerRaws: []string{"incarnation.run"},
			required:   []string{"incarnation.*"},
			wantHeld:   true,
		},
		{
			name:       "resource-wildcard does NOT flow to another resource (concrete action)",
			callerRaws: []string{"incarnation.*"},
			required:   []string{"service.register"},
			wantHeld:   true,
		},
		{
			name:       "resource-wildcard owner incarnation.* does NOT grant service.*",
			callerRaws: []string{"incarnation.*"},
			required:   []string{"service.*"},
			wantHeld:   true,
		},
		{
			name:       "resource-wildcard does NOT flow to sensitive role.grant-operator",
			callerRaws: []string{"incarnation.*"},
			required:   []string{"role.grant-operator"},
			wantHeld:   true,
		},
		{
			name:       "resource-wildcard only covers the same resource-wildcard",
			callerRaws: []string{"incarnation.*"},
			required:   []string{"incarnation.*"},
			wantHeld:   false,
		},
		{
			name:       "full * covers a resource-wildcard of any resource",
			callerRaws: []string{"*"},
			required:   []string{"service.*", "role.*", "operator.*"},
			wantHeld:   false,
		},
		{
			name:       "empty required -> always covered (nothing to check)",
			callerRaws: []string{},
			required:   []string{},
			wantHeld:   false,
		},
		{
			name:       "caller with no roles covers nothing",
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
	t.Run("caller with the same selector covers", func(t *testing.T) {
		caller := mustParse(t, "incarnation.run on coven=prod")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod"))
		if errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want covered", err)
		}
	})

	t.Run("caller with no selector covers any selector", func(t *testing.T) {
		// `incarnation.run` (no filter) matches any context (Selector=nil).
		caller := mustParse(t, "incarnation.run")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod,stage"))
		if errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want covered (caller with no filter covers everything)", err)
		}
	})

	t.Run("caller with one value does NOT cover granting two", func(t *testing.T) {
		// Granting `coven=prod,stage` while only holding `coven=prod` is escalation onto stage.
		caller := mustParse(t, "incarnation.run on coven=prod")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod,stage"))
		if !errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want ErrPermissionNotHeld (stage not covered)", err)
		}
	})

	t.Run("caller with both values covers", func(t *testing.T) {
		caller := mustParse(t, "incarnation.run on coven=prod,stage")
		err := assertCallerCovers(caller, mustParse(t, "incarnation.run on coven=prod,stage"))
		if errors.Is(err, ErrPermissionNotHeld) {
			t.Fatalf("err = %v, want covered", err)
		}
	})
}

// TestWidenedRights is the diff every write gate on UpdateRolePermissions reads:
// what a mutation newly puts into a role, measured on the rights it grants rather
// than on the rows it stores.
//
// The two rows that make the difference are the last ones. A ceiling lifted off
// unchanged rows is a widening that a row diff reports as ∅ — the NIM-230
// escalation — and a scope narrowed onto unchanged rows is a trim that a row diff
// would equally have called ∅, correctly, which is why the fix had to be a
// comparison rather than a longer list of fields to watch.
func TestWidenedRights(t *testing.T) {
	tests := []struct {
		name string
		was  []string
		now  []string
		want []string
	}{
		{name: "nothing new", was: []string{"soul.list"}, now: []string{"soul.list"}, want: nil},
		{name: "a right the old form did not grant", was: []string{"soul.list"}, now: []string{"soul.list", "*"}, want: []string{"*"}},
		{name: "removal is not a widening", was: []string{"soul.list", "*"}, now: []string{"soul.list"}, want: nil},
		{name: "replacement reports only the new right", was: []string{"soul.list"}, now: []string{"operator.create"}, want: []string{"operator.create"}},
		{name: "an empty result grants nothing new", was: []string{"*"}, now: nil, want: nil},
		{name: "a wildcard covers everything under it", was: []string{"*"}, now: []string{"soul.list", "incarnation.run"}, want: nil},
		{
			name: "ceiling lifted off unchanged rows",
			was:  []string{"incarnation.run on coven=prod"},
			now:  []string{"incarnation.run"},
			want: []string{"incarnation.run"},
		},
		{
			name: "narrowing the same row is not a widening",
			was:  []string{"incarnation.run"},
			now:  []string{"incarnation.run on coven=prod"},
			want: nil,
		},
		{
			name: "a scope moved sideways is a widening on the new term",
			was:  []string{"incarnation.run on coven=prod"},
			now:  []string{"incarnation.run on coven=staging"},
			want: []string{"incarnation.run on coven=staging"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := permStrings(widenedRights(mustParse(t, tc.was...), mustParse(t, tc.now...)))
			if !equalStrings(got, tc.want) {
				t.Fatalf("widenedRights = %v, want %v", got, tc.want)
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
