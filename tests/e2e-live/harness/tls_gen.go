package harness

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// GenerateRedisTLSMaterial генерит self-signed CA + серверный cert/key, подписанный
// им (все три — PEM). Вызывается ДВАЖДЫ (CA1, CA2 — независимые) для CA-rollover-
// ротации rotate_tls (находка #4).
//
// ★ Серверный cert несёт IPAddresses=[127.0.0.1] (SAN): плагин community.redis и
// health-probe create коннектятся go-tls на 127.0.0.1:<tls_port>, а go-tls по
// умолчанию проверяет ServerName против SAN — без IP-SAN 127.0.0.1 коннект упёрся бы
// в «certificate is valid for … not 127.0.0.1». DNSNames=[localhost] — на всякий.
//
// fingerprintSHA256 — sha256 от DER серверного cert в hex; после
// normalizeHexFingerprint совпадает с выводом `openssl x509 -fingerprint -sha256`
// (тот же нормализатор в AssertRedisTLSCertServed: регистр/двоеточия отбрасываются).
func GenerateRedisTLSMaterial(t *testing.T) (caPEM, certPEM, keyPEM, fingerprintSHA256 string) {
	t.Helper()

	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := time.Now().Add(72 * time.Hour)

	// Self-signed CA (IsCA, CertSign).
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateRedisTLSMaterial: CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          randSerial(t),
		Subject:               pkix.Name{CommonName: "soul-stack-redis-test-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("GenerateRedisTLSMaterial: create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("GenerateRedisTLSMaterial: parse CA cert: %v", err)
	}

	// Серверный leaf-cert, подписанный CA.
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateRedisTLSMaterial: server key: %v", err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: randSerial(t),
		Subject:      pkix.Name{CommonName: "redis-server"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("GenerateRedisTLSMaterial: create server cert: %v", err)
	}
	srvKeyDER, err := x509.MarshalPKCS8PrivateKey(srvKey)
	if err != nil {
		t.Fatalf("GenerateRedisTLSMaterial: marshal server key: %v", err)
	}

	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: srvKeyDER}))

	// fingerprint = sha256(DER серверного cert) — ровно то, что отдаёт openssl по wire.
	sum := sha256.Sum256(srvDER)
	fingerprintSHA256 = hex.EncodeToString(sum[:])
	return
}

// randSerial — случайный 128-битный серийник сертификата.
func randSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("randSerial: %v", err)
	}
	return n
}
