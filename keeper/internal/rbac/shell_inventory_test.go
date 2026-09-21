package rbac

import (
	"strings"
	"testing"
)

// The console-gate inventory (NIM-197) answers one operator question during the
// deprecation window: which roles stop reaching a shell once the gate enforces.
func TestShellErrandLegacyRoles(t *testing.T) {
	t.Parallel()
	snap := &Snapshot{Roles: map[string][]string{
		// Breaks: errand.run, no console.
		"ops-runner":   {"errand.run", "soul.list"},
		"scoped-ops":   {"errand.run on coven=prod"},
		"errand-star":  {"errand.*"},
		"no-perm-role": {"soul.list"},
		// Safe: holds both, or holds a wildcard that covers console.
		"shell-ops":     {"errand.run", "soul.console"},
		"soul-star":     {"errand.run", "soul.*"},
		"cluster-admin": {"*"},
	}}
	enf, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}

	got := strings.Join(enf.ShellErrandLegacyRoles(), ",")
	want := "errand-star,ops-runner,scoped-ops"
	if got != want {
		t.Fatalf("ShellErrandLegacyRoles = %q, want %q", got, want)
	}
}

// TestShellErrandLegacyRoles_Empty — a catalog nobody has to fix reports nothing,
// which is what an operator watches the gauge fall to before flipping the gate.
func TestShellErrandLegacyRoles_Empty(t *testing.T) {
	t.Parallel()
	enf, err := NewEnforcerFromSnapshot(&Snapshot{Roles: map[string][]string{
		"shell-ops": {"errand.run", "soul.console"},
	}})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if got := enf.ShellErrandLegacyRoles(); len(got) != 0 {
		t.Fatalf("ShellErrandLegacyRoles = %v, want empty", got)
	}
	var nilEnf *Enforcer
	if got := nilEnf.ShellErrandLegacyRoles(); got != nil {
		t.Fatalf("nil enforcer returned %v, want nil", got)
	}
}
