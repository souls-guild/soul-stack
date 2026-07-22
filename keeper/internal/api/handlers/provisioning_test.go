package handlers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// provPool — fake ServicePool: SetSetting (upsert) succeeds; QueryRow on upsert
// scans RETURNING updated_at.
type provPool struct {
	setSQLSeen *string
	setValue   *string
}

func (p *provPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (p *provPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if p.setSQLSeen != nil {
		*p.setSQLSeen = sql
	}
	// upsertSettingSQL: $1=key, $2=value, $3=updated_by_aid.
	if len(args) >= 2 && p.setValue != nil {
		if v, ok := args[1].(string); ok {
			*p.setValue = v
		}
	}
	return provScanRow{}
}

func (p *provPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("provPool.Query unused")
}

type provScanRow struct{}

func (provScanRow) Scan(dest ...any) error {
	// RETURNING updated_at.
	for _, d := range dest {
		if tp, ok := d.(*time.Time); ok {
			*tp = time.Now()
		}
	}
	return nil
}

// countingInv — cluster-invalidate counter.
type countingInv struct{ calls atomic.Int64 }

func (c *countingInv) Invalidate(context.Context) { c.calls.Add(1) }

// provReader — fake ProvisioningPolicyReader (for previous-payload + GET).
type provReader struct {
	methods []string
	set     bool
}

func (r provReader) ProvisioningPolicy() ([]string, bool) { return r.methods, r.set }

func provClaims(aid string) *keeperjwt.Claims { return &keeperjwt.Claims{Subject: aid} }

// TestProvisioningPut_InvalidateAndAuditPayload — B5 case 6: PUT writes the CSV
// via Service.SetSetting (invalidate called — counting-invalidator) + AuditPayload
// carries allowed_methods + previous. The provisioning.policy_changed event itself is
// written by huma-audit-middleware (variant B) from this AuditPayload — here we check
// the payload is correct and non-empty (S6 invariant "there's something to write").
func TestProvisioningPut_InvalidateAndAuditPayload(t *testing.T) {
	var seenValue string
	pool := &provPool{setValue: &seenValue}
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	inv := &countingInv{}
	svc.SetInvalidator(inv)

	reader := provReader{methods: []string{"oidc"}, set: true} // previous policy
	h := NewProvisioningPolicyHandler(reader, svc, nil)

	reply, err := h.PutTyped(context.Background(), provClaims("archon-alice"),
		ProvisioningPolicyUpdateInput{AllowedMethods: []string{"user", "ldap"}})
	if err != nil {
		t.Fatalf("PutTyped: %v", err)
	}

	// invalidate called exactly once (cluster-wide snapshot refresh).
	if got := inv.calls.Load(); got != 1 {
		t.Errorf("invalidate calls = %d, want 1", got)
	}
	// normalized CSV written (sorted set).
	if seenValue != "ldap,user" {
		t.Errorf("SetSetting value = %q, want %q", seenValue, "ldap,user")
	}

	// AuditPayload: new list + previous.
	p := reply.AuditPayload()
	am, ok := p["allowed_methods"].([]string)
	if !ok || len(am) != 2 {
		t.Errorf("audit allowed_methods = %v, want [ldap user]", p["allowed_methods"])
	}
	prev, ok := p["previous"].([]string)
	if !ok || len(prev) != 1 || prev[0] != "oidc" {
		t.Errorf("audit previous = %v, want [oidc]", p["previous"])
	}

	// 200 body: policy_set=true, list normalized.
	if !reply.Body.PolicySet {
		t.Error("reply.Body.PolicySet = false, want true")
	}
}

// TestProvisioningPut_EmptyList_422 — B5 anti-lockout: empty list → 422
// validation-failed, SetSetting NOT called.
func TestProvisioningPut_EmptyList_422(t *testing.T) {
	var seenValue string
	pool := &provPool{setValue: &seenValue}
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	inv := &countingInv{}
	svc.SetInvalidator(inv)
	h := NewProvisioningPolicyHandler(provReader{}, svc, nil)

	_, err = h.PutTyped(context.Background(), provClaims("archon-alice"),
		ProvisioningPolicyUpdateInput{AllowedMethods: nil})
	if err == nil {
		t.Fatal("PutTyped empty list err=nil, want 422 (anti-lockout)")
	}
	d, ok := AsProblemDetails(err)
	if !ok || d.Status != 422 {
		t.Fatalf("err = %v, want 422 validation-failed", err)
	}
	if inv.calls.Load() != 0 {
		t.Errorf("invalidate called on failure, want 0 (no write happened)")
	}
	if seenValue != "" {
		t.Errorf("SetSetting called (value=%q), want empty", seenValue)
	}
}

// TestProvisioningPut_InvalidMethod_422 — method outside the domain → 422, no write.
func TestProvisioningPut_InvalidMethod_422(t *testing.T) {
	pool := &provPool{}
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h := NewProvisioningPolicyHandler(provReader{}, svc, nil)

	_, err = h.PutTyped(context.Background(), provClaims("archon-alice"),
		ProvisioningPolicyUpdateInput{AllowedMethods: []string{"user", "bootstrap"}})
	d, ok := AsProblemDetails(err)
	if !ok || d.Status != 422 {
		t.Fatalf("err = %v, want 422 (bootstrap cannot be set in policy)", err)
	}
}

// TestProvisioningGet_DefaultPolicySetFalse — GET with no policy set →
// policy_set=false. B5 case 7 (handler projection).
func TestProvisioningGet_DefaultPolicySetFalse(t *testing.T) {
	pool := &provPool{}
	svc, _ := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: pool})
	h := NewProvisioningPolicyHandler(provReader{set: false}, svc, nil)
	view := h.GetTyped()
	if view.PolicySet {
		t.Errorf("PolicySet = true, want false (policy not set)")
	}
}
