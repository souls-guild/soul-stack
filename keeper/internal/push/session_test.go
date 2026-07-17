package push

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testEd25519PEM generates a fresh ed25519 private key in OpenSSH-PEM (for
// SignReply.private_key in dispatcher tests). ssh.ParsePrivateKey must be
// able to parse it.
func testEd25519PEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// testCAKey generates an ed25519 host-CA key. Returns the signer (for
// signing host certs) and the public part as ssh.PublicKey (trust anchor).
func testCAKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ca genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ca signer: %v", err)
	}
	return signer, signer.PublicKey()
}

// testCAPub — just the public part of a fresh CA (for Deps.HostAuthority).
func testCAPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, pub := testCAKey(t)
	return pub
}

// makeHostCert issues a host certificate for hostKey, signed by ca, with
// principal host. Mimics a Vault SSH CA host cert.
func makeHostCert(t *testing.T, ca ssh.Signer, hostKey ssh.PublicKey, host string) *ssh.Certificate {
	t.Helper()
	cert := &ssh.Certificate{
		Key:             hostKey,
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{host},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
		Serial:          1,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatalf("sign host cert: %v", err)
	}
	return cert
}

func freshHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("host genkey: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	return sshPub
}

// singleCASet — a test helper: a singleton set of one CA with a default name.
func singleCASet(pub ssh.PublicKey) []NamedHostKeyAuthority {
	return []NamedHostKeyAuthority{{Name: "default", CAPubKey: pub}}
}

// TestHostCertCallback_ValidCert — the host presented a cert signed by our
// CA → the callback accepts it (ok).
func TestHostCertCallback_ValidCert(t *testing.T) {
	caSigner, caPub := testCAKey(t)
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, caSigner, hostKey, "host-1.example.com")

	cb := hostCertCallback(singleCASet(caPub), nil)
	if err := cb("host-1.example.com:22", nil, cert); err != nil {
		t.Errorf("valid host-cert from our CA rejected: %v", err)
	}
}

// TestHostCertCallback_ForeignCA — the cert is signed by ANOTHER CA → reject.
func TestHostCertCallback_ForeignCA(t *testing.T) {
	foreignSigner, _ := testCAKey(t) // a foreign CA signs
	_, ourCAPub := testCAKey(t)      // we trust our own
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, foreignSigner, hostKey, "host-1.example.com")

	cb := hostCertCallback(singleCASet(ourCAPub), nil)
	if err := cb("host-1.example.com:22", nil, cert); err == nil {
		t.Error("cert from a foreign CA should be rejected")
	}
}

// TestHostCertCallback_SelfSignedBareKey — the host presented a bare key
// (not a cert) → reject (refusal of TOFU: no trusted path for a bare host
// key).
func TestHostCertCallback_SelfSignedBareKey(t *testing.T) {
	_, ourCAPub := testCAKey(t)
	bareKey := freshHostKey(t) // not a certificate

	cb := hostCertCallback(singleCASet(ourCAPub), nil)
	if err := cb("host-1.example.com:22", nil, bareKey); err == nil {
		t.Error("bare host-key (not cert) should be rejected - refusal of TOFU")
	}
}

// TestHostCertCallback_WrongPrincipal — a cert from our CA, but the
// principal doesn't cover the requested host → reject (CertChecker checks
// principals).
func TestHostCertCallback_WrongPrincipal(t *testing.T) {
	caSigner, caPub := testCAKey(t)
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, caSigner, hostKey, "other-host.example.com")

	cb := hostCertCallback(singleCASet(caPub), nil)
	if err := cb("host-1.example.com:22", nil, cert); err == nil {
		t.Error("cert with a foreign principal should be rejected for a different host")
	}
}

// TestDial_RequiresCA — Dial without a CA is fail-closed (InsecureIgnoreHostKey is forbidden).
func TestDial_RequiresCA(t *testing.T) {
	_, err := Dial(t.Context(), DialConfig{Host: "h", Port: 22, User: "u"})
	if err == nil {
		t.Fatal("Dial without CA should be rejected")
	}
}

// TestHostCertCallback_MultiCA_MatchFirst — S7-3: the host cert is signed by
// the first CA in the set → the callback accepts + OnHostCAMatch records the
// first CA's name.
func TestHostCertCallback_MultiCA_MatchFirst(t *testing.T) {
	ca1Signer, ca1Pub := testCAKey(t)
	_, ca2Pub := testCAKey(t)
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, ca1Signer, hostKey, "host-1.example.com")

	var matched string
	cb := hostCertCallback(
		[]NamedHostKeyAuthority{
			{Name: "trusted-bastion-1", CAPubKey: ca1Pub},
			{Name: "trusted-bastion-2", CAPubKey: ca2Pub},
		},
		func(caName string) { matched = caName },
	)
	if err := cb("host-1.example.com:22", nil, cert); err != nil {
		t.Fatalf("cert from the first CA rejected: %v", err)
	}
	if matched != "trusted-bastion-1" {
		t.Errorf("OnHostCAMatch caName = %q, want trusted-bastion-1", matched)
	}
}

// TestHostCertCallback_MultiCA_MatchSecond — S7-3: the host cert is signed
// by the second CA in the set → the OR-check finds a match, OnHostCAMatch
// records the second CA's name specifically (no false-match with the
// first).
func TestHostCertCallback_MultiCA_MatchSecond(t *testing.T) {
	_, ca1Pub := testCAKey(t)
	ca2Signer, ca2Pub := testCAKey(t)
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, ca2Signer, hostKey, "host-1.example.com")

	var matched string
	cb := hostCertCallback(
		[]NamedHostKeyAuthority{
			{Name: "trusted-bastion-1", CAPubKey: ca1Pub},
			{Name: "trusted-bastion-2", CAPubKey: ca2Pub},
		},
		func(caName string) { matched = caName },
	)
	if err := cb("host-1.example.com:22", nil, cert); err != nil {
		t.Fatalf("cert from the second CA rejected: %v", err)
	}
	if matched != "trusted-bastion-2" {
		t.Errorf("OnHostCAMatch caName = %q, want trusted-bastion-2", matched)
	}
}

// TestHostCertCallback_MultiCA_NoMatch — S7-3: the cert is signed by a CA
// outside the set → reject; OnHostCAMatch is not called.
func TestHostCertCallback_MultiCA_NoMatch(t *testing.T) {
	foreignSigner, _ := testCAKey(t)
	_, ca1Pub := testCAKey(t)
	_, ca2Pub := testCAKey(t)
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, foreignSigner, hostKey, "host-1.example.com")

	var matched string
	cb := hostCertCallback(
		[]NamedHostKeyAuthority{
			{Name: "ca-a", CAPubKey: ca1Pub},
			{Name: "ca-b", CAPubKey: ca2Pub},
		},
		func(caName string) { matched = caName },
	)
	if err := cb("host-1.example.com:22", nil, cert); err == nil {
		t.Error("cert from a CA outside the set should be rejected")
	}
	if matched != "" {
		t.Errorf("OnHostCAMatch called (caName = %q), expected empty (no match)", matched)
	}
}
