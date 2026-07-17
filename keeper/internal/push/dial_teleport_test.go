package push

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravitational/teleport/api/client/proxy"
	"github.com/gravitational/teleport/api/identityfile"
	apissh "github.com/gravitational/teleport/api/ssh"
	"github.com/gravitational/teleport/api/utils/keys"
	"golang.org/x/crypto/ssh"
)

// TestNewTeleportDialer_RejectsEmptyFields — the constructor rejects empty
// required fields (proxy_addr / identity_file / cluster). Checked BEFORE
// preflight-load (an empty identity_file is caught by the same gate).
func TestNewTeleportDialer_RejectsEmptyFields(t *testing.T) {
	id := writeValidIdentityFile(t)
	full := TeleportDialerConfig{ProxyAddr: "proxy.example.com:443", IdentityFile: id, Cluster: "c1"}

	cases := []struct {
		name string
		cfg  TeleportDialerConfig
		want string
	}{
		{"empty proxy_addr", TeleportDialerConfig{IdentityFile: id, Cluster: "c1"}, "proxy_addr"},
		{"empty identity_file", TeleportDialerConfig{ProxyAddr: full.ProxyAddr, Cluster: "c1"}, "identity_file"},
		{"empty cluster", TeleportDialerConfig{ProxyAddr: full.ProxyAddr, IdentityFile: id}, "cluster"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewTeleportDialer(tc.cfg)
			if err == nil {
				t.Fatalf("expected error for %s, got nil (dialer=%v)", tc.name, d != nil)
			}
			if d != nil {
				t.Errorf("dialer must be nil on error, got non-nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestNewTeleportDialer_PreflightRejectsBadIdentity — fail-fast: the
// constructor loads the identity file once and checks that
// TLSConfig()/SSHClientConfig() come up cleanly. A missing/corrupt file →
// constructor error (keeper refuses to start via buildBootstrapTeleportDialer
// → errSetupFailed), instead of failing late on the first Dial.
func TestNewTeleportDialer_PreflightRejectsBadIdentity(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		d, err := NewTeleportDialer(TeleportDialerConfig{
			ProxyAddr:    "proxy.example.com:443",
			IdentityFile: filepath.Join(t.TempDir(), "does-not-exist"),
			Cluster:      "c1",
		})
		if err == nil {
			t.Fatalf("expected preflight error for missing identity file, got nil (dialer=%v)", d != nil)
		}
		if d != nil {
			t.Errorf("dialer must be nil on preflight error")
		}
	})

	t.Run("corrupt file", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "broken-identity")
		if werr := os.WriteFile(bad, []byte("not a teleport identity file\n"), 0o600); werr != nil {
			t.Fatalf("write corrupt identity: %v", werr)
		}
		d, err := NewTeleportDialer(TeleportDialerConfig{
			ProxyAddr:    "proxy.example.com:443",
			IdentityFile: bad,
			Cluster:      "c1",
		})
		if err == nil {
			t.Fatalf("expected preflight error for corrupt identity file, got nil (dialer=%v)", d != nil)
		}
		if d != nil {
			t.Errorf("dialer must be nil on preflight error")
		}
	})
}

// TestNewTeleportDialer_ValidIdentityOK — a valid identity file: preflight
// passes, the constructor returns a non-nil Dialer without error. Dial itself
// is not called here (needs a live Teleport network) — only the constructor
// is checked.
func TestNewTeleportDialer_ValidIdentityOK(t *testing.T) {
	d, err := NewTeleportDialer(TeleportDialerConfig{
		ProxyAddr:    "proxy.example.com:443",
		IdentityFile: writeValidIdentityFile(t),
		Cluster:      "c1",
	})
	if err != nil {
		t.Fatalf("valid identity rejected by preflight: %v", err)
	}
	if d == nil {
		t.Fatal("dialer is nil despite valid identity")
	}
}

// TestApplyProxyTLSTrust_SystemTrust — guard for ADR-063 amendment
// ("Teleport proxy behind an L7 TLS load balancer"): with UseSystemTrust=true,
// the resulting tls.Config has RootCAs=nil (system trust store) +
// ServerName=host(proxy_addr) (the `teleport.cluster.local` sentinel is
// dropped), while the client cert (mTLS auth to the proxy) is PRESERVED.
func TestApplyProxyTLSTrust_SystemTrust(t *testing.T) {
	clientCert := tls.Certificate{Certificate: [][]byte{{0x01, 0x02}}}
	cfg := &tls.Config{
		RootCAs:      x509.NewCertPool(),
		ServerName:   "teleport.cluster.local",
		Certificates: []tls.Certificate{clientCert},
	}

	applyProxyTLSTrust(cfg, true, "tp.rwb.ru")

	if cfg.RootCAs != nil {
		t.Error("RootCAs must be nil (system trust store) under use_system_trust")
	}
	if cfg.ServerName != "tp.rwb.ru" {
		t.Errorf("ServerName = %q, want host from proxy_addr %q", cfg.ServerName, "tp.rwb.ru")
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must stay false (RootCAs=nil verifies via system trust, not skip)")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("client cert dropped: Certificates len = %d, want 1", len(cfg.Certificates))
	}
}

// TestApplyProxyTLSTrust_Default — guard: with UseSystemTrust=false,
// tls.Config is left unchanged (identity-CA pool + sentinel ServerName
// `teleport.cluster.local`), so existing Teleport-issued-proxy installs keep
// working bit-for-bit as before.
func TestApplyProxyTLSTrust_Default(t *testing.T) {
	pool := x509.NewCertPool()
	cfg := &tls.Config{
		RootCAs:    pool,
		ServerName: "teleport.cluster.local",
	}

	applyProxyTLSTrust(cfg, false, "tp.rwb.ru")

	if cfg.RootCAs != pool {
		t.Error("RootCAs must keep identity-CA pool under default (use_system_trust=false)")
	}
	if cfg.ServerName != "teleport.cluster.local" {
		t.Errorf("ServerName = %q, want sentinel teleport.cluster.local under default", cfg.ServerName)
	}
}

// TestApplyProxyTLSTrustALPN_UnionTrust — guard for ADR-063 amendment
// ("Teleport proxy behind an L7 TLS load balancer", decision "a"): under
// alpn-conn-upgrade, the inner gRPC mTLS layer gets a UNION trust RootCAs =
// system pool ∪ identity-CA. Checks that the resulting pool contains BOTH
// the identity-CA (by subject) AND the system CAs (if the machine's system
// pool is non-empty), ServerName stays the `teleport.cluster.local` sentinel,
// InsecureSkipVerify=false, and the client cert is preserved.
func TestApplyProxyTLSTrustALPN_UnionTrust(t *testing.T) {
	caPEM, caSubject := makeCACertPEM(t)
	clientCert := tls.Certificate{Certificate: [][]byte{{0x01, 0x02}}}
	cfg := &tls.Config{
		RootCAs:      x509.NewCertPool(), // identity-CA pool from creds.TLSConfig() (opaque)
		ServerName:   "teleport.cluster.local",
		Certificates: []tls.Certificate{clientCert},
	}

	if err := applyProxyTLSTrustALPN(cfg, [][]byte{caPEM}); err != nil {
		t.Fatalf("applyProxyTLSTrustALPN: unexpected error: %v", err)
	}

	if cfg.RootCAs == nil {
		t.Fatal("RootCAs must be a non-nil union pool, got nil")
	}
	if cfg.ServerName != "teleport.cluster.local" {
		t.Errorf("ServerName = %q, want sentinel teleport.cluster.local (untouched under alpn)", cfg.ServerName)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must stay false (union trust verifies, not skip)")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("client cert dropped: Certificates len = %d, want 1", len(cfg.Certificates))
	}

	subjects := cfg.RootCAs.Subjects() //nolint:staticcheck // intentional: inspect union pool contents in guard
	if !containsSubject(subjects, caSubject) {
		t.Error("union RootCAs must contain identity-CA subject (Teleport-CA from identity), not found")
	}

	// System CAs land in the union if the machine's system trust store is
	// non-empty. On minimal runtimes (empty system pool) we don't check this
	// invariant — the union degenerates to just the identity-CA there, which
	// is correct.
	if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
		sysSubjects := sys.Subjects() //nolint:staticcheck // intentional: baseline system subjects in guard
		if len(sysSubjects) > 0 && !containsSubject(subjects, sysSubjects[0]) {
			t.Error("union RootCAs must include system CAs alongside identity-CA, a system subject is missing")
		}
	}
}

