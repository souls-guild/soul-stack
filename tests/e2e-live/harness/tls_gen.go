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

// GenerateRedisTLSMaterial generates a self-signed CA + a server cert/key signed
// by it (all three as PEM). Called TWICE (CA1, CA2 - independent) for the
// rotate_tls CA-rollover rotation (finding #4).
//
// * The server cert carries IPAddresses=[127.0.0.1] (SAN): the community.redis
// plugin and the create health-probe connect via go-tls to 127.0.0.1:<tls_port>,
// and go-tls by default validates ServerName against SAN - without an IP SAN for
// 127.0.0.1 the connection would fail with "certificate is valid for ... not
// 127.0.0.1". DNSNames=[localhost] is added just in case.
//
// fingerprintSHA256 - sha256 of the server cert DER in hex; after
// normalizeHexFingerprint it matches the output of `openssl x509 -fingerprint
// -sha256` (same normalizer used in AssertRedisTLSCertServed: case/colons
// stripped).
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

	// Server leaf cert, signed by the CA.
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

	// fingerprint = sha256(server cert DER) - exactly what openssl reports over the wire.
	sum := sha256.Sum256(srvDER)
	fingerprintSHA256 = hex.EncodeToString(sum[:])
	return
}

// randSerial - random 128-bit certificate serial number.
func randSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("randSerial: %v", err)
	}
	return n
}
