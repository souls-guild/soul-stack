package rbac

import (
	"errors"
	"testing"
)

// mustScope parses a default_scope string into a selector (nil for an empty string).
func mustScope(t *testing.T, raw string) map[string][]string {
	t.Helper()
	sc, err := ParseDefaultScope(raw)
	if err != nil {
		t.Fatalf("ParseDefaultScope(%q): %v", raw, err)
	}
	return sc
}

// TestSubset_DefaultScope_Escalation is the least-privilege subset check
// WITH role default_scope on both sides (ADR-047 S1, security fix for
// privilege escalation).
//
// The hole: subset used to compare RAW permission strings without applying
// default_scope. A caller with a role default_scope=coven=prod + bare
// `incarnation.run` has an effective scope of prod, but when granting
// `incarnation.run on coven=staging`, the raw bare perm (Selector==nil)
// covered ANY coven → the grant to staging would go through (escalation).
// Fix: compare EFFECTIVE (resource,action,coven) points instead — the
// caller's bare perm inherits its default_scope, and the granted role's bare
// perm inherits its own default_scope.
func TestSubset_DefaultScope_Escalation(t *testing.T) {
	tests := []struct {
		name string
		// caller: its permissions + the default_scope of the role granting them.
		callerRaws  []string
		callerScope string
		// granted: the granted role's permissions + its default_scope.
		grantedRaws  []string
		grantedScope string
		wantHeld     bool // true → ErrPermissionNotHeld (grant denied)
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
