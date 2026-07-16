package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// PKI-mount-defaults. Match keeper.yml::vault.pki_mount / pki_role
// (see docs/keeper/config.md → vault). With empty values, NewClient in
// M2.1.b doesn't substitute defaults — the caller (keeper run) must pass
// resolved values; the constants are kept for readiness checks /
// documentation.
const (
	DefaultPKIMount = "pki"
	DefaultPKIRole  = "soul-seed"
)

// SignedCertificate is the result of signing a CSR via the Vault PKI sign RPC.
//
// CertificatePEM contains exactly one CERTIFICATE PEM block (issued cert).
// CAChainPEM is the concatenation of the `issuing_ca` + `ca_chain[]` PEM
// blocks (if the backend returns a chain), in the order Vault returns them.
// Not validated — the caller passes it to the client as-is.
type SignedCertificate struct {
	CertificatePEM []byte
	CAChainPEM     []byte
	SerialNumber   string
	NotAfter       time.Time
}

// Sentinel errors for the PKI flow.
var (
	// ErrPKIMountEmpty means the mount is empty. The caller (gRPC handler)
	// maps this to an internal error (config misconfig).
	ErrPKIMountEmpty = errors.New("vault pki: mount is empty")
	// ErrPKIRoleEmpty means the role is empty.
	ErrPKIRoleEmpty = errors.New("vault pki: role is empty")
	// ErrPKICSREmpty means an empty CSR PEM (validated BEFORE the round trip to Vault).
	ErrPKICSREmpty = errors.New("vault pki: csr is empty")
	// ErrPKIResponseInvalid means Vault returned a successful response, but
	// without the `certificate` / `serial_number` fields. This signals the
	// PKI backend is misconfigured; the caller returns 500.
	ErrPKIResponseInvalid = errors.New("vault pki: response missing required fields")
)

// SignCSR signs a PEM CSR via Vault PKI `<mount>/sign/<role>`.
//
// Doesn't compute the fingerprint or write to the registry — that's the
// caller's responsibility (gRPC Bootstrap handler).
//
// Parameters:
//   - mount — path of the PKI engine mount (no trailing slash). Pass
//     `cfg.Vault.PKIMount` from keeper.yml.
//   - role — name of the PKI role Vault issues SoulSeed certificates under.
//     Pass `cfg.Vault.PKIRole`.
//   - csrPEM — PEM-encoded CSR from the Soul (BootstrapRequest.csr_pem). Not
//     validated here — Vault itself rejects an invalid CSR with 400.
//
// Returns:
//   - [SignedCertificate] with CertificatePEM + CAChainPEM + SerialNumber + NotAfter.
//   - sentinel ErrPKI* on pre-flight checks.
//   - wrapped fmt.Errorf on transport / Vault errors.
func (c *Client) SignCSR(ctx context.Context, mount, role, csrPEM string) (*SignedCertificate, error) {
	if mount == "" {
		return nil, ErrPKIMountEmpty
	}
	if role == "" {
		return nil, ErrPKIRoleEmpty
	}
	if strings.TrimSpace(csrPEM) == "" {
		return nil, ErrPKICSREmpty
	}

	mountClean := strings.TrimSuffix(mount, "/")
	path := mountClean + "/sign/" + role

	resp, err := c.c.Logical().WriteWithContext(ctx, path, map[string]any{
		// `format: pem` is the Vault default; passing it explicitly removes
		// ambiguity if the backend is configured with `default_format: der`.
		"format": "pem",
		"csr":    csrPEM,
	})
	if err != nil {
		return nil, fmt.Errorf("vault pki: sign %q: %w", path, err)
	}
	if resp == nil || resp.Data == nil {
		return nil, fmt.Errorf("%w: empty response from %s", ErrPKIResponseInvalid, path)
	}

	out := &SignedCertificate{}

	certVal, ok := resp.Data["certificate"].(string)
	if !ok || certVal == "" {
		return nil, fmt.Errorf("%w: missing 'certificate' field", ErrPKIResponseInvalid)
	}
	out.CertificatePEM = []byte(certVal)

	if serial, ok := resp.Data["serial_number"].(string); ok && serial != "" {
		out.SerialNumber = serial
	} else {
		return nil, fmt.Errorf("%w: missing 'serial_number' field", ErrPKIResponseInvalid)
	}

	if exp, ok := resp.Data["expiration"]; ok {
		t, err := coerceExpiration(exp)
		if err != nil {
			return nil, fmt.Errorf("vault pki: parse expiration: %w", err)
		}
		out.NotAfter = t
	}

	// Vault PKI sign returns `issuing_ca` (single PEM) and `ca_chain`
	// ([]interface{} with PEM strings). We concatenate them into one field
	// in issuing_ca → ca_chain[…] order, so Soul can drop it straight into
	// TrustPool without further parsing.
	var chain strings.Builder
	if ica, ok := resp.Data["issuing_ca"].(string); ok && ica != "" {
		chain.WriteString(ica)
		if !strings.HasSuffix(ica, "\n") {
			chain.WriteByte('\n')
		}
	}
	if raw, ok := resp.Data["ca_chain"].([]any); ok {
		for _, e := range raw {
			s, ok := e.(string)
			if !ok || s == "" {
				continue
			}
			chain.WriteString(s)
			if !strings.HasSuffix(s, "\n") {
				chain.WriteByte('\n')
			}
		}
	}
	out.CAChainPEM = []byte(chain.String())

	return out, nil
}

// coerceExpiration parses `expiration` from a Vault PKI sign response.
//
// Vault returns this field as a **unix timestamp** in a JSON number
// (decoded by the vaultapi client as json.Number → string). We support
// json.Number, numeric types, and the string form (for custom backends).
func coerceExpiration(v any) (time.Time, error) {
	switch x := v.(type) {
	case nil:
		return time.Time{}, errors.New("nil")
	case int:
		return time.Unix(int64(x), 0).UTC(), nil
	case int64:
		return time.Unix(x, 0).UTC(), nil
	case float64:
		return time.Unix(int64(x), 0).UTC(), nil
	default:
		// json.Number / fmt.Stringer / string — a single branch via String().
		// vaultapi configures the json decoder with UseNumber, so
		// json.Number is the main case.
		type numberer interface{ Int64() (int64, error) }
		if n, ok := v.(numberer); ok {
			i, err := n.Int64()
			if err != nil {
				return time.Time{}, fmt.Errorf("json.Number: %w", err)
			}
			return time.Unix(i, 0).UTC(), nil
		}
		if s, ok := v.(string); ok {
			// fallback: RFC3339 for custom backends.
			t, err := time.Parse(time.RFC3339, s)
			if err == nil {
				return t.UTC(), nil
			}
			return time.Time{}, fmt.Errorf("string %q: %w", s, err)
		}
		return time.Time{}, fmt.Errorf("unsupported type %T", v)
	}
}

// _enforceVaultapiNumber is a compile-time assertion that the vaultapi
// import wasn't dropped by a stray go-mod-tidy: SignCSR uses Logical() from
// vaultapi.Client. Without this check, a "pki.go no longer depends on
// vaultapi" regression isn't caught by unit tests (they mock at the Client level).
var _ = (*vaultapi.Client)(nil)
