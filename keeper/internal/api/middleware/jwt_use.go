package middleware

import (
	"context"
	"net/http"
)

// JWTUseRecorder takes note that a token was presented (`operator_jwts.last_used_*`,
// ADR-014 amendment NIM-1045). It must not block: it runs on every
// authenticated request. *operator.JWTUseRecorder satisfies it.
type JWTUseRecorder interface {
	RecordUse(jwtID, ip, userAgent string)
}

// RecordJWTUse — a middleware factory, after [RequireJWT]: hands the token id,
// the peer address and the User-Agent to rec. nil rec → pass-through (a
// Keeper built without a database to write to, tests).
func RecordJWTUse(rec JWTUseRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if rec == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if claims, ok := ClaimsFromContext(r.Context()); ok && claims.ID != "" {
				rec.RecordUse(claims.ID, PeerIP(r), r.UserAgent())
			}
			next.ServeHTTP(w, r)
		})
	}
}

type clientMetaCtxKey struct{}

// ClientMeta is what a request said about its sender: the peer address and the
// User-Agent. Stored with a token that request mints; untrusted, for a human.
type ClientMeta struct {
	IP        string
	UserAgent string
}

// WithClientMeta — a middleware that puts the request's [ClientMeta] into the
// context, so a handler that has no *http.Request (huma) can record who minted
// a token.
func WithClientMeta(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := ClientMeta{IP: PeerIP(r), UserAgent: r.UserAgent()}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientMetaCtxKey{}, meta)))
	})
}

// ClientMetaFromContext returns what [WithClientMeta] stored; zero value when
// the middleware did not run.
func ClientMetaFromContext(ctx context.Context) ClientMeta {
	meta, _ := ctx.Value(clientMetaCtxKey{}).(ClientMeta)
	return meta
}

// PeerIP is the address of the client, without the port: the direct peer, or
// the client a trusted proxy names ([ResolveClientAddr]).
func PeerIP(r *http.Request) string { return clientIP(r) }
