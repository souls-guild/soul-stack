package rbac

import (
	"errors"
	"testing"
)

// Spec for renaming the `incarnation.update` permission to
// `incarnation.update-hosts` (PM-decision 2026-06-02): clearer name + room
// for future update-covens/update-spec. The `PATCH /v1/incarnations/
// {name}/hosts` endpoint is now gated by the new `incarnation.update-hosts`.
//
// Backcompat invariant: roles/operators still holding the OLD
// `incarnation.update` (in keeper.yml / DB) must NOT lose access — the old
// name is a deprecated alias for the canonical `incarnation.update-hosts`
// (parse-time canonicalization).

// TestEnforcer_UpdateHosts_NewPermissionGrants — a role with the new
// canonical name has access to update-hosts.
func TestEnforcer_UpdateHosts_NewPermissionGrants(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "host-editor",
		operators:   []string{"archon-alice"},
		permissions: []string{"incarnation.update-hosts"},
	})
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-alice", "incarnation", "update-hosts", nil); err != nil {
		t.Errorf("incarnation.update-hosts should grant update-hosts: %v", err)
	}
}

// TestEnforcer_UpdateHosts_DeprecatedAliasGrants — a role with the OLD
// `incarnation.update` still has access to update-hosts (backcompat —
// existing roles aren't locked out).
func TestEnforcer_UpdateHosts_DeprecatedAliasGrants(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "legacy-editor",
		operators:   []string{"archon-bob"},
		permissions: []string{"incarnation.update"},
	})
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-bob", "incarnation", "update-hosts", nil); err != nil {
		t.Errorf("deprecated incarnation.update must still grant update-hosts: %v", err)
	}
}

// TestEnforcer_UpdateHosts_DeprecatedAliasPreservesSelector — the alias
// preserves the scope selector: `incarnation.update on coven=prod` gates
// update-hosts only in coven=prod.
func TestEnforcer_UpdateHosts_DeprecatedAliasPreservesSelector(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "legacy-prod-editor",
		operators:   []string{"archon-carol"},
		permissions: []string{"incarnation.update on coven=prod"},
	})
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-carol", "incarnation", "update-hosts", map[string]string{"coven": "prod"}); err != nil {
		t.Errorf("alias must preserve selector (coven=prod allowed): %v", err)
	}
	err = e.Check("archon-carol", "incarnation", "update-hosts", map[string]string{"coven": "staging"})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("alias must preserve selector (coven=staging denied): %v, want ErrPermissionDenied", err)
	}
}

// TestEnforcer_UpdateHosts_BareWithoutEitherDenied — a role without either
// update/update-hosts is denied.
func TestEnforcer_UpdateHosts_BareWithoutEitherDenied(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "reader",
		operators:   []string{"archon-dave"},
		permissions: []string{"incarnation.get"},
	})
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	err = e.Check("archon-dave", "incarnation", "update-hosts", nil)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("role without update/update-hosts must be denied: %v, want ErrPermissionDenied", err)
	}
}

// TestParsePermission_DeprecatedUpdateCanonicalized — the old name is
// canonicalized to the new one on parse (Action="update-hosts"), so Matches
// stays a plain string comparison and the router only needs to know the new
// name.
func TestParsePermission_DeprecatedUpdateCanonicalized(t *testing.T) {
	p, err := ParsePermission("incarnation.update")
	if err != nil {
		t.Fatalf("ParsePermission(incarnation.update): %v", err)
	}
	if p.Resource != "incarnation" || p.Action != "update-hosts" {
		t.Errorf("alias not canonicalized: Resource=%q Action=%q, want incarnation/update-hosts", p.Resource, p.Action)
	}
}

// TestParsePermission_DeprecatedUpdateStillValid — the old name remains
// valid in the catalog (closed enum, names are never removed): loading
// keeper.yml / a DB snapshot with `incarnation.update` must NOT fail.
func TestParsePermission_DeprecatedUpdateStillValid(t *testing.T) {
	if !IsAllowedPermission("incarnation", "update") {
		t.Errorf("deprecated incarnation.update must remain in catalog (closed enum, no removal)")
	}
	if !IsAllowedPermission("incarnation", "update-hosts") {
		t.Errorf("incarnation.update-hosts must be in catalog")
	}
}
