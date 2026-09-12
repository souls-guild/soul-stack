// Package grpc — Keeper-side gRPC server per [ADR-012].
//
// MVP scope (M2.1.b.2): a Bootstrap listener with server-only TLS and a
// Ping RPC for health checks. The EventStream listener (mTLS, long-lived
// bidi stream) comes up in parallel as a stub (Unimplemented); the real
// implementation is M2.2+.
//
// The two-listener architecture reflects ADR-012(b): before onboarding, a
// Soul has no SoulSeed certificate, so Bootstrap requires server-only TLS.
// The listeners are independent (different TLS modes, different ports,
// different [grpc.Server]s); shared business logic goes through
// [BootstrapHandler.Deps].
//
// [ADR-012]: docs/adr/0012-keeper-soul-grpc.md
package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/net/netutil"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/tlsx"
)

// graceDuration — the grace period for GracefulStop. Matches api/mcp
// (10s) — a single server class for Keeper.
const graceDuration = 10 * time.Second

// Resource limits for the Bootstrap listener (DoS protection, H3).
// Bootstrap is pre-auth (server-only TLS, the token hasn't been checked yet
// at message-read time), so the limits are strict.
const (
	// bootstrapMaxRecvMsgSize — the maximum size of an incoming message.
	// BootstrapRequest carries SID + bootstrap_token + one CSR PEM +
	// soul_version; an FQDN, 43 base64url characters, an RSA-4096 CSR and a
	// version string come to under 3 KiB, so 16 KiB is five times the largest
	// legitimate request.
	//
	// It was 256 KiB, on the reasoning that headroom was free. It stopped being
	// free when the pre-auth budget gained a wait (NIM-839): a request now
	// stays live for as long as its caller queues, and 256 conns × 10 streams
	// of the old ceiling was ~640 MiB of heap an unauthenticated peer could
	// pin with one precomputed CSR and a padded token.
	bootstrapMaxRecvMsgSize = 16 * 1024

	// bootstrapMaxConcurrentStreams — the limit on concurrent RPCs per
	// connection. Bootstrap is a short unary call (Ping/Bootstrap); a
	// legitimate client doesn't need parallelism. 10 closes off a
	// stream-flood over a single conn.
	bootstrapMaxConcurrentStreams = 10

	// bootstrapKeepaliveMinTime — the minimum interval between client pings.
	// The Bootstrap client (soul, push) doesn't configure keepalive on this
	// listener and holds the connection for a few seconds; any ping more
	// frequent than 30s is a flood.
	bootstrapKeepaliveMinTime = 30 * time.Second

	// defaultBootstrapMaxConns — the ceiling on concurrent TCP connections the
	// listener accepts (NIM-839). [bootstrapMaxConcurrentStreams] is a
	// per-connection ceiling and nothing bounded the number of connections, so
	// pre-auth concurrency was M×10 for an M the attacker chose. Over the
	// ceiling a connection is not accepted at all — it waits in the kernel
	// backlog and the client's own deadline ends it.
	defaultBootstrapMaxConns = 256

	// bootstrapConnectionTimeout — the ceiling on connection setup (TLS
	// handshake plus HTTP/2 preface). Without it a peer that opens a socket and
	// then says nothing holds an accept slot for as long as it likes, which
	// makes the connection ceiling above the cheaper thing to attack.
	bootstrapConnectionTimeout = 10 * time.Second

	// bootstrapMaxConnectionIdle / Age / AgeGrace — the same reasoning past the
	// handshake. A bootstrap exchange is a Ping plus one unary call and is over
	// in seconds, so a connection with no RPC on it has no reason to keep a
	// slot; Age bounds the held-open case where a Ping arrives just often
	// enough to look busy. A gRPC client treats the resulting GOAWAY as a
	// reconnect, so a health-checker polling Ping sees nothing.
	bootstrapMaxConnectionIdle     = 2 * time.Minute
	bootstrapMaxConnectionAge      = 5 * time.Minute
	bootstrapMaxConnectionAgeGrace = 20 * time.Second
)

