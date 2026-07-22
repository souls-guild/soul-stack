package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
	"golang.org/x/crypto/ssh"
)

// writeKey generates an ed25519 private key in OpenSSH PEM and writes it to a
// temp file, returning path + PEM (ssh.ParsePrivateKey must parse it - the same
// format keeper.push expects).
func writeKey(t *testing.T) (path, pemStr string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemStr = string(pem.EncodeToMemory(block))
	path = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte(pemStr), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path, pemStr
}

func TestSignReturnsKeyPairFromFile(t *testing.T) {
	keyPath, keyPEM := writeKey(t)
	p := &StaticProvider{cfg: params{KeyPath: keyPath}}

	reply, err := p.Sign(context.Background(), &pluginv1.SignRequest{Host: "web-1", User: "soul"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if reply.GetPrivateKey() != keyPEM {
		t.Errorf("private_key did not match the file")
	}
	if reply.GetCertificate() != "" {
		t.Errorf("certificate=%q, static provider does not sign (expected empty)", reply.GetCertificate())
	}
	// keeper.push parses private_key through ssh.ParsePrivateKey; verify the
	// returned material is valid (fail-closed invariant passed).
	if _, perr := ssh.ParsePrivateKey([]byte(reply.GetPrivateKey())); perr != nil {
		t.Errorf("returned private_key is not parseable: %v", perr)
	}
}

func TestSignFailClosedOnMissingFile(t *testing.T) {
	p := &StaticProvider{cfg: params{KeyPath: filepath.Join(t.TempDir(), "absent")}}
	_, err := p.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u"})
	if err == nil {
		t.Fatal("expected error for missing key (fail-closed)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailReadKey)+": ") {
		t.Errorf("reason code=%q, expected prefix %q", err.Error(), sshprovider.SignFailReadKey)
	}
}

func TestSignFailClosedOnCorruptKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(path, []byte("not a pem key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := &StaticProvider{cfg: params{KeyPath: path}}
	_, err := p.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u"})
	if err == nil {
		t.Fatal("expected error for corrupt key (fail-closed)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailReadKey)+": ") {
		t.Errorf("reason code=%q, expected prefix %q", err.Error(), sshprovider.SignFailReadKey)
	}
}

func TestAuthorizeAllowByDefault(t *testing.T) {
	p := &StaticProvider{cfg: params{KeyPath: "/x"}} // empty deny
	reply, err := p.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "web-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !reply.GetAllowed() {
		t.Errorf("expected allow with empty deny-list")
	}
}

func TestAuthorizeDenyExplicit(t *testing.T) {
	p := &StaticProvider{cfg: params{Deny: []denyRule{{Host: "prod-1", User: "root"}}}}

	deny, err := p.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "prod-1", User: "root"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if deny.GetAllowed() {
		t.Fatal("expected deny for deny-list pair")
	}
	if !strings.HasPrefix(deny.GetReason(), string(sshprovider.DenyExplicitDeny)) {
		t.Errorf("reason=%q, expected prefix %q", deny.GetReason(), sshprovider.DenyExplicitDeny)
	}

	// Another user on the same host is not affected by the {host, user} rule.
	allow, err := p.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "prod-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !allow.GetAllowed() {
		t.Errorf("expected allow for user outside the rule")
	}
}

func TestAuthorizeDenyWildcard(t *testing.T) {
	// host:"" -> wildcard by host: root is denied everywhere.
	p := &StaticProvider{cfg: params{Deny: []denyRule{{User: "root"}}}}
	for _, host := range []string{"web-1", "db-2", "any"} {
		reply, err := p.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: host, User: "root"})
		if err != nil {
			t.Fatalf("Authorize %s: %v", host, err)
		}
		if reply.GetAllowed() {
			t.Errorf("expected deny for root on %s (wildcard host)", host)
		}
	}
}

func TestLoadParams(t *testing.T) {
	t.Run("ok key_path", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"key_path":"/etc/k","deny":[{"user":"root"}]}`)
		p, err := loadParams()
		if err != nil {
			t.Fatalf("loadParams: %v", err)
		}
		if p.KeyPath != "/etc/k" || len(p.Deny) != 1 || p.Deny[0].User != "root" {
			t.Errorf("params=%+v", p)
		}
	})
	t.Run("empty env fail-closed", func(t *testing.T) {
		t.Setenv(paramsEnv, "")
		if _, err := loadParams(); err == nil {
			t.Error("expected error for empty env (fail-closed)")
		}
	})
	t.Run("no key source fail-closed", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"deny":[]}`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error without key_path/vault_ref")
		}
	})
	t.Run("vault_ref without key_path fail-closed", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_ref":"secret/k"}`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error: vault_ref is resolved by keeper.push into key_path before startup")
		}
	})
	t.Run("bad json fail-closed", func(t *testing.T) {
		t.Setenv(paramsEnv, `{not json`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error for bad JSON")
		}
	})
}
