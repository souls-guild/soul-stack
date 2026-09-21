//go:build integration

package sshdtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/crypto/ssh"

	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// DefaultUser is the unprivileged account the container creates and sshd
// admits. Unprivileged on purpose: delivery writes under
// /var/lib/soul-stack/, which is chowned to it at boot, so a test cannot pass
// by being root where production is not.
const DefaultUser = "soul"

// certValidity is how far either side of now an issued certificate is valid.
// Generous, because container clock skew is not the subject of any test here.
const certValidity = time.Hour

// CA is one ed25519 keypair used as BOTH the host CA (it signs sshd's host
// certificate) and the user CA (sshd's TrustedUserCAKeys). One key is enough:
// the separation between the two roles is a deployment property, and no test
// here is about that separation.
type CA struct {
	Signer ssh.Signer
	Pub    ssh.PublicKey
}

// NewCA generates a fresh CA per test.
func NewCA(t *testing.T) CA {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("sshdtest: ca genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("sshdtest: ca signer: %v", err)
	}
	return CA{Signer: signer, Pub: signer.PublicKey()}
}

// SignUserCert issues a user certificate for an authorized-keys-form public
// key, with `user` as the only principal. This is what an SshProvider plugin
// does in Vault-SSH-CA mode.
func (ca CA) SignUserCert(pubAuthorized, user string) (string, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubAuthorized))
	if err != nil {
		return "", fmt.Errorf("sshdtest: parse pubkey: %w", err)
	}
	cert := &ssh.Certificate{
		Key:             pub,
		CertType:        ssh.UserCert,
		ValidPrincipals: []string{user},
		ValidAfter:      uint64(time.Now().Add(-certValidity).Unix()),
		ValidBefore:     uint64(time.Now().Add(certValidity).Unix()),
		Serial:          1,
	}
	if err := cert.SignCert(rand.Reader, ca.Signer); err != nil {
		return "", fmt.Errorf("sshdtest: sign user cert: %w", err)
	}
	return string(ssh.MarshalAuthorizedKey(cert)), nil
}

