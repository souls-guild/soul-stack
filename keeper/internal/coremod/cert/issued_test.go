package cert_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/certpolicy"
	coremodcert "github.com/souls-guild/soul-stack/keeper/internal/coremod/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// --- fakes для state `issued` (fakeStore/fakeAudit/mustStruct/makeCertPEM
// переиспользуются из registered_test.go) ---

type fakeSigner struct {
	certPEM  string
	serial   string
	notAfter time.Time
	err      error
	gotMount string
	gotRole  string
}

func (f *fakeSigner) SignCSR(_ context.Context, mount, role, _ string) (*certissue.SignedCert, error) {
	f.gotMount, f.gotRole = mount, role
	if f.err != nil {
		return nil, f.err
	}
	return &certissue.SignedCert{
		CertificatePEM: []byte(f.certPEM),
		SerialNumber:   f.serial,
		NotAfter:       f.notAfter,
	}, nil
}

type fakeVaultWriter struct {
	writes map[string]map[string]any
	err    error
}

func (f *fakeVaultWriter) WriteKV(_ context.Context, path string, data map[string]any) error {
	if f.err != nil {
		return f.err
	}
	if f.writes == nil {
		f.writes = map[string]map[string]any{}
	}
	f.writes[path] = data
	return nil
}

type fakePolicyResolver struct {
	pol certpolicy.Policy
	err error
}

func (f *fakePolicyResolver) Resolve(_ context.Context, _ string) (certpolicy.Policy, error) {
	return f.pol, f.err
}

func issuedCSRGen(_ string, _ []string) (privPEM, csrPEM []byte, err error) {
	return []byte("PRIVATE-KEY-PEM"), []byte("CSR-PEM"), nil
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// newIssuedModule собирает полностью сконфигурированный модуль под state `issued`.
func newIssuedModule(fs *fakeStore, fa *fakeAudit, signer certissue.Signer, vw certissue.KVWriter, pol coremodcert.IssuePolicyResolver) *coremodcert.Module {
	m := coremodcert.New(nil, fs, fa, "kid-1")
	m.Signer = signer
	m.VaultWriter = vw
	m.Policy = pol
	m.CSRGen = issuedCSRGen
	m.PKIMount = func() string { return "pki-int" }
	return m
}

func runIssued(t *testing.T, m *coremodcert.Module, params map[string]any) *internaltest.ApplyStream {
	t.Helper()
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "issued", Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return stream
}

// --- Validate ---

func TestValidate_Issued(t *testing.T) {
	m := coremodcert.New(nil, &fakeStore{}, &fakeAudit{}, "kid-1")
	if rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "issued",
		Params: mustStruct(t, map[string]any{}),
	}); rep.Ok {
		t.Error("issued без incarnation должен быть невалиден")
	}
	// issued требует только incarnation (certs НЕ обязателен, в отличие от registered).
	if rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "issued",
		Params: mustStruct(t, map[string]any{"incarnation": "redis-prod"}),
	}); !rep.Ok {
		t.Errorf("issued с incarnation должен быть валиден, errors=%v", rep.Errors)
	}
}

func TestValidate_TrulyUnknownState(t *testing.T) {
	m := coremodcert.New(nil, &fakeStore{}, &fakeAudit{}, "kid-1")
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "bogus",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("ожидалась ошибка для неизвестного state")
	}
}

// --- Apply: issued ---

