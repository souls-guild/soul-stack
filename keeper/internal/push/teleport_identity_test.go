package push

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	transportv1pb "github.com/gravitational/teleport/api/gen/proto/go/teleport/transport/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// liveExpiredRefusal is the error the WB stand logged on every attempt while the
// Keeper's identity had lapsed (NIM-1171), as the proxy client's DialHost wraps it.
const liveExpiredRefusal = `failed connecting to host vm-1.example.com: opening proxy stream
	rpc error: code = Unavailable desc = connection error: desc = "error reading server preface: remote error: tls: expired certificate"`

func TestIsProxyRefusal(t *testing.T) {
	refusals := map[string]error{
		"the live expired-certificate refusal": status.Error(codes.Unavailable, liveExpiredRefusal),
		"an alert as crypto/tls records it":    &net.OpError{Op: "remote error", Err: tls.AlertError(45)},
		"a wrapped bad-certificate alert":      fmt.Errorf("handshake: %w", &net.OpError{Op: "remote error", Err: tls.AlertError(42)}),
		"an unknown-CA alert flattened by gRPC": status.Error(codes.Unavailable,
			`connection error: desc = "transport: authentication handshake failed: remote error: tls: unknown certificate authority"`),
		"the proxy answering Unauthenticated": fmt.Errorf("dial: %w", status.Error(codes.Unauthenticated, "no credentials")),
		"an access denial":                    fmt.Errorf("failed connecting to host: %w", deniedError{}),
		"a denial at stream open, a status":   fmt.Errorf("opening proxy stream: %w", status.Error(codes.PermissionDenied, "access denied")),
	}
	for name, err := range refusals {
		if !isProxyRefusal(err) {
			t.Errorf("%s: %v not classified as a refusal of the identity", name, err)
		}
	}

	waits := map[string]error{
		"a node that has not joined":            errors.New("node offline or does not exist"),
		"a node that has not joined, as status": status.Error(codes.Unavailable, "target host vm-1.example.com:22 is offline or does not exist"),
		"a proxy that is not listening":         &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		"an alert about something else":         &net.OpError{Op: "remote error", Err: tls.AlertError(80)},
		"an alert we sent, not received":        &net.OpError{Op: "local error", Err: tls.AlertError(45)},
	}
	for name, err := range waits {
		if isProxyRefusal(err) {
			t.Errorf("%s: %v classified as a refusal of the identity — the join wait would stop on it", name, err)
		}
	}
}

// deniedError has the marker trace.AccessDeniedError carries.
type deniedError struct{}

func (deniedError) Error() string             { return "access denied" }
func (deniedError) IsAccessDeniedError() bool { return true }

// ★ GUARD. An identity whose TLS certificate has expired fails the dial before
// anything is sent to the proxy, naming the file and the moment, and not the host.
// The constructor still takes it: tbot may renew it before the first step runs.
//
// Mutation: drop the NotAfter check in the dial closure and this reddens — the
// dial reaches the proxy and fails as a host that is not there.
func TestTeleportDial_ExpiredIdentity_IsRefusedBeforeAnyDial(t *testing.T) {
	addr, accepted := countingListener(t)
	notAfter := time.Now().Add(-time.Minute).Truncate(time.Second).UTC()
	path := writeIdentityFileWith(t, identityOpts{withHostCA: true, tlsNotAfter: notAfter})
	dial, err := NewTeleportDialer(TeleportDialerConfig{ProxyAddr: addr, IdentityFile: path, Cluster: "c1"})
	if err != nil {
		t.Fatalf("an expired identity must not stop the Keeper from starting: %v", err)
	}

	err = dialOnce(dial)
	if !IsTeleportIdentityError(err) {
		t.Fatalf("dial through an expired identity = %v, want a TeleportIdentityError", err)
	}
	msg := err.Error()
	for _, want := range []string{path, notAfter.Format(time.RFC3339), "expired"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal = %q, want %q in it", msg, want)
		}
	}
	if strings.Contains(msg, "vm-1.example.com") {
		t.Errorf("refusal = %q names the host; the host is not what failed", msg)
	}
	if n := accepted.Load(); n != 0 {
		t.Errorf("the proxy was connected to %d time(s) through an identity known to be expired", n)
	}
}

