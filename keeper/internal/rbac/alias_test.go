package rbac

import (
	"errors"
	"testing"
)

// Спецификация переименования permission `incarnation.update` →
// `incarnation.update-hosts` (PM-decision 2026-06-02): чёткость имени +
// задел под update-covens/update-spec. Эндпоинт `PATCH /v1/incarnations/
// {name}/hosts` теперь гейтится новым `incarnation.update-hosts`.
//
// Backcompat-инвариант: роли/operators со СТАРЫМ `incarnation.update` (в
// keeper.yml / БД) НЕ должны потерять доступ — старое имя — deprecated-alias
// канонического `incarnation.update-hosts` (parse-time canonicalization).

// TestEnforcer_UpdateHosts_NewPermissionGrants — роль с новым каноническим
// именем имеет доступ к update-hosts.
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

// TestEnforcer_UpdateHosts_DeprecatedAliasGrants — роль со СТАРЫМ
// `incarnation.update` всё ещё имеет доступ к update-hosts (backcompat —
// существующие роли не залочены).
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

// TestEnforcer_UpdateHosts_DeprecatedAliasPreservesSelector — alias сохраняет
// scope-селектор: `incarnation.update on coven=prod` гейтит update-hosts
// только в coven=prod.
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

// TestEnforcer_UpdateHosts_BareWithoutEitherDenied — роль без update/
// update-hosts получает deny.
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

// TestParsePermission_DeprecatedUpdateCanonicalized — старое имя при парсе
// канонизируется в новое (Action="update-hosts"), чтобы Matches остался
// чистым строковым сравнением, а роутер знал только новое имя.
func TestParsePermission_DeprecatedUpdateCanonicalized(t *testing.T) {
	p, err := ParsePermission("incarnation.update")
	if err != nil {
		t.Fatalf("ParsePermission(incarnation.update): %v", err)
	}
	if p.Resource != "incarnation" || p.Action != "update-hosts" {
		t.Errorf("alias not canonicalized: Resource=%q Action=%q, want incarnation/update-hosts", p.Resource, p.Action)
	}
}

// TestParsePermission_DeprecatedUpdateStillValid — старое имя остаётся
// валидным в каталоге (closed enum, removed-имена — never): load keeper.yml /
// БД-снимка с `incarnation.update` НЕ фейлится.
func TestParsePermission_DeprecatedUpdateStillValid(t *testing.T) {
	if !IsAllowedPermission("incarnation", "update") {
		t.Errorf("deprecated incarnation.update must remain in catalog (closed enum, no removal)")
	}
	if !IsAllowedPermission("incarnation", "update-hosts") {
		t.Errorf("incarnation.update-hosts must be in catalog")
	}
}
