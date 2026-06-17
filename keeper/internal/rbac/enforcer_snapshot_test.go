package rbac

import (
	"errors"
	"sort"
	"testing"
	"time"
)

// Тест-кейсы Enforcer-от-БД-снимка (ADR-028(d)). Симметричны config-based
// TestEnforcer_* в enforcer_test.go — источник сменён на Snapshot, матчинг
// тот же. Доказывают, что переход config→БД не меняет семантику Check.

func TestEnforcerFromSnapshot_NilSnapshot(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(nil)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot(nil): %v", err)
	}
	if err := e.Check("archon-anyone", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("nil-snapshot Check: %v, want ErrPermissionDenied", err)
	}
}

func TestEnforcerFromSnapshot_UnknownPermissionFatal(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"bad": {"unknown.create"}},
		Membership: map[string][]string{"archon-x": {"bad"}},
	}
	if _, err := NewEnforcerFromSnapshot(snap); err == nil {
		t.Fatal("NewEnforcerFromSnapshot with unknown permission returned nil")
	}
}

func TestEnforcerFromSnapshot_WildcardAllowsAll(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"cluster-admin": {"*"}},
		Membership: map[string][]string{"archon-alice": {"cluster-admin"}},
	}
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

func TestEnforcerFromSnapshot_BarePermission(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"creator": {"operator.create"}},
		Membership: map[string][]string{"archon-bob": {"creator"}},
	}
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-bob", "operator", "create", nil); err != nil {
		t.Errorf("bare match: %v", err)
	}
	if err := e.Check("archon-bob", "operator", "revoke", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("bare deny: %v, want ErrPermissionDenied", err)
	}
}

func TestEnforcerFromSnapshot_ActionWildcard(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"incarnation-mgr": {"incarnation.*"}},
		Membership: map[string][]string{"archon-mgr": {"incarnation-mgr"}},
	}
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-mgr", "incarnation", "create", nil); err != nil {
		t.Errorf("create: %v", err)
	}
	if err := e.Check("archon-mgr", "incarnation", "destroy", nil); err != nil {
		t.Errorf("destroy: %v", err)
	}
	if err := e.Check("archon-mgr", "operator", "create", nil); err == nil {
		t.Errorf("incarnation.* should NOT match operator.create")
	}
}

func TestEnforcerFromSnapshot_Selector(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"db-op": {"incarnation.create on service=redis,vault"}},
		Membership: map[string][]string{"archon-db": {"db-op"}},
	}
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-db", "incarnation", "create", map[string]string{"service": "redis"}); err != nil {
		t.Errorf("match service=redis: %v", err)
	}
	if err := e.Check("archon-db", "incarnation", "create", map[string]string{"service": "vault"}); err != nil {
		t.Errorf("match service=vault: %v", err)
	}
	if err := e.Check("archon-db", "incarnation", "create", map[string]string{"service": "postgres"}); err == nil {
		t.Errorf("service=postgres should NOT match selector service=redis,vault")
	}
	if err := e.Check("archon-db", "incarnation", "create", nil); err == nil {
		t.Errorf("nil context should NOT match selector permission")
	}
}

func TestEnforcerFromSnapshot_MultipleRolesOR(t *testing.T) {
	snap := &Snapshot{
		Roles: map[string][]string{
			"a": {"operator.create"},
			"b": {"soul.list"},
		},
		Membership: map[string][]string{"archon-x": {"a", "b"}},
	}
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-x", "operator", "create", nil); err != nil {
		t.Errorf("op.create via role a: %v", err)
	}
	if err := e.Check("archon-x", "soul", "list", nil); err != nil {
		t.Errorf("soul.list via role b: %v", err)
	}
}

