package grpc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// errBeginner — BootstrapPool, у которого Begin всегда падает. QueryRow
// отдаёт валидный активный токен, чтобы pre-check прошёл и handler дошёл до
// транзакции (и проксировал её ошибку как Internal).
type errBeginner struct{ err error }

func (e errBeginner) Begin(_ context.Context) (pgx.Tx, error) {
	return nil, e.err
}

func (e errBeginner) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return activeTokenRow{sid: "host.example.com"}
}

func (e errBeginner) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (e errBeginner) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

// activeTokenRow — pgx.Row, имитирующий строку bootstrap_tokens с активным
// (не сожжённым, не истёкшим) токеном для заданного SID. Колонки —
// в порядке selectByHashSQL: token_id, sid, token_hash, created_at,
// expires_at, used_at, used_by_kid, created_by_aid.
type activeTokenRow struct{ sid string }

func (r activeTokenRow) Scan(dest ...any) error {
	*(dest[0].(*string)) = "00000000-0000-0000-0000-000000000001"
	*(dest[1].(*string)) = r.sid
	*(dest[2].(*string)) = "0000000000000000000000000000000000000000000000000000000000000000"
	*(dest[3].(*time.Time)) = time.Now().UTC().Add(-time.Minute)
	*(dest[4].(*time.Time)) = time.Now().UTC().Add(time.Hour)
	// used_at / used_by_kid / created_by_aid — nil-pointer-цели (активный токен).
	*(dest[5].(**time.Time)) = nil
	*(dest[6].(**string)) = nil
	*(dest[7].(**string)) = nil
	return nil
}

// vaultPoolFake — BootstrapPool с настраиваемым pre-check-результатом.
// notFound=true → QueryRow отдаёт ErrNoRows (мусорный токен), early-reject.
// Begin не вызывается в early-reject-тестах.
type vaultPoolFake struct{ notFound bool }

func (p vaultPoolFake) Begin(_ context.Context) (pgx.Tx, error) {
	return nil, errors.New("vaultPoolFake: Begin must not be reached in early-reject test")
}

func (p vaultPoolFake) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	if p.notFound {
		return scanErrRow{err: pgx.ErrNoRows}
	}
	return activeTokenRow{sid: "host.example.com"}
}

func (p vaultPoolFake) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (p vaultPoolFake) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

// signerStub — CSRSigner, возвращающий заранее заданный результат / ошибку.
type signerStub struct {
	res *keepervault.SignedCertificate
	err error
}

func (s signerStub) SignCSR(_ context.Context, _, _, _ string) (*keepervault.SignedCertificate, error) {
	return s.res, s.err
}

// countingSigner — CSRSigner со счётчиком вызовов. Для проверки M3:
// early-reject мусорного токена НЕ должен дёргать Vault (calls == 0).
type countingSigner struct {
	calls int
	res   *keepervault.SignedCertificate
	err   error
}

func (s *countingSigner) SignCSR(_ context.Context, _, _, _ string) (*keepervault.SignedCertificate, error) {
	s.calls++
	return s.res, s.err
}

func TestBootstrap_InvalidSID(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: fakeTxBeginner{}, VaultClient: fakeSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "BAD_SID",
		BootstrapToken: "tok",
		CsrPem:         []byte("dummy"),
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
}

// TestBootstrap_ReservedSID — Soul с зарезервированным sid (keeper) отклоняется
// до Vault/DB: reject синтетики прогона на bootstrap (NIM-36).
func TestBootstrap_ReservedSID(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: fakeTxBeginner{}, VaultClient: fakeSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "keeper",
		BootstrapToken: "tok",
		CsrPem:         []byte("dummy"),
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
}

func TestBootstrap_EmptyToken(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: fakeTxBeginner{}, VaultClient: fakeSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "   ",
		CsrPem:         []byte("dummy"),
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
}

func TestBootstrap_EmptyCSR(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: fakeTxBeginner{}, VaultClient: fakeSigner{}, AuditWriter: nopAudit{},
		KID: "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		CsrPem:         nil,
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
}

func TestBootstrap_VaultErr_PKIMountEmpty(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool:        vaultPoolFake{notFound: false},
		VaultClient: signerStub{err: keepervault.ErrPKIMountEmpty},
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", got)
	}
}

