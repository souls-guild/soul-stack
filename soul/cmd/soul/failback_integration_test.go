//go:build integration

// Failback integration-test (Soul-side) — два priority-endpoint-а, проверка
// proactive-возврата на higher-priority endpoint и graceful-swap живой сессии
// без message-loss.
//
// Setup:
//   - два mock EventStream-сервера на разных портах (server-A priority=1,
//     server-B priority=2), оба под mTLS с общим CA;
//   - server-A initially down (listener закрыт), server-B up;
//   - reconnectLoop в горутине теста с failback.interval=200ms.
//
// Фазы (по ТЗ Soul-runtime-extras / failback integration):
//  1. Initial connect → server-B (A unreachable).
//  2. server-A goes up → failback swap → активная сессия на server-A,
//     старая на B закрыта gracefully.
//
// Phase 3 (A down again → fallback back to B) — nice-to-have по ТЗ — здесь
// сознательно опущена: gRPC-client при abrupt-close TLS-conn-а зависает в
// Recv() на десятки секунд (keepalive Time=30s / Timeout=10s в client.go);
// раскручивать это в integration-тесте — раздувание scope. Reconnect-fallback
// и так покрыт unit-тестами `TestClientDial_FallsBackToNextEndpoint` /
// `TestDialPriority_PicksLowerOnly`.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/soul/internal/coremod"
	soulgrpc "github.com/souls-guild/soul-stack/soul/internal/grpc"
	"github.com/souls-guild/soul-stack/soul/internal/runtime"
)

