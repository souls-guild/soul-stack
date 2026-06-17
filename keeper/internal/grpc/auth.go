package grpc

import (
	"context"
	"errors"
	"log/slog"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
)

// SeedAuthenticator — application-level аутентификация по SoulSeed
// fingerprint. mTLS-уровня (RequireAndVerifyClientCert + CA) недостаточно:
// сертификат подписан нашей PKI, но мог быть отозван оператором или
// заменён ротацией — это видно только в `soul_seeds.status='active'`.
//
// Возвращает SID привязанного seed-а при успехе. На ошибки маппится:
//   - cert не известен реестру → Unauthenticated;
//   - cert не active (superseded/revoked/expired) → Unauthenticated;
//   - DB-ошибка lookup-а → Unavailable.
type SeedAuthenticator struct {
	db     soulseed.ExecQueryRower
	logger *slog.Logger
}

// NewSeedAuthenticator собирает аутентификатор. db — pool / tx-like
// объект для `SELECT FROM soul_seeds WHERE fingerprint = $1`.
func NewSeedAuthenticator(db soulseed.ExecQueryRower, logger *slog.Logger) *SeedAuthenticator {
	return &SeedAuthenticator{db: db, logger: logger}
}

// Authenticate проверяет peer-cert против `soul_seeds`. Возвращает SID
// при успехе или gRPC-status-ошибку.
func (a *SeedAuthenticator) Authenticate(ctx context.Context) (sid string, err error) {
	fp, err := peerFingerprint(ctx)
	if err != nil {
		switch {
		case errors.Is(err, ErrPeerNotInContext),
			errors.Is(err, ErrPeerNotTLS),
			errors.Is(err, ErrPeerNoCert):
			return "", status.Error(codes.Unauthenticated, err.Error())
		default:
			return "", status.Errorf(codes.Internal, "peer cert extract: %v", err)
		}
	}
	seed, err := soulseed.SelectByFingerprint(ctx, a.db, fp)
	if err != nil {
		if errors.Is(err, soulseed.ErrSeedNotFound) {
			a.logger.Warn("eventstream: unknown peer fingerprint",
				slog.String("fingerprint", fp))
			return "", status.Error(codes.Unauthenticated, "unknown soul seed")
		}
		// DB-ошибка / неверный формат fingerprint-а от реестра — transient.
		a.logger.Error("eventstream: soul_seed lookup failed",
			slog.String("fingerprint", fp), slog.Any("error", err))
		return "", status.Errorf(codes.Unavailable, "seed lookup failed")
	}
	if seed.Status != soulseed.StatusActive {
		a.logger.Warn("eventstream: peer presented non-active seed",
			slog.String("sid", seed.SID),
			slog.String("seed_id", seed.SeedID),
			slog.String("status", string(seed.Status)),
		)
		return "", status.Errorf(codes.Unauthenticated, "soul seed status %q (not active)", seed.Status)
	}
	return seed.SID, nil
}

// streamSeedAuthInterceptor — gRPC stream-interceptor, дёргает [SeedAuthenticator]
// до передачи стрима handler-у. При ошибке handler не вызывается, клиент
// получает соответствующий gRPC-status и стрим закрывается.
//
// SID кладётся в context через [withAuthenticatedSID] — handler читает
// его через [authenticatedSIDFrom].
func streamSeedAuthInterceptor(auth *SeedAuthenticator) grpclib.StreamServerInterceptor {
	return func(srv any, ss grpclib.ServerStream, info *grpclib.StreamServerInfo, handler grpclib.StreamHandler) error {
		sid, err := auth.Authenticate(ss.Context())
		if err != nil {
			return err
		}
		wrapped := &authStream{ServerStream: ss, ctx: withAuthenticatedSID(ss.Context(), sid)}
		return handler(srv, wrapped)
	}
}

// authStream — обёртка над [grpclib.ServerStream] с подменённым ctx,
// несущим SID в value. Только Context() переопределяем; остальное
// делегируем (Recv/Send/SetHeader/...).
type authStream struct {
	grpclib.ServerStream
	ctx context.Context
}

func (s *authStream) Context() context.Context { return s.ctx }

// authSIDKey — приватный тип-ключ для контекстного value, чтобы не было
// коллизий с другими пакетами.
type authSIDKey struct{}

func withAuthenticatedSID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, authSIDKey{}, sid)
}

// authenticatedSIDFrom возвращает SID, проверенный
// [streamSeedAuthInterceptor]. Пустая строка + false, если interceptor
// не отработал (только в unit-тестах handler-а без interceptor-цепочки).
func authenticatedSIDFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(authSIDKey{}).(string)
	return v, ok && v != ""
}
