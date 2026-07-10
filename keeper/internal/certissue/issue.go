// Package certissue — канонический issue-material TLS-сертов: генерация
// keypair+CSR, подпись через Vault PKI и запись cert+key в Vault по E3-путям
// (NIM-99). Общий фундамент для reaper (авто-ротация) и coremod/cert (issue-шаг):
// логика выпуска живёт здесь, чтобы оба потребителя не расходились.
//
// ★ R2-инвариант: приватный ключ (privPEM) генерится Keeper-ом, пишется только в
// Vault и НИКОГДА не логируется / не кладётся в audit / не попадает в текст
// ошибки (осознанное исключение из identity-инварианта).
package certissue

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	keepercert "github.com/souls-guild/soul-stack/keeper/internal/cert"
)

// SignedCert — результат PKI-подписи (зеркало vault.SignedCertificate; пакет не
// тянет vault ради типа, adapter в wire-up конвертирует).
type SignedCert struct {
	CertificatePEM []byte
	CAChainPEM     []byte
	SerialNumber   string
	NotAfter       time.Time
}

// Signer — узкая поверхность Vault PKI sign-RPC (vault.Client.SignCSR).
type Signer interface {
	SignCSR(ctx context.Context, mount, role, csrPEM string) (*SignedCert, error)
}

// KVWriter — узкая поверхность записи в Vault KV (vault.Client.WriteKV).
type KVWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// CSRGenFunc генерит keypair+CSR (keeper-side, R2). Прод — обёртка над
// vault.GenerateServiceCSR; тесты подставляют детерминированный fake.
type CSRGenFunc func(commonName string, dnsNames []string) (privPEM, csrPEM []byte, err error)

// Params — вход [Issue]: identity серта (CN/DNS), PKI-координаты (Mount/Role) и
// E3-пути записи материала в Vault (CertPath/KeyPath).
type Params struct {
	CommonName string
	DNSNames   []string
	Mount      string
	Role       string
	CertPath   string
	KeyPath    string
}

// Material — результат [Issue]: новый cert+key + Vault-refs (`<path>#field`) для
// регистрации warrant-а.
type Material struct {
	CertPEM      []byte
	KeyPEM       []byte
	SerialNumber string
	Fingerprint  string
	NotAfter     time.Time
	CertRef      string
	KeyRef       string
}

// VaultPath — E3-путь TLS-материала инкарнации: secret/<service>/<incarnation>/tls/<kind>.
// Единый источник convention для выпуска (core.cert.issued) и ротации (Reaper):
// issue и rotate обязаны строить ОДИН путь, иначе серт пишется в один, читается
// из другого. Для service="redis" идентичен прежнему хардкоду (backcompat).
func VaultPath(service, incarnation string, kind keepercert.Kind) string {
	// Defense-in-depth: service/incarnation — валидированные идентификаторы; `/`,
	// `..` или пустое = баг вызывающего, иначе путь вырвался бы из secret/<svc>/<inc>/.
	if !safeVaultSegment(service) || !safeVaultSegment(incarnation) {
		panic(fmt.Sprintf("certissue.VaultPath: небезопасный сегмент (service=%q incarnation=%q)", service, incarnation))
	}
	return "secret/" + service + "/" + incarnation + "/tls/" + string(kind)
}

// safeVaultSegment отвергает пустой сегмент и всё, что может вырваться из secret/
// (разделитель `/`, traversal `..`).
func safeVaultSegment(s string) bool {
	return s != "" && !strings.Contains(s, "/") && !strings.Contains(s, "..")
}

// Issue выпускает TLS-серт: csrgen(CN,DNS) → SignCSR → WriteKV(cert)+WriteKV(key)
// → fingerprint. CAChainPEM отбрасывается (CA не ротируется). ★ privPEM в текст
// ошибки не попадает ни на одном шаге (R2).
func Issue(ctx context.Context, s Signer, w KVWriter, csrgen CSRGenFunc, p Params) (*Material, error) {
	privPEM, csrPEM, err := csrgen(p.CommonName, p.DNSNames)
	if err != nil {
		return nil, fmt.Errorf("certissue: csrgen: %w", err)
	}

	signed, err := s.SignCSR(ctx, p.Mount, p.Role, string(csrPEM))
	if err != nil {
		return nil, fmt.Errorf("certissue: sign csr: %w", err)
	}

	// cert-PEM в поле `cert`, key-PEM в поле `key` (парити essence-конвенции
	// tls_cert_ref "<path>#cert"). WriteKV значения в текст ошибки не кладёт.
	if werr := w.WriteKV(ctx, p.CertPath, map[string]any{"cert": string(signed.CertificatePEM)}); werr != nil {
		return nil, fmt.Errorf("certissue: write cert to vault: %w", werr)
	}
	if werr := w.WriteKV(ctx, p.KeyPath, map[string]any{"key": string(privPEM)}); werr != nil {
		return nil, fmt.Errorf("certissue: write key to vault: %w", werr)
	}

	fingerprint, err := fingerprintFromPEM(signed.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("certissue: fingerprint new cert: %w", err)
	}

	return &Material{
		CertPEM:      signed.CertificatePEM,
		KeyPEM:       privPEM,
		SerialNumber: signed.SerialNumber,
		Fingerprint:  fingerprint,
		NotAfter:     signed.NotAfter.UTC(),
		CertRef:      p.CertPath + "#cert",
		KeyRef:       p.KeyPath + "#key",
	}, nil
}

// fingerprintFromPEM парсит первый CERTIFICATE-блок PEM и считает fingerprint
// SHA-256(SubjectPublicKeyInfo) через [keepercert.FingerprintFromCert].
func fingerprintFromPEM(pemBytes []byte) (string, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return "", fmt.Errorf("certissue: no CERTIFICATE block in PEM")
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return "", fmt.Errorf("certissue: parse certificate: %w", err)
			}
			return keepercert.FingerprintFromCert(cert), nil
		}
	}
}