// TestReconnect_LeaseHeld_BackoffNotReset — стрим отвергнут AlreadyExists на
// handshake (SID-lease ещё держит живой holder после краха keeper-а). Soul НЕ
// долбит выжившего keeper-а на rate ~1/initial: backoff растёт (модест-cap), а не
// сбрасывается. Доказательство: за окно T число handshake-попыток ограничено
// растущей задержкой, а не initial-rate. Когда lease снимается (force-release
// после presence) — Soul переподключается в пределах нескольких секунд (cap),
// recovery-latency сохранена.
func TestReconnect_LeaseHeld_BackoffNotReset(t *testing.T) {
	ca, caKey := mustGenCA(t)
	clientCertDER, clientKey := mustGenLeaf(t, ca, caKey, "soul-host.example", false)
	dir := t.TempDir()
	caPath := writePEMBlock(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	clientCertPath := writePEMBlock(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	clientKeyPath := writeRSAPriv(t, dir, "client.key", clientKey)

	srvTLS := serverTLS(t, ca, caKey)
	srv := startMockKeeper(t, srvTLS, "L")
	defer srv.stop()
	srv.rejectLease.Store(true) // lease занят живым holder-ом

	endpoints := []soulgrpc.Endpoint{{Addr: srv.addr, Priority: 1}}
	logger := testLogger(t)
	cli, err := soulgrpc.NewClient(soulgrpc.ClientConfig{
		Endpoints:        endpoints,
		SeedCert:         clientCertPath,
		SeedKey:          clientKeyPath,
		CAPath:           caPath,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "soul-host.example",
		SoulVersion:      "0.0.0-test",
	}, logger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// transport-max намеренно МАЛ (20ms): если бы lease-held шёл по обычной
	// transport-семантике с reset-к-initial, за окно набралось бы ~окно/20ms
	// попыток. Lease-held cap (leaseHeldBackoffCap, секунды) и отсутствие reset
	// держат число попыток в единицах. initial=20ms/no-jitter — детерминируем.
	store := backoffOnlyStore(t, srv.addr, "20ms", "20ms")

	runner := runtime.NewApplyRunner(coremod.Default(), nil)
	sp := newTestPusher("soul-host.example")

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		reconnectLoop(ctx, store, cli, runner, nil, sp, nil, nil, nil, nil, logger)
	}()
	defer func() {
		cancel()
		<-loopDone
	}()

	// Окно наблюдения 2s. При reset-к-initial(20ms) было бы ~100 попыток. При
	// lease-held прогрессии (20→40→80→160→320→640→1280→2560→cap=3000) — ≤ ~8.
	// Порог 25 — щедрый запас над растущей прогрессией, но далеко ниже initial-rate.
	if !waitFor(func() bool { return srv.helloCount() >= 1 }, 2*time.Second) {
		t.Fatal("no handshake attempt observed under lease-held")
	}
	time.Sleep(2 * time.Second)
	attempts := srv.helloCount()
	if attempts > 25 {
		t.Fatalf("lease-held: %d handshake attempts in ~2s — backoff appears reset to initial (~1/initial spam)", attempts)
	}
	t.Logf("lease-held handshake attempts in ~2s window: %d (backoff applied, not reset)", attempts)

	// === recovery-latency: lease снят (force-release) → переподключение за секунды ===
	srv.rejectLease.Store(false)
	// cap=leaseHeldBackoffCap (3s) + handshake/запас. Должно успеть.
	if !waitFor(func() bool { return srv.activeStreams() >= 1 }, leaseHeldBackoffCap+3*time.Second) {
		t.Fatalf("recovery: did not reconnect within %s after lease released", leaseHeldBackoffCap+3*time.Second)
	}
}

// TestReconnect_LeaseHeld_SpraysToOtherEndpoint — AlreadyExists на priority=1
// endpoint НЕ заклинивает spray/failover: Dial доходит до живого priority=2
// endpoint-а той же сессией, не дожидаясь снятия lease на первом. Страхует
// легитимный fallback по fallback-list при lease-held на одном keeper-е.
func TestReconnect_LeaseHeld_SpraysToOtherEndpoint(t *testing.T) {
	ca, caKey := mustGenCA(t)
	clientCertDER, clientKey := mustGenLeaf(t, ca, caKey, "soul-host.example", false)
	dir := t.TempDir()
	caPath := writePEMBlock(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	clientCertPath := writePEMBlock(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	clientKeyPath := writeRSAPriv(t, dir, "client.key", clientKey)

	srvTLS := serverTLS(t, ca, caKey)
	// priority=1 — lease-held; priority=2 — живой.
	leaseHeld := startMockKeeper(t, srvTLS, "P1")
	defer leaseHeld.stop()
	leaseHeld.rejectLease.Store(true)
	alive := startMockKeeper(t, srvTLS, "P2")
	defer alive.stop()

	endpoints := []soulgrpc.Endpoint{
		{Addr: leaseHeld.addr, Priority: 1},
		{Addr: alive.addr, Priority: 2},
	}
	logger := testLogger(t)
	cli, err := soulgrpc.NewClient(soulgrpc.ClientConfig{
		Endpoints:        endpoints,
		SeedCert:         clientCertPath,
		SeedKey:          clientKeyPath,
		CAPath:           caPath,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "soul-host.example",
		SoulVersion:      "0.0.0-test",
	}, logger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// failback disabled тут не нужен — alive это priority=2, failback потащил бы
	// на priority=1 (lease-held), что усложнит наблюдение. interval большой.
	store := backoffOnlyStore(t, leaseHeld.addr, "20ms", "200ms")

	runner := runtime.NewApplyRunner(coremod.Default(), nil)
	sp := newTestPusher("soul-host.example")
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		reconnectLoop(ctx, store, cli, runner, nil, sp, nil, nil, nil, nil, logger)
	}()
	defer func() {
		cancel()
		<-loopDone
	}()

	// Несмотря на lease-held priority=1, сессия поднимается на живом priority=2.
	if !waitFor(func() bool { return alive.activeStreams() >= 1 }, 3*time.Second) {
		t.Fatalf("spray: did not connect to alive priority=2 endpoint; P1-hello=%d P2-active=%d",
			leaseHeld.helloCount(), alive.activeStreams())
	}
}

// testLogger — discard по умолчанию, debug на stderr при -v.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// backoffOnlyStore — Store[SoulConfig] с одним endpoint-ом и заданными
// backoff.initial/max (no-jitter), failback выключен. Для lease-held тестов, где
// важна именно reconnect-backoff-семантика, а не failback. primaryAddr —
// единственный endpoint в конфиге; реальный дайлинг идёт через отдельный Client
// (его endpoints могут включать больше адресов — конфиг лишь источник backoff).
func backoffOnlyStore(t *testing.T, primaryAddr, initial, max string) *config.Store[config.SoulConfig] {
	t.Helper()
	host, port := splitHostPort(t, primaryAddr)
	yml := fmt.Sprintf(`keeper:
  endpoints:
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
      priority: 1
  retry:
    backoff:
      initial: %s
      max: %s
      jitter: false
  failback:
    enabled: false
`, host, port, port, initial, max)
	path := filepath.Join(t.TempDir(), "soul.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatalf("write soul.yml: %v", err)
	}
	store, diags, err := config.LoadSoulStore(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulStore: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("soul.yml has error diag: %s [%s] %s", d.Phase, d.Code, d.Message)
		}
	}
	return store
}

// TestFailbackIntegration_TwoEndpoints — happy-path конвейера failback:
// fallback на priority=2 при недоступном priority=1, потом proactive-swap
// обратно на priority=1 (zero-downtime: новая сессия открыта до закрытия
// старой).
func TestFailbackIntegration_TwoEndpoints(t *testing.T) {
	ca, caKey := mustGenCA(t)
	clientCertDER, clientKey := mustGenLeaf(t, ca, caKey, "soul-host.example", false)
	dir := t.TempDir()
	caPath := writePEMBlock(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	clientCertPath := writePEMBlock(t, dir, "client.crt", "CERTIFICATE", clientCertDER)
	clientKeyPath := writeRSAPriv(t, dir, "client.key", clientKey)

	srvTLS := serverTLS(t, ca, caKey)

	// server-B (priority=2) — up на весь тест.
	srvB := startMockKeeper(t, srvTLS, "B")
	defer srvB.stop()

	// server-A (priority=1) — занимаем порт, чтобы клиент в фазе 1 получил
	// connection-refused, а в фазе 2 поднимаем mock-сервер на том же addr,
	// без правки endpoints в Client после старта reconnectLoop.
	srvAAddr := acquirePort(t)

	endpoints := []soulgrpc.Endpoint{
		{Addr: srvAAddr, Priority: 1},
		{Addr: srvB.addr, Priority: 2},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	cli, err := soulgrpc.NewClient(soulgrpc.ClientConfig{
		Endpoints:        endpoints,
		SeedCert:         clientCertPath,
		SeedKey:          clientKeyPath,
		CAPath:           caPath,
		HandshakeTimeout: 500 * time.Millisecond,
		SID:              "soul-host.example",
		SoulVersion:      "0.0.0-test",
	}, logger)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	runner := runtime.NewApplyRunner(coremod.Default(), nil)

	// backoff/failback больше не передаются параметрами — reconnectLoop читает
	// их из store на каждой итерации (hot-reload, ADR-021). Собираем store со
	// снимком, дающим прежние тест-значения: backoff initial=50ms/max=200ms без
	// jitter, failback interval=200ms (spray=0 — детерминированный swap).
	store := failbackTestStore(t, srvAAddr, srvB.addr)

	// soulprintPusher — обязательный аргумент сигнатуры; Pusher реальный
	// (handleSession шлёт initial-report при установке сессии). Остальные deps —
	// nil: тест проверяет только failback-swap сессий и не доходит до Apply/
	// Errand/beacon, где они востребованы:
	//   - errandRunner — nil: Errand-команды в тесте не приходят;
	//   - metrics — nil (nil-safe, см. EventStreamMetrics.*);
	//   - sigils/anchors — nil: verify Sigil нужен только на Apply custom-плагина;
	//   - scheduler — nil: VigilSnapshot в тесте не приходит.
	sp := newTestPusher("soul-host.example")

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		// Сигнатура: reconnectLoop(ctx, store, client, runner, errandRunner,
		// sp, metrics, sigils, anchors, scheduler, logger).
		reconnectLoop(ctx, store, cli, runner, nil, sp, nil, nil, nil, nil, logger)
	}()
	defer func() {
		cancel()
		<-loopDone
	}()

	// === Phase 1: initial connect — A unreachable, fallback to B ===
	if !waitFor(func() bool { return srvB.activeStreams() >= 1 }, 3*time.Second) {
		t.Fatalf("phase 1: server-B did not get connection; A=%d B=%d", 0, srvB.activeStreams())
	}
	// В этот момент A должен оставаться неподключенным.
	if got := srvB.activeStreams(); got != 1 {
		t.Errorf("phase 1: server-B active=%d, want 1", got)
	}

	// === Phase 2: A goes up → failback swap ===
	srvA := startMockKeeperOnAddr(t, srvTLS, "A", srvAAddr)
	defer srvA.stop()

	// failback.interval=200ms; даём 6× запас (race-сборка медленнее).
	if !waitFor(func() bool { return srvA.activeStreams() >= 1 }, 3*time.Second) {
		t.Fatalf("phase 2: server-A did not pick up failback swap; A=%d B=%d",
			srvA.activeStreams(), srvB.activeStreams())
	}
	// После swap-а B должен закрыться gracefully — handleSession делает
	// _ = oldSess.Close() сразу после получения новой сессии.
	if !waitFor(func() bool { return srvB.activeStreams() == 0 }, 2*time.Second) {
		t.Fatalf("phase 2: server-B not gracefully closed; B active=%d", srvB.activeStreams())
	}
	if got := srvA.activeStreams(); got != 1 {
		t.Errorf("phase 2: server-A active=%d, want 1", got)
	}

	// Phase 3 (A down → fallback to B) — сознательно опущена (см. doc-comment
	// в начале файла). Coverage реконнект-fallback-а закрыт unit-тестами
	// soul/internal/grpc/client_test.go.
}

// --- mock EventStream-сервер (mTLS) ---

type mockKeeper struct {
	keeperv1.UnimplementedKeeperServer
	label string
	addr  string

	active        int64       // atomic — счётчик in-flight EventStream-handler-ов
	helloAttempts int64       // atomic — счётчик принятых Hello (handshake-попыток)
	rejectLease   atomic.Bool // когда true — отвергать handshake с AlreadyExists (lease-held)

	srv *grpc.Server
	ln  *trackingListener
	wg  sync.WaitGroup

	stopOnce sync.Once
}

func (m *mockKeeper) EventStream(stream grpc.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper]) error {
	atomic.AddInt64(&m.active, 1)
	defer atomic.AddInt64(&m.active, -1)

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return errors.New("expected Hello")
	}
	atomic.AddInt64(&m.helloAttempts, 1)
	// lease-held: отвергаем ДО HelloReply (как keeper при занятом SID-lease —
	// acquireSoulLease отдаёт AlreadyExists до handshake-reply).
	if m.rejectLease.Load() {
		return status.Errorf(codes.AlreadyExists, "soul lease held by another keeper for sid=%q", hello.GetSidEcho())
	}
	if err := stream.Send(&keeperv1.FromKeeper{Payload: &keeperv1.FromKeeper_HelloReply{HelloReply: &keeperv1.HelloReply{
		SessionId:  "sess-" + m.label,
		Kid:        "kid-" + m.label,
		ServerTime: timestamppb.Now(),
	}}}); err != nil {
		return err
	}
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return nil
		}
	}
}

