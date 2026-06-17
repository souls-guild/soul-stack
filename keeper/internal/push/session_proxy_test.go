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

// liveSSHServer — общий каркас in-process SSH-сервера для proxy_jump-тестов.
// Поднимается на 127.0.0.1:0 (порт ОС-выдаётся), host-key подписан переданным
// host-CA, авторизация — любой публичный ключ принимается (тесты не проверяют
// authn-policy, эта зона — session.Dial и direct-tcpip).
type liveSSHServer struct {
	listener   net.Listener
	host       string
	port       int
	srvConfig  *ssh.ServerConfig
	handleConn func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request)
	wg         sync.WaitGroup
	stopCh     chan struct{}

	// telemetry — флаги, которые тесты читают для assert-ов.
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

// newLiveSSHServer запускает SSH-сервер; host-cert выпускается на принципала
// principal (тесты подставляют "127.0.0.1", чтобы CertChecker по умолчанию его
// принял для соединения с этим IP).
func newLiveSSHServer(
	t *testing.T,
	caSigner ssh.Signer,
	principal string,
	handle func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request),
) *liveSSHServer {
	t.Helper()

	// Host-key для сервера + host-cert от CA.
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
			// Тесты не проверяют user-cert на server-side; разрешаем всё, чтобы
			// сосредоточиться на проверке dispatcher proxy_jump-логики.
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

// proxyHandle — обработчик SSH-сервера, играющего роль bastion: принимает
// direct-tcpip-каналы и пробрасывает их до фактического target-адреса (берётся
// из payload канала).
func proxyHandle(s *liveSSHServer) func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	return func(t *testing.T, _ *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
		go ssh.DiscardRequests(reqs)
		for newCh := range chans {
			if newCh.ChannelType() != "direct-tcpip" {
				_ = newCh.Reject(ssh.UnknownChannelType, "только direct-tcpip")
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

// targetHandle — обработчик target: принимает session-channel, ждёт exec-запрос
// `soul apply`, читает stdin до EOF, пишет в stdout NDJSON с одним успешным
// RunResult и завершает с exit 0.
func targetHandle(s *liveSSHServer) func(t *testing.T, sc *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	return func(t *testing.T, _ *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
		go ssh.DiscardRequests(reqs)
		for newCh := range chans {
			if newCh.ChannelType() != "session" {
				_ = newCh.Reject(ssh.UnknownChannelType, "только session")
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
				// Читаем stdin до EOF (так dispatcher закрывает stdin после
				// подачи ApplyRequest protojson).
				_, _ = io.Copy(io.Discard, ch)
				// Пишем RunResult в stdout и сообщаем exit 0.
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

// directTCPIPPayload — payload канала "direct-tcpip" (RFC 4254 §7.2).
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

// parseExecPayload — payload типа exec: ssh-string с командой.
func parseExecPayload(b []byte) string {
	var pkt struct{ Command string }
	if err := ssh.Unmarshal(b, &pkt); err != nil {
		return ""
	}
	return pkt.Command
}

// bidirectionalCopy — pipe двух потоков (ssh-channel ↔ net.Conn) для proxy.
func bidirectionalCopy(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// userAuthForLiveTests — auth-методы Keeper-а для live-тестов: ed25519-ключ + cert
// от user-CA. На server-side authn-policy выключена (PublicKeyCallback allows
// all), но клиенту нужен валидный signer для прохождения handshake.
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

// TestDial_ProxyJump_EndToEnd — поднимает proxy + target SSH-серверы in-process,
// затем Dial с непустым ProxyJump. Проверяет: трафик идёт ЧЕРЕЗ proxy (proxy
// зафиксировал direct-tcpip), target зафиксировал exec, sess.Run возвращает
// stdout, который успешно парсится в RunResult.
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
		t.Fatalf("Dial через proxy_jump: %v", err)
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
		t.Error("proxy не зафиксировал direct-tcpip-канал (dispatcher шёл напрямую, не через proxy)")
	}
	if !target.sawTargetExec.Load() {
		t.Error("target не получил exec — туннель не довёл до него команду")
	}
}

// TestDial_ProxyJump_Empty_DirectFlowUnchanged — regression S0: при пустом
// ProxyJump Dial идёт напрямую к target без поднятого proxy. Поднимаем только
// target и убеждаемся, что Dial успешен (== direct-flow не сломан правкой).
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

// TestDial_ProxyJump_ProxyUnavailable — proxy_jump указан, но proxy недоступен →
// fail-closed (ошибка про proxy, до target дойти не должно).
func TestDial_ProxyJump_ProxyUnavailable(t *testing.T) {
	caSigner, caPub := testCAKey(t)

	// Поднимаем listener и сразу закрываем — порт станет «отказывает в коннекте».
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
		Port:            22, // не важно — до target дойти не должны
		User:            "soul",
		Auth:            userAuthForLiveTests(t, caSigner),
		HostAuthorities: []NamedHostKeyAuthority{{Name: "test-ca", CAPubKey: caPub}},
		ProxyJump:       deadAddr,
		Timeout:         1 * time.Second,
	})
	if err == nil {
		t.Fatal("ждали ошибку: proxy недоступен — fail-closed")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Errorf("ошибка не помечена как proxy-fail: %v", err)
	}
}

// TestDial_ProxyJump_TargetUnavailable — proxy жив, target — нет. direct-tcpip
// reject → ошибка про direct-tcpip / target.
func TestDial_ProxyJump_TargetUnavailable(t *testing.T) {
	caSigner, caPub := testCAKey(t)

	proxy := newLiveSSHServer(t, caSigner, "127.0.0.1", nil)
	proxy.handleConn = proxyHandle(proxy)
	defer proxy.close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Адрес target → закрытый listener (port отказывает).
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
		t.Fatal("ждали ошибку: target недоступен через proxy — fail-closed")
	}
	if !strings.Contains(err.Error(), "direct-tcpip") && !strings.Contains(err.Error(), "target") {
		t.Errorf("ошибка не помечена как target-fail через proxy: %v", err)
	}
}

// TestParseProxyJump — таблица.
func TestParseProxyJump(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		defUser    string
		wantUser   string
		wantAddr   string
		expectFail bool
	}{
		{"host:port без user → default", "proxy.example.com:3023", "soul", "soul", "proxy.example.com:3023", false},
		{"user@host:port", "jumper@proxy.example.com:3023", "soul", "jumper", "proxy.example.com:3023", false},
		{"IPv4 host:port", "127.0.0.1:2222", "soul", "soul", "127.0.0.1:2222", false},
		{"IPv6 host:port", "[::1]:2222", "soul", "soul", "[::1]:2222", false},
		{"пустой", "", "soul", "", "", true},
		{"только host без port", "proxy.example.com", "soul", "", "", true},
		{"@-без user", "@host:22", "soul", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user, addr, err := parseProxyJump(c.raw, c.defUser)
			if c.expectFail {
				if err == nil {
					t.Errorf("ждали ошибку для %q", c.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if user != c.wantUser || addr != c.wantAddr {
				t.Errorf("got (%q, %q), want (%q, %q)", user, addr, c.wantUser, c.wantAddr)
			}
		})
	}
}
