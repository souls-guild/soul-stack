package push

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravitational/teleport/api/identityfile"
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