func (m *mockKeeper) helloCount() int64 { return atomic.LoadInt64(&m.helloAttempts) }

func (m *mockKeeper) activeStreams() int64 { return atomic.LoadInt64(&m.active) }

func (m *mockKeeper) stop() {
	m.stopOnce.Do(func() {
		// Сначала рвём live TCP-конны через trackingListener — это
		// гарантирует, что Recv() на клиенте сразу получит error
		// (grpc.Server.Stop() в Go-runtime закрывает listener, но
		// не всегда мгновенно бьёт уже установленные streams; для
		// быстрого phase 3-reconnect нам нужен именно жёсткий разрыв).
		n := m.ln.killAll()
		_ = n // для отладки можно залогировать m.label, n
		m.srv.Stop()
		m.wg.Wait()
	})
}

// startMockKeeper поднимает mock-сервер на 127.0.0.1:0 (kernel выдаст порт).
func startMockKeeper(t *testing.T, tlsCfg *tls.Config, label string) *mockKeeper {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return runMockKeeper(t, ln, tlsCfg, label)
}

// startMockKeeperOnAddr поднимает сервер на конкретном адресе (используется
// для перезапуска server-A на том же порту, чтобы не править endpoints
// в Client после старта reconnectLoop).
func startMockKeeperOnAddr(t *testing.T, tlsCfg *tls.Config, label, addr string) *mockKeeper {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	return runMockKeeper(t, ln, tlsCfg, label)
}