func TestBootstrap_VaultErr_TransientUnavailable(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool:        vaultPoolFake{notFound: false},
		VaultClient: signerStub{err: errors.New("connection refused")},
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	})
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", got)
	}
}

// TestBootstrap_VaultBadCert — Vault вернул не-PEM «cert». Должна
// сложиться Internal.
func TestBootstrap_VaultBadCert(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{
		Pool: vaultPoolFake{notFound: false},
		VaultClient: signerStub{res: &keepervault.SignedCertificate{
			CertificatePEM: []byte("not a cert"),
			SerialNumber:   "1",
		}},
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal", got)
	}
}

// TestBootstrap_TxErr_AfterValidCert — Vault выдал реальный cert,
// транзакция падает на Begin → ожидаем Internal.
func TestBootstrap_TxErr_AfterValidCert(t *testing.T) {
	certPEM := makeFakeCertPEM(t)
	h := newBootstrapHandler(BootstrapDeps{
		Pool: errBeginner{err: errors.New("pg unavailable")},
		VaultClient: signerStub{res: &keepervault.SignedCertificate{
			CertificatePEM: certPEM,
			SerialNumber:   "0xDEADBEEF",
		}},
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		CsrPem:         makeCSRPEM(t, "host.example.com"),
	})
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal", got)
	}
}

