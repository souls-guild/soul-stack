package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ServerDeps — wire-up dependencies for the MCP HTTP listener.
//
// JWTVerifier is required — without it caller identity can't be
// guaranteed. Bus is required — without it the SSE route `/mcp/events`
// can't deliver events (harmless in unit tests without a publisher: the
// bus works fine with none). All other deps go to [HandlerDeps] (the
// internal MCP-method dispatcher).
//
// ApplyAccess and RBAC gate SSE-subscription RBAC (M1): only the run's
// initiator or an Archon with `incarnation.get` may subscribe to an
// apply_id. ApplyAccess == nil disables SSE RBAC checking (single-node dev
// without apply_runs); production main must supply [ApplyAccessPG].
type ServerDeps struct {
	JWTVerifier *jwt.Verifier
	Handler     *Handler
	Bus         *applybus.EventBus
	ApplyAccess applyAccessStore
	RBAC        PermissionChecker
	Logger      *slog.Logger
}

// Server — wraps http.Server for the MCP listener. Listens on
// `listen.mcp.addr` (separate port from Operator API); auth and dispatch
// are done by the embedded handler.
//
// Mu guards addr, updated in Start on a `:0` bind (for tests).
type Server struct {
	srv        *http.Server
	configAddr string

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// MCP-listener limits. Intentionally mirror Operator API (16 KiB headers,
// 1 MiB body) — both are the same class of Keeper HTTP facade.
const (
	mcpMaxHeaderBytes = 16 * 1024
	mcpMaxBodyBytes   = 1 << 20 // 1 MiB

	mcpReadHeaderTimeout = 5 * time.Second
	mcpReadTimeout       = 30 * time.Second
	mcpWriteTimeout      = 60 * time.Second
	mcpIdleTimeout       = 120 * time.Second
)

// NewServer builds the MCP HTTP server. cfg.Addr is required (without it
// the config is mis-configured: listen.mcp block declared but no addr).
//
// The server registers two routes:
//
//   - `POST /mcp`        — JSON-RPC 2.0 over HTTP (tools/list, tools/call,
//     initialize). Auth — JWT Bearer.
//   - `GET /mcp/events`  — SSE stream of apply events via `?apply_id=<ULID>`
//     (M0.7.c). Auth — same JWT Bearer.
//
// Any other URL/method → 404 JSON {error: "not found"}. The MCP listener
// does NOT serve /healthz or /metrics — health/metrics live on separate
// listeners (see operator-api.md → Health/Meta).
func NewServer(cfg config.KeeperListenSimple, deps ServerDeps) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("mcp: listen.mcp.addr is empty")
	}
	if deps.JWTVerifier == nil {
		return nil, errors.New("mcp: JWTVerifier is required")
	}
	if deps.Handler == nil {
		return nil, errors.New("mcp: Handler is required")
	}
	if deps.Bus == nil {
		return nil, errors.New("mcp: Bus is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("mcp: Logger is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", buildMCPHandler(deps))
	mux.HandleFunc("/mcp/events", buildSSEHandler(sseDeps{
		JWTVerifier: deps.JWTVerifier,
		Bus:         deps.Bus,
		Access:      deps.ApplyAccess,
		RBAC:        deps.RBAC,
		Limiter:     newSSEConnLimiter(sseMaxConnsGlobal, sseMaxConnsPerAID),
		Logger:      deps.Logger,
	}))
	mux.HandleFunc("/", buildNotFoundHandler())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: mcpReadHeaderTimeout,
		ReadTimeout:       mcpReadTimeout,
		WriteTimeout:      mcpWriteTimeout,
		IdleTimeout:       mcpIdleTimeout,
		MaxHeaderBytes:    mcpMaxHeaderBytes,
	}

	return &Server{
		srv:        srv,
		configAddr: cfg.Addr,
		addr:       cfg.Addr,
		logger:     deps.Logger,
	}, nil
}

// Start — blocking listener startup. Does a graceful shutdown (10s) on
// ctx.Done(). Mirrors api.Server.Start.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.configAddr)
	if err != nil {
		return fmt.Errorf("mcp: listen %q: %w", s.configAddr, err)
	}
	actual := ln.Addr().String()
	s.mu.Lock()
	s.addr = actual
	s.mu.Unlock()
	s.logger.Info("MCP listener started", slog.String("addr", actual))

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("MCP listener received shutdown signal")
		shutErr := s.Shutdown(context.Background())
		select {
		case serveErr := <-errCh:
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				s.logger.Warn("MCP Serve returned non-ErrServerClosed after shutdown",
					slog.Any("error", serveErr))
			}
		case <-time.After(2 * time.Second):
			s.logger.Warn("MCP Serve did not exit within 2s after shutdown — leak suspected")
		}
		return shutErr
	case err := <-errCh:
		return err
	}
}

