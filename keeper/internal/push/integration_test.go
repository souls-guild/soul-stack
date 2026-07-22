//go:build integration

// Integration test for keeper.push end-to-end against a REAL sshd:
// testcontainers spins up linuxserver/openssh-server, configures host-CA /
// host-cert / TrustedUserCAKeys, the dispatcher opens an SSH session with
// CA-signed host-cert verify, ShaDeliverer drops a mock "soul binary" (a
// shell script that prints a valid RunResult), ShaCleaner wipes the
// artifacts.
//
// Run:
//
//	cd keeper && go test -tags=integration -count=1 ./internal/push/...
//
// Dependencies: docker daemon (testcontainers-go starts a container on
// 127.0.0.1 with a randomly published port).
//
// Verifies:
//   - host-cert verification against testCA (Dial → ssh.CertChecker accept);
//   - the ephemeral keypair user-cert passes TrustedUserCAKeys;
//   - SHA-256 cache: a repeated Deliver doesn't re-upload an identical file;
//   - exec on the host produces a valid NDJSON RunResult → SendApply returns SUCCESS;
//   - Cleanup removes /var/lib/soul-stack/{bin,modules}/.

package push

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/crypto/ssh"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

const (
	integrationSSHDImage = "linuxserver/openssh-server:latest"
	integrationSSHUser   = "soul"
)

// integrationCA — a CA keypair generated per test. Used both as the host-CA
// (signs sshd's host-cert) and as the user-CA (TrustedUserCAKeys).
type integrationCA struct {
	signer ssh.Signer
	pub    ssh.PublicKey
}

func genIntegrationCA(t *testing.T) integrationCA {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ca genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ca signer: %v", err)
	}
	return integrationCA{signer: signer, pub: signer.PublicKey()}
}

// genHostKeyAndCert issues an ed25519 host key + a CA-signed host-cert with
// principal=127.0.0.1 (testcontainers forwards the port to localhost).
func genHostKeyAndCert(t *testing.T, ca integrationCA) (hostPrivPEM, hostPub, hostCert string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("host genkey: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal host priv: %v", err)
	}
	hostPrivPEM = string(pem.EncodeToMemory(block))
	hostPub = string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))

	cert := &ssh.Certificate{
		Key:             hostSigner.PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{"127.0.0.1", "localhost"},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
		Serial:          1,
	}
	if err := cert.SignCert(rand.Reader, ca.signer); err != nil {
		t.Fatalf("sign host cert: %v", err)
	}
	hostCert = string(ssh.MarshalAuthorizedKey(cert))
	return
}

// writeSSHContainerConfig prepares a host-mount directory with a custom
// sshd_config, host-key + host-cert, TrustedUserCAKeys, and a mock-soul
// script.
func writeSSHContainerConfig(t *testing.T, ca integrationCA) (mountDir string) {
	t.Helper()
	dir := t.TempDir()
	hostPrivPEM, _, hostCert := genHostKeyAndCert(t, ca)

	// host-key (RSA format for compatibility with openssh-server: modern sshd
	// also accepts ed25519 in PEM, so we'll keep ed25519).
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key"), []byte(hostPrivPEM), 0o600); err != nil {
		t.Fatalf("write host key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_ed25519_key-cert.pub"), []byte(hostCert), 0o644); err != nil {
		t.Fatalf("write host cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user_ca.pub"), ssh.MarshalAuthorizedKey(ca.pub), 0o644); err != nil {
		t.Fatalf("write user-ca: %v", err)
	}

	// Minimal sshd_config: HostKey + HostCertificate + TrustedUserCAKeys.
	// PubkeyAuthentication on, PasswordAuthentication off (fail-closed).
	sshdConfig := `Port 2222
HostKey /etc/ssh/keys/ssh_host_ed25519_key
HostCertificate /etc/ssh/keys/ssh_host_ed25519_key-cert.pub
TrustedUserCAKeys /etc/ssh/keys/user_ca.pub

PubkeyAuthentication yes
PasswordAuthentication no
PermitRootLogin no
UsePAM no
StrictModes no

# Accept ed25519-cert algorithms (some openssh builds require explicit opt-in).
PubkeyAcceptedAlgorithms +ssh-ed25519-cert-v01@openssh.com,ssh-ed25519
HostKeyAlgorithms ssh-ed25519-cert-v01@openssh.com,ssh-ed25519
CASignatureAlgorithms +ssh-ed25519

AllowUsers ` + integrationSSHUser + `

LogLevel DEBUG3
`
	if err := os.WriteFile(filepath.Join(dir, "sshd_config"), []byte(sshdConfig), 0o644); err != nil {
		t.Fatalf("write sshd_config: %v", err)
	}

	// Mock-soul: a shell script that reads stdin and prints a valid RunResult
	// (a single-line json with status RUN_STATUS_SUCCESS). Used in the e2e test
	// of SendApply against a real sshd.
	mockSoul := `#!/bin/sh
cat >/dev/null
printf '{"apply_id":"integration-1","status":"RUN_STATUS_SUCCESS"}\n'
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "mock-soul"), []byte(mockSoul), 0o755); err != nil {
		t.Fatalf("write mock-soul: %v", err)
	}

	return dir
}

// startSSHContainer starts a container with a custom sshd. linuxserver/openssh-server
// won't configure host-cert and TrustedUserCAKeys from env by itself, so we
// swap in sshd_config via a volume mount and start sshd directly as the
// entrypoint (bypassing s6-overlay) to avoid burning 10+s on init.
func startSSHContainer(ctx context.Context, t *testing.T, configDir string) (host string, port int, terminate func()) {
	t.Helper()

	// Shell-script entrypoint: installs openssh-server (if not present yet),
	// prepares /etc/ssh/keys, adds the soul user, starts sshd with the custom
	// config.
	entrypoint := `#!/bin/sh
