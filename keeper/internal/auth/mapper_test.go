package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// fakeMapperDB — in-memory operator.ExecQueryRower для unit-тестов Mapper-а.
// Различает SELECT (QueryRow по operators) от INSERT/GRANT (Exec) по SQL-тексту.
type fakeMapperDB struct {
	// existing — оператор, который вернёт SelectByAID; nil → ErrOperatorNotFound.
	existing *operator.Operator
	selErr   error

	inserts []*insertCapture // захваченные INSERT operators
	grants  int              // число rbac.GrantOperator
	execErr error
}

type insertCapture struct {
	aid        string
	authMethod string
	createdBy  any
	createdVia any
}

func (f *fakeMapperDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	switch {
	case strings.Contains(sql, "INSERT INTO operators"):
		// args: aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata
		f.inserts = append(f.inserts, &insertCapture{
			aid:        toStr(args[0]),
			authMethod: toStr(args[2]),
			createdBy:  args[4],
			createdVia: args[5],
		})
	case strings.Contains(sql, "rbac_role_operators") || strings.Contains(sql, "INSERT INTO rbac"):
		f.grants++
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeMapperDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return &fakeOperatorRow{op: f.existing, err: f.selErr}
}

func (f *fakeMapperDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("Query not used by Mapper")
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// fakeOperatorRow — pgx.Row, отдающий поля Operator-а в порядке selectOperatorByAIDSQL.
type fakeOperatorRow struct {
	op  *operator.Operator
	err error
}

func (r *fakeOperatorRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.op == nil {
		return pgx.ErrNoRows
	}
	// порядок: aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata
	createdVia := r.op.CreatedVia
	if createdVia == "" {
		createdVia = operator.CreatedViaUser
	}
	*(dest[0].(*string)) = r.op.AID
	*(dest[1].(*string)) = r.op.DisplayName
	*(dest[2].(*string)) = string(r.op.AuthMethod)
	*(dest[3].(*time.Time)) = r.op.CreatedAt
	*(dest[4].(**string)) = r.op.CreatedByAID
	*(dest[5].(*string)) = createdVia
	*(dest[6].(**time.Time)) = r.op.RevokedAt
	*(dest[7].(*[]byte)) = []byte("{}")
	return nil
}

// fakeAudit — in-memory audit.Writer, копит записанные события.
type fakeAudit struct {
	events []*audit.Event
}

func (a *fakeAudit) Write(_ context.Context, ev *audit.Event) error {
	a.events = append(a.events, ev)
	return nil
}

func (a *fakeAudit) has(t audit.EventType) bool {
	for _, e := range a.events {
		if e.EventType == t {
			return true
		}
	}
	return false
}

func newMapper(db operator.ExecQueryRower, aw audit.Writer, grm map[string][]string) *DBMapper {
	return NewMapper(MapperConfig{Method: operator.AuthMethodLDAP, GroupRoleMap: grm, DB: db, Audit: aw})
}

// TestMapper_HappyProvision — юзер в группе → auto-provision (Insert вызван с
// auth_method=ldap), роли из групп, JWT-вызова тут нет (он в endpoint).
// Provisioned=true, audit operator.provisioned записан.
func TestMapper_HappyProvision(t *testing.T) {
	db := &fakeMapperDB{existing: nil} // SelectByAID → not found
	aw := &fakeAudit{}
	m := newMapper(db, aw, map[string][]string{"ops": {"cluster-admin"}})

	got, err := m.Map(context.Background(), ExternalIdentity{
		AID:      "alice",
		Username: "alice",
		Groups:   []string{"ops"},
	})
	if err != nil {
		t.Fatalf("Map: unexpected error: %v", err)
	}
	if !got.Provisioned {
		t.Fatalf("expected Provisioned=true")
	}
	if len(db.inserts) != 1 {
		t.Fatalf("expected exactly 1 Insert, got %d", len(db.inserts))
	}
	if db.inserts[0].authMethod != string(operator.AuthMethodLDAP) {
		t.Errorf("Insert auth_method = %q, want ldap", db.inserts[0].authMethod)
	}
	// ADR-058(d): federated-provision пишет created_by_aid=NULL (нет
	// оператора-инициатора) + created_via='ldap'. Раньше тут стоял
	// reserved-AID archon-system из-за старого bootstrap-индекса; индекс
	// перенесён на created_via='bootstrap' (миграция 085), NULL легален.
	if db.inserts[0].createdBy != nil {
		t.Errorf("Insert created_by_aid = %v, want nil (federated, no initiator)", db.inserts[0].createdBy)
	}
	if db.inserts[0].createdVia != operator.CreatedViaLDAP {
		t.Errorf("Insert created_via = %v, want %q", db.inserts[0].createdVia, operator.CreatedViaLDAP)
	}
	if got.Roles == nil || got.Roles[0] != "cluster-admin" {
		t.Errorf("roles = %v, want [cluster-admin]", got.Roles)
	}
	if !aw.has(audit.EventOperatorProvisioned) {
		t.Errorf("expected operator.provisioned audit event")
	}
	if db.grants < 1 {
		t.Errorf("expected at least 1 role grant, got %d", db.grants)
	}
}

// TestMapper_TwoFederatedProvisionsBothNullCreatedBy — ADR-058(d) guard (кейс 3):
// два разных federated-оператора провижинятся подряд, оба с created_by_aid=NULL.
// До ADR-058(d) это было невозможно: partial unique index `operators_first_archon_idx`
// держался на `created_by_aid IS NULL` → второй NULL ловился как UNIQUE-violation.
// После переноса индекса на created_via='bootstrap' (миграция 085) NULL у
// created_by_aid легален для federated-строк — оба provision формируют
// created_by_aid=nil + created_via=ldap.
func TestMapper_TwoFederatedProvisionsBothNullCreatedBy(t *testing.T) {
	db := &fakeMapperDB{existing: nil} // оба SELECT → not found → provision
	aw := &fakeAudit{}
	m := newMapper(db, aw, map[string][]string{"ops": {"read-only"}})

	for _, aid := range []string{"alice", "bob"} {
		got, err := m.Map(context.Background(), ExternalIdentity{AID: aid, Username: aid, Groups: []string{"ops"}})
		if err != nil {
			t.Fatalf("Map(%q): %v", aid, err)
		}
		if !got.Provisioned {
			t.Errorf("Map(%q): Provisioned=false, want true", aid)
		}
	}
	if len(db.inserts) != 2 {
		t.Fatalf("expected 2 federated Inserts, got %d", len(db.inserts))
	}
	for i, ins := range db.inserts {
		if ins.createdBy != nil {
			t.Errorf("insert[%d] created_by_aid = %v, want nil (federated, no initiator)", i, ins.createdBy)
		}
		if ins.createdVia != operator.CreatedViaLDAP {
			t.Errorf("insert[%d] created_via = %v, want %q", i, ins.createdVia, operator.CreatedViaLDAP)
		}
	}
}

// TestMapper_NoRoleMappingDoesNotProvision — вне-групп юзер → ErrNoRoleMapping,
// оператор НЕ создан (Insert НЕ вызван).
func TestMapper_NoRoleMappingDoesNotProvision(t *testing.T) {
	db := &fakeMapperDB{existing: nil}
	aw := &fakeAudit{}
	m := newMapper(db, aw, map[string][]string{"ops": {"cluster-admin"}})

	_, err := m.Map(context.Background(), ExternalIdentity{
		AID:    "bob",
		Groups: []string{"interns"}, // не пересекает group_role_map
	})
	if !errors.Is(err, ErrNoRoleMapping) {
		t.Fatalf("expected ErrNoRoleMapping, got %v", err)
	}
	if len(db.inserts) != 0 {
		t.Fatalf("expected 0 Inserts (no provision for ungrouped), got %d", len(db.inserts))
	}
	if aw.has(audit.EventOperatorProvisioned) {
		t.Errorf("must NOT provision ungrouped user")
	}
}

// TestMapper_RevokedRejected — SelectByAID вернул revoked-оператора →
// ErrOperatorRevoked, Insert НЕ вызван.
func TestMapper_RevokedRejected(t *testing.T) {
	revoked := time.Now()
	db := &fakeMapperDB{existing: &operator.Operator{
		AID: "carol", DisplayName: "carol", AuthMethod: operator.AuthMethodLDAP,
		RevokedAt: &revoked,
	}}
	aw := &fakeAudit{}
	m := newMapper(db, aw, map[string][]string{"ops": {"cluster-admin"}})

	_, err := m.Map(context.Background(), ExternalIdentity{AID: "carol", Groups: []string{"ops"}})
	if !errors.Is(err, ErrOperatorRevoked) {
		t.Fatalf("expected ErrOperatorRevoked, got %v", err)
	}
	if len(db.inserts) != 0 {
		t.Errorf("revoked operator must not be re-provisioned")
	}
}

// TestMapper_ExistingActiveNoInsert — существующий активный оператор →
// Provisioned=false, Insert НЕ вызван, роли из group_role_map.
func TestMapper_ExistingActiveNoInsert(t *testing.T) {
	db := &fakeMapperDB{existing: &operator.Operator{
		AID: "dave", DisplayName: "dave", AuthMethod: operator.AuthMethodLDAP,
	}}
	aw := &fakeAudit{}
	m := newMapper(db, aw, map[string][]string{"ops": {"read-only"}})

	got, err := m.Map(context.Background(), ExternalIdentity{AID: "dave", Groups: []string{"ops"}})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if got.Provisioned {
		t.Errorf("existing operator must have Provisioned=false")
	}
	if len(db.inserts) != 0 {
		t.Errorf("existing operator must not be re-inserted, got %d inserts", len(db.inserts))
	}
	if got.Roles == nil || got.Roles[0] != "read-only" {
		t.Errorf("roles = %v, want [read-only]", got.Roles)
	}
	if aw.has(audit.EventOperatorProvisioned) {
		t.Errorf("existing operator must NOT emit provisioned")
	}
}

// TestMapper_InvalidAIDIsOpaque — невалидный AID → ErrAuthFailed (anti-oracle),
// без утечки причины.
func TestMapper_InvalidAIDIsOpaque(t *testing.T) {
	db := &fakeMapperDB{}
	m := newMapper(db, &fakeAudit{}, map[string][]string{"ops": {"cluster-admin"}})
	_, err := m.Map(context.Background(), ExternalIdentity{AID: "BAD/AID", Groups: []string{"ops"}})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed for invalid AID, got %v", err)
	}
}

// TestMapper_RolesDedupSorted — несколько групп с пересекающимися ролями →
// дедуп + стабильный порядок.
func TestMapper_RolesDedupSorted(t *testing.T) {
	db := &fakeMapperDB{existing: &operator.Operator{AID: "eve", DisplayName: "eve", AuthMethod: operator.AuthMethodLDAP}}
	m := newMapper(db, &fakeAudit{}, map[string][]string{
		"ops":    {"cluster-admin", "read-only"},
		"admins": {"cluster-admin"},
	})
	got, err := m.Map(context.Background(), ExternalIdentity{AID: "eve", Groups: []string{"ops", "admins"}})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	want := []string{"cluster-admin", "read-only"}
	if len(got.Roles) != len(want) {
		t.Fatalf("roles = %v, want %v", got.Roles, want)
	}
	for i := range want {
		if got.Roles[i] != want[i] {
			t.Fatalf("roles[%d] = %q, want %q (sorted, deduped)", i, got.Roles[i], want[i])
		}
	}
}

// TestMapper_OIDCProvisionStampsOIDC — ADR-058 стадия 2 guard: mapper с
// Method=oidc провижинит оператора с auth_method=oidc И created_via=oidc
// (источник атрибутируется методом, не хардкодом ldap). Доказывает generalize
// mapper-а под оба метода.
func TestMapper_OIDCProvisionStampsOIDC(t *testing.T) {
	db := &fakeMapperDB{existing: nil} // not found → provision
	aw := &fakeAudit{}
	m := NewMapper(MapperConfig{
		Method:       operator.AuthMethodOIDC,
		GroupRoleMap: map[string][]string{"ops": {"cluster-admin"}},
		DB:           db,
		Audit:        aw,
	})

	got, err := m.Map(context.Background(), ExternalIdentity{AID: "alice", Username: "alice", Groups: []string{"ops"}})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !got.Provisioned {
		t.Fatal("expected Provisioned=true")
	}
	if len(db.inserts) != 1 {
		t.Fatalf("expected 1 Insert, got %d", len(db.inserts))
	}
	if db.inserts[0].authMethod != string(operator.AuthMethodOIDC) {
		t.Errorf("Insert auth_method = %q, want oidc", db.inserts[0].authMethod)
	}
	if db.inserts[0].createdVia != operator.CreatedViaOIDC {
		t.Errorf("Insert created_via = %v, want %q", db.inserts[0].createdVia, operator.CreatedViaOIDC)
	}
}

// TestMapper_EmptyMethodRejected — defense-in-depth guard: mapper без явного
// Method не должен молча создавать оператора (ErrAuthFailed, Insert не вызван).
func TestMapper_EmptyMethodRejected(t *testing.T) {
	db := &fakeMapperDB{existing: nil}
	m := NewMapper(MapperConfig{GroupRoleMap: map[string][]string{"ops": {"cluster-admin"}}, DB: db, Audit: &fakeAudit{}})
	if _, err := m.Map(context.Background(), ExternalIdentity{AID: "alice", Groups: []string{"ops"}}); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("empty Method must fail with ErrAuthFailed, got %v", err)
	}
	if len(db.inserts) != 0 {
		t.Errorf("empty Method must not provision, got %d inserts", len(db.inserts))
	}
}
