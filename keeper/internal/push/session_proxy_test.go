package push

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/encoding/protojson"
)

// liveSSHServer — a shared in-process SSH-server harness for proxy_jump
// tests. Comes up on 127.0.0.1:0 (OS-assigned port), the host key is signed
// by the passed-in host-CA, authorization accepts any public key (the tests
// don't check authn policy — that's session.Dial and direct-tcpip's zone).
type liveSSHServer struct {
	listener   net.Listener
	host       string
	port       int
	srvConfig  *ssh.ServerConfig
	handleConn func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request)
	wg         sync.WaitGroup
	stopCh     chan struct{}

	// telemetry — flags the tests read for assertions.
	sawDirectTCPIP atomic.Bool
	sawTargetExec  atomic.Bool
}

func (s *liveSSHServer) addr() string {
	return net.JoinHostPort(s.host, strconv.Itoa(s.port))
}

func (s *liveSSHServer) close() {
	close(s.stopCh)
	_ = s.listener.Close()
	s.wg.Wait()
}

// newLiveSSHServer starts an SSH server; the host cert is issued for the
// principal `principal` (tests pass "127.0.0.1" so CertChecker accepts it by
// default for a connection to this IP).
func newLiveSSHServer(
	t *testing.T,
	caSigner ssh.Signer,
	principal string,
	handle func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request),
) *liveSSHServer {
	t.Helper()

	// Host key for the server + a host cert from the CA.
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("host genkey: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}
	hostCert := &ssh.Certificate{
		Key:             hostSigner.PublicKey(),
		CertType:        ssh.HostCert,
		ValidPrincipals: []string{principal},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
		Serial:          1,
	}
	if err := hostCert.SignCert(rand.Reader, caSigner); err != nil {
		t.Fatalf("sign host cert: %v", err)
	}
	hostCertSigner, err := ssh.NewCertSigner(hostCert, hostSigner)
	if err != nil {
		t.Fatalf("host cert signer: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			// Tests don't check the user-cert server-side; allow everything to
			// focus on checking the dispatcher's proxy_jump logic.
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostCertSigner)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)

	s := &liveSSHServer{
		listener:   l,
		host:       host,
		port:       port,
		srvConfig:  cfg,
		handleConn: handle,
		stopCh:     make(chan struct{}),
	}

	s.wg.Add(1)
	go s.acceptLoop(t)
	return s
}

func (s *liveSSHServer) acceptLoop(t *testing.T) {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			sc, chans, reqs, err := ssh.NewServerConn(conn, s.srvConfig)
			if err != nil {
				return
			}
			defer sc.Close()
			s.handleConn(t, sc, chans, reqs)
		}()
	}
}

// proxyHandle — a handler for an SSH server playing the bastion role:
// accepts direct-tcpip channels and forwards them to the actual target
// address (taken from the channel payload).
func proxyHandle(s *liveSSHServer) func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	return func(t *testing.T, _ *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
		go ssh.DiscardRequests(reqs)
		for newCh := range chans {
			if newCh.ChannelType() != "direct-tcpip" {
				_ = newCh.Reject(ssh.UnknownChannelType, "direct-tcpip only")
				continue
			}
			s.sawDirectTCPIP.Store(true)
			payload, err := parseDirectTCPIP(newCh.ExtraData())
			if err != nil {
				_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			upstream, err := net.DialTimeout("tcp", net.JoinHostPort(payload.raddr, strconv.Itoa(int(payload.rport))), 5*time.Second)
			if err != nil {
				_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				_ = upstream.Close()
				continue
			}
			go ssh.DiscardRequests(chReqs)
			go bidirectionalCopy(ch, upstream)
		}
	}
}

