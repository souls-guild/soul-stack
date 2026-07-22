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

// fakeMapperDB — in-memory operator.ExecQueryRower for Mapper unit tests.
// Distinguishes SELECT (QueryRow over operators) from INSERT/GRANT (Exec) by SQL text.
type fakeMapperDB struct {
	// existing — the operator SelectByAID will return; nil → ErrOperatorNotFound.
	existing *operator.Operator
	selErr   error

	inserts []*insertCapture // captured INSERT operators
	grants  int              // count of rbac.GrantOperator calls
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

// fakeOperatorRow — a pgx.Row emitting Operator fields in selectOperatorByAIDSQL order.
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
	// order: aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata
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

// fakeAudit — in-memory audit.Writer, accumulates recorded events.
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

// TestMapper_HappyProvision — user in a group → auto-provision (Insert called
// with auth_method=ldap), roles from groups, no JWT call here (that's in the
// endpoint). Provisioned=true, operator.provisioned audit recorded.
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
	// ADR-058(d): federated provisioning writes created_by_aid=NULL (no
	// initiating operator) + created_via='ldap'. This used to hold the
	// reserved AID archon-system because of the old bootstrap index; the
	// index moved to created_via='bootstrap' (migration 085), NULL is legal now.
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

// TestMapper_TwoFederatedProvisionsBothNullCreatedBy — ADR-058(d) guard (case 3):
// two different federated operators are provisioned back to back, both with
// created_by_aid=NULL. Before ADR-058(d) this was impossible: the partial
// unique index `operators_first_archon_idx` was keyed on `created_by_aid IS
// NULL` → the second NULL would trip a UNIQUE violation. After moving the
// index to created_via='bootstrap' (migration 085), NULL is legal for
// federated rows — both provisions produce created_by_aid=nil + created_via=ldap.
func TestMapper_TwoFederatedProvisionsBothNullCreatedBy(t *testing.T) {
	db := &fakeMapperDB{existing: nil} // both SELECTs → not found → provision
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

// TestMapper_NoRoleMappingDoesNotProvision — an ungrouped user →
// ErrNoRoleMapping, no operator is created (Insert NOT called).
func TestMapper_NoRoleMappingDoesNotProvision(t *testing.T) {
	db := &fakeMapperDB{existing: nil}
	aw := &fakeAudit{}
	m := newMapper(db, aw, map[string][]string{"ops": {"cluster-admin"}})

	_, err := m.Map(context.Background(), ExternalIdentity{
		AID:    "bob",
		Groups: []string{"interns"}, // doesn't intersect group_role_map
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

// TestMapper_RevokedRejected — SelectByAID returns a revoked operator →
// ErrOperatorRevoked, Insert NOT called.
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

// TestMapper_ExistingActiveNoInsert — an existing active operator →
// Provisioned=false, Insert NOT called, roles from group_role_map.
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

// TestMapper_InvalidAIDIsOpaque — an invalid AID → ErrAuthFailed (anti-oracle),
// no leak of the cause.
func TestMapper_InvalidAIDIsOpaque(t *testing.T) {
	db := &fakeMapperDB{}
	m := newMapper(db, &fakeAudit{}, map[string][]string{"ops": {"cluster-admin"}})
	_, err := m.Map(context.Background(), ExternalIdentity{AID: "BAD/AID", Groups: []string{"ops"}})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed for invalid AID, got %v", err)
	}
}

// TestMapper_RolesDedupSorted — several groups with overlapping roles →
// deduped + stable order.
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

// TestMapper_OIDCProvisionStampsOIDC — ADR-058 stage 2 guard: a mapper with
// Method=oidc provisions an operator with auth_method=oidc AND created_via=oidc
// (the source is attributed by method, not hardcoded to ldap). Proves the
// mapper generalizes across both methods.
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

// TestMapper_EmptyMethodRejected — defense-in-depth guard: a mapper without
// an explicit Method must not silently create an operator (ErrAuthFailed,
// Insert not called).
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
