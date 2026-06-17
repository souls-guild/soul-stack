// Package grpc — Keeper-side gRPC-сервер по [ADR-012].
//
// MVP scope (M2.1.b.2): Bootstrap-listener c server-only TLS и Ping-RPC
// для health-check-а. EventStream-listener (mTLS, долгоживущий
// bidi-стрим) поднимается параллельно как заглушка (Unimplemented),
// реальная реализация — M2.2+.
//
// Архитектура двух listener-ов отражает ADR-012(b): у Soul-а до
// онбординга нет SoulSeed-сертификата, поэтому Bootstrap требует
// server-only TLS. Listener-ы независимы (разные TLS-режимы, разные
// порты, разные [grpc.Server]-ы); общая бизнес-логика — через
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

// graceDuration — grace-period для GracefulStop. Совпадает с api/mcp
// (10s) — единый класс серверов Keeper-а.
const graceDuration = 10 * time.Second

// Resource-лимиты Bootstrap-listener-а (защита от DoS, H3). Bootstrap —
// pre-auth (server-only TLS, токен ещё не проверен в момент чтения
// сообщения), поэтому лимиты строгие.
const (
	// bootstrapMaxRecvMsgSize — максимальный размер входящего сообщения.
	// BootstrapRequest несёт SID + bootstrap_token + один CSR PEM; даже
	// RSA-4096 CSR укладывается в единицы КиБ. 256 КиБ — запас на порядки
	// без открытия 4-МиБ-вектора grpc-дефолта.
	bootstrapMaxRecvMsgSize = 256 * 1024

	// bootstrapMaxConcurrentStreams — лимит одновременных RPC на одно
	// соединение. Bootstrap — короткий unary (Ping/Bootstrap), легитимному
	// клиенту параллелизм не нужен; 10 закрывает stream-flood по одному conn.
	bootstrapMaxConcurrentStreams = 10

	// bootstrapKeepaliveMinTime — минимальный интервал между client-ping-ами.
	// Bootstrap-клиент (soul, push) keepalive на этом listener-е не настраивает
	// и держит соединение единицы секунд; любой ping чаще 30s — флуд.
	bootstrapKeepaliveMinTime = 30 * time.Second
)

// BootstrapServer — gRPC-сервер Bootstrap-listener-а.
//
// Слушает `listen.grpc.bootstrap.addr` с server-only TLS (см.
// [tlsx.LoadServerOnlyTLS]). Регистрирует Ping и Bootstrap RPC; для
// EventStream возвращает Unimplemented (см. [keeperv1.UnimplementedKeeperServer]).
//
// Mu защищает поле addr, обновляемое в Start при `:0`-bind (тесты).
type BootstrapServer struct {
	srv        *grpclib.Server
	configAddr string

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// NewBootstrapServer собирает Bootstrap-listener с server-only TLS и
// зарегистрированным [BootstrapHandler].
//
// Возвращает error на:
//   - пустой `cfg.Addr` / `cfg.TLS.Cert` / `cfg.TLS.Key`;
//   - неверные file paths TLS (передаются в [tlsx.LoadServerOnlyTLS]);
//   - nil deps (см. [BootstrapDeps.validate]).
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

// Start — блокирующий запуск listener-а. На ctx.Done() делает
// GracefulStop с [graceDuration]-timeout-ом; превышение → forced Stop.
//
// Возвращает nil при штатном graceful shutdown. Listen-ошибка
// (bind-конфликт, EACCES) — wrapped fmt.Errorf.
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
		// Serve возвращает grpc.ErrServerStopped после GracefulStop/Stop —
		// для нас это штатный shutdown.
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

// Addr возвращает фактический bind-адрес. После Start — actual port
// (важно для тестов с `:0`).
func (s *BootstrapServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}