func runMockKeeper(t *testing.T, ln net.Listener, tlsCfg *tls.Config, label string) *mockKeeper {
	t.Helper()
	tracked := newTrackingListener(ln)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	mk := &mockKeeper{
		label: label,
		addr:  ln.Addr().String(),
		srv:   srv,
		ln:    tracked,
	}
	keeperv1.RegisterKeeperServer(srv, mk)
	mk.wg.Add(1)
	go func() {
		defer mk.wg.Done()
		_ = srv.Serve(tracked)
	}()
	return mk
}

// trackingListener — net.Listener, держащий ссылки на все Accepted-net.Conn,
// чтобы при stop() их можно было принудительно закрыть. Без этого
// grpc.Server.Stop() в некоторых ситуациях оставляет уже-открытые streams
// без явного RST/FIN, и клиенту приходится ждать keepalive-timeout (десятки
// секунд) до error из Recv().
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newTrackingListener(ln net.Listener) *trackingListener {
	return &trackingListener{Listener: ln}
}

func (t *trackingListener) Accept() (net.Conn, error) {
	c, err := t.Listener.Accept()
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.conns = append(t.conns, c)
	t.mu.Unlock()
	return c, nil
}

func (t *trackingListener) killAll() int {
	t.mu.Lock()
	conns := t.conns
	t.conns = nil
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}