// TestApplyProxyTLSTrustALPN_RejectsBadPEM — guard: an invalid identity-CA
// PEM → error (fail-closed on Dial, no silent empty trust). ServerName/RootCAs
// don't matter on error — Dial aborts before tls.Config is ever used.
func TestApplyProxyTLSTrustALPN_RejectsBadPEM(t *testing.T) {
	cfg := &tls.Config{ServerName: "teleport.cluster.local"}
	err := applyProxyTLSTrustALPN(cfg, [][]byte{[]byte("not a pem block")})
	if err == nil {
		t.Fatal("expected error for invalid identity-CA PEM, got nil")
	}
	if !strings.Contains(err.Error(), "identity-CA") {
		t.Errorf("error %q does not mention identity-CA", err.Error())
	}
}

// TestNewTeleportDialer_SystemTrustProxyAddr — with UseSystemTrust=true, host
// is sliced out of proxy_addr at startup: a malformed proxy_addr (no
// `:port`) → constructor error (fail-closed), a valid `host:port` → ok.
func TestNewTeleportDialer_SystemTrustProxyAddr(t *testing.T) {
	id := writeValidIdentityFile(t)

	t.Run("malformed proxy_addr fails", func(t *testing.T) {
		d, err := NewTeleportDialer(TeleportDialerConfig{
			ProxyAddr:      "tp.rwb.ru", // no port → SplitHostPort error
			IdentityFile:   id,
			Cluster:        "c1",
			UseSystemTrust: true,
		})
		if err == nil {
			t.Fatalf("expected error for malformed proxy_addr under use_system_trust, got nil (dialer=%v)", d != nil)
		}
		if d != nil {
			t.Error("dialer must be nil on proxy_addr error")
		}
		if !strings.Contains(err.Error(), "proxy_addr") {
			t.Errorf("error %q does not mention proxy_addr", err.Error())
		}
	})

	t.Run("valid proxy_addr ok", func(t *testing.T) {
		d, err := NewTeleportDialer(TeleportDialerConfig{
			ProxyAddr:      "tp.rwb.ru:443",
			IdentityFile:   id,
			Cluster:        "c1",
			UseSystemTrust: true,
		})
		if err != nil {
			t.Fatalf("valid config with use_system_trust rejected: %v", err)
		}
		if d == nil {
			t.Fatal("dialer is nil despite valid config")
		}
	})
}