// targetHandle — a target handler: accepts a session channel, waits for the
// `soul apply` exec request, reads stdin to EOF, writes an NDJSON with one
// successful RunResult to stdout, and finishes with exit 0.
func targetHandle(s *liveSSHServer) func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	return func(t *testing.T, _ *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
		go ssh.DiscardRequests(reqs)
		for newCh := range chans {
			if newCh.ChannelType() != "session" {
				_ = newCh.Reject(ssh.UnknownChannelType, "session only")
				continue
			}
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer ch.Close()
				var execCmd string
				execReady := make(chan struct{}, 1)
				go func() {
					for req := range chReqs {
						switch req.Type {
						case "exec":
							execCmd = parseExecPayload(req.Payload)
							if req.WantReply {
								_ = req.Reply(true, nil)
							}
							execReady <- struct{}{}
						default:
							if req.WantReply {
								_ = req.Reply(false, nil)
							}
						}
					}
				}()
				select {
				case <-execReady:
				case <-time.After(5 * time.Second):
					return
				}
				if !strings.Contains(execCmd, "apply") {
					return
				}
				s.sawTargetExec.Store(true)
				// Read stdin to EOF (that's how the dispatcher closes stdin
				// after feeding in the ApplyRequest protojson).
				_, _ = io.Copy(io.Discard, ch)
				// Write RunResult to stdout and report exit 0.
				rrBytes, _ := protojson.Marshal(&keeperv1.RunResult{
					ApplyId: "live-pj",
					Status:  keeperv1.RunStatus_RUN_STATUS_SUCCESS,
				})
				_, _ = ch.Write(append(rrBytes, '\n'))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			}()
		}
	}
}

// directTCPIPPayload — the "direct-tcpip" channel payload (RFC 4254 §7.2).
type directTCPIPPayload struct {
	raddr string
	rport uint32
	laddr string
	lport uint32
}

func parseDirectTCPIP(b []byte) (directTCPIPPayload, error) {
	var p directTCPIPPayload
	pkt := struct {
		Raddr string
		Rport uint32
		Laddr string
		Lport uint32
	}{}
	if err := ssh.Unmarshal(b, &pkt); err != nil {
		return p, fmt.Errorf("direct-tcpip payload: %w", err)
	}
	p.raddr, p.rport, p.laddr, p.lport = pkt.Raddr, pkt.Rport, pkt.Laddr, pkt.Lport
	return p, nil
}

// parseExecPayload — an exec-type payload: an ssh-string with the command.
func parseExecPayload(b []byte) string {
	var pkt struct{ Command string }
	if err := ssh.Unmarshal(b, &pkt); err != nil {
		return ""
	}
	return pkt.Command
}

// bidirectionalCopy — pipes two streams (ssh-channel ↔ net.Conn) for the proxy.
func bidirectionalCopy(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// userAuthForLiveTests — Keeper's auth methods for live tests: an ed25519
// key + a cert from the user-CA. authn policy is off server-side
// (PublicKeyCallback allows all), but the client needs a valid signer to get
// through the handshake.
func userAuthForLiveTests(t *testing.T, userCASigner ssh.Signer) []ssh.AuthMethod {
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
		ValidPrincipals: []string{"soul"},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
	}
	if err := cert.SignCert(rand.Reader, userCASigner); err != nil {
		t.Fatalf("sign user cert: %v", err)
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		t.Fatalf("user cert signer: %v", err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(certSigner)}
}

// TestDial_ProxyJump_EndToEnd — brings up proxy + target SSH servers
// in-process, then Dial with a non-empty ProxyJump. Checks: traffic goes
// THROUGH the proxy (the proxy recorded direct-tcpip), the target recorded
// exec, sess.Run returns stdout that parses successfully into a RunResult.
func TestDial_ProxyJump_EndToEnd(t *testing.T) {
	caSigner, caPub := testCAKey(t)

	target := newLiveSSHServer(t, caSigner, "127.0.0.1", nil)
	target.handleConn = targetHandle(target)
	defer target.close()

	proxy := newLiveSSHServer(t, caSigner, "127.0.0.1", nil)
	proxy.handleConn = proxyHandle(proxy)
	defer proxy.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := Dial(ctx, DialConfig{
		Host:            target.host,
		Port:            target.port,
		User:            "soul",
		Auth:            userAuthForLiveTests(t, caSigner),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: caPub}},
		ProxyJump:       proxy.addr(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial via proxy_jump: %v", err)
	}
	defer sess.Close()

	stdout, runErr := sess.Run(ctx, "soul apply", []byte(`{"apply_id":"live-pj"}`))
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	rr, _, parseErr := func() (*keeperv1.RunResult, int, error) {
		rr, perr := ParseStream(strings.NewReader(stdout), nil)
		return rr, 0, perr
	}()
	if parseErr != nil {
		t.Fatalf("ParseStream: %v", parseErr)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", rr.GetStatus())
	}
	if !proxy.sawDirectTCPIP.Load() {
		t.Error("proxy did not record a direct-tcpip channel (dispatcher went straight, not via proxy)")
	}
	if !target.sawTargetExec.Load() {
		t.Error("target did not receive exec - the tunnel did not carry the command to it")
	}
}

// TestDial_ProxyJump_Empty_DirectFlowUnchanged — regression S0: with an
// empty ProxyJump, Dial goes directly to the target without a proxy up.
// Bring up only the target and confirm Dial succeeds (== direct-flow wasn't
// broken by the change).
func TestDial_ProxyJump_Empty_DirectFlowUnchanged(t *testing.T) {
	caSigner, caPub := testCAKey(t)

	target := newLiveSSHServer(t, caSigner, "127.0.0.1", nil)
	target.handleConn = targetHandle(target)
	defer target.close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := Dial(ctx, DialConfig{
		Host:            target.host,
		Port:            target.port,
		User:            "soul",
		Auth:            userAuthForLiveTests(t, caSigner),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: caPub}},
		ProxyJump:       "", // direct-flow
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial direct-flow: %v", err)
	}
	defer sess.Close()
	stdout, runErr := sess.Run(ctx, "soul apply", []byte(`{"apply_id":"live-direct"}`))
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if _, perr := ParseStream(strings.NewReader(stdout), nil); perr != nil {
		t.Fatalf("ParseStream: %v", perr)
	}
}

