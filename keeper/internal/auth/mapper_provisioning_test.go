package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

// fakeGate — a controllable ProvisioningGate: allows methods from allowed.
type fakeGate struct {
	allowed map[string]bool
}

func (g fakeGate) ProvisioningMethodAllowed(method string) bool {
	return g.allowed[method]
}

func newMapperWithGate(db operator.ExecQueryRower, grm map[string][]string, gate ProvisioningGate) *DBMapper {
	return NewMapper(MapperConfig{Method: operator.AuthMethodLDAP, GroupRoleMap: grm, DB: db, Audit: &fakeAudit{}, ProvisioningGate: gate})
}

func newOIDCMapperWithGate(db operator.ExecQueryRower, grm map[string][]string, gate ProvisioningGate) *DBMapper {
	return NewMapper(MapperConfig{Method: operator.AuthMethodOIDC, GroupRoleMap: grm, DB: db, Audit: &fakeAudit{}, ProvisioningGate: gate})
}

// TestMapper_OIDCDisabled_ProvisionRejected — ADR-058 stage 2: a policy
// without oidc → auto-provisioning a NEW OIDC operator is rejected
// (ErrProvisioningDisabled), no operators row is created. The gate checks
// specifically the "oidc" method (not ldap).
func TestMapper_OIDCDisabled_ProvisionRejected(t *testing.T) {
	db := &fakeMapperDB{existing: nil}
	gate := fakeGate{allowed: map[string]bool{"user": true, "ldap": true}} // oidc forbidden
	m := newOIDCMapperWithGate(db, map[string][]string{"ops": {"cluster-admin"}}, gate)

	_, err := m.Map(context.Background(), ExternalIdentity{AID: "newbie", Username: "newbie", Groups: []string{"ops"}})
	if !errors.Is(err, ErrProvisioningDisabled) {
		t.Fatalf("Map err=%v, want ErrProvisioningDisabled", err)
	}
	if len(db.inserts) != 0 {
		t.Errorf("Insert вызван %d раз, want 0 (provision отвергнут ДО Insert)", len(db.inserts))
	}
}

// TestMapper_OIDCAllowed_ProvisionSucceeds — positive case: oidc∈methods →
// provision succeeds, the operator is created.
func TestMapper_OIDCAllowed_ProvisionSucceeds(t *testing.T) {
	db := &fakeMapperDB{existing: nil}
	gate := fakeGate{allowed: map[string]bool{"oidc": true}}
	m := newOIDCMapperWithGate(db, map[string][]string{"ops": {"cluster-admin"}}, gate)

	got, err := m.Map(context.Background(), ExternalIdentity{AID: "alice", Username: "alice", Groups: []string{"ops"}})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !got.Provisioned || len(db.inserts) != 1 {
		t.Errorf("Provisioned=%v inserts=%d, want true/1", got.Provisioned, len(db.inserts))
	}
}

// TestMapper_LdapDisabled_ProvisionRejected — B5 case 2: a policy without
// ldap → auto-provisioning a NEW federated operator is rejected
// (ErrProvisioningDisabled), no operators row is created (Insert not called).
func TestMapper_LdapDisabled_ProvisionRejected(t *testing.T) {
	db := &fakeMapperDB{existing: nil}                                     // SelectByAID → not found → provision branch
	gate := fakeGate{allowed: map[string]bool{"user": true, "oidc": true}} // ldap forbidden
	m := newMapperWithGate(db, map[string][]string{"ops": {"cluster-admin"}}, gate)

	_, err := m.Map(context.Background(), ExternalIdentity{
		AID:      "newbie",
		Username: "newbie",
		Groups:   []string{"ops"},
	})
	if !errors.Is(err, ErrProvisioningDisabled) {
		t.Fatalf("Map err=%v, want ErrProvisioningDisabled", err)
	}
	if len(db.inserts) != 0 {
		t.Errorf("Insert вызван %d раз, want 0 (provision отвергнут ДО Insert)", len(db.inserts))
	}
	if db.grants != 0 {
		t.Errorf("role grant вызван %d раз, want 0 (оператор не создан)", db.grants)
	}
}

// TestMapper_LdapAllowed_ProvisionSucceeds — positive case: ldap∈methods →
// provision succeeds, the operator is created.
func TestMapper_LdapAllowed_ProvisionSucceeds(t *testing.T) {
	db := &fakeMapperDB{existing: nil}
	gate := fakeGate{allowed: map[string]bool{"ldap": true}}
	m := newMapperWithGate(db, map[string][]string{"ops": {"cluster-admin"}}, gate)

	got, err := m.Map(context.Background(), ExternalIdentity{
		AID:      "alice",
		Username: "alice",
		Groups:   []string{"ops"},
	})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !got.Provisioned {
		t.Errorf("Provisioned=false, want true")
	}
	if len(db.inserts) != 1 {
		t.Errorf("Insert вызван %d раз, want 1", len(db.inserts))
	}
}

// TestMapper_ExistingLoginNotGated_WhenLdapDisabled — ★ B5 case 3 (CRITICAL
// invariant "gate only on creation"): an EXISTING federated operator logs in
// SUCCESSFULLY even under a policy without ldap. The err==nil case in Map
// does NOT engage the gate — only the provision (creation) branch is gated.
func TestMapper_ExistingLoginNotGated_WhenLdapDisabled(t *testing.T) {
	existing := &operator.Operator{
		AID:         "dave",
		DisplayName: "dave",
		AuthMethod:  operator.AuthMethodLDAP,
		CreatedVia:  operator.CreatedViaLDAP,
	}
	db := &fakeMapperDB{existing: existing}                  // SelectByAID → active operator
	gate := fakeGate{allowed: map[string]bool{"user": true}} // ldap FORBIDDEN by policy
	m := newMapperWithGate(db, map[string][]string{"ops": {"cluster-admin"}}, gate)

	got, err := m.Map(context.Background(), ExternalIdentity{AID: "dave", Groups: []string{"ops"}})
	if err != nil {
		t.Fatalf("Map существующего оператора при ldap-disabled err=%v, want nil (гейт только на создании)", err)
	}
	if got.Provisioned {
		t.Errorf("Provisioned=true, want false (оператор уже существовал)")
	}
	if len(db.inserts) != 0 {
		t.Errorf("Insert вызван %d раз, want 0 (существующий не создаётся)", len(db.inserts))
	}
}
