package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

// --- CRIT-1: account-takeover guard (ADR-058(d) revocation-инвариант усилен) ---

// TestMapper_CRIT1_RejectsExistingOperatorOfOtherMethod — существующий оператор
// под ДРУГИМ auth_method (bootstrap `jwt`, cluster-admin) НЕ может быть присвоен
// federated-логином с совпавшим derived-AID: отказ ErrAuthFailed, JWT не
// выпускается (Map не возвращает MappedOperator), роли не реконсилируются.
//
// Это ядро account-takeover-фикса: без проверки auth_method любой, кто
// контролирует внешний IdP, мог бы выпустить себе uid=archon-alice и присвоить
// сессию bootstrap-админа archon-alice.
func TestMapper_CRIT1_RejectsExistingOperatorOfOtherMethod(t *testing.T) {
	cases := []struct {
		name       string
		authMethod operator.AuthMethod
		createdVia operator.CreatedVia
	}{
		{"bootstrap-jwt-admin", operator.AuthMethodJWT, operator.CreatedViaBootstrap},
		{"system-jwt", operator.AuthMethodJWT, operator.CreatedViaSystem},
		{"user-created-jwt", operator.AuthMethodJWT, operator.CreatedViaUser},
		{"mtls-machine", operator.AuthMethodMTLS, operator.CreatedViaUser},
		{"other-federated-oidc", operator.AuthMethodOIDC, operator.CreatedViaOIDC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeMapperDB{existing: &operator.Operator{
				AID: "archon-alice", DisplayName: "alice",
				AuthMethod: tc.authMethod, CreatedVia: tc.createdVia,
			}}
			aw := &fakeAudit{}
			// LDAP-mapper: обслуживает ТОЛЬКО auth_method=ldap.
			m := newMapper(db, aw, map[string][]string{"ops": {"cluster-admin"}})

			got, err := m.Map(context.Background(), ExternalIdentity{
				AID: "archon-alice", Username: "alice", Groups: []string{"ops"},
			})
			if !errors.Is(err, ErrAuthFailed) {
				t.Fatalf("expected ErrAuthFailed (account-takeover blocked), got err=%v, mapped=%+v", err, got)
			}
			if got.AID != "" || got.Roles != nil {
				t.Errorf("must not return MappedOperator on rejection, got %+v", got)
			}
			if db.grants != 0 {
				t.Errorf("must not reconcile roles on rejection, got %d grants", db.grants)
			}
			if len(db.inserts) != 0 {
				t.Errorf("must not provision on rejection, got %d inserts", len(db.inserts))
			}
		})
	}
}

// TestMapper_CRIT1_AllowsSameMethodOperator — оператор, заведённый ТЕМ ЖЕ
// federated-методом (auth_method=ldap), логинится нормально (контр-кейс к
// CRIT-1: проверка не ломает легитимный путь).
func TestMapper_CRIT1_AllowsSameMethodOperator(t *testing.T) {
	db := &fakeMapperDB{existing: &operator.Operator{
		AID: "archon-alice", DisplayName: "alice",
		AuthMethod: operator.AuthMethodLDAP, CreatedVia: operator.CreatedViaLDAP,
	}}
	m := newMapper(db, &fakeAudit{}, map[string][]string{"ops": {"operator"}})

	got, err := m.Map(context.Background(), ExternalIdentity{
		AID: "archon-alice", Groups: []string{"ops"},
	})
	if err != nil {
		t.Fatalf("same-method operator must log in, got %v", err)
	}
	if got.AID != "archon-alice" || got.Provisioned {
		t.Errorf("unexpected mapped: %+v", got)
	}
}

// --- HIGH-1: scoped role-revoke reconcile (fulfilling ADR-058d) ---

// reconcileTx — fake auth.Txer + pgx.Tx, маршрутизирующий Query/Exec в
// reconcileDB. Считает Commit/Rollback. Делает grant/revoke наблюдаемыми.
type reconcileTx struct {
	db        *reconcileDB
	committed bool
}

func (t *reconcileTx) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return t, nil
}
func (t *reconcileTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *reconcileTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *reconcileTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (t *reconcileTx) Commit(_ context.Context) error   { t.committed = true; return nil }
func (t *reconcileTx) Rollback(_ context.Context) error { return nil }
func (t *reconcileTx) Begin(context.Context) (pgx.Tx, error) {
	panic("reconcileTx.Begin: unused")
}
func (t *reconcileTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("reconcileTx.CopyFrom: unused")
}
func (t *reconcileTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("reconcileTx.SendBatch: unused")
}
func (t *reconcileTx) LargeObjects() pgx.LargeObjects { panic("reconcileTx.LargeObjects: unused") }
func (t *reconcileTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("reconcileTx.Prepare: unused")
}
func (t *reconcileTx) Conn() *pgx.Conn { return nil }

// reconcileDB — fake operator.ExecQueryRower: SelectByAID → existing,
// DirectRolesOf (Query) → currentRoles, Exec захватывает grant/revoke по SQL.
type reconcileDB struct {
	existing     *operator.Operator
	currentRoles []string // прямой membership для DirectRolesOf

	granted []string // role_name из INSERT rbac_role_operators
	revoked []string // role_name из DELETE rbac_role_operators
}

func (d *reconcileDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM rbac_role_operators"):
		// deleteRoleOperatorSQL: ($1 role_name, $2 aid)
		d.revoked = append(d.revoked, toStr(args[0]))
		// RowsAffected=1, чтобы RevokeOperator не вернул ErrRoleOperatorNotFound.
		return pgconn.NewCommandTag("DELETE 1"), nil
	case strings.Contains(sql, "rbac_role_operators"):
		// insertRoleOperatorSQL: ($1 role_name, $2 aid, $3 granted_by)
		d.granted = append(d.granted, toStr(args[0]))
	}
	return pgconn.CommandTag{}, nil
}

