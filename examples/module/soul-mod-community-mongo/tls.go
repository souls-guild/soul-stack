// TLS connection for the community.mongo plugin. Security model (default secure):
// when tls=true, the plugin VERIFIES server certificate by default (RootCAs from
// provided PEM CA). Client certificate (mTLS) is optional. Verification can be
// disabled ONLY with explicit tls_skip_verify=true (default false).
//
// PEM arrives WHOLE in params (scenario resolves it from Vault during render
// phase); plugin does not use its own Vault access (capability - network_outbound).
// PEM fields (tls_ca/tls_cert/tls_key) are marked secret in manifest and masked
// by output layer by key name, so they do not reach events/logs/errors.
//
// PILOT: MongoDB service starts in plain mode (net.tls.mode disabled). Connection
// params are declared here for symmetry with community.redis and forward-compat
// (mongo TLS on port 27017 via net.tls.mode is a separate slice); pilot scenario
// does not set them (tls=false -> plaintext).
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// tlsParams contains raw TLS connection params from params, separate from
// *tls.Config: keep PEM strings until config construction so factory stays a pure
// function and can be tested without a live socket (L0: buildTLSConfig over fake PEM).
type tlsParams struct {
	enabled    bool
	caPEM      string // PEM CA for server certificate verification (RootCAs)
	certPEM    string // PEM client cert for mTLS (optional; together with keyPEM)
	keyPEM     string // PEM client key for mTLS (optional; together with certPEM)
	skipVerify bool   // EXPLICIT opt-out of certificate verification (default false)
}

// parseTLS extracts TLS params from params. All fields are optional: absent/false
// tls -> enabled=false, plaintext connection. PEM strings stay separate from
// anything that reaches events (like password - security invariant ADR-010).
func parseTLS(f map[string]*structpb.Value) tlsParams {
	return tlsParams{
		enabled:    boolOrDefault(f["tls"], false),
		caPEM:      stringOrEmpty(f["tls_ca"]),
		certPEM:    stringOrEmpty(f["tls_cert"]),
		keyPEM:     stringOrEmpty(f["tls_key"]),
		skipVerify: boolOrDefault(f["tls_skip_verify"], false),
	}
}

// buildTLSConfig builds *tls.Config from tlsParams. Returns nil, nil when TLS is
// disabled (caller builds plaintext connection). Error only on broken PEM.
// Pure function (no I/O) -> L0 checks result directly.
func buildTLSConfig(p tlsParams) (*tls.Config, error) {
	if !p.enabled {
		return nil, nil
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: p.skipVerify, //nolint:gosec // EXPLICIT operator opt-out (tls_skip_verify), default false - verification enabled
	}

	if p.caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(p.caPEM)) {
			return nil, fmt.Errorf("tls_ca: failed to parse PEM CA certificate")
		}
		cfg.RootCAs = pool
	}

	switch {
	case p.certPEM != "" && p.keyPEM != "":
		pair, err := tls.X509KeyPair([]byte(p.certPEM), []byte(p.keyPEM))
		if err != nil {
			return nil, fmt.Errorf("tls_cert/tls_key: invalid client-cert pair (mTLS)")
		}
		cfg.Certificates = []tls.Certificate{pair}
	case p.certPEM != "" || p.keyPEM != "":
		return nil, fmt.Errorf("tls_cert and tls_key must be set only TOGETHER (mTLS client-cert)")
	}

	return cfg, nil
}
