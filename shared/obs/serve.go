package obs

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// BasicAuth holds already-resolved credentials for HTTP Basic-auth on `/metrics`.
//
// Source-agnostic: the helper does not know where Username/Password came from
// (Vault KV, env, static config) — the caller passes ready values. This keeps
// the ADR-011 boundary: shared/obs does not pull the vault client (Soul has none
// per ADR-012). A nil *BasicAuth → a listener without auth.
type BasicAuth struct {
	Username string
	Password string
}

// MetricsServer is the descriptor of a running metrics listener. Owns the
// http.Server + net.Listener; closed via [MetricsServer.Shutdown] in main's
// defer chain.
//
// The listener is created synchronously in [ServeMetrics] (fail-fast on a busy
// port), Serve goes into a background goroutine. Addr() returns the actual
// address — important for tests with `:0` (ephemeral port).
type MetricsServer struct {
	srv *http.Server
	ln  net.Listener
}

// ServeMetrics starts a dedicated HTTP listener for `GET /metrics` over the
// given Registry. Used by both server binaries (keeper — on listen.metrics.addr
// with optional basic-auth; soul — on loopback without auth).
//
// reg is required (nothing to expose without it). auth == nil → the endpoint is
// open; non-nil → every request goes through Basic-auth (constant-time
// comparison, see [basicAuthMiddleware]).
//
// The listener binds synchronously — a busy port errors immediately, before the
// Serve goroutine starts (fail-fast on a port conflict).
func ServeMetrics(addr string, reg *Registry, auth *BasicAuth) (*MetricsServer, error) {
	if reg == nil {
		return nil, errors.New("obs: ServeMetrics requires non-nil Registry")
	}
	if addr == "" {
		return nil, errors.New("obs: ServeMetrics requires non-empty addr")
	}
	if auth != nil && (auth.Username == "" || auth.Password == "") {
		return nil, errors.New("obs: ServeMetrics basic-auth requires non-empty username and password")
	}

	mux := http.NewServeMux()
	var handler http.Handler = reg.MetricsHandler()
	if auth != nil {
		handler = basicAuthMiddleware(*auth, handler)
	}
	mux.Handle("/metrics", handler)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("obs: listen metrics %q: %w", addr, err)
	}

	srv := &http.Server{
		Handler: mux,
		// ReadHeaderTimeout — anti-Slowloris; a scrape request is tiny, 5s with
		// margin. No WriteTimeout: exposition of a large registry may take time,
		// and cutting it off by timeout = a broken scrape.
		ReadHeaderTimeout: 5 * time.Second,
	}

	ms := &MetricsServer{srv: srv, ln: ln}
	go func() {
		// Serve returns ErrServerClosed on a normal Shutdown — not an error.
		// Other errors (accept-loop crash) are observed nowhere, but crashing the
		// process over a failed metrics port is not worth it — metrics are
		// secondary to the main traffic.
		_ = srv.Serve(ln)
	}()
	return ms, nil
}

// Addr returns the listener's actual address (`host:port`). With `:0` in the
// config it returns the kernel-assigned ephemeral port — needed for tests.
func (m *MetricsServer) Addr() string {
	if m == nil || m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// Shutdown gracefully stops the metrics listener. Safe on nil. Hooked into
// main's defer chain with a timeout context (the reaper/renewer pattern).
func (m *MetricsServer) Shutdown(ctx context.Context) error {
	if m == nil || m.srv == nil {
		return nil
	}
	if err := m.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("obs: shutdown metrics server: %w", err)
	}
	return nil
}

// basicAuthMiddleware guards next behind HTTP Basic-auth. Both username and
// password are compared constant-time ([subtle.ConstantTimeCompare]) so timing
// does not leak the length or match of a credential.
//
// ConstantTimeCompare on differing lengths returns 0 quickly (without comparing
// bytes), which is itself a length timing-leak. So that both comparisons always
// run and combine into one result, the flags are joined with `&`, not the
// short-circuit `&&`.
func basicAuthMiddleware(want BasicAuth, next http.Handler) http.Handler {
	wantUser := []byte(want.Username)
	wantPass := []byte(want.Password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), wantUser) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), wantPass) == 1
		if !ok || !(userOK && passOK) {
			w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
