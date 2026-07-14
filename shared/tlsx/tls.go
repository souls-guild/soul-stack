// Package tlsx provides shared TLS helpers for the Keeper/Soul/lint binaries.
//
// MVP scope:
//   - [LoadServerOnlyTLS] (M2.1.b.1) — server-only TLS for the Keeper Bootstrap-RPC
//     ([ADR-012(b)]). Before onboarding a Soul has no SoulSeed cert.
//   - [LoadMutualTLS] (M2.2) — mTLS for the EventStream listener: server cert, key
//     and CA bundle to validate incoming SoulSeed client certificates
//     (`RequireAndVerifyClientCert`).
//
// Post-MVP:
//   - LoadClientTLS for the Soul side (client cert+key + server CA) — M2.3.
//
// The package is named tlsx to avoid clashing with stdlib `crypto/tls` in callers'
// imports.
//
// [ADR-012(b)]: docs/adr/0012-keeper-soul-grpc.md
package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// ServerConfig — parameters for loading a server TLS config from files.
//
// The `CAPath` field is reserved for a future extension (M2.2 mTLS); it is ignored
// in [LoadServerOnlyTLS] — its presence does not turn the config into mTLS mode.
type ServerConfig struct {
	// CertPath — PEM-encoded x509 server certificate (a full chain is allowed —
	// `tls.LoadX509KeyPair` reads all PEM blocks).
	CertPath string
	// KeyPath — private key for CertPath (PEM).
	KeyPath string
	// CAPath — reserved for mTLS (M2.2). Ignored in LoadServerOnlyTLS.
	CAPath string
}