// ★ GUARD. The expiry is read from the file on every attempt, not from the one the
// Keeper started with: an identity that lapses during a join wait ends it at the
// next attempt, and one tbot renews after it lapsed is dialed through again.
//
// Mutation: compare against a NotAfter parsed once in the constructor and either
// case reddens — the first dials through the lapsed file, the second never dials.
func TestTeleportDial_IdentityExpiryIsReadOnEveryAttempt(t *testing.T) {
	valid := func(t *testing.T) []byte {
		return readFile(t, writeIdentityFileWith(t, identityOpts{withHostCA: true}))
	}
	expired := func(t *testing.T) []byte {
		return readFile(t, writeIdentityFileWith(t, identityOpts{withHostCA: true, tlsNotAfter: time.Now().Add(-time.Minute)}))
	}

	t.Run("lapses after the Keeper started", func(t *testing.T) {
		addr, accepted := countingListener(t)
		path := writeIdentityFileWith(t, identityOpts{withHostCA: true})
		dial, err := NewTeleportDialer(TeleportDialerConfig{ProxyAddr: addr, IdentityFile: path, Cluster: "c1"})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, expired(t))
		if err := dialOnce(dial); !IsTeleportIdentityError(err) {
			t.Fatalf("dial through the lapsed file = %v, want a TeleportIdentityError", err)
		}
		if n := accepted.Load(); n != 0 {
			t.Errorf("the proxy was connected to %d time(s) through a lapsed identity", n)
		}
	})
	t.Run("renewed after it lapsed", func(t *testing.T) {
		addr, accepted := countingListener(t)
		path := filepath.Join(t.TempDir(), "identity")
		writeFile(t, path, expired(t))
		dial, err := NewTeleportDialer(TeleportDialerConfig{ProxyAddr: addr, IdentityFile: path, Cluster: "c1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := dialOnce(dial); !IsTeleportIdentityError(err) {
			t.Fatalf("dial before the renewal = %v, want a TeleportIdentityError", err)
		}
		writeFile(t, path, valid(t))
		if err := dialOnce(dial); IsTeleportIdentityError(err) {
			t.Fatalf("dial after the renewal = %v, still refused as the lapsed identity", err)
		}
		if accepted.Load() == 0 {
			t.Error("the renewed identity never reached the proxy")
		}
	})
}

// countingListener accepts and drops connections, counting them: a stand-in for a
// proxy that only has to say whether it was contacted.
func countingListener(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	accepted := new(atomic.Int32)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), accepted
}

func dialOnce(dial Dialer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := dial(ctx, DialConfig{Host: "vm-1.example.com", Port: 22, User: "root", Timeout: 5 * time.Second})
	if sess != nil {
		_ = sess.Close()
	}
	return err
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ★ GUARD. A proxy refusing the Keeper's certificate is a TeleportIdentityError,
// end to end through the real Teleport client and gRPC: a local TLS listener stands
// in for the proxy and refuses the client certificate the way the live one did.
// The first case reproduces the WB stand's alert; to get past the pre-dial expiry
// check it puts the expiry on the proxy's clock only (a clock skew), whereas the
// live identity had expired outright.
//
// Mutation: drop the text fallback or the access-denied branch of isProxyRefusal,
// or the isProxyRefusal branch after DialHost, and this reddens.
func TestTeleportDial_ProxyRefusingTheIdentity_IsAnIdentityError(t *testing.T) {
	ca := newTestTLSCA(t)
	cases := []struct {
		name    string
		tune    func(*tls.Config)
		answer  error
		inCause string
	}{
		{
			name:    "certificate expired by the proxy's clock",
			tune:    func(c *tls.Config) { c.Time = func() time.Time { return time.Now().Add(3 * time.Hour) } },
			inCause: "remote error: tls: expired certificate",
		},
		{
			name:    "certificate from a CA the proxy does not trust",
			tune:    func(c *tls.Config) { c.ClientCAs = newTestTLSCA(t).pool() },
			inCause: "remote error: tls: unknown certificate authority",
		},
		{
			name:    "identity denied by the proxy",
			answer:  status.Error(codes.PermissionDenied, `lock targeting User:"bot-keeper" is in force`),
			inCause: "lock targeting",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := startProxyFixture(t, ca, tc.tune, tc.answer)
			path := writeIdentityFileWith(t, identityOpts{withHostCA: true, tlsCA: ca})
			err := dialThrough(t, addr, path)
			if !IsTeleportIdentityError(err) {
				t.Fatalf("dial = %v, want a TeleportIdentityError", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.inCause) {
				t.Fatalf("refusal = %q does not carry %q — the fixture exercised a different failure", msg, tc.inCause)
			}
			if !strings.HasPrefix(msg, "push: Teleport proxy refused Keeper's own identity "+fmt.Sprintf("%q", path)) {
				t.Errorf("refusal = %q, want it to open on the Keeper's identity", msg)
			}
		})
	}
}

// ★ GUARD, the other side. What a node that has not joined yet looks like from an
// accepting proxy — and a proxy not listening at all — stays an ordinary error, so
// the join wait keeps waiting for it.
func TestTeleportDial_NodeNotJoined_IsNotAnIdentityError(t *testing.T) {
	ca := newTestTLSCA(t)
	path := writeIdentityFileWith(t, identityOpts{withHostCA: true, tlsCA: ca})
	for name, answer := range map[string]error{
		"offline or does not exist": status.Error(codes.Unavailable, "target host vm-1.example.com:22 is offline or does not exist"),
		"not found":                 status.Error(codes.NotFound, "node vm-1.example.com not found"),
	} {
		t.Run(name, func(t *testing.T) {
			err := dialThrough(t, startProxyFixture(t, ca, nil, answer), path)
			if err == nil || IsTeleportIdentityError(err) {
				t.Fatalf("dial = %v, want an ordinary error the join wait retries", err)
			}
		})
	}
	t.Run("proxy not listening", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		err = dialThrough(t, addr, path)
		if err == nil || IsTeleportIdentityError(err) {
			t.Fatalf("dial = %v, want an ordinary error the join wait retries", err)
		}
	})
}

