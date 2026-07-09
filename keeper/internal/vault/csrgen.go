package vault

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
)

// serviceCSRKeySize — размер RSA-ключа сервисного серта. 2048 — паритет с
// SoulSeed-CSR (soul/internal/bootstrap, rsaKeySize) и достаточная стойкость
// для TLS-материала с регулярной ротацией.
const serviceCSRKeySize = 2048

// CSRParams — параметры генерации CSR сервисного серта.
type CSRParams struct {
	// CommonName — CN субъекта (обязателен). Для серверного TLS обычно
	// совпадает с логическим именем сервиса/инкарнации.
	CommonName string
	// DNSNames — SAN-имена (опционально). Пустой список → SAN без DNS.
	DNSNames []string
}

// GeneratedCSR — результат [GenerateServiceCSR].
//
// ★ R2-ИСКЛЮЧЕНИЕ (cert-rotation Вар1): PrivateKeyPEM генерится Keeper-ом
// централизованно и ПОКИДАЕТ границу генерации (caller кладёт его в Vault через
// WriteKV). Это осознанное отступление от инварианта identity-модели «приватник
// НИКОГДА не покидает хост» (soul_seeds/009, ADR-018): здесь материал — не
// identity-серт Soul-агента, а СЕРВИСНЫЙ серт (напр. серверный TLS Redis),
// который и так лежит в Vault для ручного rotate_tls. Решение зафиксировано
// отдельным ADR cert-rotation. Приватник в PG (реестр warrant) НЕ пишется —
// только vault_ref + fingerprint + serial.
type GeneratedCSR struct {
	// PrivateKeyPEM — PKCS#8 PEM приватного ключа. Caller обязан положить его
	// в Vault и НЕ логировать / не класть в audit-payload / не возвращать
	// наружу (secret-material).
	PrivateKeyPEM []byte
	// CSRPEM — PKCS#10 CSR PEM (несёт только публичный ключ). Идёт в
	// [Client.SignCSR].
	CSRPEM []byte
}

// GenerateServiceCSR генерирует RSA keypair и PKCS#10 CSR сервисного серта.
// Приватник остаётся в памяти (в возвращаемом [GeneratedCSR.PrivateKeyPEM]);
// сеть/файловая система/Vault здесь НЕ трогаются — это чистая крипто-функция.
//
// Flow cert-rotation (keeper/internal/reaper/rotate_certs.go):
// GenerateServiceCSR → [Client.SignCSR] (Vault PKI) → [Client.WriteKV]
// (приватник+cert в Vault) → INSERT warrant.
func GenerateServiceCSR(p CSRParams) (*GeneratedCSR, error) {
	if p.CommonName == "" {
		return nil, fmt.Errorf("vault csrgen: common_name is empty")
	}

	key, err := rsa.GenerateKey(rand.Reader, serviceCSRKeySize)
	if err != nil {
		return nil, fmt.Errorf("vault csrgen: generate rsa key: %w", err)
	}

	tmpl := x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: p.CommonName},
		DNSNames: p.DNSNames,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("vault csrgen: create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})

	// PKCS#8 — универсальная форма приватника (Redis/openssl читают её наравне
	// с PKCS#1); единый формат для сервисных сертов упрощает контракт с Vault.
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("vault csrgen: marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return &GeneratedCSR{PrivateKeyPEM: keyPEM, CSRPEM: csrPEM}, nil
}
