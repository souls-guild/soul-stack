package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

type fakeIssuer struct {
	hosts []coremodbootstrap.IssuedHost
	err   error
	calls [][]string
}

func (f *fakeIssuer) IssueBatch(_ context.Context, sids []string) ([]coremodbootstrap.IssuedHost, error) {
	f.calls = append(f.calls, append([]string(nil), sids...))
	return f.hosts, f.err
}

func issuedReq(t *testing.T, sids ...string) *pluginv1.ApplyRequest {
	t.Helper()
	values := make([]any, len(sids))
	for i, sid := range sids {
		values[i] = sid
	}
	return &pluginv1.ApplyRequest{
		State:  coremodbootstrap.StateIssued,
		Params: mustStruct(t, map[string]any{"sids": values}),
	}
}

func TestApplyIssued_OutputForDeliveryAndAuditWithoutSecret(t *testing.T) {
	tok1, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	issuer := &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{
		{SID: "vm1.example.com", Token: tok1, ExpiresAt: expires, Created: true},
		{SID: "vm2.example.com", Token: tok2, ExpiresAt: expires, Reissued: true},
	}}
	aud := &fakeAudit{}
	m := &coremodbootstrap.Module{Issuer: issuer, Audit: aud}
	stream := internaltest.NewApplyStream()

	if err := m.Apply(issuedReq(t, "vm1.example.com", "vm2.example.com"), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || last.GetFailed() || !last.GetChanged() {
		t.Fatalf("issued final flags = present:%t failed:%t changed:%t, want present changed success",
			last != nil, last != nil && last.GetFailed(), last != nil && last.GetChanged())
	}
	out := last.GetOutput().AsMap()
	if out["action"] != coremodbootstrap.StateIssued || out["count"] != float64(2) || out["created"] != float64(1) || out["reissued"] != float64(1) {
		t.Fatalf("issued action/count/created/reissued = %v/%v/%v/%v",
			out["action"], out["count"], out["created"], out["reissued"])
	}
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("hosts length = %d, want 2", len(hosts))
	}
	first := hosts[0].(map[string]any)
	if first["sid"] != "vm1.example.com" || first["bootstrap_token"] != tok1.Reveal() || first["expires_at"] != expires.Format(time.RFC3339) {
		t.Fatalf("first host delivery fields are incomplete or mismatched (sid=%v expires_at=%v)",
			first["sid"], first["expires_at"])
	}

	// The register is the one intentional plaintext boundary. Every observable
	// projection that uses the common masker redacts bootstrap_token by key.
	masked := audit.MaskSecrets(out)
	maskedHosts := masked["hosts"].([]any)
	if got := maskedHosts[0].(map[string]any)["bootstrap_token"]; got != "***MASKED***" {
		t.Fatalf("bootstrap_token mask = %v, want ***MASKED***", got)
	}

	if len(aud.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aud.events))
	}
	ev := aud.events[0]
	if ev.EventType != audit.EventBootstrapIssued || ev.Payload["action"] != coremodbootstrap.StateIssued {
		t.Fatalf("audit type/action = %q/%v, want %q/%q",
			ev.EventType, ev.Payload["action"], audit.EventBootstrapIssued, coremodbootstrap.StateIssued)
	}
	for _, token := range []string{tok1.Reveal(), tok2.Reveal()} {
		if containsTokenDeep(ev.Payload, token) {
			t.Fatal("SECURITY: plaintext token leaked into audit payload")
		}
	}
}

func TestApplyIssued_InputGuardBeforeMutation(t *testing.T) {
	cases := []struct {
		name string
		req  *pluginv1.ApplyRequest
		want string
	}{
		{name: "empty", req: issuedReq(t), want: "empty list"},
		{name: "duplicate", req: issuedReq(t, "vm1.example.com", "vm1.example.com"), want: "duplicate sid"},
		{name: "invalid", req: issuedReq(t, "UPPER.example.com"), want: "invalid sid"},
		{name: "reserved", req: issuedReq(t, "keeper"), want: "invalid sid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer := &fakeIssuer{}
			m := &coremodbootstrap.Module{Issuer: issuer}
			stream := internaltest.NewApplyStream()
			if err := m.Apply(tc.req, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			last := stream.Last()
			if last == nil || !last.GetFailed() || !strings.Contains(last.GetMessage(), tc.want) {
				t.Fatalf("failed event present/failed/message-match = %t/%t/%t, want true/true/true",
					last != nil, last != nil && last.GetFailed(), last != nil && strings.Contains(last.GetMessage(), tc.want))
			}
			if len(issuer.calls) != 0 {
				t.Fatalf("issuer called for invalid input: %v", issuer.calls)
			}
		})
	}
}

func TestApplyIssued_HostFailureIsDiagnosableAndHasNoPartialOutput(t *testing.T) {
	issuer := &fakeIssuer{err: &coremodbootstrap.SIDIssueError{
		SID: "vm2.example.com",
		Err: errors.New("Soul is already onboarded (status \"connected\"); refusing identity takeover"),
	}}
	m := &coremodbootstrap.Module{Issuer: issuer, Audit: &fakeAudit{}}
	stream := internaltest.NewApplyStream()
	if err := m.Apply(issuedReq(t, "vm1.example.com", "vm2.example.com"), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("final flags = present:%t failed:%t, want failed", last != nil, last != nil && last.GetFailed())
	}
	if !strings.Contains(last.GetMessage(), "vm2.example.com") || !strings.Contains(last.GetMessage(), "identity takeover") {
		t.Fatalf("failure is not host-diagnostic: %q", last.GetMessage())
	}
	if last.GetOutput() != nil {
		t.Fatal("failed issuance exposed partial output")
	}
}

func TestApplyIssued_RejectsIncompleteIssuerResultWithoutLoggingToken(t *testing.T) {
	tok, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		host coremodbootstrap.IssuedHost
	}{
		{name: "wrong sid", host: coremodbootstrap.IssuedHost{SID: "other.example.com", Token: tok, ExpiresAt: time.Now().Add(time.Hour)}},
		{name: "missing token", host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", ExpiresAt: time.Now().Add(time.Hour)}},
		{name: "missing expiry", host: coremodbootstrap.IssuedHost{SID: "vm1.example.com", Token: tok}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := internaltest.NewApplyStream()
			m := &coremodbootstrap.Module{Issuer: &fakeIssuer{hosts: []coremodbootstrap.IssuedHost{tc.host}}}
			if err := m.Apply(issuedReq(t, "vm1.example.com"), stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			last := stream.Last()
			if last == nil || !last.GetFailed() || strings.Contains(last.GetMessage(), tok.Reveal()) {
				t.Fatalf("incomplete issuer result was not rejected safely (present=%t failed=%t)",
					last != nil, last != nil && last.GetFailed())
			}
			if last.GetOutput() != nil {
				t.Fatal("incomplete issuer result exposed output")
			}
		})
	}
}

func TestValidateIssued_UsesSameUniqueSIDGuard(t *testing.T) {
	m := &coremodbootstrap.Module{}
	good, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  coremodbootstrap.StateIssued,
		Params: issuedReq(t, "vm1.example.com").Params,
	})
	if err != nil || !good.Ok {
		t.Fatalf("valid issued rejected: reply=%v err=%v", good, err)
	}
	bad, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  coremodbootstrap.StateIssued,
		Params: issuedReq(t, "vm1.example.com", "vm1.example.com").Params,
	})
	if err != nil || bad.Ok || len(bad.Errors) == 0 || !strings.Contains(bad.Errors[0], "duplicate") {
		t.Fatalf("duplicate issued validation = reply=%v err=%v", bad, err)
	}
}