// TestBootstrap_CSRCommonNameMismatch — CSR с CN ≠ запрашиваемый SID
// отвергается InvalidArgument ДО Vault SignCSR (defense-in-depth, crypto).
// countingSigner.calls обязан остаться 0: невалидный CN не должен триггерить
// PKI-round-trip.
func TestBootstrap_CSRCommonNameMismatch(t *testing.T) {
	signer := &countingSigner{}
	h := newBootstrapHandler(BootstrapDeps{
		Pool:        vaultPoolFake{notFound: false},
		VaultClient: signer,
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		// CSR подписан под ЧУЖОЙ CN — атакующий просит cert на host.example.com,
		// но кладёт CSR с CN=evil.example.com.
		CsrPem: makeCSRPEM(t, "evil.example.com"),
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
	if signer.calls != 0 {
		t.Fatalf("SignCSR called %d times, want 0 (CN mismatch must reject before Vault)", signer.calls)
	}
}

// TestBootstrap_CSREmptyCommonName — CSR с пустым CN (нет Subject.CommonName)
// тоже отвергается InvalidArgument ДО Vault, не считается совпадением с SID.
func TestBootstrap_CSREmptyCommonName(t *testing.T) {
	signer := &countingSigner{}
	h := newBootstrapHandler(BootstrapDeps{
		Pool:        vaultPoolFake{notFound: false},
		VaultClient: signer,
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		CsrPem:         makeCSRPEM(t, ""),
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
	if signer.calls != 0 {
		t.Fatalf("SignCSR called %d times, want 0 (empty CN must reject before Vault)", signer.calls)
	}
}

// TestBootstrap_CSRMalformedPEM — мусорный (не-PEM) csr_pem отвергается
// InvalidArgument ДО Vault: парсинг CSR падает на pem.Decode.
func TestBootstrap_CSRMalformedPEM(t *testing.T) {
	signer := &countingSigner{}
	h := newBootstrapHandler(BootstrapDeps{
		Pool:        vaultPoolFake{notFound: false},
		VaultClient: signer,
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "tok",
		CsrPem:         []byte("not a pem at all"),
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
	if signer.calls != 0 {
		t.Fatalf("SignCSR called %d times, want 0 (malformed CSR must reject before Vault)", signer.calls)
	}
}

// TestBootstrap_EarlyReject_NoVaultCall — M3: мусорный (несуществующий)
// токен отвергается ДО Vault-sign-а. countingSigner.calls обязан остаться 0.
func TestBootstrap_EarlyReject_NoVaultCall(t *testing.T) {
	signer := &countingSigner{}
	h := newBootstrapHandler(BootstrapDeps{
		Pool:        vaultPoolFake{notFound: true},
		VaultClient: signer,
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "host.example.com",
		BootstrapToken: "garbage-token",
		// Валидный CSR с правильным CN — чтобы тест проверял именно token-precheck,
		// а не CSR-валидацию (которая стоит раньше).
		CsrPem: makeCSRPEM(t, "host.example.com"),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", got)
	}
	if signer.calls != 0 {
		t.Fatalf("SignCSR called %d times, want 0 (early-reject must not touch Vault)", signer.calls)
	}
}

// TestBootstrap_EarlyReject_WrongSID — токен существует и активен, но SID в
// запросе не совпадает с привязкой токена → anti-enum PermissionDenied и
// никакого Vault-вызова.
func TestBootstrap_EarlyReject_WrongSID(t *testing.T) {
	signer := &countingSigner{}
	h := newBootstrapHandler(BootstrapDeps{
		// activeTokenRow привязан к host.example.com; запрос придёт с другим SID.
		Pool:        vaultPoolFake{notFound: false},
		VaultClient: signer,
		AuditWriter: nopAudit{},
		KID:         "k1", PKIMount: "pki", PKIRole: "soul-seed",
	}, discardLogger(t))

	_, err := h.Bootstrap(context.Background(), &keeperv1.BootstrapRequest{
		Sid:            "other.example.com",
		BootstrapToken: "tok",
		// CSR-CN совпадает с запрошенным SID (other.example.com) — CSR-валидация
		// проходит, отказ приходит из token-precheck (токен привязан к
		// host.example.com), что и проверяет тест.
		CsrPem: makeCSRPEM(t, "other.example.com"),
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", got)
	}
	if signer.calls != 0 {
		t.Fatalf("SignCSR called %d times, want 0 (wrong-SID must early-reject)", signer.calls)
	}
}

// TestBootstrap_Ping — Ping реализован независимо от Bootstrap, должен
// проходить даже без рабочих deps.
func TestBootstrap_Ping(t *testing.T) {
	h := newBootstrapHandler(BootstrapDeps{KID: "kid-x"}, discardLogger(t))
	reply, err := h.Ping(context.Background(), &keeperv1.PingRequest{})
	if err != nil {
		t.Fatalf("Ping err: %v", err)
	}
	if reply.GetVersion() != "kid-x" {
		t.Errorf("Ping.Version = %q, want kid-x", reply.GetVersion())
	}
}

func TestParseCertificatePEM_BadInput(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"not-pem", []byte("garbage")},
		{"wrong-type", pemBlock(t, "PRIVATE KEY", []byte{0x01, 0x02})},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCertificatePEM(tc.in); err == nil {
				t.Fatalf("expected error for %q", tc.name)
			}
		})
	}
}

func pemBlock(t *testing.T, typ string, body []byte) []byte {
	t.Helper()
	var b strings.Builder
	if err := pem.Encode(&blockToWriter{&b}, &pem.Block{Type: typ, Bytes: body}); err != nil {
		t.Fatalf("pem.Encode: %v", err)
	}
	return []byte(b.String())
}

// blockToWriter — io.Writer-обёртка над strings.Builder для pem.Encode.
type blockToWriter struct{ b *strings.Builder }

func (w *blockToWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

// makeFakeCertPEM генерит self-signed RSA-cert и возвращает PEM. Используется
// в тесте transaction-фейл-после-Vault: x509.ParseCertificate должна пройти.
func makeFakeCertPEM(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		Subject:      pkix.Name{CommonName: "host.example.com"},
		SerialNumber: bigIntOne(),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	var b strings.Builder
	if err := pem.Encode(&blockToWriter{&b}, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("pem.Encode: %v", err)
	}
	return []byte(b.String())
}

func bigIntOne() *big.Int { return big.NewInt(1) }

// makeCSRPEM генерит валидный RSA-CSR с заданным CommonName и возвращает PEM.
// Пустой cn → CSR без Subject.CommonName (для теста empty-CN-reject). Подпись
// настоящая (x509.CreateCertificateRequest), парсинг в validateCSRCommonName
// проходит, проверяется именно CN.
func makeCSRPEM(t *testing.T, cn string) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		t.Fatalf("CreateCertificateRequest: %v", err)
	}
	var b strings.Builder
	if err := pem.Encode(&blockToWriter{&b}, &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}); err != nil {
		t.Fatalf("pem.Encode csr: %v", err)
	}
	return []byte(b.String())
}