// Addr returns the actual bind address. After Start — the actual port
// (for tests using `:0`).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown — graceful with a 10s grace period.
func (s *Server) Shutdown(ctx context.Context) error {
	shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("mcp: shutdown: %w", err)
	}
	s.logger.Info("MCP listener stopped")
	return nil
}

// buildMCPHandler returns the http.HandlerFunc for `POST /mcp`.
// Pipeline:
//  1. Method check: POST only.
//  2. Body limit: 1 MiB via http.MaxBytesReader.
//  3. JWT verify: Bearer token from the Authorization header (same
//     [jwt.Verifier] as Operator API).
//  4. JSON-RPC parse → Handler.Dispatch → JSON-RPC response.
//
// Any auth error → JSON-RPC error code -32600 (Invalid Request) with
// `data.code: "unauthenticated"`. This deliberately departs from HTTP-style
// 401: the MCP client operates at the JSON-RPC level, not HTTP status codes.
func buildMCPHandler(deps ServerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed,
				"method not allowed (use POST)")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, mcpMaxBodyBytes)

		token, ok := jwt.ParseBearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeRPCErrorHTTP(w, http.StatusOK, json.RawMessage("null"),
				rpcCodeInvalidRequest,
				"missing or malformed Authorization header (expect: Bearer <jwt>)",
				mcpToolError{Code: mcpCodeUnauthenticated})
			return
		}
		claims, err := deps.JWTVerifier.Verify(token)
		if err != nil {
			writeRPCErrorHTTP(w, http.StatusOK, json.RawMessage("null"),
				rpcCodeInvalidRequest,
				jwt.ClassifyVerifyErr(err),
				mcpToolError{Code: mcpCodeUnauthenticated})
			return
		}

		// Parse body as a JSON-RPC request. Batch requests (array) are out
		// of scope for MVP (MCP-spec 2025-06 allows them, but MCP clients
		// usually send one at a time; add in M0.7.x if actually needed).
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeRPCErrorHTTP(w, http.StatusOK, json.RawMessage("null"),
				rpcCodeParseError, "failed to read request body: "+err.Error(), nil)
			return
		}
		if len(raw) == 0 {
			writeRPCErrorHTTP(w, http.StatusOK, json.RawMessage("null"),
				rpcCodeInvalidRequest, "empty request body", nil)
			return
		}
		if isJSONArray(raw) {
			writeRPCErrorHTTP(w, http.StatusOK, json.RawMessage("null"),
				rpcCodeInvalidRequest,
				"batch requests are not supported in this build (M0.7.a)", nil)
			return
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeRPCErrorHTTP(w, http.StatusOK, json.RawMessage("null"),
				rpcCodeParseError, "invalid JSON-RPC request: "+err.Error(), nil)
			return
		}

		resp, isNotification := deps.Handler.Dispatch(r.Context(), claims, req)
		if isNotification {
			// JSON-RPC §4.1: the server doesn't respond to a notification.
			// HTTP equivalent — 204 No Content.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			deps.Logger.Warn("mcp: encode response failed", slog.Any("error", err))
		}
	}
}

// buildNotFoundHandler — plain 404 for every path except `/mcp`. The
// MCP spec doesn't define any other URL structure for Streamable HTTP,
// so landing here is a client path error. `/mcp` never reaches this
// (intercepted by [buildMCPHandler] above via ServeMux); `/` catches
// `/mcp/<suffix>`, `/healthz`, and any other URL.
func buildNotFoundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "no such endpoint")
	}
}

// writeJSONError writes a short JSON {error: "<msg>"} with the given HTTP
// code. Used before we know it's a JSON-RPC request (auth failure, bad
// method, …). After auth, all errors go out as JSON-RPC errors over HTTP
// 200 (see writeRPCErrorHTTP).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + jsonString(msg) + `}`))
}

// writeRPCErrorHTTP serializes a JSON-RPC error response and writes it to
// the HTTP response. status is usually 200 — the JSON-RPC-over-HTTP
// contract: errors live at the JSON-RPC layer, not HTTP.
func writeRPCErrorHTTP(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	resp := newRPCError(id, code, message, data)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// isJSONArray — true if the first non-whitespace byte is `[`. Used to
// reject batch requests (M0.7.a).
func isJSONArray(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

// jsonString — a minimal JSON string-encode for short error messages in
// writeJSONError. Not meant to be general-purpose (that's what
// encoding/json is for), just escapes the basics.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
