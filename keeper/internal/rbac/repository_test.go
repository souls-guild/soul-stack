package rbac

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// snapPool — мульти-Query pool-stub для [LoadSnapshot]: реальный repository
// делает четыре SELECT-а (roles / permissions / membership / revoked), а
// fakeDB из operator-пакета вмещает только один queryFunc. Свой stub —
// один файл, нет общих helper-ов; рутина копируется намеренно.
type snapPool struct {
	roles      []string             // selectRolesSQL (name); default_scope NULL
	roleScopes map[string]string    // ОПЦ.: name → default_scope (ADR-047 S1)
	perms      []rolePermRow        // selectRolePermissionsSQL
	membership []membershipRow      // selectRoleOperatorsSQL
	revoked    []revokedOperatorRow // selectRevokedOperatorsSQL

	synodOps   []synodOperatorRow // selectSynodOperatorsSQL (ADR-049)
	synodRoles []synodRoleRow     // selectSynodRolesSQL (ADR-049)

	// failQuery — если непусто, Query возвращает эту ошибку для SQL,
	// содержащего соответствующую подстроку. Используется для негативных
	// сценариев (loadRevoked возвращает err).
	failQuery map[string]error
}

type rolePermRow struct{ roleName, permission string }
type membershipRow struct{ roleName, aid string }
type revokedOperatorRow struct {
	aid       string
	revokedAt time.Time
}
type synodOperatorRow struct{ synodName, aid string }
type synodRoleRow struct{ synodName, roleName string }

func (p *snapPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("snapPool.Exec: not expected")
}

func (p *snapPool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	for k, err := range p.failQuery {
		if contains(sql, k) {
			return nil, err
		}
	}
	switch {
	case contains(sql, "FROM synod_operators"):
		return &snapSynodOpRows{values: p.synodOps}, nil
	case contains(sql, "FROM synod_roles"):
		return &snapSynodRoleRows{values: p.synodRoles}, nil
	case contains(sql, "FROM rbac_roles"):
		return &snapRoleRows{names: p.roles, scopes: p.roleScopes}, nil
	case contains(sql, "FROM rbac_role_permissions"):
		return &snapPermRows{values: p.perms}, nil
	case contains(sql, "FROM rbac_role_operators"):
		return &snapMembershipRows{values: p.membership}, nil
	case contains(sql, "FROM operators"):
		return &snapRevokedRows{values: p.revoked}, nil
	}
	return nil, errors.New("snapPool.Query: unexpected SQL: " + sql)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// snapRoleRows — для selectRolesSQL (name, default_scope). default_scope
// nullable (ADR-047 S1): scope из scopes-map, отсутствие ключа → NULL (*string=nil).
type snapRoleRows struct {
	names  []string
	scopes map[string]string
	idx    int
}

func (r *snapRoleRows) Next() bool {
	if r.idx >= len(r.names) {
		return false
	}
	r.idx++
	return true
}
func (r *snapRoleRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("snapRoleRows: expected 2 dest (name, default_scope)")
	}
	name := r.names[r.idx-1]
	*(dest[0].(*string)) = name
	scopeDest := dest[1].(**string)
	if s, ok := r.scopes[name]; ok {
		v := s
		*scopeDest = &v
	} else {
		*scopeDest = nil
	}
	return nil
}
func (r *snapRoleRows) Err() error                                   { return nil }
func (r *snapRoleRows) Close()                                       {}
func (r *snapRoleRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *snapRoleRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *snapRoleRows) Values() ([]any, error)                       { return nil, nil }
func (r *snapRoleRows) RawValues() [][]byte                          { return nil }
func (r *snapRoleRows) Conn() *pgx.Conn                              { return nil }

// snapPermRows — для selectRolePermissionsSQL (role_name, permission).
type snapPermRows struct {
	values []rolePermRow
	idx    int
}

func (r *snapPermRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *snapPermRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("snapPermRows: expected 2 dest")
	}
	row := r.values[r.idx-1]
	*(dest[0].(*string)) = row.roleName
	*(dest[1].(*string)) = row.permission
	return nil
}
func (r *snapPermRows) Err() error                                   { return nil }
func (r *snapPermRows) Close()                                       {}
func (r *snapPermRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *snapPermRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *snapPermRows) Values() ([]any, error)                       { return nil, nil }
func (r *snapPermRows) RawValues() [][]byte                          { return nil }
func (r *snapPermRows) Conn() *pgx.Conn                              { return nil }

