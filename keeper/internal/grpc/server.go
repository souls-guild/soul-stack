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
// [ADR-012]: docs/adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add
package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

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
	// BootstrapRequest carries SID + bootstrap_token + one CSR PEM; even an
	// RSA-4096 CSR fits in a few KiB. 256 KiB leaves orders of magnitude of
	// headroom without opening up the gRPC default's 4 MiB attack surface.
	bootstrapMaxRecvMsgSize = 256 * 1024

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
		grpclib.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             bootstrapKeepaliveMinTime,
			PermitWithoutStream: false,
		}),
	)
	keeperv1.RegisterKeeperServer(srv, newBootstrapHandler(deps, logger))

	return &BootstrapServer{
		srv:        srv,
		configAddr: cfg.Addr,
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
	s.logger.Info("gRPC Bootstrap listener started", slog.String("addr", actual))

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