// TestBuildProxyClientConfig_AlpnUpgrade — guard for ADR-063 amendment
// ("Teleport proxy behind an L7 TLS load balancer"): AlpnUpgrade=true →
// proxy.ClientConfig.ALPNConnUpgradeRequired=true (ALPN-conn-upgrade
// WebSocket tunnel for the L7 LB); AlpnUpgrade=false (default) → false, the
// rest of the fields pass through bit-for-bit.
func TestBuildProxyClientConfig_AlpnUpgrade(t *testing.T) {
	tlsCfg := &tls.Config{ServerName: "tp.rwb.ru"}
	sshCfg := apissh.ClientConfig{User: "root"}

	t.Run("enabled", func(t *testing.T) {
		got := buildProxyClientConfig(
			TeleportDialerConfig{ProxyAddr: "tp.rwb.ru:443", AlpnUpgrade: true},
			tlsCfg, sshCfg, 5*time.Second,
		)
		if !got.ALPNConnUpgradeRequired {
			t.Error("ALPNConnUpgradeRequired must be true when AlpnUpgrade=true")
		}
		if got.ProxyAddress != "tp.rwb.ru:443" {
			t.Errorf("ProxyAddress = %q, want tp.rwb.ru:443", got.ProxyAddress)
		}
		if got.SSHConfig.User != "root" {
			t.Errorf("SSHConfig must be passed through unchanged: User = %q, want root", got.SSHConfig.User)
		}
		if got.DialTimeout != 5*time.Second {
			t.Errorf("DialTimeout = %v, want 5s", got.DialTimeout)
		}
		if got.TLSConfigFunc == nil {
			t.Fatal("TLSConfigFunc must be set")
		}
		c, err := got.TLSConfigFunc("")
		if err != nil {
			t.Fatalf("TLSConfigFunc returned error: %v", err)
		}
		if c != tlsCfg {
			t.Error("TLSConfigFunc must return the provided tls.Config")
		}
	})

	t.Run("disabled is default", func(t *testing.T) {
		got := buildProxyClientConfig(
			TeleportDialerConfig{ProxyAddr: "tp.rwb.ru:443"}, // AlpnUpgrade zero-value
			tlsCfg, sshCfg, 5*time.Second,
		)
		if got.ALPNConnUpgradeRequired {
			t.Error("ALPNConnUpgradeRequired must be false by default (AlpnUpgrade=false)")
		}
	})

	// Compile-time guard: pins the field name
	// proxy.ClientConfig.ALPNConnUpgradeRequired (catches a rename in
	// upstream teleport on bump).
	_ = proxy.ClientConfig{ALPNConnUpgradeRequired: true}
}

// TestBuildProxyClientConfig_ResetsNextProtos — guard for the ALPN pitfall
// "h2-first → 403 web stack" (see the comment at the NextProtos reset in
// buildProxyClientConfig). The invariant holds for BOTH branches (alpn and
// direct-gRPC) — there's a single call site.
func TestBuildProxyClientConfig_ResetsNextProtos(t *testing.T) {
	for _, tc := range []struct {
		name string
		alpn bool
	}{
		{"alpn_upgrade", true},
		{"direct_grpc", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tlsCfg := &tls.Config{NextProtos: []string{"h2"}} // as it comes from creds.TLSConfig()
			got := buildProxyClientConfig(
				TeleportDialerConfig{ProxyAddr: "tp.rwb.ru:443", AlpnUpgrade: tc.alpn},
				tlsCfg, apissh.ClientConfig{}, time.Second,
			)
			c, err := got.TLSConfigFunc("")
			if err != nil {
				t.Fatalf("TLSConfigFunc returned error: %v", err)
			}
			if len(c.NextProtos) != 0 {
				t.Errorf("NextProtos = %v, want empty: h2 first would route the TLS-routing proxy into the web stack (403)", c.NextProtos)
			}
		})
	}
}