// snapMembershipRows — для selectRoleOperatorsSQL (role_name, aid).
type snapMembershipRows struct {
	values []membershipRow
	idx    int
}

func (r *snapMembershipRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *snapMembershipRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("snapMembershipRows: expected 2 dest")
	}
	row := r.values[r.idx-1]
	*(dest[0].(*string)) = row.roleName
	*(dest[1].(*string)) = row.aid
	return nil
}
func (r *snapMembershipRows) Err() error                                   { return nil }
func (r *snapMembershipRows) Close()                                       {}
func (r *snapMembershipRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *snapMembershipRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *snapMembershipRows) Values() ([]any, error)                       { return nil, nil }
func (r *snapMembershipRows) RawValues() [][]byte                          { return nil }
func (r *snapMembershipRows) Conn() *pgx.Conn                              { return nil }

// snapRevokedRows — для selectRevokedOperatorsSQL (aid, revoked_at).
type snapRevokedRows struct {
	values []revokedOperatorRow
	idx    int
}

func (r *snapRevokedRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *snapRevokedRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("snapRevokedRows: expected 2 dest")
	}
	row := r.values[r.idx-1]
	*(dest[0].(*string)) = row.aid
	*(dest[1].(*time.Time)) = row.revokedAt
	return nil
}
func (r *snapRevokedRows) Err() error                                   { return nil }
func (r *snapRevokedRows) Close()                                       {}
func (r *snapRevokedRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *snapRevokedRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *snapRevokedRows) Values() ([]any, error)                       { return nil, nil }
func (r *snapRevokedRows) RawValues() [][]byte                          { return nil }
func (r *snapRevokedRows) Conn() *pgx.Conn                              { return nil }

// snapSynodOpRows — для selectSynodOperatorsSQL (synod_name, aid) (ADR-049).
type snapSynodOpRows struct {
	values []synodOperatorRow
	idx    int
}

func (r *snapSynodOpRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *snapSynodOpRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("snapSynodOpRows: expected 2 dest")
	}
	row := r.values[r.idx-1]
	*(dest[0].(*string)) = row.synodName
	*(dest[1].(*string)) = row.aid
	return nil
}
func (r *snapSynodOpRows) Err() error                                   { return nil }
func (r *snapSynodOpRows) Close()                                       {}
func (r *snapSynodOpRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *snapSynodOpRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *snapSynodOpRows) Values() ([]any, error)                       { return nil, nil }
func (r *snapSynodOpRows) RawValues() [][]byte                          { return nil }
func (r *snapSynodOpRows) Conn() *pgx.Conn                              { return nil }

// snapSynodRoleRows — для selectSynodRolesSQL (synod_name, role_name) (ADR-049).
type snapSynodRoleRows struct {
	values []synodRoleRow
	idx    int
}

func (r *snapSynodRoleRows) Next() bool {
	if r.idx >= len(r.values) {
		return false
	}
	r.idx++
	return true
}
func (r *snapSynodRoleRows) Scan(dest ...any) error {
	if len(dest) != 2 {
		return errors.New("snapSynodRoleRows: expected 2 dest")
	}
	row := r.values[r.idx-1]
	*(dest[0].(*string)) = row.synodName
	*(dest[1].(*string)) = row.roleName
	return nil
}
func (r *snapSynodRoleRows) Err() error                                   { return nil }
func (r *snapSynodRoleRows) Close()                                       {}
func (r *snapSynodRoleRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *snapSynodRoleRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *snapSynodRoleRows) Values() ([]any, error)                       { return nil, nil }
func (r *snapSynodRoleRows) RawValues() [][]byte                          { return nil }
func (r *snapSynodRoleRows) Conn() *pgx.Conn                              { return nil }

