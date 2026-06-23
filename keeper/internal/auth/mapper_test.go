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
}

func (f *fakeMapperDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	switch {
	case strings.Contains(sql, "INSERT INTO operators"):
		// args: aid, display_name, auth_method, created_at, created_by_aid, revoked_at, metadata
		f.inserts = append(f.inserts, &insertCapture{
			aid:        toStr(args[0]),
			authMethod: toStr(args[2]),
			createdBy:  args[4],
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
	// порядок: aid, display_name, auth_method, created_at, created_by_aid, revoked_at, metadata
	*(dest[0].(*string)) = r.op.AID
	*(dest[1].(*string)) = r.op.DisplayName
	*(dest[2].(*string)) = string(r.op.AuthMethod)
	*(dest[3].(*time.Time)) = r.op.CreatedAt
	*(dest[4].(**string)) = r.op.CreatedByAID
	*(dest[5].(**time.Time)) = r.op.RevokedAt
	*(dest[6].(*[]byte)) = []byte("{}")
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
	return NewMapper(MapperConfig{GroupRoleMap: grm, DB: db, Audit: aw})
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
	if db.inserts[0].createdBy != FederatedSourceAID {
		t.Errorf("Insert created_by_aid = %v, want %q (not nil, not self)", db.inserts[0].createdBy, FederatedSourceAID)
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
