package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// fakeProvisioningGate — controllable ProvisioningGate for the handler test.
type fakeProvisioningGate struct {
	allowed map[string]bool
}

func (g fakeProvisioningGate) ProvisioningMethodAllowed(method string) bool {
	return g.allowed[method]
}

// TestOperatorCreateTyped_UserDisabled_403 — B5 case 1: policy without "user" →
// POST /v1/operators (CreateTyped) returns problem provisioning_method_disabled
// (403), svc.Create is NOT called (operator not created).
func TestOperatorCreateTyped_UserDisabled_403(t *testing.T) {
	pool := &fakePool{
		selectFn: func(string) (*operator.Operator, error) {
			t.Fatal("svc.Create не должен дойти до БД при запрещённом методе")
			return nil, nil
		},
	}
	h, _ := newHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	h.SetProvisioningGate(fakeProvisioningGate{allowed: map[string]bool{"ldap": true}}) // user disallowed

	_, err := h.CreateTyped(context.Background(), claims("archon-alice"),
		OperatorCreateInput{AID: "archon-bob", DisplayName: "Bob"})
	if err == nil {
		t.Fatal("CreateTyped err=nil, want provisioning_method_disabled (403)")
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("err не *problemError: %v", err)
	}
	if d.Type != problem.TypeProvisioningMethodDisabled {
		t.Errorf("problem type = %q, want %q", d.Type, problem.TypeProvisioningMethodDisabled)
	}
	if pool.insertCalls != 0 {
		t.Errorf("insertCalls = %d, want 0 (оператор не создан)", pool.insertCalls)
	}
}

// TestOperatorCreateTyped_UserAllowed_Proceeds — positive: user∈methods →
// CreateTyped proceeds (operator created).
func TestOperatorCreateTyped_UserAllowed_Proceeds(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{
				AID:         aid,
				DisplayName: "Bob",
				AuthMethod:  operator.AuthMethodJWT,
				CreatedAt:   time.Now(),
			}, nil
		},
	}
	h, _ := newHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	h.SetProvisioningGate(fakeProvisioningGate{allowed: map[string]bool{"user": true}})

	reply, err := h.CreateTyped(context.Background(), claims("archon-alice"),
		OperatorCreateInput{AID: "archon-bob", DisplayName: "Bob"})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if reply.AID != "archon-bob" {
		t.Errorf("AID = %q, want archon-bob", reply.AID)
	}
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", pool.insertCalls)
	}
}

// TestOperatorCreateTyped_NilGate_Proceeds — gate not configured (nil) →
// CreateTyped proceeds (back-compat).
func TestOperatorCreateTyped_NilGate_Proceeds(t *testing.T) {
	pool := &fakePool{
		selectFn: func(aid string) (*operator.Operator, error) {
			return &operator.Operator{AID: aid, AuthMethod: operator.AuthMethodJWT, CreatedAt: time.Now()}, nil
		},
	}
	h, _ := newHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	// gate NOT set.
	if _, err := h.CreateTyped(context.Background(), claims("archon-alice"),
		OperatorCreateInput{AID: "archon-bob"}); err != nil {
		t.Fatalf("CreateTyped с nil-gate err=%v, want nil (back-compat)", err)
	}
	if pool.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", pool.insertCalls)
	}
}

// verifies provisioning_method_disabled really is 403.
func TestProvisioningMethodDisabled_Status403(t *testing.T) {
	d := problem.New(problem.TypeProvisioningMethodDisabled, "", "x")
	if d.Status != 403 {
		t.Errorf("provisioning-method-disabled status = %d, want 403", d.Status)
	}
}
