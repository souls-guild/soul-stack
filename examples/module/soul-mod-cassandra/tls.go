// TLS for the cassandra plugin. Security model (default secure): when tls=true the
// plugin VERIFIES the node certificate by default (RootCAs from the provided PEM
// CA). A client certificate (mTLS) is optional. Verification is disabled ONLY by an
// explicit tls_skip_verify=true (default false).
//
// PEM arrives WHOLE in params (the render phase resolves it from Vault); the plugin
// has no Vault client of its own (capability — network_outbound). The PEM fields are
// declared secret in the manifest and masked by key name, so they do not reach
// events, logs or errors.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// tlsParams holds the raw TLS params, kept separate from *tls.Config so the factory
// stays a pure function and is testable without a live socket.
type tlsParams struct {
	enabled    bool
	caPEM      string // PEM CA for node certificate verification (RootCAs)
	certPEM    string // PEM client cert for mTLS (optional; together with keyPEM)
	keyPEM     string // PEM client key for mTLS (optional; together with certPEM)
	skipVerify bool   // EXPLICIT opt-out of verification (default false)
}

// parseTLS extracts the TLS params. All are optional: an absent or false `tls` means
// enabled=false and a plaintext connection.
func parseTLS(f map[string]*structpb.Value) tlsParams {
	return tlsParams{
		enabled:    boolOrDefault(f["tls"], false),
		caPEM:      stringOrEmpty(f["tls_ca"]),
		certPEM:    stringOrEmpty(f["tls_cert"]),
		keyPEM:     stringOrEmpty(f["tls_key"]),
		skipVerify: boolOrDefault(f["tls_skip_verify"], false),
	}
}

// buildTLSConfig builds the *tls.Config. Returns nil, nil when TLS is disabled (the
// caller then builds a plaintext connection). An error only on a broken PEM. Pure
// function, so its result is checked directly in tests.
func buildTLSConfig(p tlsParams) (*tls.Config, error) {
	if !p.enabled {
		return nil, nil
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: p.skipVerify, //nolint:gosec // EXPLICIT operator opt-out (tls_skip_verify), default false — verification enabled
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
