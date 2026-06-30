package push

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravitational/teleport/api/client/proxy"
	"github.com/gravitational/teleport/api/identityfile"
	apissh "github.com/gravitational/teleport/api/ssh"
	"github.com/gravitational/teleport/api/utils/keys"
	"golang.org/x/crypto/ssh"
)

// TestNewTeleportDialer_RejectsEmptyFields — конструктор отвергает пустые
// обязательные поля (proxy_addr / identity_file / cluster). Проверяется ДО
// preflight-load (пустой identity_file ловится этим же гейтом).
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

// TestNewTeleportDialer_PreflightRejectsBadIdentity — fail-fast: конструктор
// один раз загружает identity-file и проверяет, что TLSConfig()/SSHClientConfig()
// поднимаются. Несуществующий/битый файл → конструктор-ошибка (keeper отказывает
// на старте через buildBootstrapTeleportDialer → errSetupFailed), а не падает
// поздно на первом Dial.
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

// TestNewTeleportDialer_ValidIdentityOK — валидный identity-file: preflight
// проходит, конструктор возвращает непустой Dialer без ошибки. Сам Dial здесь не
// вызывается (нужна живая Teleport-сеть) — проверяется только конструктор.
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

// TestApplyProxyTLSTrust_SystemTrust — guard ADR-063 amendment («Teleport-proxy
// за L7-TLS-балансировщиком»): при UseSystemTrust=true итоговый tls.Config имеет
// RootCAs=nil (системный trust store) + ServerName=host(proxy_addr) (снят sentinel
// `teleport.cluster.local`), а client-cert (mTLS-auth на proxy) СОХРАНЁН.
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

// TestApplyProxyTLSTrust_Default — guard: при UseSystemTrust=false tls.Config не
// меняется (identity-CA-pool + sentinel-ServerName `teleport.cluster.local`),
// существующие Teleport-issued-proxy инсталляции работают бит-в-бит как раньше.
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

// TestApplyProxyTLSTrustALPN_UnionTrust — guard ADR-063 amendment («Teleport-
// proxy за L7-TLS-балансировщиком», решение «a»): при alpn-conn-upgrade
// внутренний gRPC-mTLS-слой получает ОБЪЕДИНЁННЫЙ trust RootCAs = системный пул ∪
// identity-CA. Проверяется, что итоговый пул содержит И identity-CA (по subject),
// И системные CA (если системный пул на машине непуст), ServerName остаётся
// sentinel `teleport.cluster.local`, InsecureSkipVerify=false, client-cert сохранён.
func TestApplyProxyTLSTrustALPN_UnionTrust(t *testing.T) {
	caPEM, caSubject := makeCACertPEM(t)
	clientCert := tls.Certificate{Certificate: [][]byte{{0x01, 0x02}}}
	cfg := &tls.Config{
		RootCAs:      x509.NewCertPool(), // identity-CA-pool из creds.TLSConfig() (непрозрачный)
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

	// Системные CA попадают в объединение, если на машине системный trust непуст.
	// На минимальных рантаймах (пустой системный пул) этот инвариант не проверяем —
	// объединение там вырождается в один identity-CA, что корректно.
	if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
		sysSubjects := sys.Subjects() //nolint:staticcheck // intentional: baseline system subjects in guard
		if len(sysSubjects) > 0 && !containsSubject(subjects, sysSubjects[0]) {
			t.Error("union RootCAs must include system CAs alongside identity-CA, a system subject is missing")
		}
	}
}

// TestApplyProxyTLSTrustALPN_RejectsBadPEM — guard: невалидный identity-CA PEM →
// ошибка (fail-closed на Dial, без тихого пустого trust). ServerName/RootCAs не
// важны при ошибке — Dial аварийно завершится до использования tls.Config.
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

// TestNewTeleportDialer_SystemTrustProxyAddr — при UseSystemTrust=true host
// режется из proxy_addr на старте: битый proxy_addr (нет `:port`) → конструктор-
// ошибка (fail-closed), валидный `host:port` → ok.
func TestNewTeleportDialer_SystemTrustProxyAddr(t *testing.T) {
	id := writeValidIdentityFile(t)

	t.Run("malformed proxy_addr fails", func(t *testing.T) {
		d, err := NewTeleportDialer(TeleportDialerConfig{
			ProxyAddr:      "tp.rwb.ru", // нет порта → SplitHostPort ошибка
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

// TestBuildProxyClientConfig_AlpnUpgrade — guard ADR-063 amendment («Teleport-
// proxy за L7-TLS-балансировщиком»): AlpnUpgrade=true → proxy.ClientConfig.
// ALPNConnUpgradeRequired=true (ALPN-conn-upgrade WebSocket-туннель для L7-LB);
// AlpnUpgrade=false (дефолт) → false, остальные поля проброшены бит-в-бит.
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

	// Compile-time guard: имя поля proxy.ClientConfig.ALPNConnUpgradeRequired
	// зафиксировано (ловит переименование в апстрим-teleport при bump).
	_ = proxy.ClientConfig{ALPNConnUpgradeRequired: true}
}

// writeValidIdentityFile собирает минимально-валидный Teleport identity-file
// (ed25519 priv + self-signed SSH user-cert + self-signed X.509 TLS-cert под тот
// же ключ) и возвращает путь. Достаточен, чтобы creds.TLSConfig() И
// creds.SSHClientConfig() поднялись без живой Teleport-сети (known_hosts опущен,
// чтобы ProxyClientSSHConfig не требовал CA). Не валиден для реального коннекта —
// нужен только для проверки preflight-парсинга.
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

	// SSH user-cert, self-signed тем же ключом (CA == subject — для парсинга
	// этого достаточно, SSHSigner проверяет лишь соответствие priv↔cert pubkey).
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

	// X.509 TLS-cert, self-signed тем же ed25519-ключом.
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

// makeCACertPEM собирает self-signed CA-cert (ed25519) и возвращает его PEM-блок
// вместе с RAW-DER subject (для сверки присутствия в x509.CertPool.Subjects()).
// Имитирует identity-CA из identity-file (CACerts.TLS), который объединённый
// alpn-trust добавляет в системный пул.
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