// journalCloser writes a label into the shared closing journal.
type journalCloser struct {
	label   string
	journal *[]string
	closed  int
}

func (c *journalCloser) Close() error {
	c.closed++
	*c.journal = append(*c.journal, c.label)
	return nil
}

// journalSession — inner Session for teleportSession, records the moment of Close.
type journalSession struct {
	journal *[]string
}

func (s *journalSession) Run(context.Context, string, []byte) (string, error) { return "", nil }
func (s *journalSession) Close() error {
	*s.journal = append(*s.journal, "client")
	return nil
}

// TestTeleportSession_ProxyClientOwnership — guard for the ownership
// contract: the conn from DialHost is multiplexed over the proxy client's
// gRPC stream, an early Close kills the transport under *ssh.Client (live
// bug: the first sess.Run → `ssh: unexpected packet in response to channel
// open: <nil>`). Requirements: the proxy client is NOT closed right after
// Session is obtained, it's closed exactly once in sess.Close(), AFTER the
// SSH client; a repeated Close is idempotent.
func TestTeleportSession_ProxyClientOwnership(t *testing.T) {
	var journal []string
	proxyClient := &journalCloser{label: "proxy", journal: &journal}

	sess := newTeleportSession(&journalSession{journal: &journal}, proxyClient)

	if proxyClient.closed != 0 {
		t.Fatalf("proxy client closed right after dial (closed=%d) - conn is dead before the first Run", proxyClient.closed)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if proxyClient.closed != 1 {
		t.Fatalf("after sess.Close(): proxy closed=%d, want 1", proxyClient.closed)
	}
	if want := []string{"client", "proxy"}; !slices.Equal(journal, want) {
		t.Fatalf("close order %v, want %v (proxy closing before client would sever the tunnel under a live ssh)", journal, want)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	if proxyClient.closed != 1 {
		t.Fatalf("repeated Close closed proxy again: closed=%d", proxyClient.closed)
	}
}

// writeValidIdentityFile assembles a minimally-valid Teleport identity file
// (ed25519 priv + a self-signed SSH user cert + a self-signed X.509 TLS cert
// under the same key) and returns its path. Enough for both
// creds.TLSConfig() and creds.SSHClientConfig() to come up without a live
// Teleport network (known_hosts is omitted so ProxyClientSSHConfig doesn't
// require a CA). Not valid for a real connection — it's only for checking
// preflight parsing.
func writeValidIdentityFile(t *testing.T) string {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	privPEM, err := keys.MarshalPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}

	// SSH user cert, self-signed with the same key (CA == subject — that's
	// enough for parsing, SSHSigner only checks that priv matches cert pubkey).
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh public key: %v", err)
	}
	signer, err := ssh.NewSignerFromSigner(priv)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}
	cert := &ssh.Certificate{
		Key:             sshPub,
		Serial:          1,
		CertType:        ssh.UserCert,
		KeyId:           "test",
		ValidPrincipals: []string{"root"},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
	}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatalf("sign ssh cert: %v", err)
	}
	sshCertPEM := ssh.MarshalAuthorizedKey(cert)

	// X.509 TLS cert, self-signed with the same ed25519 key.
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create x509 cert: %v", err)
	}
	tlsCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	idf := &identityfile.IdentityFile{
		PrivateKey: privPEM,
		Certs:      identityfile.Certs{SSH: sshCertPEM, TLS: tlsCertPEM},
	}
	path := filepath.Join(t.TempDir(), "identity")
	if err := identityfile.Write(idf, path); err != nil {
		t.Fatalf("write identity file: %v", err)
	}
	return path
}

// makeCACertPEM assembles a self-signed CA cert (ed25519) and returns its PEM
// block along with the RAW-DER subject (for checking presence in
// x509.CertPool.Subjects()). Simulates the identity-CA from the identity file
// (CACerts.TLS) that the union alpn-trust adds to the system pool.
func makeCACertPEM(t *testing.T) (pemBlock []byte, rawSubject []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "teleport-identity-ca-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), parsed.RawSubject
}

func containsSubject(subjects [][]byte, want []byte) bool {
	for _, s := range subjects {
		if string(s) == string(want) {
			return true
		}
	}
	return false
}
