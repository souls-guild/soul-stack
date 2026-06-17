package rbac

import (
	"errors"
	"sort"
	"testing"
)

// fixtureRole — плоская форма роли для фикстур этого пакета: имя, привязанные
// AID-ы, permission-строки. snapshotOf собирает из неё [Snapshot] (источник
// enforcer-а после hard-cut-а config-RBAC, ADR-028(g)). Внутри пакета rbac
// нельзя использовать rbactest (циклический импорт), поэтому хелпер локальный.
type fixtureRole struct {
	name        string
	operators   []string
	permissions []string
	// defaultScope — role default_scope-строка (ADR-047 S1); пустая = NULL
	// (измерение НЕ введено). Синтаксис как у per-perm-селектора: `coven=v1,v2`.
	defaultScope string
}

func snapshotOf(roles ...fixtureRole) *Snapshot {
	snap := &Snapshot{
		Roles:      make(map[string][]string, len(roles)),
		RoleScopes: make(map[string]string),
		Membership: make(map[string][]string),
	}
	for _, r := range roles {
		snap.Roles[r.name] = r.permissions
		if r.defaultScope != "" {
			snap.RoleScopes[r.name] = r.defaultScope
		}
		for _, aid := range r.operators {
			snap.Membership[aid] = append(snap.Membership[aid], r.name)
		}
	}
	return snap
}

func TestNewEnforcerFromSnapshot_Nil(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(nil)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot(nil): %v", err)
	}
	if err := e.Check("archon-anyone", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("nil-snapshot Check: %v, want ErrPermissionDenied", err)
	}
}

func TestNewEnforcerFromSnapshot_UnknownPermissionFatal(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "bad",
		operators:   []string{"archon-x"},
		permissions: []string{"unknown.create"},
	})
	_, err := NewEnforcerFromSnapshot(snap)
	if err == nil {
		t.Fatal("NewEnforcerFromSnapshot with unknown permission returned nil")
	}
}

func TestEnforcer_Check_WildcardAllowsAll(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name: "cluster-admin", operators: []string{"archon-alice"}, permissions: []string{"*"},
	})
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-alice", "incarnation", "create", nil); err != nil {
		t.Errorf("wildcard should allow: %v", err)
	}
	if err := e.Check("archon-alice", "operator", "revoke", nil); err != nil {
		t.Errorf("wildcard should allow operator.revoke: %v", err)
	}
}

func TestEnforcer_Check_BarePermissionAllow(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name: "creator", operators: []string{"archon-bob"}, permissions: []string{"operator.create"},
	})
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-bob", "operator", "create", nil); err != nil {
		t.Errorf("bare match: %v", err)
	}
}

func TestEnforcer_Check_BarePermissionDeny(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name: "creator", operators: []string{"archon-bob"}, permissions: []string{"operator.create"},
	})
	e, _ := NewEnforcerFromSnapshot(snap)
	err := e.Check("archon-bob", "operator", "revoke", nil)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestEnforcer_Check_ActionWildcard(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "incarnation-mgr",
		operators:   []string{"archon-mgr"},
		permissions: []string{"incarnation.*"},
	})
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-mgr", "incarnation", "create", nil); err != nil {
		t.Errorf("create: %v", err)
	}
	if err := e.Check("archon-mgr", "incarnation", "destroy", nil); err != nil {
		t.Errorf("destroy: %v", err)
	}
	if err := e.Check("archon-mgr", "operator", "create", nil); err == nil {
		t.Errorf("wildcard for incarnation should NOT match operator.create")
	}
}

func TestEnforcer_Check_SelectorAllow(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "db-op",
		operators:   []string{"archon-db"},
		permissions: []string{"incarnation.create on service=redis,vault"},
	})
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-db", "incarnation", "create", map[string]string{"service": "redis"}); err != nil {
		t.Errorf("match service=redis: %v", err)
	}
	if err := e.Check("archon-db", "incarnation", "create", map[string]string{"service": "vault"}); err != nil {
		t.Errorf("match service=vault: %v", err)
	}
}

func TestEnforcer_Check_SelectorDeny(t *testing.T) {
	snap := snapshotOf(fixtureRole{
		name:        "db-op",
		operators:   []string{"archon-db"},
		permissions: []string{"incarnation.create on service=redis"},
	})
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-db", "incarnation", "create", map[string]string{"service": "postgres"}); err == nil {
		t.Errorf("service=postgres should NOT match selector service=redis")
	}
	// Контекст без `service` → deny (permission с селектором требует ключ).
	if err := e.Check("archon-db", "incarnation", "create", nil); err == nil {
		t.Errorf("nil context should NOT match selector permission")
	}
}

func TestEnforcer_Check_MultipleRolesOR(t *testing.T) {
	// Один AID в двух ролях: union permissions.
	snap := snapshotOf(
		fixtureRole{name: "a", operators: []string{"archon-x"}, permissions: []string{"operator.create"}},
		fixtureRole{name: "b", operators: []string{"archon-x"}, permissions: []string{"soul.list"}},
	)
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-x", "operator", "create", nil); err != nil {
		t.Errorf("op.create via role a: %v", err)
	}
	if err := e.Check("archon-x", "soul", "list", nil); err != nil {
		t.Errorf("soul.list via role b: %v", err)
	}
}

func TestEnforcer_HasWildcard(t *testing.T) {
	snap := snapshotOf(
		fixtureRole{name: "admin", operators: []string{"archon-alice"}, permissions: []string{"*"}},
		fixtureRole{name: "ro", operators: []string{"archon-bob"}, permissions: []string{"soul.list"}},
	)
	e, _ := NewEnforcerFromSnapshot(snap)
	if !e.HasWildcard("archon-alice") {
		t.Errorf("alice should have wildcard")
	}
	if e.HasWildcard("archon-bob") {
		t.Errorf("bob should NOT have wildcard")
	}
}

func TestEnforcer_ClusterAdmins(t *testing.T) {
	snap := snapshotOf(
		fixtureRole{name: "admin1", operators: []string{"archon-a"}, permissions: []string{"*"}},
		fixtureRole{name: "admin2", operators: []string{"archon-b", "archon-a"}, permissions: []string{"*"}},
		fixtureRole{name: "ro", operators: []string{"archon-c"}, permissions: []string{"soul.list"}},
	)
	e, _ := NewEnforcerFromSnapshot(snap)
	got := e.ClusterAdmins()
	sort.Strings(got)
	want := []string{"archon-a", "archon-b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEnforcer_RolesOf(t *testing.T) {
	snap := snapshotOf(
		fixtureRole{name: "creator", operators: []string{"archon-x"}, permissions: []string{"operator.create"}},
		fixtureRole{name: "viewer", operators: []string{"archon-x"}, permissions: []string{"soul.list"}},
	)
	e, _ := NewEnforcerFromSnapshot(snap)
	roles := e.RolesOf("archon-x")
	sort.Strings(roles)
	if len(roles) != 2 || roles[0] != "creator" || roles[1] != "viewer" {
		t.Errorf("RolesOf = %v, want [creator viewer]", roles)
	}
	if r := e.RolesOf("archon-ghost"); r != nil {
		t.Errorf("RolesOf(ghost) = %v, want nil", r)
	}
}

func TestEnforcer_Check_EmptyResourceOrAction(t *testing.T) {
	e, _ := NewEnforcerFromSnapshot(nil)
	if err := e.Check("archon-x", "", "create", nil); err == nil {
		t.Errorf("empty resource should error")
	}
	if err := e.Check("archon-x", "operator", "", nil); err == nil {
		t.Errorf("empty action should error")
	}
}