func (d *reconcileDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return &fakeOperatorRow{op: d.existing}
}

func (d *reconcileDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM rbac_role_operators WHERE aid") {
		return &directRoleRows{names: d.currentRoles}, nil
	}
	return nil, errors.New("reconcileDB.Query: unexpected sql")
}

// directRoleRows — pgx.Rows для DirectRolesOf (одна колонка role_name).
type directRoleRows struct {
	names []string
	idx   int
}

func (r *directRoleRows) Next() bool {
	if r.idx >= len(r.names) {
		return false
	}
	r.idx++
	return true
}
func (r *directRoleRows) Scan(dest ...any) error {
	*(dest[0].(*string)) = r.names[r.idx-1]
	return nil
}
func (r *directRoleRows) Err() error                                   { return nil }
func (r *directRoleRows) Close()                                       {}
func (r *directRoleRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *directRoleRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *directRoleRows) Values() ([]any, error)                       { return nil, nil }
func (r *directRoleRows) RawValues() [][]byte                          { return nil }
func (r *directRoleRows) Conn() *pgx.Conn                              { return nil }

// TestMapper_HIGH1_ScopedRoleRevoke — пользователь был [cluster-admin] (из
// группы), сменил группы → теперь маппится в [operator]: cluster-admin СНЯТ,
// operator выдан. Роль, выданная ВНЕ group_role_map (Synod/ручная — здесь
// `manual-extra`), СОХРАНЕНА (вне managed-домена mapper-а).
func TestMapper_HIGH1_ScopedRoleRevoke(t *testing.T) {
	db := &reconcileDB{
		existing: &operator.Operator{
			AID: "archon-bob", DisplayName: "bob",
			AuthMethod: operator.AuthMethodLDAP, CreatedVia: operator.CreatedViaLDAP,
		},
		// Текущий прямой membership: managed cluster-admin (уходит) + managed
		// operator (приходит — уже был? нет, его в current нет) + manual-extra (вне домена).
		currentRoles: []string{"cluster-admin", "manual-extra"},
	}
	tx := &reconcileTx{db: db}

	// group_role_map управляет доменом {cluster-admin, operator}. manual-extra
	// в значениях карты НЕ упомянут → mapper им не управляет.
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"admins": {"cluster-admin"}, "ops": {"operator"}},
		DB:           db,
		Tx:           tx,
		Audit:        &fakeAudit{},
	})

	// Новые группы пользователя: только ops → want=[operator].
	got, err := m.Map(context.Background(), ExternalIdentity{
		AID: "archon-bob", Groups: []string{"ops"},
	})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "operator" {
		t.Fatalf("mapped roles = %v, want [operator]", got.Roles)
	}
	if !tx.committed {
		t.Errorf("reconcile transaction must commit")
	}

	// cluster-admin (managed, ушёл из групп) — СНЯТ.
	if !contains(db.revoked, "cluster-admin") {
		t.Errorf("cluster-admin must be revoked (managed role no longer in groups); revoked=%v", db.revoked)
	}
	// manual-extra (вне managed-домена) — НЕ тронут.
	if contains(db.revoked, "manual-extra") {
		t.Errorf("manual-extra is outside group_role_map domain and must NOT be revoked; revoked=%v", db.revoked)
	}
	// operator (новый из групп) — выдан.
	if !contains(db.granted, "operator") {
		t.Errorf("operator must be granted (new from groups); granted=%v", db.granted)
	}
}

// TestMapper_HIGH1_NoChurnWhenGroupsStable — группы не менялись: роль уже есть в
// прямом membership → НЕ выдаётся повторно, и НЕ снимается (идемпотентность,
// нет лишних мутаций).
func TestMapper_HIGH1_NoChurnWhenGroupsStable(t *testing.T) {
	db := &reconcileDB{
		existing: &operator.Operator{
			AID: "archon-carol", AuthMethod: operator.AuthMethodLDAP,
		},
		currentRoles: []string{"operator", "manual-extra"},
	}
	tx := &reconcileTx{db: db}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodLDAP,
		GroupRoleMap: map[string][]string{"ops": {"operator"}},
		DB:           db, Tx: tx, Audit: &fakeAudit{},
	})

	if _, err := m.Map(context.Background(), ExternalIdentity{
		AID: "archon-carol", Groups: []string{"ops"},
	}); err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(db.granted) != 0 {
		t.Errorf("operator already present → no re-grant; granted=%v", db.granted)
	}
	if len(db.revoked) != 0 {
		t.Errorf("nothing to revoke (operator stays, manual-extra unmanaged); revoked=%v", db.revoked)
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