// EphemeralAuth mints a throwaway keypair, has the CA sign it, and returns
// ssh.AuthMethods for it — a caller that wants its own session beside the one
// the dispatcher opens (post-condition assertions, for instance).
func (ca CA) EphemeralAuth(t *testing.T, user string) []ssh.AuthMethod {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("sshdtest: user genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("sshdtest: user signer: %v", err)
	}
	certText, err := ca.SignUserCert(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), user)
	if err != nil {
		t.Fatalf("sshdtest: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(certText))
	if err != nil {
		t.Fatalf("sshdtest: parse issued cert: %v", err)
	}
	certSigner, err := ssh.NewCertSigner(pub.(*ssh.Certificate), signer)
	if err != nil {
		t.Fatalf("sshdtest: cert signer: %v", err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(certSigner)}
}

// Provider is an in-process SshProvider: it allows every host and signs the
// dispatcher's ephemeral public key with the CA. Structurally satisfies
// push.SshProvider without importing it — sshdtest is used BY that package's
// tests.
type Provider struct {
	CA   CA
	User string
}

// Authorize allows everything. A deny path is a unit concern; here the subject
// is the connection.
func (p *Provider) Authorize(_ context.Context, _ *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

// Sign returns a certificate and NO private key — the canonical ephemeral mode,
// where the key never leaves the Keeper.
func (p *Provider) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	user := p.User
	if user == "" {
		user = DefaultUser
	}
	certText, err := p.CA.SignUserCert(req.GetPublicKey(), user)
	if err != nil {
		return nil, err
	}
	return &pluginv1.SignReply{Certificate: certText, TtlSeconds: 300}, nil
}

// Host is a running sshd container.
type Host struct {
	// Addr is the address the sshd is reachable at from this process, and a
	// principal on its host certificate. Under testcontainers this is a
	// loopback address with a published port.
	Addr string
	// Port is the published port.
	Port int
	// User is the account sshd admits.
	User string

	ctr testcontainers.Container
	t   *testing.T
}

// Options tunes the container. The zero value is what both suites want.
type Options struct {
	// User overrides [DefaultUser].
	User string
}

// Start brings up alpine + openssh with a CA-signed host certificate, a
// TrustedUserCAKeys pointing at the same CA, and /var/lib/soul-stack/ owned by
// the unprivileged user. Registers its own termination with t, dumping
// container logs when the test failed.
func Start(ctx context.Context, t *testing.T, ca CA, opts Options) *Host {
	t.Helper()
	user := opts.User
	if user == "" {
		user = DefaultUser
	}

	dir := t.TempDir()
	writeHostMaterial(t, dir, ca, user)

	req := testcontainers.ContainerRequest{
		Image:        "alpine:3.20",
		ExposedPorts: []string{"2222/tcp"},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: filepath.Join(dir, "entrypoint.sh"), ContainerFilePath: "/entrypoint.sh", FileMode: 0o755},
		},
		Mounts: testcontainers.ContainerMounts{
			testcontainers.ContainerMount{
				Source: testcontainers.GenericBindMountSource{HostPath: dir},
				Target: "/custom",
			},
		},
		Entrypoint: []string{"/bin/sh", "/entrypoint.sh"},
		WaitingFor: wait.ForLog("Server listening on").WithStartupTimeout(60 * time.Second),
	}
	c, err := integrationenv.Start(ctx, "sshd", func(ctx context.Context) (testcontainers.Container, error) {
		return testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
	})
	if err != nil {
		t.Fatalf("sshdtest: start sshd container: %v", err)
	}

	h := &Host{User: user, ctr: c, t: t}
	t.Cleanup(h.terminate)

	addr, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("sshdtest: container host: %v", err)
	}
	mp, err := c.MappedPort(ctx, "2222")
	if err != nil {
		t.Fatalf("sshdtest: mapped port: %v", err)
	}
	port, err := strconv.Atoi(mp.Port())
	if err != nil {
		t.Fatalf("sshdtest: parse port %q: %v", mp.Port(), err)
	}
	h.Addr, h.Port = addr, port
	return h
}

// Exec runs a shell command inside the container, bypassing ssh — for
// post-conditions, where going back in over ssh would re-create the very
// directories the assertion is about.
//
// Multiplexed: the raw docker exec stream frames every chunk with an 8-byte
// header, which lands in the middle of the output an assertion compares.
func (h *Host) Exec(ctx context.Context, cmd string) (string, error) {
	code, reader, err := h.ctr.Exec(ctx, []string{"/bin/sh", "-c", cmd}, tcexec.Multiplexed())
	if err != nil {
		return "", err
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return string(out), fmt.Errorf("sshdtest: exec %q: exit %d", cmd, code)
	}
	return string(out), nil
}

func (h *Host) terminate() {
	if h.t.Failed() {
		if r, err := h.ctr.Logs(context.Background()); err == nil {
			data, _ := io.ReadAll(r)
			h.t.Logf("--- sshd container logs ---\n%s\n--- end logs ---", string(data))
		}
	}
	_ = h.ctr.Terminate(context.Background())
}