// TestApplyIssued_EnrollByDefault — GUARD: без param auto_rotate cert enroll-ится
// в авто-ротацию (AutoRotate=true), PKIMount/PKIRole берутся ИЗ ПОЛИТИКИ (не из
// params), key-спутник AutoRotate=false, пути Vault = certissue.VaultPath(service).
func TestApplyIssued_EnrollByDefault(t *testing.T) {
	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	certPEM, wantFP := makeCertPEM(t, "redis-prod.tls", notAfter)
	signer := &fakeSigner{certPEM: certPEM, serial: "0A0B0C", notAfter: notAfter}
	vw := &fakeVaultWriter{}
	fs := &fakeStore{}
	fa := &fakeAudit{}
	pol := certpolicy.Policy{Service: "redis", Present: true, Enabled: true, PKIRole: "redis-tls", Scenario: "rotate_tls", KnownScenarios: []string{"rotate_tls"}}
	m := newIssuedModule(fs, fa, signer, vw, &fakePolicyResolver{pol: pol})

	stream := runIssued(t, m, map[string]any{"incarnation": "redis-prod"})

	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("unexpected failure: %s", ev.Message)
	}
	if !ev.Changed {
		t.Fatal("expected changed=true")
	}
	if len(fs.registered) != 2 {
		t.Fatalf("expected 2 RegisterActive (cert+key), got %d", len(fs.registered))
	}

	certW := fs.registered[0].w
	if certW.Kind != keepercert.KindCert {
		t.Errorf("registered[0].Kind = %q, want cert", certW.Kind)
	}
	if !certW.AutoRotate {
		t.Error("cert AutoRotate должен быть true по умолчанию (enroll)")
	}
	if certW.PKIMount == nil || *certW.PKIMount != "pki-int" {
		t.Errorf("cert PKIMount = %v, want pki-int (из PKIMount())", certW.PKIMount)
	}
	if certW.PKIRole == nil || *certW.PKIRole != "redis-tls" {
		t.Errorf("cert PKIRole = %v, want redis-tls (из политики, НЕ из params)", certW.PKIRole)
	}
	if certW.VaultRef != "secret/redis/redis-prod/tls/cert#cert" {
		t.Errorf("cert VaultRef = %q", certW.VaultRef)
	}
	if certW.Fingerprint != wantFP {
		t.Errorf("cert Fingerprint = %q, want %q", certW.Fingerprint, wantFP)
	}
	if certW.SerialNumber != "0A0B0C" {
		t.Errorf("cert SerialNumber = %q, want 0A0B0C (из signer)", certW.SerialNumber)
	}
	if !certW.NotAfter.Equal(notAfter.UTC()) {
		t.Errorf("cert NotAfter = %v, want %v", certW.NotAfter, notAfter.UTC())
	}
	if certW.IssuedByKID == nil || *certW.IssuedByKID != "kid-1" {
		t.Errorf("cert IssuedByKID = %v, want kid-1", certW.IssuedByKID)
	}

	keyW := fs.registered[1].w
	if keyW.Kind != keepercert.KindKey {
		t.Errorf("registered[1].Kind = %q, want key", keyW.Kind)
	}
	if keyW.AutoRotate {
		t.Error("key AutoRotate обязан быть false (спутник, не драйвер ротации)")
	}
	if keyW.VaultRef != "secret/redis/redis-prod/tls/key#key" {
		t.Errorf("key VaultRef = %q", keyW.VaultRef)
	}
	if keyW.Fingerprint != wantFP || keyW.SerialNumber != "0A0B0C" || !keyW.NotAfter.Equal(notAfter.UTC()) {
		t.Errorf("key должен зеркалить cert fingerprint/serial/not_after: %+v", keyW)
	}

	if _, ok := vw.writes["secret/redis/redis-prod/tls/cert"]; !ok {
		t.Errorf("ожидалась запись cert в secret/redis/redis-prod/tls/cert, got %v", keysOf(vw.writes))
	}
	if _, ok := vw.writes["secret/redis/redis-prod/tls/key"]; !ok {
		t.Errorf("ожидалась запись key в secret/redis/redis-prod/tls/key, got %v", keysOf(vw.writes))
	}

	if len(fa.events) != 1 || fa.events[0].EventType != audit.EventCertIssued {
		t.Fatalf("expected 1 cert.issued event, got %v", fa.events)
	}
	if _, leaked := fa.events[0].Payload["key"]; leaked {
		t.Error("audit payload не должен нести приватный ключ")
	}

	if signer.gotRole != "redis-tls" || signer.gotMount != "pki-int" {
		t.Errorf("signer got mount=%q role=%q, want pki-int/redis-tls", signer.gotMount, signer.gotRole)
	}
}

// TestApplyIssued_AutoRotateFalse — auto_rotate:false → cert-строка AutoRotate=false.
func TestApplyIssued_AutoRotateFalse(t *testing.T) {
	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	certPEM, _ := makeCertPEM(t, "redis-prod.tls", notAfter)
	signer := &fakeSigner{certPEM: certPEM, serial: "S1", notAfter: notAfter}
	fs := &fakeStore{}
	pol := certpolicy.Policy{Service: "redis", Present: true, Enabled: true, PKIRole: "redis-tls", Scenario: "rotate_tls", KnownScenarios: []string{"rotate_tls"}}
	m := newIssuedModule(fs, &fakeAudit{}, signer, &fakeVaultWriter{}, &fakePolicyResolver{pol: pol})

	stream := runIssued(t, m, map[string]any{"incarnation": "redis-prod", "auto_rotate": false})

	if stream.Last().Failed {
		t.Fatalf("unexpected failure: %s", stream.Last().Message)
	}
	if len(fs.registered) != 2 {
		t.Fatalf("expected 2 RegisterActive, got %d", len(fs.registered))
	}
	if fs.registered[0].w.AutoRotate {
		t.Error("cert AutoRotate обязан быть false при auto_rotate:false")
	}
}