// LoadServerOnlyTLS reads cert + key from files and returns a `*tls.Config` with
// `ClientAuth = NoClientCert` (server-only TLS).
//
// Minimum TLS version is 1.3 (cf. requirements.md "security first" + ADR-012).
// Cipher suites are not set: Go's TLS 1.3 picks AEAD-only suites itself.
//
// Errors:
//   - ErrServerCertEmpty / ErrServerKeyEmpty — empty paths.
//   - wrapped fmt.Errorf on file reads (e.g. a missing path) — the caller must
//     surface it with config context.
func LoadServerOnlyTLS(cfg ServerConfig) (*tls.Config, error) {
	if cfg.CertPath == "" {
		return nil, ErrServerCertEmpty
	}
	if cfg.KeyPath == "" {
		return nil, ErrServerKeyEmpty
	}

	// Pre-flight — gives a clearer error than `tls.LoadX509KeyPair` on a missing file.
	if _, err := os.Stat(cfg.CertPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat cert %q: %w", cfg.CertPath, err)
	}
	if _, err := os.Stat(cfg.KeyPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat key %q: %w", cfg.KeyPath, err)
	}

	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: load cert/key pair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// MutualConfig — parameters for loading an mTLS config for a server listener with
// mandatory client-certificate validation (`ClientAuth = RequireAndVerifyClientCert`).
type MutualConfig struct {
	// CertPath — server cert.
	CertPath string
	// KeyPath — private key for CertPath.
	KeyPath string
	// CAPath — PEM CA bundle used to validate client certificates (for the Keeper
	// EventStream — the SoulSeed PKI root).
	CAPath string
}

// LoadMutualTLS reads cert + key + CA bundle and returns a `*tls.Config` with
// `ClientAuth = RequireAndVerifyClientCert` and a populated `ClientCAs`.
//
// Behavior is symmetric to [LoadServerOnlyTLS]:
//   - MinVersion = TLS 1.3;
//   - cipher suites are not set (TLS 1.3 — AEAD-only by spec);
//   - pre-flight stat on all three paths gives human-readable errors before
//     `tls.LoadX509KeyPair`.
//
// Additional fingerprint authentication (lookup in `soul_seeds`) is done
// **application-side**, in the caller's gRPC interceptor: at the TLS layer it's
// enough to check the certificate is signed by our CA — that guarantees the cert
// was issued by the Keeper's Vault PKI, but does not distinguish active from revoked.
func LoadMutualTLS(cfg MutualConfig) (*tls.Config, error) {
	if cfg.CertPath == "" {
		return nil, ErrServerCertEmpty
	}
	if cfg.KeyPath == "" {
		return nil, ErrServerKeyEmpty
	}
	if cfg.CAPath == "" {
		return nil, ErrServerCAEmpty
	}

	if _, err := os.Stat(cfg.CertPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat cert %q: %w", cfg.CertPath, err)
	}
	if _, err := os.Stat(cfg.KeyPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat key %q: %w", cfg.KeyPath, err)
	}
	if _, err := os.Stat(cfg.CAPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat ca %q: %w", cfg.CAPath, err)
	}

	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: load cert/key pair: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: read ca %q: %w", cfg.CAPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tlsx: ca bundle %q has no valid PEM certificates", cfg.CAPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientConfig — parameters for a client TLS config for Soul → Keeper.
//
// Used on the Soul side in two modes:
//   - Bootstrap phase (`soul init`): only CAPath, mode=ServerOnly — the Soul has no
//     SoulSeed certificate yet;
//   - EventStream phase (`soul run`): full mTLS, all three paths required.
type ClientConfig struct {
	// CertPath — client SoulSeed cert (PEM). Empty for ServerOnly mode.
	CertPath string
	// KeyPath — private key for CertPath (PEM). Empty for ServerOnly mode.
	KeyPath string
	// CAPath — PEM CA bundle the client uses to validate the Keeper's server
	// certificate. Required in both modes.
	CAPath string
	// ServerName — expected CN/SAN of the server certificate. Empty string =
	// derived automatically from the connection address (host:port → host).
	ServerName string
}

// LoadClientTLS returns a `*tls.Config` for a client dial to Keeper.
//
// Semantics:
//   - CertPath/KeyPath empty → server-only TLS (for the Bootstrap RPC);
//   - all three paths set → mTLS (for EventStream).
//
// Minimum TLS version is 1.3 (cf. requirements.md "security first"). Cipher suites
// are not set (TLS 1.3 — AEAD-only by spec).
//
// ServerName is applied via `tls.Config.ServerName` for hostname verification; for
// cluster configs with several endpoints across different host:port the caller passes
// it explicitly (otherwise gRPC sets the `authority` host from the target automatically).
func LoadClientTLS(cfg ClientConfig) (*tls.Config, error) {
	if cfg.CAPath == "" {
		return nil, ErrServerCAEmpty
	}
	if _, err := os.Stat(cfg.CAPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat ca %q: %w", cfg.CAPath, err)
	}
	caPEM, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: read ca %q: %w", cfg.CAPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tlsx: ca bundle %q has no valid PEM certificates", cfg.CAPath)
	}

	out := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS13,
		ServerName: cfg.ServerName,
	}

	// Bootstrap mode: cert+key not needed, the Soul doesn't have them yet.
	if cfg.CertPath == "" && cfg.KeyPath == "" {
		return out, nil
	}
	if cfg.CertPath == "" {
		return nil, ErrServerCertEmpty
	}
	if cfg.KeyPath == "" {
		return nil, ErrServerKeyEmpty
	}
	if _, err := os.Stat(cfg.CertPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat cert %q: %w", cfg.CertPath, err)
	}
	if _, err := os.Stat(cfg.KeyPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat key %q: %w", cfg.KeyPath, err)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: load cert/key pair: %w", err)
	}
	out.Certificates = []tls.Certificate{cert}
	return out, nil
}

// Sentinel errors for callers that want to map to specific diags / HTTP statuses.
// `errors.Is(err, ErrServerCertEmpty)` is stable, independent of the message text.
var (
	ErrServerCertEmpty = errors.New("tlsx: cert path is empty")
	ErrServerKeyEmpty  = errors.New("tlsx: key path is empty")
	ErrServerCAEmpty   = errors.New("tlsx: ca path is empty")
)
