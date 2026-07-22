package cert_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	coremodcert "github.com/souls-guild/soul-stack/keeper/internal/coremod/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- fakes ---

type fakeVault struct {
	resp map[string]any
	err  error
	last string
}

func (f *fakeVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	f.last = path
	return f.resp, f.err
}

type registerCall struct {
	w *keepercert.Warrant
}

type fakeStore struct {
	active     *keepercert.Warrant // returned by SelectActive (nil → ErrNotFound)
	registered []registerCall
	regErr     error
}

func (s *fakeStore) SelectActive(_ context.Context, _ string, _ keepercert.Kind) (*keepercert.Warrant, error) {
	if s.active == nil {
		return nil, keepercert.ErrNotFound
	}
	return s.active, nil
}

func (s *fakeStore) RegisterActive(_ context.Context, w *keepercert.Warrant) error {
	if s.regErr != nil {
		return s.regErr
	}
	w.CertID = "generated-cert-id"
	s.registered = append(s.registered, registerCall{w: w})
	return nil
}

type fakeAudit struct {
	events []*audit.Event
}

func (a *fakeAudit) Write(_ context.Context, e *audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// makeCertPEM generates self-signed leaf cert with given not_after and returns
// (PEM, fingerprint) — fingerprint computed same way as module.
func makeCertPEM(t *testing.T, cn string, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return pemStr, keepercert.FingerprintFromCert(c)
}

// --- Validate ---

func TestValidate_UnknownState(t *testing.T) {
	m := coremodcert.New(&fakeVault{}, &fakeStore{}, &fakeAudit{}, "kid-1")
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "issued",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("expected error for unknown state")
	}
}

func TestValidate_RequiresIncarnationAndCerts(t *testing.T) {
	m := coremodcert.New(&fakeVault{}, &fakeStore{}, &fakeAudit{}, "kid-1")
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "registered",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("expected error: registered requires incarnation + certs")
	}
}

func TestValidate_RejectsBadKind(t *testing.T) {
	m := coremodcert.New(&fakeVault{}, &fakeStore{}, &fakeAudit{}, "kid-1")
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"incarnation": "redis-prod",
			"certs": []any{
				map[string]any{"kind": "bogus", "vault_ref": "secret/x#cert"},
			},
		}),
	})
	if rep.Ok {
		t.Fatal("expected error for bad kind")
	}
}

// --- Apply: initial registration ---

// TestApply_RegistersNewCert is GUARD for initial registration (design.md): create
// writes warrant → Reaper sees cert. Module reads PEM from Vault, extracts
// metadata and registers active row; changed=true.
func TestApply_RegistersNewCert(t *testing.T) {
	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	certPEM, wantFP := makeCertPEM(t, "redis-prod.tls", notAfter)
	fv := &fakeVault{resp: map[string]any{"cert": certPEM}}
	fs := &fakeStore{} // no active → ErrNotFound
	fa := &fakeAudit{}
	m := coremodcert.New(fv, fs, fa, "kid-1")
	stream := internaltest.NewApplyStream()

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"incarnation": "redis-prod",
			"certs": []any{
				map[string]any{"kind": "cert", "vault_ref": "secret/redis/redis-prod/tls/cert#cert"},
			},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("unexpected failure: %s", ev.Message)
	}
	if !ev.Changed {
		t.Fatal("expected changed=true (new cert registered)")
	}
	if len(fs.registered) != 1 {
		t.Fatalf("expected 1 RegisterActive, got %d", len(fs.registered))
	}
	got := fs.registered[0].w
	if got.IncarnationID != "redis-prod" || got.Kind != keepercert.KindCert {
		t.Errorf("warrant = %+v", got)
	}
	if got.Fingerprint != wantFP {
		t.Errorf("fingerprint = %q, want %q (must be extracted from the cert itself)", got.Fingerprint, wantFP)
	}
	if !got.NotAfter.Equal(notAfter.UTC()) {
		t.Errorf("not_after = %v, want %v", got.NotAfter, notAfter.UTC())
	}
	if got.IssuedByKID == nil || *got.IssuedByKID != "kid-1" {
		t.Errorf("issued_by_kid = %v, want kid-1", got.IssuedByKID)
	}
	if !got.AutoRotate {
		t.Error("auto_rotate default should be true")
	}
	// audit event written, no secrets (PEM doesn't leak).
	if len(fa.events) != 1 || fa.events[0].EventType != audit.EventCertRegistered {
		t.Fatalf("expected 1 cert.registered event, got %v", fa.events)
	}
}