// TestDial_ProxyJump_ProxyUnavailable — proxy_jump is set, but the proxy is
// unreachable → fail-closed (an error about the proxy, must not reach the
// target).
func TestDial_ProxyJump_ProxyUnavailable(t *testing.T) {
	caSigner, caPub := testCAKey(t)

	// Bring up a listener and close it right away — the port becomes "refuses connection".
	deadL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := deadL.Addr().String()
	_ = deadL.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = Dial(ctx, DialConfig{
		Host:            "127.0.0.1",
		Port:            22, // doesn't matter — must not reach the target
		User:            "soul",
		Auth:            userAuthForLiveTests(t, caSigner),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: caPub}},
		ProxyJump:       deadAddr,
		Timeout:         1 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error: proxy unreachable - fail-closed")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Errorf("error not marked as proxy-fail: %v", err)
	}
}

// TestDial_ProxyJump_TargetUnavailable — the proxy is alive, the target is
// not. direct-tcpip reject → an error about direct-tcpip / target.
func TestDial_ProxyJump_TargetUnavailable(t *testing.T) {
	caSigner, caPub := testCAKey(t)

	proxy := newLiveSSHServer(t, caSigner, "127.0.0.1", nil)
	proxy.handleConn = proxyHandle(proxy)
	defer proxy.close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Target address → a closed listener (the port refuses).
	deadL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadHost, deadPortStr, _ := net.SplitHostPort(deadL.Addr().String())
	deadPort, _ := strconv.Atoi(deadPortStr)
	_ = deadL.Close()

	_, err = Dial(ctx, DialConfig{
		Host:            deadHost,
		Port:            deadPort,
		User:            "soul",
		Auth:            userAuthForLiveTests(t, caSigner),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: caPub}},
		ProxyJump:       proxy.addr(),
		Timeout:         1 * time.Second,
	})
	if err == nil {
		t.Fatal("expected an error: target unreachable via proxy - fail-closed")
	}
	if !strings.Contains(err.Error(), "direct-tcpip") && !strings.Contains(err.Error(), "target") {
		t.Errorf("error not marked as target-fail via proxy: %v", err)
	}
}

// TestParseProxyJump — table-driven.
func TestParseProxyJump(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		defUser    string
		wantUser   string
		wantAddr   string
		expectFail bool
	}{
		{"host:port without user -> default", "proxy.example.com:3023", "soul", "soul", "proxy.example.com:3023", false},
		{"user@host:port", "jumper@proxy.example.com:3023", "soul", "jumper", "proxy.example.com:3023", false},
		{"IPv4 host:port", "127.0.0.1:2222", "soul", "soul", "127.0.0.1:2222", false},
		{"IPv6 host:port", "[::1]:2222", "soul", "soul", "[::1]:2222", false},
		{"empty", "", "soul", "", "", true},
		{"host only, no port", "proxy.example.com", "soul", "", "", true},
		{"@ without user", "@host:22", "soul", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user, addr, err := parseProxyJump(c.raw, c.defUser)
			if c.expectFail {
				if err == nil {
					t.Errorf("expected an error for %q", c.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if user != c.wantUser || addr != c.wantAddr {
				t.Errorf("got (%q, %q), want (%q, %q)", user, addr, c.wantUser, c.wantAddr)
			}
		})
	}
}