// acquirePort — взять свободный порт у kernel-а и сразу его освободить.
// Между Close-ом и повторным bind-ом окно race; в localhost-тесте этого
// окна обычно достаточно (ядро держит порт в FREE-state >5s по умолчанию
// после явного Close listener-а без активных соединений).
func acquirePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("acquirePort listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// serverTLS — серверная TLS-конфигурация: server-cert на 127.0.0.1, mTLS
// требует клиентский cert от того же CA.
func serverTLS(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey) *tls.Config {
	t.Helper()
	serverCertDER, serverKey := mustGenLeaf(t, ca, caKey, "127.0.0.1", true)
	tlsCert := tls.Certificate{Certificate: [][]byte{serverCertDER}, PrivateKey: serverKey}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}
}

// --- self-signed certs (в-памяти) ---

func mustGenCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("CreateCertificate(ca): %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert, key
}

func mustGenLeaf(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, host string, isServer bool) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, key.Public(), caKey)
	if err != nil {
		t.Fatalf("CreateCertificate(%s): %v", host, err)
	}
	return der, key
}

func writePEMBlock(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func writeRSAPriv(t *testing.T, dir, name string, key *rsa.PrivateKey) string {
	t.Helper()
	path := filepath.Join(dir, name)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func waitFor(pred func() bool, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pred()
}

// failbackTestStore собирает Store[SoulConfig] со снимком, из которого
// reconnectLoop/handleSession резолвят backoff и failback (hot-reload-источник,
// ADR-021): initial=50ms/max=200ms без jitter, failback interval=200ms, spray=0.
// endpoints в конфиге нужны только для прохождения schema-валидации
// (keeper.endpoints[] обязателен) — реальный дайлинг идёт через soulgrpc.Client,
// сконструированный отдельно. Паттерн повторяет soulFixtureStore из
// hotreload_test.go: YAML → временный файл → LoadSoulStore.
func failbackTestStore(t *testing.T, addrA, addrB string) *config.Store[config.SoulConfig] {
	t.Helper()
	hostA, portA := splitHostPort(t, addrA)
	hostB, portB := splitHostPort(t, addrB)
	yml := fmt.Sprintf(`keeper:
  endpoints:
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
      priority: 1
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
      priority: 2
  retry:
    backoff:
      initial: 50ms
      max: 200ms
      jitter: false
  failback:
    enabled: true
    interval: 200ms
    spray: 0s
`, hostA, portA, portA, hostB, portB, portB)

	path := filepath.Join(t.TempDir(), "soul.yml")
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatalf("write soul.yml: %v", err)
	}
	store, diags, err := config.LoadSoulStore(path, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulStore: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("soul.yml has error diag: %s [%s] %s", d.Phase, d.Code, d.Message)
		}
	}
	return store
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}