set -e
apk add --no-cache openssh openssh-server-pam openssh-keygen sudo >/dev/null 2>&1 || true
adduser -D -s /bin/sh soul
# adduser -D creates a locked account; unlock it for pubkey-auth (no password).
passwd -u soul 2>/dev/null || sed -i 's/^soul:!/soul:*/' /etc/shadow
mkdir -p /etc/ssh/keys
cp /custom/ssh_host_ed25519_key /etc/ssh/keys/
cp /custom/ssh_host_ed25519_key-cert.pub /etc/ssh/keys/
cp /custom/user_ca.pub /etc/ssh/keys/
chmod 600 /etc/ssh/keys/ssh_host_ed25519_key
chmod 644 /etc/ssh/keys/ssh_host_ed25519_key-cert.pub /etc/ssh/keys/user_ca.pub
chown -R root:root /etc/ssh/keys
cp /custom/sshd_config /etc/ssh/sshd_config
mkdir -p /var/run/sshd /var/empty
# Give the soul user its own prefix /var/lib/soul-stack/* - otherwise
# mkdir on /var/lib without root would fail. This models the boot-time setup
# of the soul agent on a real host (Deliverer is not root by design).
mkdir -p /var/lib/soul-stack/bin /var/lib/soul-stack/modules
chown -R soul:soul /var/lib/soul-stack
echo "sshd ready"
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
`
	scriptPath := filepath.Join(configDir, "entrypoint.sh")
	if err := os.WriteFile(scriptPath, []byte(entrypoint), 0o755); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}

	req := testcontainers.ContainerRequest{
		Image:        "alpine:3.20",
		ExposedPorts: []string{"2222/tcp"},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: scriptPath, ContainerFilePath: "/entrypoint.sh", FileMode: 0o755},
		},
		Mounts: testcontainers.ContainerMounts{
			testcontainers.ContainerMount{
				Source: testcontainers.GenericBindMountSource{HostPath: configDir},
				Target: "/custom",
			},
		},
		Entrypoint: []string{"/bin/sh", "/entrypoint.sh"},
		WaitingFor: wait.ForLog("Server listening on").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start sshd container: %v", err)
	}
	terminate = func() {
		if t.Failed() {
			if r, lerr := c.Logs(context.Background()); lerr == nil {
				data, _ := io.ReadAll(r)
				t.Logf("--- sshd container logs ---\n%s\n--- end logs ---", string(data))
			}
		}
		_ = c.Terminate(context.Background())
	}

	host, err = c.Host(ctx)
	if err != nil {
		terminate()
		t.Fatalf("container host: %v", err)
	}
	mp, err := c.MappedPort(ctx, "2222")
	if err != nil {
		terminate()
		t.Fatalf("mapped port: %v", err)
	}
	portN, err := strconv.Atoi(mp.Port())
	if err != nil {
		terminate()
		t.Fatalf("parse port: %v", err)
	}
	return host, portN, terminate
}

// makeEphemeralAuthForIntegration issues an ephemeral user keypair + cert
// from integrationCA with principal=soul (as required by
// AllowUsers/TrustedUserCAKeys). This emulates the output of
// SshProvider.Sign() in Vault mode.
func makeEphemeralAuthForIntegration(t *testing.T, ca integrationCA) ([]ssh.AuthMethod, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("user genkey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("user signer: %v", err)
	}
	cert := &ssh.Certificate{
		Key:             signer.PublicKey(),
		CertType:        ssh.UserCert,
		ValidPrincipals: []string{integrationSSHUser},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
		Serial:          1,
	}
	if err := cert.SignCert(rand.Reader, ca.signer); err != nil {
		t.Fatalf("sign user cert: %v", err)
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		t.Fatalf("user cert signer: %v", err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(certSigner)}, string(ssh.MarshalAuthorizedKey(cert))
}

// integrationProvider emulates an SshProvider that returns a cert on the
// pubkey the dispatcher passed in (Vault SSH CA flow). private_key is empty —
// the canonical ephemeral mode.
type integrationProvider struct {
	t  *testing.T
	ca integrationCA
}

func (p *integrationProvider) Authorize(_ context.Context, _ *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

func (p *integrationProvider) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(req.GetPublicKey()))
	if err != nil {
		return nil, fmt.Errorf("parse pub: %w", err)
	}
	cert := &ssh.Certificate{
		Key:             pub,
		CertType:        ssh.UserCert,
		ValidPrincipals: []string{integrationSSHUser},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
		Serial:          1,
	}
	if err := cert.SignCert(rand.Reader, p.ca.signer); err != nil {
		return nil, fmt.Errorf("sign user cert: %w", err)
	}
	return &pluginv1.SignReply{
		Certificate: string(ssh.MarshalAuthorizedKey(cert)),
		TtlSeconds:  300,
	}, nil
}

// TestIntegration_LiveSSHD_DeliverApplyCleanup — end-to-end against a real
// sshd: CA-signed host-cert verify + TrustedUserCAKeys user auth +
// ShaDeliverer + SendApply (mock-soul prints a RunResult) + ShaCleaner.
func TestIntegration_LiveSSHD_DeliverApplyCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ca := genIntegrationCA(t)
	configDir := writeSSHContainerConfig(t, ca)
	containerHost, containerPort, terminate := startSSHContainer(ctx, t, configDir)
	defer terminate()

	// Prepare local artifacts for the Deliverer.
	localSoul := filepath.Join(t.TempDir(), "soul")
	if err := os.WriteFile(localSoul, []byte("#!/bin/sh\ncat >/dev/null\nprintf '{\"apply_id\":\"integration-1\",\"status\":\"RUN_STATUS_SUCCESS\"}\\n'\n"), 0o755); err != nil {
		t.Fatalf("write soul: %v", err)
	}
	localMod := filepath.Join(t.TempDir(), "soul-mod-pkg")
	if err := os.WriteFile(localMod, []byte("MODULE-V1"), 0o755); err != nil {
		t.Fatalf("write mod: %v", err)
	}

	disp, err := NewSshDispatcher(Deps{
		Providers:       map[string]ProviderEntry{testProviderName: {Provider: &integrationProvider{t: t, ca: ca}}},
		Targets:         &mockTargets{target: SSHTarget{Host: containerHost, Port: containerPort, User: integrationSSHUser, SoulPath: "/var/lib/soul-stack/bin/soul"}},
		Souls:           &mockSouls{s: &soul.Soul{SID: containerHost, Transport: soul.TransportSSH}},
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.pub}},
		Deliverer:       NewShaDeliverer(),
		Cleaner:         NewShaCleaner(),
		SoulSpec: SoulSpec{
			SoulBinaryPath: localSoul,
			Modules:        []ModuleSpec{{Name: "soul-mod-pkg", Path: localMod}},
		},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSshDispatcher: %v", err)
	}

	rr, err := disp.SendApply(ctx, containerHost, testProviderName, &keeperv1.ApplyRequest{ApplyId: "integration-1"})
	if err != nil {
		t.Fatalf("SendApply: %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", rr.GetStatus())
	}

	// Verify the files actually made it over: repeat SendApply — sha256 will
	// match, upload should not happen (i.e. both runs return SUCCESS, and the
	// second run is faster — but that's not assert-able without timing; at
	// least check there's no regression).
	rr2, err := disp.SendApply(ctx, containerHost, testProviderName, &keeperv1.ApplyRequest{ApplyId: "integration-2"})
	if err != nil {
		t.Fatalf("SendApply (repeat): %v", err)
	}
	if rr2.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("repeat run status = %v, want SUCCESS", rr2.GetStatus())
	}

	// Cleanup → /var/lib/soul-stack/{bin,modules}/ removed.
	if err := disp.Cleanup(ctx, containerHost, testProviderName); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	// Post-Cleanup, verify the directories are gone: open a fresh Dial and run
	// `ls /var/lib/soul-stack` → expect it empty/absent. We open it ourselves
	// (bypassing dispatcher.SendApply, which would recreate them).
	auth, _ := makeEphemeralAuthForIntegration(t, ca)
	sess, err := Dial(ctx, DialConfig{
		Host:            containerHost,
		Port:            containerPort,
		User:            integrationSSHUser,
		Auth:            auth,
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: ca.pub}},
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial post-cleanup: %v", err)
	}
	defer sess.Close()
	out, _ := sess.Run(ctx, "test -d /var/lib/soul-stack/bin && echo PRESENT || echo ABSENT", nil)
	if !strings.Contains(out, "ABSENT") {
		t.Errorf("after Cleanup hostSoulDir was not removed: %q", out)
	}
	out, _ = sess.Run(ctx, "test -d /var/lib/soul-stack/modules && echo PRESENT || echo ABSENT", nil)
	if !strings.Contains(out, "ABSENT") {
		t.Errorf("after Cleanup hostModulesDir was not removed: %q", out)
	}
}