// TestApply_IdempotentSameFingerprint is GUARD for idempotency (design.md):
// active row with same fingerprint already exists → no-op, RegisterActive NOT
// called, changed=false, no audit event written.
func TestApply_IdempotentSameFingerprint(t *testing.T) {
	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	certPEM, fp := makeCertPEM(t, "redis-prod.tls", notAfter)
	fv := &fakeVault{resp: map[string]any{"cert": certPEM}}
	fs := &fakeStore{active: &keepercert.Warrant{
		IncarnationID: "redis-prod",
		Kind:          keepercert.KindCert,
		Fingerprint:   fp, // same cert already active
	}}
	fa := &fakeAudit{}
	m := coremodcert.New(fv, fs, fa, "kid-1")
	stream := internaltest.NewApplyStream()

	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"incarnation": "redis-prod",
			"certs": []any{
				map[string]any{"kind": "cert", "vault_ref": "secret/redis/redis-prod/tls/cert#cert"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("unexpected failure: %s", ev.Message)
	}
	if ev.Changed {
		t.Fatal("expected changed=false (same fingerprint already registered)")
	}
	if len(fs.registered) != 0 {
		t.Errorf("RegisterActive must not be called on idempotent no-op, got %d calls", len(fs.registered))
	}
	if len(fa.events) != 0 {
		t.Errorf("no audit event on no-op, got %d", len(fa.events))
	}
}

// TestApply_NewFingerprintSupersedes tests changed cert (different fingerprint) →
// RegisterActive called (supersede+insert inside), changed=true.
func TestApply_NewFingerprintSupersedes(t *testing.T) {
	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	certPEM, newFP := makeCertPEM(t, "redis-prod.tls", notAfter)
	fv := &fakeVault{resp: map[string]any{"cert": certPEM}}
	fs := &fakeStore{active: &keepercert.Warrant{
		IncarnationID: "redis-prod",
		Kind:          keepercert.KindCert,
		Fingerprint:   "old" + newFP[3:], // differs from new
	}}
	m := coremodcert.New(fv, fs, &fakeAudit{}, "kid-1")
	stream := internaltest.NewApplyStream()

	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"incarnation": "redis-prod",
			"certs": []any{
				map[string]any{"kind": "cert", "vault_ref": "secret/redis/redis-prod/tls/cert#cert"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("expected changed=true (fingerprint changed)")
	}
	if len(fs.registered) != 1 {
		t.Fatalf("expected 1 RegisterActive, got %d", len(fs.registered))
	}
}

// TestApply_FailsOnUnparsablePEM tests garbage instead of PEM → failed event
// (scenario-applier goes to onfail), RegisterActive NOT called.
func TestApply_FailsOnUnparsablePEM(t *testing.T) {
	fv := &fakeVault{resp: map[string]any{"cert": "not-a-pem"}}
	fs := &fakeStore{}
	m := coremodcert.New(fv, fs, &fakeAudit{}, "kid-1")
	stream := internaltest.NewApplyStream()

	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "registered",
		Params: mustStruct(t, map[string]any{
			"incarnation": "redis-prod",
			"certs": []any{
				map[string]any{"kind": "cert", "vault_ref": "secret/redis/redis-prod/tls/cert#cert"},
			},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed event on unparsable PEM")
	}
	if len(fs.registered) != 0 {
		t.Errorf("RegisterActive must not be called on parse failure")
	}
}
