package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

// fakeGate — управляемый ProvisioningGate: разрешает методы из allowed.
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

// TestMapper_OIDCDisabled_ProvisionRejected — ADR-058 стадия 2: политика без
// oidc → auto-provision НОВОГО OIDC-оператора отвергнут (ErrProvisioningDisabled),
// строка operators НЕ создаётся. Гейт проверяет именно метод "oidc" (а не ldap).
func TestMapper_OIDCDisabled_ProvisionRejected(t *testing.T) {
	db := &fakeMapperDB{existing: nil}
	gate := fakeGate{allowed: map[string]bool{"user": true, "ldap": true}} // oidc запрещён
	m := newOIDCMapperWithGate(db, map[string][]string{"ops": {"cluster-admin"}}, gate)

	_, err := m.Map(context.Background(), ExternalIdentity{AID: "newbie", Username: "newbie", Groups: []string{"ops"}})
	if !errors.Is(err, ErrProvisioningDisabled) {
		t.Fatalf("Map err=%v, want ErrProvisioningDisabled", err)
	}
	if len(db.inserts) != 0 {
		t.Errorf("Insert вызван %d раз, want 0 (provision отвергнут ДО Insert)", len(db.inserts))
	}
}

// TestMapper_OIDCAllowed_ProvisionSucceeds — позитив: oidc∈methods → provision
// проходит, оператор создаётся.
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

// TestMapper_LdapDisabled_ProvisionRejected — B5 кейс 2: политика без ldap →
// auto-provision НОВОГО federated-оператора отвергнут (ErrProvisioningDisabled),
// строка operators НЕ создаётся (Insert не вызван).
func TestMapper_LdapDisabled_ProvisionRejected(t *testing.T) {
	db := &fakeMapperDB{existing: nil}                                     // SelectByAID → not found → ветка provision
	gate := fakeGate{allowed: map[string]bool{"user": true, "oidc": true}} // ldap запрещён
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

// TestMapper_LdapAllowed_ProvisionSucceeds — позитив: ldap∈methods → provision
// проходит, оператор создаётся.
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

// TestMapper_ExistingLoginNotGated_WhenLdapDisabled — ★ B5 кейс 3 (КРИТИЧНЫЙ
// инвариант «гейт только на создании»): СУЩЕСТВУЮЩИЙ federated-оператор логинится
// УСПЕШНО даже при политике без ldap. case err==nil в Map НЕ задействует gate —
// гейтится только ветка provision (создание).
func TestMapper_ExistingLoginNotGated_WhenLdapDisabled(t *testing.T) {
	existing := &operator.Operator{
		AID:         "dave",
		DisplayName: "dave",
		AuthMethod:  operator.AuthMethodLDAP,
		CreatedVia:  operator.CreatedViaLDAP,
	}
	db := &fakeMapperDB{existing: existing}                  // SelectByAID → активный оператор
	gate := fakeGate{allowed: map[string]bool{"user": true}} // ldap ЗАПРЕЩЁН политикой
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
