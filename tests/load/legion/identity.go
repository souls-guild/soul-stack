// Package legion -- load stub generator soul-legion (Phase 0, see
// docs/testing/load-testing.md §3 "component 1"). Brings up N concurrent
// fake-Soul streams (gRPC bidi over mTLS) to a live Keeper to measure
// throughput on axis A (streams) and compare against the scaling.md
// projection table.
//
// Test-only: NOT a shipped binary (ADR-004). The emulation contract is the
// same as tests/e2e/internal/soulstub: Hello -> hold the stream -> keepalive
// -> SoulprintReport -> RunResult on ApplyRequest. soul-legion does NOT parse
// Destiny and does NOT apply -- the load is measured on Keeper, not on the
// load-generating host.
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

// Identity -- mTLS material of one fake-Soul: SID + leaf-cert/key from
// dev-CA + fingerprint (SHA-256 over SubjectPublicKeyInfo, Keeper's
// authorization key in soul_seeds) + serial (unique serial from the PKI
// response for soul_seeds).
type Identity struct {
	SID         string
	CertPEM     []byte
	KeyPEM      []byte
	Fingerprint string
	Serial      string
}

// VaultPKI -- minimal HTTP client over Vault PKI (issue leaf-cert). Direct
// HTTP, like tests/e2e/harness/vault.go: tests/load is a separate Go module,
// it cannot import keeper/internal/* (Go internal rules).
type VaultPKI struct {
	addr       string // http://127.0.0.1:8200
	token      string // dev root token
	mount      string // pki
	role       string // soul-seed
	httpClient *http.Client
}

// NewVaultPKI assembles the client. mount/role -- per the dev stand
// (pki / soul-seed, see dev/provision.sh steps 3-5).
func NewVaultPKI(addr, token, mount, role string) *VaultPKI {
	return &VaultPKI{
		addr:       addr,
		token:      token,
		mount:      mount,
		role:       role,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Issue issues a leaf-cert with CN=sid via `<mount>/issue/<role>`. Returns a
// ready Identity with the computed fingerprint (Keeper's authorization key in
// soul_seeds) and the serial from the PKI response.
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
	// serial_number from Vault -- format "3a:5f:..."; soul_seeds.serial_number
	// is unique and free-form, store it as-is.
	return Identity{
		SID:         sid,
		CertPEM:     certPEM,
		KeyPEM:      []byte(out.Data.PrivateKey),
		Fingerprint: fp,
		Serial:      out.Data.SerialNumber,
	}, nil
}

// fingerprintFromPEM computes the fingerprint exactly like the keeper-side
// soulseed.FingerprintFromCert: SHA-256 over RawSubjectPublicKeyInfo (NOT
// over the PEM bytes). Mismatch -> Keeper rejects the stream "unknown soul
// seed".
func fingerprintFromPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("cert is not a PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse cert: %w", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), nil
}