// TestApplyIssued_PolicyResolveError — policy.Resolve вернул ошибку → SendFailed,
// RegisterActive НЕ вызывается.
func TestApplyIssued_PolicyResolveError(t *testing.T) {
	fs := &fakeStore{}
	m := newIssuedModule(fs, &fakeAudit{}, &fakeSigner{}, &fakeVaultWriter{}, &fakePolicyResolver{err: errors.New("boom")})

	stream := runIssued(t, m, map[string]any{"incarnation": "redis-prod"})

	if !stream.Last().Failed {
		t.Fatal("ожидался failed при ошибке резолва политики")
	}
	if len(fs.registered) != 0 {
		t.Errorf("RegisterActive не должен вызываться при ошибке политики, got %d", len(fs.registered))
	}
}

// TestApplyIssued_PolicyDisabled — !pol.Enabled → SendFailed (выпуск невозможен).
func TestApplyIssued_PolicyDisabled(t *testing.T) {
	fs := &fakeStore{}
	pol := certpolicy.Policy{Service: "redis", Present: true, Enabled: false, PKIRole: "redis-tls"}
	m := newIssuedModule(fs, &fakeAudit{}, &fakeSigner{}, &fakeVaultWriter{}, &fakePolicyResolver{pol: pol})

	stream := runIssued(t, m, map[string]any{"incarnation": "redis-prod"})

	if !stream.Last().Failed {
		t.Fatal("ожидался failed при выключенном certificate_rotation")
	}
	if len(fs.registered) != 0 {
		t.Errorf("нет RegisterActive при disabled, got %d", len(fs.registered))
	}
}

// validIssueSigner — signer с валидным cert-PEM: без него certissue.Issue упал бы
// на пустом PEM и тест «зеленел» бы по НЕ той причине. Так фейл может прийти только
// от гейта, который проверяем.
func validIssueSigner(t *testing.T) *fakeSigner {
	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	certPEM, _ := makeCertPEM(t, "redis-prod.tls", notAfter)
	return &fakeSigner{certPEM: certPEM, serial: "S1", notAfter: notAfter}
}

// TestApplyIssued_UnknownScenario_FailsFast — GUARD (NIM-99 review MAJOR): манифест
// объявил scenario, которого нет среди scenario/ сервиса → SendFailed ДО энролла
// (иначе серт с auto_rotate=true молча скипается ротатором → тихое истечение).
func TestApplyIssued_UnknownScenario_FailsFast(t *testing.T) {
	fs := &fakeStore{}
	pol := certpolicy.Policy{Service: "redis", Present: true, Enabled: true, PKIRole: "redis-tls",
		Scenario: "rotate_tls", KnownScenarios: []string{"some_other_scenario"}}
	m := newIssuedModule(fs, &fakeAudit{}, validIssueSigner(t), &fakeVaultWriter{}, &fakePolicyResolver{pol: pol})

	ev := runIssued(t, m, map[string]any{"incarnation": "redis-prod"}).Last()

	if !ev.Failed || !strings.Contains(ev.Message, "сценарий ротации") {
		t.Fatalf("ожидался failed про сценарий ротации, got failed=%v msg=%q", ev.Failed, ev.Message)
	}
	if len(fs.registered) != 0 {
		t.Errorf("RegisterActive не должен вызываться при неизвестном сценарии, got %d", len(fs.registered))
	}
}

