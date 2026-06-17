// Package handshake — общий helper SDK Soul Stack для авторов плагинов.
//
// Реализует gRPC-stdio handshake-протокол всех трёх kind-ов плагинов
// (soul_module / cloud_driver / ssh_provider) по спецификации
// docs/keeper/plugins.md → Handshake / Lifecycle и ADR-020.
//
// Контракт SDK с host-ом:
//
//   - Host передаёт путь к Unix-socket через env-var SOUL_PLUGIN_SOCKET
//     (ADR-020(d), naming-rules.md → SOUL_PLUGIN_SOCKET).
//   - Плагин слушает gRPC на этом сокете.
//   - Плагин пишет в stdout ровно одну строку с JSON-payload поля
//     Handshake (`soul_stack:"plugin-v1"`) и переводом строки.
//   - При получении SIGTERM/SIGINT плагин завершает in-flight RPC
//     в течение ShutdownGrace, по истечении — жёсткий Stop().
package handshake

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

// SocketEnv — имя env-var, через который host передаёт плагину путь к
// Unix-socket. Значение фиксировано в naming-rules.md и ADR-020(d).
const SocketEnv = "SOUL_PLUGIN_SOCKET"

// Magic — значение поля Handshake.SoulStack, маркер формата handshake-строки v1.
// Меняется только при breaking-смене самого формата (отдельный ADR).
const Magic = "plugin-v1"

// NetworkUnix — единственное значение Handshake.Network в MVP
// (docs/keeper/plugins.md → Handshake). Расширение named_pipe/tcp — post-MVP.
const NetworkUnix = "unix"

// defaultShutdownGrace дублирует plugin_runtime.shutdown_grace (ADR-020(d))
// для случая, когда плагин-автор не задал явное значение в Config.
// Host со своей стороны держит свой таймер на тот же интервал.
const defaultShutdownGrace = 10 * time.Second

// Config — параметры handshake-сессии.
//
// Address и ServerCert опциональны: Address по умолчанию читается из
// env-var SocketEnv, ServerCert в MVP всегда пустой (mTLS — post-MVP,
// ADR-020(h)).
type Config struct {
	// ProtocolVersion — версия plugin-протокола; должна совпадать с
	// manifest.protocol_version. В MVP единственное допустимое значение — 1.
	ProtocolVersion int32

	// Kind — kind плагина. Должен совпадать с manifest.kind, иначе host
	// отвергает плагин (ADR-020(c)).
	Kind pluginv1.Kind

	// Address — путь к Unix-socket; если пуст, берётся из env-var SocketEnv.
	Address string

	// ShutdownGrace — окно от SIGTERM до жёсткого Stop(). По умолчанию 10s
	// (ADR-020(d) — симметрично host-side таймеру).
	ShutdownGrace time.Duration
}

// Register — функция, регистрирующая service-implementation на сервере.
// Вызывается ровно один раз внутри Serve до записи handshake-строки.
type Register func(*grpc.Server)

// Serve — типовой main() плагина.
//
// Жизненный цикл:
//
//  1. читает Address (явный или из env);
//  2. слушает Unix-socket;
//  3. создаёт grpc.Server, передаёт его register-callback;
//  4. пишет JSON-handshake в stdout;
//  5. подписывается на SIGTERM/SIGINT/SIGHUP, по сигналу — GracefulStop
//     с тайм-аутом ShutdownGrace, по истечении — жёсткий Stop;
//  6. блокирует на grpc.Server.Serve до завершения.
//
// Возвращает первую ошибку из listen/serve/marshal.
func Serve(cfg Config, register Register) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	return serveWithStop(cfg, register, os.Stdout, sigCh)
}

// serveWithStop — testable-форма Serve: канал остановки и destination для
// handshake-строки внедряются параметрами. Внешний Serve тривиально
// проксирует через os.Stdout + signal.Notify.
func serveWithStop(cfg Config, register Register, stdout io.Writer, stop <-chan os.Signal) error {
	if register == nil {
		return errors.New("handshake: nil register callback")
	}
	if cfg.ProtocolVersion == 0 {
		return errors.New("handshake: ProtocolVersion is required")
	}
	if cfg.Kind == pluginv1.Kind_KIND_UNSPECIFIED {
		return errors.New("handshake: Kind is required")
	}

	addr := cfg.Address
	if addr == "" {
		addr = os.Getenv(SocketEnv)
	}
	if addr == "" {
		return fmt.Errorf("handshake: socket address not provided (env %s empty)", SocketEnv)
	}

	grace := cfg.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}

	rawListener, err := net.Listen("unix", addr)
	if err != nil {
		return fmt.Errorf("handshake: listen unix %q: %w", addr, err)
	}
	listener := newTrackingListener(rawListener)

	server := grpc.NewServer()
	register(server)

	line, err := marshalHandshake(cfg, addr)
	if err != nil {
		_ = listener.Close()
		return err
	}
	if _, err := fmt.Fprintln(stdout, line); err != nil {
		_ = listener.Close()
		return fmt.Errorf("handshake: write stdout: %w", err)
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-stop
		done := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(grace):
			// GracefulStop держит in-flight raw-коннекты, которые могут
			// застрять в HTTP/2 handshake-state и не отпускать сервер
			// даже после Stop(). Принудительно закрываем все accepted
			// коннекты, потом вызываем Stop — accept-loop возвращается,
			// GracefulStop разблокируется.
			listener.closeAllConns()
			server.Stop()
			<-done
		}
	}()

	serveErr := server.Serve(listener)
	<-stopped
	if serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return fmt.Errorf("handshake: grpc serve: %w", serveErr)
	}
	return nil
}

// trackingListener — net.Listener, который запоминает все принятые
// коннекты и умеет закрыть их разом. Нужен для hard-stop сценария:
// grpc.Server.Stop сам по себе не закрывает raw-коннект, застрявший
// в HTTP/2 handshake (см. shutdown-логику в Serve).
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newTrackingListener(l net.Listener) *trackingListener {
	return &trackingListener{Listener: l, conns: make(map[net.Conn]struct{})}
}

func (t *trackingListener) Accept() (net.Conn, error) {
	c, err := t.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c, parent: t}
	t.mu.Lock()
	t.conns[tc] = struct{}{}
	t.mu.Unlock()
	return tc, nil
}

func (t *trackingListener) closeAllConns() {
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.conns = make(map[net.Conn]struct{})
	t.mu.Unlock()
	// Close без удержания mu — trackedConn.Close возьмёт mu сам.
	for _, c := range conns {
		_ = c.Close()
	}
}

type trackedConn struct {
	net.Conn
	parent *trackingListener
}

func (c *trackedConn) Close() error {
	c.parent.mu.Lock()
	delete(c.parent.conns, c)
	c.parent.mu.Unlock()
	return c.Conn.Close()
}

func marshalHandshake(cfg Config, addr string) (string, error) {
	hs := &pluginv1.Handshake{
		SoulStack:       Magic,
		ProtocolVersion: cfg.ProtocolVersion,
		Kind:            cfg.Kind,
		Network:         NetworkUnix,
		Address:         addr,
		ServerCert:      "",
	}
	// protojson по умолчанию даёт многострочный pretty-output; плагин обязан
	// писать ровно одну строку (ADR-020(b)).
	m := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: true,
	}
	raw, err := m.Marshal(hs)
	if err != nil {
		return "", fmt.Errorf("handshake: marshal: %w", err)
	}
	return string(raw), nil
}