// writeHostMaterial lays out the mounted directory: host key + host cert, the
// user CA, an sshd_config that uses them, and the boot script.
func writeHostMaterial(t *testing.T, dir string, ca CA, user string) {
	t.Helper()
	hostPrivPEM, hostCert := genHostKeyAndCert(t, ca)

	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatalf("sshdtest: write %s: %v", name, err)
		}
	}
	write("ssh_host_ed25519_key", hostPrivPEM, 0o600)
	write("ssh_host_ed25519_key-cert.pub", hostCert, 0o644)
	write("user_ca.pub", string(ssh.MarshalAuthorizedKey(ca.Pub)), 0o644)

	// PubkeyAcceptedAlgorithms / HostKeyAlgorithms are spelled out because some
	// openssh builds do not offer the ed25519 cert algorithms unless asked.
	write("sshd_config", `Port 2222
HostKey /etc/ssh/keys/ssh_host_ed25519_key
HostCertificate /etc/ssh/keys/ssh_host_ed25519_key-cert.pub
TrustedUserCAKeys /etc/ssh/keys/user_ca.pub

PubkeyAuthentication yes
PasswordAuthentication no
PermitRootLogin no
UsePAM no
StrictModes no

PubkeyAcceptedAlgorithms +ssh-ed25519-cert-v01@openssh.com,ssh-ed25519
HostKeyAlgorithms ssh-ed25519-cert-v01@openssh.com,ssh-ed25519
CASignatureAlgorithms +ssh-ed25519

AllowUsers `+user+`

LogLevel DEBUG3
`, 0o644)

	// /var/lib/soul-stack/{bin,modules} is created and chowned here, standing
	// in for the boot-time setup of a real host: the Deliverer connects as an
	// unprivileged user and cannot mkdir under /var/lib itself.
	write("entrypoint.sh", `#!/bin/sh
set -e
apk add --no-cache openssh openssh-server-pam openssh-keygen sudo >/dev/null 2>&1 || true
adduser -D -s /bin/sh `+user+`
# adduser -D leaves the account locked; unlock it for pubkey auth (no password).
passwd -u `+user+` 2>/dev/null || sed -i 's/^`+user+`:!/`+user+`:*/' /etc/shadow
mkdir -p /etc/ssh/keys
cp /custom/ssh_host_ed25519_key /custom/ssh_host_ed25519_key-cert.pub /custom/user_ca.pub /etc/ssh/keys/
chmod 600 /etc/ssh/keys/ssh_host_ed25519_key
chmod 644 /etc/ssh/keys/ssh_host_ed25519_key-cert.pub /etc/ssh/keys/user_ca.pub
chown -R root:root /etc/ssh/keys
cp /custom/sshd_config /etc/ssh/sshd_config
mkdir -p /var/run/sshd /var/empty
mkdir -p /var/lib/soul-stack/bin /var/lib/soul-stack/modules
chown -R `+user+`:`+user+` /var/lib/soul-stack
echo "sshd ready"
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
`, 0o755)
}

// genHostKeyAndCert issues an ed25519 host key and a CA-signed host
// certificate whose principals cover the loopback names testcontainers
// publishes on.
func genHostKeyAndCert(t *testing.T, ca CA) (privPEM, certText string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("sshdtest: host genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("sshdtest: host signer: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("sshdtest: marshal host priv: %v", err)
	}
	cert := &ssh.Certificate{
		Key:             signer.PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{"127.0.0.1", "localhost"},
		ValidAfter:      uint64(time.Now().Add(-certValidity).Unix()),
		ValidBefore:     uint64(time.Now().Add(certValidity).Unix()),
		Serial:          1,
	}
	if err := cert.SignCert(rand.Reader, ca.Signer); err != nil {
		t.Fatalf("sshdtest: sign host cert: %v", err)
	}
	return string(pem.EncodeToMemory(block)), string(ssh.MarshalAuthorizedKey(cert))
}

// AssertHostCertPrincipal fails when addr is not one of the host
// certificate's principals — the handshake would then be rejected for a reason
// that reads like a CA problem. testcontainers normally publishes on
// 127.0.0.1; a remote docker host does not, and this says so.
func AssertHostCertPrincipal(t *testing.T, addr string) {
	t.Helper()
	for _, p := range []string{"127.0.0.1", "localhost"} {
		if strings.EqualFold(addr, p) {
			return
		}
	}
	t.Fatalf("sshdtest: docker published the container on %q, which the host certificate does not "+
		"cover (principals: 127.0.0.1, localhost) — this suite needs a local docker daemon", addr)
}
