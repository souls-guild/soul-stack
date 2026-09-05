package main

import (
	"crypto/tls"
	"strings"
	"testing"
)

// TestBuildTLSConfig_DisabledIsPlaintext — no TLS config means the caller builds a
// plaintext connection, which is the documented default.
func TestBuildTLSConfig_DisabledIsPlaintext(t *testing.T) {
	cfg, err := buildTLSConfig(tlsParams{enabled: false, caPEM: "whatever"})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg != nil {
		t.Error("tls=false must produce no TLS config")
	}
}

// TestBuildTLSConfig_VerifiesByDefault is the ADR-010 default-secure stance: enabling
// TLS without saying anything else must VERIFY.
func TestBuildTLSConfig_VerifiesByDefault(t *testing.T) {
	cfg, err := buildTLSConfig(tlsParams{enabled: true})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("tls=true without tls_skip_verify must verify the node certificate")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2 or above", cfg.MinVersion)
	}
}

func TestBuildTLSConfig_SkipVerifyIsExplicitOnly(t *testing.T) {
	cfg, err := buildTLSConfig(tlsParams{enabled: true, skipVerify: true})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("an explicit tls_skip_verify=true must be honoured")
	}
}

func TestBuildTLSConfig_RefusesABrokenCA(t *testing.T) {
	_, err := buildTLSConfig(tlsParams{enabled: true, caPEM: "not a certificate"})
	if err == nil || !strings.Contains(err.Error(), "tls_ca") {
		t.Fatalf("expected a refusal naming tls_ca, got %v", err)
	}
}

// TestBuildTLSConfig_RefusesHalfAClientPair — a cert without its key (or the reverse)
// is a half-configured mTLS that would silently connect without a client identity.
func TestBuildTLSConfig_RefusesHalfAClientPair(t *testing.T) {
	for _, p := range []tlsParams{
		{enabled: true, certPEM: "CERT"},
		{enabled: true, keyPEM: "KEY"},
	} {
		_, err := buildTLSConfig(p)
		if err == nil || !strings.Contains(err.Error(), "TOGETHER") {
			t.Errorf("expected a refusal for half a client pair, got %v", err)
		}
	}
}

// TestParseTLS_ReadsEveryField — the keys the manifest declares are the keys the
// parser reads; a rename here fails rather than quietly leaving TLS unconfigured.
func TestParseTLS_ReadsEveryField(t *testing.T) {
	f := mustStruct(t, map[string]any{
		"tls": true, "tls_ca": "CA", "tls_cert": "CERT", "tls_key": "KEY", "tls_skip_verify": true,
	}).GetFields()

	got := parseTLS(f)
	want := tlsParams{enabled: true, caPEM: "CA", certPEM: "CERT", keyPEM: "KEY", skipVerify: true}
	if got != want {
		t.Errorf("parseTLS = %+v, want %+v", got, want)
	}
}

// TestTLSParamsReachTheConnection — the PEM material and the flag travel to the
// driver, so a scenario that asked for TLS gets it.
func TestTLSParamsReachTheConnection(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).instance(), "pinged", withParams(map[string]any{
		"tls": true, "tls_ca": "CA-PEM", "tls_skip_verify": false,
	}))

	if !s.cfg.tls.enabled || s.cfg.tls.caPEM != "CA-PEM" || s.cfg.tls.skipVerify {
		t.Errorf("TLS params did not reach the connection: %+v", s.cfg.tls)
	}
}
