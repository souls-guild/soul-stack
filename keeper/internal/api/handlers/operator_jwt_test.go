package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

func activeOperatorFakePool() *fakePool {
	return &fakePool{selectFn: func(aid string) (*operator.Operator, error) {
		return &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT, CreatedAt: time.Now()}, nil
	}}
}

// Token terms the service refuses are the caller's mistake: 422, and nothing minted.
func TestOperatorIssueTokenTyped_BadTerms_422(t *testing.T) {
	for name, req := range map[string]OperatorIssueTokenInput{
		"not a duration":     {TTL: "soon"},
		"above ttl_max":      {TTL: "2h"},
		"below the minimum":  {TTL: "10s"},
		"control char label": {Label: "a\tb"},
	} {
		t.Run(name, func(t *testing.T) {
			pool := activeOperatorFakePool()
			h, iss := newHandler(t, pool, &rbactest.Config{})
			_, err := h.IssueTokenTyped(context.Background(), claims("archon-alice"), "archon-bob", req)
			wantProblem(t, err, problem.TypeValidationFailed)
			if iss.called || len(pool.jwtInserts) != 0 {
				t.Error("a refused request still minted a token")
			}
		})
	}
}

func TestOperatorIssueTokenTyped_ReturnsTheJWTID(t *testing.T) {
	pool := activeOperatorFakePool()
	h, _ := newHandler(t, pool, &rbactest.Config{})
	reply, err := h.IssueTokenTyped(context.Background(), claims("archon-alice"), "archon-bob", OperatorIssueTokenInput{TTL: "1h", Label: "sd"})
	if err != nil {
		t.Fatalf("IssueTokenTyped: %v", err)
	}
	if !operator.ValidJWTID(reply.JWTID) {
		t.Errorf("JWTID = %q", reply.JWTID)
	}
	p := reply.AuditPayload()
	if p["jwt_id"] != reply.JWTID || p["ttl"] != "1h0m0s" || p["label"] != "sd" {
		t.Errorf("audit payload = %v, want jwt_id/ttl/label", p)
	}
}

func TestOperatorRevokeJWTTyped_MalformedID_404(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, &rbactest.Config{})
	_, err := h.RevokeJWTTyped(context.Background(), claims("archon-alice"), "archon-bob", "not-a-ulid")
	wantProblem(t, err, problem.TypeNotFound)
}

func TestOperatorListJWTsTyped_UnknownOperator_404(t *testing.T) {
	h, _ := newHandler(t, &fakePool{}, &rbactest.Config{})
	_, err := h.ListJWTsTyped(context.Background(), "archon-ghost", OperatorJWTFilterInput{}, 0, 50)
	wantProblem(t, err, problem.TypeNotFound)
}

func TestOperatorListJWTsTyped_UnknownFilter_422(t *testing.T) {
	for name, f := range map[string]OperatorJWTFilterInput{
		"kind":  {Kinds: []string{"forever"}},
		"state": {State: "dormant"},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newHandler(t, activeOperatorFakePool(), &rbactest.Config{})
			_, err := h.ListJWTsTyped(context.Background(), "archon-bob", f, 0, 50)
			wantProblem(t, err, problem.TypeValidationFailed)
		})
	}
}

// The issue-token payload names its kind, so a subscriber can tell an
// administrator's token for someone from a personal one (NIM-1052).
func TestOperatorIssueTokenTyped_AuditNamesTheKind(t *testing.T) {
	h, _ := newHandler(t, activeOperatorFakePool(), &rbactest.Config{})
	reply, err := h.IssueTokenTyped(context.Background(), claims("archon-alice"), "archon-bob", OperatorIssueTokenInput{})
	if err != nil {
		t.Fatalf("IssueTokenTyped: %v", err)
	}
	if got := reply.AuditPayload()["kind"]; got != "issued" {
		t.Errorf("audit kind = %v, want issued", got)
	}
}
