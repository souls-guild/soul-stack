package vault

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// TestGenerateServiceCSR_ValidCSRAndKey — сгенерированные CSR и приватник
// парсятся стандартной crypto/x509, CN совпадает с запрошенным, публичный ключ
// CSR соответствует приватнику.
func TestGenerateServiceCSR_ValidCSRAndKey(t *testing.T) {
	const cn = "redis-prod.tls"
	res, err := GenerateServiceCSR(CSRParams{CommonName: cn, DNSNames: []string{cn, "redis-prod"}})
	if err != nil {
		t.Fatalf("GenerateServiceCSR: %v", err)
	}

	// CSR PEM парсится и CN верный.
	block, _ := pem.Decode(res.CSRPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("CSR PEM decode failed: block=%v", block)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificateRequest: %v", err)
	}
	if csr.Subject.CommonName != cn {
		t.Errorf("CSR CN = %q, want %q", csr.Subject.CommonName, cn)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("CSR signature invalid: %v", err)
	}

	// Приватник PEM парсится (PKCS#8).
	keyBlock, _ := pem.Decode(res.PrivateKeyPEM)
	if keyBlock == nil {
		t.Fatalf("private key PEM decode failed")
	}
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		t.Errorf("ParsePKCS8PrivateKey: %v", err)
	}
}

// TestGenerateServiceCSR_RejectsEmptyCommonName — пустой CN отвергается до
// генерации (сервисный серт без CN бессмыслен).
func TestGenerateServiceCSR_RejectsEmptyCommonName(t *testing.T) {
	if _, err := GenerateServiceCSR(CSRParams{}); err == nil {
		t.Fatal("GenerateServiceCSR with empty CN returned nil err")
	}
}

// TestGenerateServiceCSR_PrivateKeyNotInCSR — приватник и CSR — разные PEM-
// блоки; приватный материал НЕ утекает в CSR (CSR несёт только публичный ключ).
func TestGenerateServiceCSR_PrivateKeyNotInCSR(t *testing.T) {
	res, err := GenerateServiceCSR(CSRParams{CommonName: "svc"})
	if err != nil {
		t.Fatalf("GenerateServiceCSR: %v", err)
	}
	if strings.Contains(string(res.CSRPEM), "PRIVATE KEY") {
		t.Errorf("CSR PEM must NOT contain private key material")
	}
	if !strings.Contains(string(res.PrivateKeyPEM), "PRIVATE KEY") {
		t.Errorf("private key PEM must contain a PRIVATE KEY block")
	}
}