func TestEnforcerFromSnapshot_ClusterAdmins(t *testing.T) {
	snap := &Snapshot{
		Roles: map[string][]string{
			"admin1": {"*"},
			"admin2": {"*"},
			"ro":     {"soul.list"},
		},
		Membership: map[string][]string{
			"archon-a": {"admin1", "admin2"},
			"archon-b": {"admin2"},
			"archon-c": {"ro"},
		},
	}
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

func TestEnforcerFromSnapshot_RolesOf(t *testing.T) {
	snap := &Snapshot{
		Roles: map[string][]string{
			"creator": {"operator.create"},
			"viewer":  {"soul.list"},
		},
		Membership: map[string][]string{"archon-x": {"creator", "viewer"}},
	}
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

func TestEnforcerFromSnapshot_HasWildcard(t *testing.T) {
	snap := &Snapshot{
		Roles: map[string][]string{
			"admin": {"*"},
			"ro":    {"soul.list"},
		},
		Membership: map[string][]string{
			"archon-alice": {"admin"},
			"archon-bob":   {"ro"},
		},
	}
	e, _ := NewEnforcerFromSnapshot(snap)
	if !e.HasWildcard("archon-alice") {
		t.Errorf("alice should have wildcard")
	}
	if e.HasWildcard("archon-bob") {
		t.Errorf("bob should NOT have wildcard")
	}
}

// TestCheck_RevokedAID_Denied — ADR-014 Amendment 2026-05-27: ревокнутый AID
// получает ErrOperatorRevoked даже при наличии `*`-роли. Проверка идёт ПЕРЕД
// permission-логикой — иначе bare `*` пропустил бы revoked AID.
func TestCheck_RevokedAID_Denied(t *testing.T) {
	revokedAt := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	snap := &Snapshot{
		Roles:      map[string][]string{"cluster-admin": {"*"}},
		Membership: map[string][]string{"archon-fired": {"cluster-admin"}},
		Revoked:    map[string]time.Time{"archon-fired": revokedAt},
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	err = e.Check("archon-fired", "operator", "create", nil)
	if !errors.Is(err, ErrOperatorRevoked) {
		t.Fatalf("Check(revoked AID): %v, want ErrOperatorRevoked", err)
	}
	// errors.Is(ErrPermissionDenied) НЕ должен сработать — revoke семантически
	// не «нет прав», а «не доверенный токен» (parity с expired).
	if errors.Is(err, ErrPermissionDenied) {
		t.Errorf("Check(revoked AID): %v неожиданно совпал с ErrPermissionDenied", err)
	}
}

// TestCheck_RevokedAID_DeniedEvenWithoutRoles — revoked AID без ролей всё
// равно даёт ErrOperatorRevoked (а не ErrPermissionDenied «no roles»):
// revoke-проверка идёт ДО проверки наличия ролей.
func TestCheck_RevokedAID_DeniedEvenWithoutRoles(t *testing.T) {
	snap := &Snapshot{
		Roles:   map[string][]string{},
		Revoked: map[string]time.Time{"archon-fired": time.Now()},
	}
	e, _ := NewEnforcerFromSnapshot(snap)
	err := e.Check("archon-fired", "operator", "create", nil)
	if !errors.Is(err, ErrOperatorRevoked) {
		t.Fatalf("Check(revoked AID without roles): %v, want ErrOperatorRevoked", err)
	}
}

// TestCheck_ActiveAIDNotAffectedByRevokedMap — активный AID, чьего ID нет в
// Revoked, проходит как раньше; revoke-проекция не нарушает существующую
// семантику.
func TestCheck_ActiveAIDNotAffectedByRevokedMap(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"admin": {"*"}},
		Membership: map[string][]string{"archon-alice": {"admin"}},
		Revoked:    map[string]time.Time{"archon-bob": time.Now()},
	}
	e, _ := NewEnforcerFromSnapshot(snap)
	if err := e.Check("archon-alice", "operator", "create", nil); err != nil {
		t.Errorf("Check(active AID): %v, want nil", err)
	}
}

// TestEnforcerFromSnapshot_DanglingMembership — membership на роль вне каталога
// игнорируется (защита от рассинхрона; FK в норме это исключает).
func TestEnforcerFromSnapshot_DanglingMembership(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"cluster-admin": {"*"}},
		Membership: map[string][]string{"archon-x": {"ghost-role"}},
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-x", "operator", "create", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("dangling membership should grant nothing: %v", err)
	}
}
