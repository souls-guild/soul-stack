package push

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testEd25519PEM генерирует свежий ed25519-приватник в OpenSSH-PEM (для
// SignReply.private_key в dispatcher-тестах). ssh.ParsePrivateKey должен его
// распарсить.
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

// testCAKey генерирует ed25519-ключ host-CA. Возвращает signer (для подписи
// host-cert-ов) и публичную часть как ssh.PublicKey (trust-anchor).
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

// testCAPub — только публичная часть свежего CA (для Deps.HostAuthority).
func testCAPub(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, pub := testCAKey(t)
	return pub
}

// makeHostCert выпускает host-сертификат на hostKey, подписанный ca, с
// principal-ом host. Имитирует Vault SSH CA host-cert.
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

// singleCASet — helper для тестов: набор-singleton из одного CA с дефолт-именем.
func singleCASet(pub ssh.PublicKey) []NamedHostKeyAuthority {
	return []NamedHostKeyAuthority{{Name: "default", CAPubKey: pub}}
}

// TestHostCertCallback_ValidCert — host предъявил cert, подписанный нашим CA →
// callback принимает (ok).
func TestHostCertCallback_ValidCert(t *testing.T) {
	caSigner, caPub := testCAKey(t)
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, caSigner, hostKey, "host-1.example.com")

	cb := hostCertCallback(singleCASet(caPub), nil)
	if err := cb("host-1.example.com:22", nil, cert); err != nil {
		t.Errorf("валидный host-cert от нашего CA отвергнут: %v", err)
	}
}

// TestHostCertCallback_ForeignCA — cert подписан ДРУГИМ CA → reject.
func TestHostCertCallback_ForeignCA(t *testing.T) {
	foreignSigner, _ := testCAKey(t) // чужой CA подписывает
	_, ourCAPub := testCAKey(t)      // доверяем нашему
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, foreignSigner, hostKey, "host-1.example.com")

	cb := hostCertCallback(singleCASet(ourCAPub), nil)
	if err := cb("host-1.example.com:22", nil, cert); err == nil {
		t.Error("cert от чужого CA должен быть отвергнут")
	}
}

// TestHostCertCallback_SelfSignedBareKey — host предъявил голый ключ (не cert) →
// reject (отказ от TOFU: нет доверенного пути для bare host-key).
func TestHostCertCallback_SelfSignedBareKey(t *testing.T) {
	_, ourCAPub := testCAKey(t)
	bareKey := freshHostKey(t) // не сертификат

	cb := hostCertCallback(singleCASet(ourCAPub), nil)
	if err := cb("host-1.example.com:22", nil, bareKey); err == nil {
		t.Error("голый host-key (не cert) должен быть отвергнут — отказ от TOFU")
	}
}

// TestHostCertCallback_WrongPrincipal — cert от нашего CA, но principal не
// покрывает запрашиваемый хост → reject (CertChecker проверяет principals).
func TestHostCertCallback_WrongPrincipal(t *testing.T) {
	caSigner, caPub := testCAKey(t)
	hostKey := freshHostKey(t)
	cert := makeHostCert(t, caSigner, hostKey, "other-host.example.com")

	cb := hostCertCallback(singleCASet(caPub), nil)
	if err := cb("host-1.example.com:22", nil, cert); err == nil {
		t.Error("cert с чужим principal-ом должен быть отвергнут для другого хоста")
	}
}

// TestDial_RequiresCA — Dial без CA fail-closed (InsecureIgnoreHostKey запрещён).
func TestDial_RequiresCA(t *testing.T) {
	_, err := Dial(t.Context(), DialConfig{Host: "h", Port: 22, User: "u"})
	if err == nil {
		t.Fatal("Dial без CA должен отвергаться")
	}
}

// TestHostCertCallback_MultiCA_MatchFirst — S7-3: host-cert подписан первым CA
// в наборе → callback принимает + OnHostCAMatch фиксирует имя первого CA.
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
		t.Fatalf("cert от первого CA отвергнут: %v", err)
	}
	if matched != "trusted-bastion-1" {
		t.Errorf("OnHostCAMatch caName = %q, want trusted-bastion-1", matched)
	}
}

// TestHostCertCallback_MultiCA_MatchSecond — S7-3: host-cert подписан вторым CA
// в наборе → OR-проверка находит совпадение, OnHostCAMatch фиксирует имя
// именно второго CA (без false-match-а с первым).
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
		t.Fatalf("cert от второго CA отвергнут: %v", err)
	}
	if matched != "trusted-bastion-2" {
		t.Errorf("OnHostCAMatch caName = %q, want trusted-bastion-2", matched)
	}
}

// TestHostCertCallback_MultiCA_NoMatch — S7-3: cert подписан CA вне набора →
// reject; OnHostCAMatch не вызывается.
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
		t.Error("cert от CA вне набора должен быть отвергнут")
	}
	if matched != "" {
		t.Errorf("OnHostCAMatch вызван (caName = %q), ждали пусто (нет совпадения)", matched)
	}
}