// TestLoadSnapshot_IncludesRevoked — ADR-014 Amendment 2026-05-27: четвёртая
// проекция Snapshot.Revoked заполняется AID-ами ревокнутых Архонтов из
// `operators`. Активные операторы в проекции отсутствуют (WHERE
// revoked_at IS NOT NULL на стороне SQL).
func TestLoadSnapshot_IncludesRevoked(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-24 * time.Hour)
	pool := &snapPool{
		roles: []string{"cluster-admin"},
		perms: []rolePermRow{{"cluster-admin", "*"}},
		membership: []membershipRow{
			{"cluster-admin", "archon-alice"},
			{"cluster-admin", "archon-bob"},
		},
		revoked: []revokedOperatorRow{
			{"archon-bob", now},
			{"archon-carol", earlier},
		},
	}

	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got := len(snap.Revoked); got != 2 {
		t.Fatalf("Revoked size = %d, want 2", got)
	}
	gotBob, ok := snap.Revoked["archon-bob"]
	if !ok || !gotBob.Equal(now) {
		t.Errorf("Revoked[archon-bob] = (%v, %v), want (%v, true)", gotBob, ok, now)
	}
	gotCarol, ok := snap.Revoked["archon-carol"]
	if !ok || !gotCarol.Equal(earlier) {
		t.Errorf("Revoked[archon-carol] = (%v, %v), want (%v, true)", gotCarol, ok, earlier)
	}
	if _, ok := snap.Revoked["archon-alice"]; ok {
		t.Errorf("Revoked[archon-alice] = true, want false (active operator не в выборке)")
	}
}