func dialThrough(t *testing.T, proxyAddr, identity string) error {
	t.Helper()
	dial, err := NewTeleportDialer(TeleportDialerConfig{ProxyAddr: proxyAddr, IdentityFile: identity, Cluster: "c1"})
	if err != nil {
		t.Fatalf("NewTeleportDialer: %v", err)
	}
	return dialOnce(dial)
}

// testTLSCA signs both the proxy fixture's server certificate and the identity's
// client certificate.
type testTLSCA struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
	pem  []byte
}

func newTestTLSCA(t *testing.T) *testTLSCA {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(10),
		Subject:               pkix.Name{CommonName: "teleport-tls-ca-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testTLSCA{cert: cert, key: priv, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testTLSCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// serverCert is the proxy's certificate, for the name the Teleport client sends
// as SNI when none is set (`teleport.cluster.local`).
func (ca *testTLSCA) serverCert(t *testing.T) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(11),
		Subject:      pkix.Name{CommonName: "proxy"},
		DNSNames:     []string{"teleport.cluster.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// startProxyFixture serves the Teleport transport service over mTLS under ca.
// tune adjusts how the client certificate is judged; ProxySSH reads the dial
// target and answers with answer.
func startProxyFixture(t *testing.T, ca *testTLSCA, tune func(*tls.Config), answer error) string {
	t.Helper()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{ca.serverCert(t)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.pool(),
	}
	if tune != nil {
		tune(cfg)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	transportv1pb.RegisterTransportServiceServer(srv, answeringTransport{answer: answer})
	go func() { _ = srv.Serve(lingeringListener{ln}) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

// lingeringListener closes each connection the write side first and reads off
// what the client already sent before the socket goes. Under TLS 1.3 the client
// sends its HTTP/2 preface before the server has judged its certificate; a plain
// Close with that preface unread makes the kernel answer with a reset, which can
// overtake the alert ("connection reset by peer") and leave the case testing a
// different failure than the one it names.
type lingeringListener struct{ net.Listener }

func (l lingeringListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return lingeringConn{c.(*net.TCPConn)}, nil
}

type lingeringConn struct{ *net.TCPConn }

func (c lingeringConn) Close() error {
	_ = c.CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.Copy(io.Discard, c.TCPConn)
	return c.TCPConn.Close()
}

type answeringTransport struct {
	transportv1pb.UnimplementedTransportServiceServer
	answer error
}

func (s answeringTransport) ProxySSH(stream grpc.BidiStreamingServer[transportv1pb.ProxySSHRequest, transportv1pb.ProxySSHResponse]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if s.answer == nil {
		return status.Error(codes.Internal, "the fixture has no answer for a dial it accepted")
	}
	return s.answer
}
