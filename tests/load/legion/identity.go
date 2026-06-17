// Package legion — нагрузочный stub-генератор soul-legion (Ф0, см.
// docs/testing/load-testing.md §3 «компонент 1»). Поднимает N одновременных
// fake-Soul-стримов (gRPC bidi поверх mTLS) к живому Keeper-у, чтобы измерить
// пропускную способность по оси A (стримы) и сверить с расчётной таблицей
// scaling.md.
//
// Test-only: НЕ поставочный бинарь (ADR-004). Контракт эмуляции тот же, что у
// tests/e2e/internal/soulstub: Hello → удержание стрима → keepalive →
// SoulprintReport → RunResult на ApplyRequest. soul-legion НЕ парсит Destiny и
// НЕ применяет — нагрузка мерится на Keeper, а не на нагрузочном хосте.
package legion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Identity — mTLS-материал одной fake-Soul: SID + leaf-cert/key из dev-CA +
// fingerprint (SHA-256 над SubjectPublicKeyInfo, ключ авторизации Keeper-а в
// soul_seeds) + serial (уникальный серийник из PKI-ответа для soul_seeds).
type Identity struct {
	SID         string
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint string
	Serial      string
}

// VaultPKI — минимальный HTTP-клиент над Vault PKI (issue leaf-cert). Прямой
// HTTP, как tests/e2e/harness/vault.go: tests/load — отдельный go-модуль, ему
// нельзя импортировать keeper/internal/* (Go internal-rules).
type VaultPKI struct {
	addr       string // http://127.0.0.1:8200
	token      string // dev root token
	mount      string // pki
	role       string // soul-seed
	httpClient *http.Client
}

// NewVaultPKI собирает клиент. mount/role — по dev-стенду (pki / soul-seed,
// см. dev/provision.sh шаги 3–5).
func NewVaultPKI(addr, token, mount, role string) *VaultPKI {
	return &VaultPKI{
		addr:       addr,
		token:      token,
		mount:      mount,
		role:       role,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Issue выпускает leaf-cert на CN=sid через `<mount>/issue/<role>`. Возвращает
// готовую Identity с вычисленным fingerprint-ом (ключ авторизации Keeper-а в
// soul_seeds) и серийником из PKI-ответа.
func (v *VaultPKI) Issue(ctx context.Context, sid string, ttl string) (Identity, error) {
	body, _ := json.Marshal(map[string]any{
		"common_name": sid,
		"ttl":         ttl,
	})
	url := fmt.Sprintf("%s/v1/%s/issue/%s", v.addr, v.mount, v.role)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("legion: build issue request: %w", err)
	}
	req.Header.Set("X-Vault-Token", v.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("legion: vault issue %s: %w", sid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return Identity{}, fmt.Errorf("legion: vault issue %s: status %d: %s", sid, resp.StatusCode, string(b))
	}

	var out struct {
		Data struct {
			Certificate  string `json:"certificate"`
			PrivateKey   string `json:"private_key"`
			SerialNumber string `json:"serial_number"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, fmt.Errorf("legion: decode issue %s: %w", sid, err)
	}
	if out.Data.Certificate == "" || out.Data.PrivateKey == "" {
		return Identity{}, fmt.Errorf("legion: vault issue %s: empty certificate/key in response", sid)
	}

	certPEM := []byte(out.Data.Certificate)
	fp, err := fingerprintFromPEM(certPEM)
	if err != nil {
		return Identity{}, fmt.Errorf("legion: fingerprint %s: %w", sid, err)
	}
	// serial_number от Vault — формат "3a:5f:..."; soul_seeds.serial_number
	// уникален и произволен по форме, кладём как есть.
	return Identity{
		SID:         sid,
		CertPEM:     certPEM,
		KeyPEM:      []byte(out.Data.PrivateKey),
		Fingerprint: fp,
		Serial:      out.Data.SerialNumber,
	}, nil
}

// fingerprintFromPEM вычисляет fingerprint ровно как keeper-side
// soulseed.FingerprintFromCert: SHA-256 над RawSubjectPublicKeyInfo (НЕ над
// PEM-байтами). Расхождение → Keeper отвергает стрим «unknown soul seed».
func fingerprintFromPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("cert не PEM-блок")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse cert: %w", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), nil
}