// TestLoadSnapshot_RevokedEmpty — Snapshot.Revoked инициализирован пустой
// map-ой даже при отсутствии revoked-операторов (nil ломал бы read-side
// Enforcer.Check на map-lookup).
func TestLoadSnapshot_RevokedEmpty(t *testing.T) {
	pool := &snapPool{
		roles: []string{"cluster-admin"},
		perms: []rolePermRow{{"cluster-admin", "*"}},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snap.Revoked == nil {
		t.Errorf("Revoked = nil, want пустая map (read-side ожидает non-nil)")
	}
	if len(snap.Revoked) != 0 {
		t.Errorf("Revoked size = %d, want 0", len(snap.Revoked))
	}
}

// TestLoadSnapshot_RoleScopes — ADR-047 S1: default_scope читается в
// Snapshot.RoleScopes. NULL (роль без scope) в проекцию НЕ попадает —
// отсутствие ключа = измерение не введено (backcompat).
func TestLoadSnapshot_RoleScopes(t *testing.T) {
	pool := &snapPool{
		roles:      []string{"prod-ops", "free-ops"},
		roleScopes: map[string]string{"prod-ops": "coven=prod"},
		perms: []rolePermRow{
			{"prod-ops", "incarnation.run"},
			{"free-ops", "incarnation.run"},
		},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got := snap.RoleScopes["prod-ops"]; got != "coven=prod" {
		t.Errorf("RoleScopes[prod-ops] = %q, want %q", got, "coven=prod")
	}
	if _, ok := snap.RoleScopes["free-ops"]; ok {
		t.Errorf("RoleScopes[free-ops] present, want absent (NULL default_scope)")
	}
}

// TestLoadSnapshot_RevokedQueryError — ошибка на четвёртом SELECT-е (revoked)
// пробрасывается caller-у, ранее загруженные roles/membership не «полу-
// валидируют» снимок.
func TestLoadSnapshot_RevokedQueryError(t *testing.T) {
	pool := &snapPool{
		roles:     []string{"cluster-admin"},
		perms:     []rolePermRow{{"cluster-admin", "*"}},
		failQuery: map[string]error{"FROM operators": errors.New("connection reset")},
	}
	_, err := LoadSnapshot(context.Background(), pool)
	if err == nil {
		t.Fatal("LoadSnapshot: err = nil, want connection reset")
	}
}

// --- Synod (ADR-049): эффективные роли архона = прямые ∪ через Synod ---

// sortedStrings — копия слайса в отсортированном виде (Membership-список ролей
// не гарантирует порядок; тесты сверяют множество).
func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestLoadSnapshot_SynodRolesUnioned — архон в Synod, у которого роль R,
// получает R в Membership (как если бы роль была прямой). ADR-049(c)/(e).
func TestLoadSnapshot_SynodRolesUnioned(t *testing.T) {
	pool := &snapPool{
		roles: []string{"prod-ops"},
		perms: []rolePermRow{{"prod-ops", "incarnation.run"}},
		// Прямых membership-строк нет — роль приходит ТОЛЬКО через Synod.
		synodOps:   []synodOperatorRow{{"team-prod", "archon-alice"}},
		synodRoles: []synodRoleRow{{"team-prod", "prod-ops"}},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	got := sortedStrings(snap.Membership["archon-alice"])
	if len(got) != 1 || got[0] != "prod-ops" {
		t.Fatalf("Membership[archon-alice] = %v, want [prod-ops]", got)
	}

	// Сквозной резолв: enforcer выдаёт permission роли через Synod так же,
	// как через прямой грант (Check / ResolvePurview / PermissionsOf).
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-alice", "incarnation", "run", nil); err != nil {
		t.Errorf("Check via Synod: %v, want nil", err)
	}
	if p := e.ResolvePurview("archon-alice", "incarnation", "run"); !p.Unrestricted {
		t.Errorf("ResolvePurview via Synod = %+v, want Unrestricted (bare-perm роли без default_scope)", p)
	}
	perms := e.PermissionsOf("archon-alice")
	if len(perms) != 1 || perms[0].Resource != "incarnation" || perms[0].Action != "run" {
		t.Errorf("PermissionsOf via Synod = %+v, want [{incarnation run}]", perms)
	}
}

// TestLoadSnapshot_SynodScopeUnion — union scope-ов: прямая роль (scope=staging)
// + роль через Synod (scope=prod) → Purview.Covens = {prod, staging} (union, не
// пересечение). ADR-047 union default_scope нескольких ролей + ADR-049.
func TestLoadSnapshot_SynodScopeUnion(t *testing.T) {
	pool := &snapPool{
		roles:      []string{"staging-ops", "prod-ops"},
		roleScopes: map[string]string{"staging-ops": "coven=staging", "prod-ops": "coven=prod"},
		perms: []rolePermRow{
			{"staging-ops", "incarnation.run"},
			{"prod-ops", "incarnation.run"},
		},
		// staging-ops — напрямую; prod-ops — через Synod.
		membership: []membershipRow{{"staging-ops", "archon-alice"}},
		synodOps:   []synodOperatorRow{{"team-prod", "archon-alice"}},
		synodRoles: []synodRoleRow{{"team-prod", "prod-ops"}},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	p := e.ResolvePurview("archon-alice", "incarnation", "run")
	if p.Unrestricted {
		t.Fatalf("Purview Unrestricted, want scoped (обе роли с default_scope)")
	}
	got := sortedStrings(p.Covens)
	if len(got) != 2 || got[0] != "prod" || got[1] != "staging" {
		t.Errorf("Purview.Covens = %v, want [prod staging] (union прямой + через Synod)", got)
	}
}

// TestLoadSnapshot_SynodRoleDedup — одна роль и напрямую, и через Synod → в
// Membership не двоится (union множества, не мультимножество). ADR-049(c).
func TestLoadSnapshot_SynodRoleDedup(t *testing.T) {
	pool := &snapPool{
		roles:      []string{"prod-ops"},
		perms:      []rolePermRow{{"prod-ops", "incarnation.run"}},
		membership: []membershipRow{{"prod-ops", "archon-alice"}},
		synodOps:   []synodOperatorRow{{"team-prod", "archon-alice"}},
		synodRoles: []synodRoleRow{{"team-prod", "prod-ops"}},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	got := snap.Membership["archon-alice"]
	if len(got) != 1 || got[0] != "prod-ops" {
		t.Errorf("Membership[archon-alice] = %v, want [prod-ops] (дедуп прямой+Synod)", got)
	}
}

// TestLoadSnapshot_SynodMultipleSynods — архон в двух Synod, разные роли →
// union обеих. Дубль роли через два Synod-а идемпотентен. ADR-049(c).
func TestLoadSnapshot_SynodMultipleSynods(t *testing.T) {
	pool := &snapPool{
		roles: []string{"role-a", "role-b"},
		perms: []rolePermRow{
			{"role-a", "soul.list"},
			{"role-b", "incarnation.run"},
		},
		synodOps: []synodOperatorRow{
			{"team-1", "archon-alice"},
			{"team-2", "archon-alice"},
		},
		synodRoles: []synodRoleRow{
			{"team-1", "role-a"},
			{"team-2", "role-b"},
			{"team-2", "role-a"}, // дубль role-a через второй Synod
		},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	got := sortedStrings(snap.Membership["archon-alice"])
	if len(got) != 2 || got[0] != "role-a" || got[1] != "role-b" {
		t.Errorf("Membership[archon-alice] = %v, want [role-a role-b] (union двух Synod, дедуп)", got)
	}
}

// TestLoadSnapshot_SynodRevokedShortcut — ревокнутый архон, у которого `*`
// приходит ТОЛЬКО через Synod, всё равно получает revoked-shortcut: Check
// возвращает ErrOperatorRevoked раньше групповых ролей. Revoked-проекция от
// Synod не зависит — гарантия, что групповой путь не обходит revoke.
func TestLoadSnapshot_SynodRevokedShortcut(t *testing.T) {
	revokedAt := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	pool := &snapPool{
		roles:      []string{"cluster-admin"},
		perms:      []rolePermRow{{"cluster-admin", "*"}},
		synodOps:   []synodOperatorRow{{"team-admins", "archon-fired"}},
		synodRoles: []synodRoleRow{{"team-admins", "cluster-admin"}},
		revoked:    []revokedOperatorRow{{"archon-fired", revokedAt}},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-fired", "operator", "create", nil); !errors.Is(err, ErrOperatorRevoked) {
		t.Errorf("Check(revoked via Synod) = %v, want ErrOperatorRevoked", err)
	}
	if p := e.PermissionsOf("archon-fired"); p != nil {
		t.Errorf("PermissionsOf(revoked via Synod) = %+v, want nil", p)
	}
}

// TestLoadSnapshot_SynodRemovalDropsRoles — убрать архона из synod_operators
// (пересборка snapshot без его строки) → права через эту группу пропадают.
// Моделирует synod.remove-operator + пересборку снимка.
func TestLoadSnapshot_SynodRemovalDropsRoles(t *testing.T) {
	ctx := context.Background()
	base := func(withMembership bool) *snapPool {
		p := &snapPool{
			roles:      []string{"prod-ops"},
			perms:      []rolePermRow{{"prod-ops", "incarnation.run"}},
			synodRoles: []synodRoleRow{{"team-prod", "prod-ops"}},
		}
		if withMembership {
			p.synodOps = []synodOperatorRow{{"team-prod", "archon-alice"}}
		}
		return p
	}

	snapBefore, err := LoadSnapshot(ctx, base(true))
	if err != nil {
		t.Fatalf("LoadSnapshot before: %v", err)
	}
	eBefore, _ := NewEnforcerFromSnapshot(snapBefore)
	if err := eBefore.Check("archon-alice", "incarnation", "run", nil); err != nil {
		t.Fatalf("before removal Check: %v, want nil", err)
	}

	// Архон убран из synod_operators — пересобираем snapshot.
	snapAfter, err := LoadSnapshot(ctx, base(false))
	if err != nil {
		t.Fatalf("LoadSnapshot after: %v", err)
	}
	if got := snapAfter.Membership["archon-alice"]; got != nil {
		t.Errorf("Membership[archon-alice] after removal = %v, want nil", got)
	}
	eAfter, _ := NewEnforcerFromSnapshot(snapAfter)
	if err := eAfter.Check("archon-alice", "incarnation", "run", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("after removal Check = %v, want ErrPermissionDenied", err)
	}
}

// TestLoadSnapshot_SynodDanglingRole — synod_roles ссылается на роль вне
// каталога (рассинхрон) → enforcer её игнорирует (та же защита, что
// dangling-membership rbac_role_operators). Membership-имя при этом попадает,
// но NewEnforcerFromSnapshot отбрасывает несуществующую роль.
func TestLoadSnapshot_SynodDanglingRole(t *testing.T) {
	pool := &snapPool{
		roles:      []string{"prod-ops"},
		perms:      []rolePermRow{{"prod-ops", "incarnation.run"}},
		synodOps:   []synodOperatorRow{{"team-prod", "archon-alice"}},
		synodRoles: []synodRoleRow{{"team-prod", "ghost-role"}},
	}
	snap, err := LoadSnapshot(context.Background(), pool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if err := e.Check("archon-alice", "incarnation", "run", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("dangling Synod role should grant nothing: %v", err)
	}
}

// TestLoadSnapshot_SynodQueryError — ошибка на SELECT-е synod_operators
// пробрасывается caller-у (как loadRevoked-ошибка): полу-собранный снимок не
// валиден.
func TestLoadSnapshot_SynodQueryError(t *testing.T) {
	pool := &snapPool{
		roles:     []string{"cluster-admin"},
		perms:     []rolePermRow{{"cluster-admin", "*"}},
		failQuery: map[string]error{"FROM synod_operators": errors.New("connection reset")},
	}
	if _, err := LoadSnapshot(context.Background(), pool); err == nil {
		t.Fatal("LoadSnapshot: err = nil, want connection reset")
	}
}