// TestApplyIssued_EmptyScenario_FailsFast — GUARD: enable:true, но scenario пуст →
// SendFailed, RegisterActive НЕ вызван.
func TestApplyIssued_EmptyScenario_FailsFast(t *testing.T) {
	fs := &fakeStore{}
	pol := certpolicy.Policy{Service: "redis", Present: true, Enabled: true, PKIRole: "redis-tls",
		Scenario: "", KnownScenarios: []string{"rotate_tls"}}
	m := newIssuedModule(fs, &fakeAudit{}, validIssueSigner(t), &fakeVaultWriter{}, &fakePolicyResolver{pol: pol})

	ev := runIssued(t, m, map[string]any{"incarnation": "redis-prod"}).Last()

	if !ev.Failed || !strings.Contains(ev.Message, "сценарий ротации") {
		t.Fatalf("ожидался failed про сценарий ротации, got failed=%v msg=%q", ev.Failed, ev.Message)
	}
	if len(fs.registered) != 0 {
		t.Errorf("RegisterActive не должен вызываться при пустом сценарии, got %d", len(fs.registered))
	}
}

// TestApplyIssued_EmptyPKIRole_FailsFast — GUARD (NIM-99 QA G2): enable:true, сценарий
// валиден, но pki_role пуст → SendFailed, RegisterActive НЕ вызван.
func TestApplyIssued_EmptyPKIRole_FailsFast(t *testing.T) {
	fs := &fakeStore{}
	pol := certpolicy.Policy{Service: "redis", Present: true, Enabled: true, PKIRole: "",
		Scenario: "rotate_tls", KnownScenarios: []string{"rotate_tls"}}
	m := newIssuedModule(fs, &fakeAudit{}, validIssueSigner(t), &fakeVaultWriter{}, &fakePolicyResolver{pol: pol})

	ev := runIssued(t, m, map[string]any{"incarnation": "redis-prod"}).Last()

	if !ev.Failed || !strings.Contains(ev.Message, "pki_role") {
		t.Fatalf("ожидался failed про pki_role, got failed=%v msg=%q", ev.Failed, ev.Message)
	}
	if len(fs.registered) != 0 {
		t.Errorf("RegisterActive не должен вызываться при пустом pki_role, got %d", len(fs.registered))
	}
}

// TestApplyIssued_PKIRoleFromPolicyNotParams — pki_role в params ИГНОРИРУЕТСЯ,
// подпись и warrant используют pol.PKIRole (роль из манифеста).
func TestApplyIssued_PKIRoleFromPolicyNotParams(t *testing.T) {
	notAfter := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	certPEM, _ := makeCertPEM(t, "redis-prod.tls", notAfter)
	signer := &fakeSigner{certPEM: certPEM, serial: "S1", notAfter: notAfter}
	fs := &fakeStore{}
	pol := certpolicy.Policy{Service: "redis", Present: true, Enabled: true, PKIRole: "policy-role", Scenario: "rotate_tls", KnownScenarios: []string{"rotate_tls"}}
	m := newIssuedModule(fs, &fakeAudit{}, signer, &fakeVaultWriter{}, &fakePolicyResolver{pol: pol})

	stream := runIssued(t, m, map[string]any{"incarnation": "redis-prod", "pki_role": "attacker-role"})

	if stream.Last().Failed {
		t.Fatalf("unexpected failure: %s", stream.Last().Message)
	}
	if signer.gotRole != "policy-role" {
		t.Errorf("signer подписал ролью %q, want policy-role (params.pki_role обязан игнорироваться)", signer.gotRole)
	}
	if got := fs.registered[0].w.PKIRole; got == nil || *got != "policy-role" {
		t.Errorf("warrant PKIRole = %v, want policy-role", got)
	}
}

// TestApplyIssued_GateNotConfigured — Signer=nil (модуль не сконфигурирован) →
// SendFailed «не сконфигурирован», RegisterActive НЕ вызывается.
func TestApplyIssued_GateNotConfigured(t *testing.T) {
	fs := &fakeStore{}
	m := coremodcert.New(nil, fs, &fakeAudit{}, "kid-1") // Signer намеренно не выставлен
	m.VaultWriter = &fakeVaultWriter{}
	m.Policy = &fakePolicyResolver{pol: certpolicy.Policy{Service: "redis", Enabled: true, PKIRole: "r"}}
	m.CSRGen = issuedCSRGen
	m.PKIMount = func() string { return "pki-int" }

	stream := runIssued(t, m, map[string]any{"incarnation": "redis-prod"})

	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("ожидался failed при nil Signer (не сконфигурирован)")
	}
	if !strings.Contains(ev.Message, "не сконфигурирован") {
		t.Errorf("message = %q, ожидалось упоминание «не сконфигурирован»", ev.Message)
	}
	if len(fs.registered) != 0 {
		t.Errorf("нет RegisterActive при неконфигурированном модуле, got %d", len(fs.registered))
	}
}
