//go:build integration

package sshdtest

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
)

// A test harness with a bug of its own reads as a bug in what it tests, so the
// two properties every caller leans on are asserted here: the container really
// speaks certificate SSH in both directions, and Exec returns what the command
// printed rather than what docker framed around it.

// TestHarness_CertificateSSHInBothDirections — the sshd presents a host cert
// this CA signed, and admits a user cert the same CA signed. Nothing in the
// keeper trusts a bare host key (there is no TOFU path), so a harness that
// quietly fell back to one would make every dispatcher suite meaningless.
func TestHarness_CertificateSSHInBothDirections(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ca := NewCA(t)
	host := Start(ctx, t, ca, Options{})
	AssertHostCertPrincipal(t, host.Addr)

	var presented ssh.PublicKey
	checker := &ssh.CertChecker{
		IsHostAuthority: func(auth ssh.PublicKey, _ string) bool {
			presented = auth
			return string(auth.Marshal()) == string(ca.Pub.Marshal())
		},
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(host.Addr, strconv.Itoa(host.Port)), &ssh.ClientConfig{
		User:            host.User,
		Auth:            ca.EphemeralAuth(t, host.User),
		HostKeyCallback: checker.CheckHostKey,
		Timeout:         20 * time.Second,
	})
	if err != nil {
		t.Fatalf("certificate handshake failed: %v", err)
	}
	defer client.Close()
	if presented == nil {
		t.Error("the host presented a bare key, not a certificate — CertChecker never consulted an authority")
	}
}

// TestHarness_ExecIsDemultiplexed — the raw docker exec stream frames each
// chunk with an 8-byte header. An assertion comparing output would then fail
// on bytes the command never wrote, and read as a failure of the subject.
func TestHarness_ExecIsDemultiplexed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	host := Start(ctx, t, NewCA(t), Options{})

	out, err := host.Exec(ctx, "printf marker")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out != "marker" {
		t.Errorf("Exec returned %q, want exactly \"marker\" — docker stream framing is leaking through", out)
	}
}

// TestHarness_ExecReportsANonZeroExit — a post-condition like `test -e <path>`
// is read through the error, so a swallowed exit code would turn every such
// assertion into an unconditional pass.
func TestHarness_ExecReportsANonZeroExit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires docker")
	}
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	host := Start(ctx, t, NewCA(t), Options{})

	if _, err := host.Exec(ctx, "test -e /definitely/not/here"); err == nil {
		t.Fatal("Exec reported success for a command that exited non-zero")
	} else if !strings.Contains(err.Error(), "exit") {
		t.Errorf("error does not name the exit status: %v", err)
	}
}
