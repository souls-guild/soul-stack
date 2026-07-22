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

// SeedAuthenticator — application-level authentication by SoulSeed
// fingerprint. mTLS alone (RequireAndVerifyClientCert + CA) isn't enough:
// the cert is signed by our PKI, but it may have been revoked by an operator
// or replaced by rotation — that's only visible via `soul_seeds.status='active'`.
//
// Returns the SID of the bound seed on success. Errors map to:
//   - cert unknown to the registry → Unauthenticated;
//   - cert not active (superseded/revoked/expired) → Unauthenticated;
//   - DB lookup error → Unavailable.
type SeedAuthenticator struct {
	db     soulseed.ExecQueryRower
	logger *slog.Logger
}

// NewSeedAuthenticator assembles the authenticator. db is a pool / tx-like
// object for `SELECT FROM soul_seeds WHERE fingerprint = $1`.
func NewSeedAuthenticator(db soulseed.ExecQueryRower, logger *slog.Logger) *SeedAuthenticator {
	return &SeedAuthenticator{db: db, logger: logger}
}

// Authenticate checks the peer cert against `soul_seeds`. Returns the SID
// on success, or a gRPC status error.
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
		// DB error / malformed fingerprint from the registry — transient.
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

// streamSeedAuthInterceptor — gRPC stream interceptor that invokes [SeedAuthenticator]
// before handing the stream to the handler. On error the handler is never called;
// the client gets the corresponding gRPC status and the stream closes.
//
// The SID is stored in the context via [withAuthenticatedSID] — the handler reads
// it back via [authenticatedSIDFrom].
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

// authStream — wraps [grpclib.ServerStream] with a substituted ctx that
// carries the SID as a value. Only Context() is overridden; everything else
// is delegated (Recv/Send/SetHeader/...).
type authStream struct {
	grpclib.ServerStream
	ctx context.Context
}

func (s *authStream) Context() context.Context { return s.ctx }

// authSIDKey — private key type for the context value, to avoid collisions
// with other packages.
type authSIDKey struct{}

func withAuthenticatedSID(ctx context.Context, sid string) context.Context {
	return context.WithValue(ctx, authSIDKey{}, sid)
}

// authenticatedSIDFrom returns the SID verified by
// [streamSeedAuthInterceptor]. Empty string + false if the interceptor
// never ran (only in handler unit tests without the interceptor chain).
func authenticatedSIDFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(authSIDKey{}).(string)
	return v, ok && v != ""
}