// BootstrapServer — the gRPC server for the Bootstrap listener.
//
// Listens on `listen.grpc.bootstrap.addr` with server-only TLS (see
// [tlsx.LoadServerOnlyTLS]). Registers the Ping and Bootstrap RPCs; returns
// Unimplemented for EventStream (see [keeperv1.UnimplementedKeeperServer]).
//
// Mu guards the addr field, which Start updates on a `:0` bind (tests).
type BootstrapServer struct {
	srv        *grpclib.Server
	configAddr string
	maxConns   int

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// NewBootstrapServer assembles a Bootstrap listener with server-only TLS
// and a registered [BootstrapHandler].
//
// Returns an error on:
//   - empty `cfg.Addr` / `cfg.TLS.Cert` / `cfg.TLS.Key`;
//   - invalid TLS file paths (passed to [tlsx.LoadServerOnlyTLS]);
//   - nil deps (see [BootstrapDeps.validate]).
func NewBootstrapServer(cfg config.KeeperListenGRPCBootstrap, deps BootstrapDeps, logger *slog.Logger) (*BootstrapServer, error) {
	if cfg.Addr == "" {
		return nil, errors.New("grpc: listen.grpc.bootstrap.addr is empty")
	}
	if logger == nil {
		return nil, errors.New("grpc: logger is required")
	}
	if err := deps.validate(); err != nil {
		return nil, err
	}

	tlsCfg, err := tlsx.LoadServerOnlyTLS(tlsx.ServerConfig{
		CertPath: cfg.TLS.Cert,
		KeyPath:  cfg.TLS.Key,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc: load bootstrap TLS: %w", err)
	}

	srv := grpclib.NewServer(
		grpclib.Creds(credentials.NewTLS(tlsCfg)),
		grpclib.MaxRecvMsgSize(bootstrapMaxRecvMsgSize),
		grpclib.MaxConcurrentStreams(bootstrapMaxConcurrentStreams),
		grpclib.ConnectionTimeout(bootstrapConnectionTimeout),
		grpclib.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     bootstrapMaxConnectionIdle,
			MaxConnectionAge:      bootstrapMaxConnectionAge,
			MaxConnectionAgeGrace: bootstrapMaxConnectionAgeGrace,
		}),
		grpclib.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             bootstrapKeepaliveMinTime,
			PermitWithoutStream: false,
		}),
	)
	keeperv1.RegisterKeeperServer(srv, newBootstrapHandler(deps, logger))

	maxConns := deps.MaxConns
	if maxConns <= 0 {
		maxConns = defaultBootstrapMaxConns
	}

	return &BootstrapServer{
		srv:        srv,
		configAddr: cfg.Addr,
		maxConns:   maxConns,
		addr:       cfg.Addr,
		logger:     logger,
	}, nil
}

// Start — a blocking listener startup. On ctx.Done() it does a
// GracefulStop with a [graceDuration] timeout; exceeding it → forced Stop.
//
// Returns nil on a normal graceful shutdown. A listen error (bind conflict,
// EACCES) is wrapped with fmt.Errorf.
func (s *BootstrapServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.configAddr)
	if err != nil {
		return fmt.Errorf("grpc: listen %q: %w", s.configAddr, err)
	}
	actual := ln.Addr().String()
	s.mu.Lock()
	s.addr = actual
	s.mu.Unlock()
	ln = netutil.LimitListener(ln, s.maxConns)
	s.logger.Info("gRPC Bootstrap listener started",
		slog.String("addr", actual), slog.Int("max_conns", s.maxConns))

	errCh := make(chan error, 1)
	go func() {
		// Serve returns grpc.ErrServerStopped after GracefulStop/Stop —
		// that's a normal shutdown for us.
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, grpclib.ErrServerStopped) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("gRPC Bootstrap listener received shutdown signal")
		stopped := make(chan struct{})
		go func() {
			s.srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(graceDuration):
			s.logger.Warn("gRPC Bootstrap GracefulStop did not finish within grace — forcing Stop")
			s.srv.Stop()
		}
		select {
		case serveErr := <-errCh:
			if serveErr != nil && !errors.Is(serveErr, grpclib.ErrServerStopped) {
				s.logger.Warn("gRPC Bootstrap Serve returned error after shutdown",
					slog.Any("error", serveErr))
			}
		case <-time.After(2 * time.Second):
			s.logger.Warn("gRPC Bootstrap Serve did not exit within 2s after Stop — leak suspected")
		}
		s.logger.Info("gRPC Bootstrap listener stopped")
		return nil
	case err := <-errCh:
		return err
	}
}

// Addr returns the actual bind address. After Start it's the actual port
// (important for tests using `:0`).
func (s *BootstrapServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}
