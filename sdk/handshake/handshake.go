// Package handshake is a shared Soul Stack SDK helper for plugin authors.
//
// It implements the gRPC-stdio handshake protocol shared by all three
// plugin kinds (soul_module / cloud_driver / ssh_provider) per the
// docs/keeper/plugins.md → Handshake / Lifecycle spec and ADR-020.
//
// SDK-to-host contract:
//
//   - The host passes the Unix-socket path via the SOUL_PLUGIN_SOCKET env
//     var (ADR-020(d), naming-rules.md → SOUL_PLUGIN_SOCKET).
//   - The plugin listens for gRPC on that socket.
//   - The plugin writes exactly one line to stdout with the JSON payload of
//     the Handshake message (`soul_stack:"plugin-v1"`) followed by a newline.
//   - On SIGTERM/SIGINT, the plugin finishes in-flight RPCs within
//     ShutdownGrace, then does a hard Stop() once that elapses.
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

// SocketEnv is the name of the env var through which the host passes the
// plugin its Unix-socket path. The value is fixed in naming-rules.md and
// ADR-020(d).
const SocketEnv = "SOUL_PLUGIN_SOCKET"

// Magic is the value of the Handshake.SoulStack field, the format marker for
// handshake-line v1. Changes only on a breaking change to the format itself
// (a separate ADR).
const Magic = "plugin-v1"

// NetworkUnix is the only value of Handshake.Network in the MVP
// (docs/keeper/plugins.md → Handshake). named_pipe/tcp support is post-MVP.
const NetworkUnix = "unix"

// defaultShutdownGrace duplicates plugin_runtime.shutdown_grace (ADR-020(d))
// for when the plugin author didn't set an explicit value in Config. The
// host keeps its own timer at the same interval.
const defaultShutdownGrace = 10 * time.Second

// Config holds the parameters of a handshake session.
//
// Address and ServerCert are optional: Address defaults to reading the
// SocketEnv env var, ServerCert is always empty in the MVP (mTLS is
// post-MVP, ADR-020(h)).
type Config struct {
	// ProtocolVersion is the plugin-protocol version; must match
	// manifest.protocol_version. The only allowed value in the MVP is 1.
	ProtocolVersion int32

	// Kind is the plugin kind. Must match manifest.kind, otherwise the host
	// rejects the plugin (ADR-020(c)).
	Kind pluginv1.Kind

	// Address is the Unix-socket path; if empty, it's read from the
	// SocketEnv env var.
	Address string

	// ShutdownGrace is the window from SIGTERM to a hard Stop(). Defaults to
	// 10s (ADR-020(d) — mirrors the host-side timer).
	ShutdownGrace time.Duration
}

// Register is a function that registers a service implementation on the
// server. Called exactly once inside Serve, before the handshake line is
// written.
type Register func(*grpc.Server)

// Serve is the typical main() of a plugin.
//
// Lifecycle:
//
//  1. reads Address (explicit or from env);
//  2. listens on the Unix socket;
//  3. creates a grpc.Server and hands it to the register callback;
//  4. writes the JSON handshake to stdout;
//  5. subscribes to SIGTERM/SIGINT/SIGHUP; on signal, GracefulStop with a
//     ShutdownGrace timeout, then a hard Stop once that elapses;
//  6. blocks on grpc.Server.Serve until it returns.
//
// Returns the first error from listen/serve/marshal.
func Serve(cfg Config, register Register) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	return serveWithStop(cfg, register, os.Stdout, sigCh)
}

// serveWithStop is a testable form of Serve: the stop channel and the
// destination for the handshake line are injected as parameters. The
// exported Serve simply forwards to it via os.Stdout + signal.Notify.
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
			// GracefulStop holds on to in-flight raw connections, which can
			// get stuck in HTTP/2 handshake state and keep the server from
			// releasing even after Stop(). Force-close all accepted
			// connections, then call Stop — the accept loop returns and
			// GracefulStop unblocks.
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

// trackingListener is a net.Listener that remembers every accepted
// connection and can close them all at once. Needed for the hard-stop
// scenario: grpc.Server.Stop by itself doesn't close a raw connection stuck
// in the HTTP/2 handshake (see the shutdown logic in Serve).
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
	// Close without holding mu — trackedConn.Close takes mu itself.
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
	// protojson defaults to multi-line pretty-output; the plugin must write
	// exactly one line (ADR-020(b)).
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
