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

// ServerDeps — wire-up зависимости MCP HTTP-listener-а.
//
// JWTVerifier обязателен — без него невозможно гарантировать identity
// caller-а. Bus обязателен — без него SSE-маршрут `/mcp/events` не сможет
// доставлять события (в unit-тестах без публикации это не мешает: bus
// корректно работает без publisher-ов). Все остальные deps приходят в
// [HandlerDeps] (внутреннему диспетчеру MCP-методов).
//
// ApplyAccess и RBAC включают RBAC-проверку SSE-подписки (M1): подписаться
// на apply_id может только инициатор прогона или Архонт с `incarnation.get`.
// ApplyAccess == nil → RBAC-проверка SSE отключена (single-node dev без
// apply_runs); в production main обязан прокинуть [ApplyAccessPG].
type ServerDeps struct {
	JWTVerifier *jwt.Verifier
	Handler     *Handler
	Bus         *applybus.EventBus
	ApplyAccess applyAccessStore
	RBAC        PermissionChecker
	Logger      *slog.Logger
}

// Server — обёртка над http.Server для MCP listener-а. Слушает
// `listen.mcp.addr` (отдельный port от Operator API); auth и dispatch
// делает встроенный handler.
//
// Mu защищает поле addr, обновляемое в Start при `:0`-bind-е (для тестов).
type Server struct {
	srv        *http.Server
	configAddr string

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// Лимиты MCP-listener-а. Тематически совпадают с Operator API
// (16 KiB headers, 1 MiB body), значения скопированы намеренно — это
// единый класс HTTP-фасадов Keeper-а.
const (
	mcpMaxHeaderBytes = 16 * 1024
	mcpMaxBodyBytes   = 1 << 20 // 1 MiB

	mcpReadHeaderTimeout = 5 * time.Second
	mcpReadTimeout       = 30 * time.Second
	mcpWriteTimeout      = 60 * time.Second
	mcpIdleTimeout       = 120 * time.Second
)

// NewServer собирает MCP HTTP-сервер. cfg.Addr — обязателен (без него
// конфиг считается mis-configured: блок listen.mcp заявлен, но без addr).
//
// Сервер регистрирует два маршрута:
//
//   - `POST /mcp`        — JSON-RPC 2.0 поверх HTTP (tools/list, tools/call,
//     initialize). Auth — JWT Bearer.
//   - `GET /mcp/events`  — SSE-stream apply-событий по `?apply_id=<ULID>`
//     (M0.7.c). Auth — тот же JWT Bearer.
//
// Любой другой URL/метод → 404 JSON {error: "not found"}. /healthz и /metrics
// MCP listener НЕ предоставляет — health/metrics живут на отдельных
// listener-ах (см. operator-api.md → Health/Meta).
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

// Start — блокирующий запуск listener-а. На ctx.Done() делает graceful
// shutdown (10s). Симметрично api.Server.Start.
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

// Addr возвращает фактический bind-адрес. После Start — actual port
// (для тестов с `:0`).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown — graceful с 10s grace.
func (s *Server) Shutdown(ctx context.Context) error {
	shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("mcp: shutdown: %w", err)
	}
	s.logger.Info("MCP listener stopped")
	return nil
}

// buildMCPHandler возвращает http.HandlerFunc для `POST /mcp`.
// Pipeline:
//  1. Method check: только POST.
//  2. Body limit: 1 MiB через http.MaxBytesReader.
//  3. JWT verify: Bearer-token из Authorization header (тот же [jwt.Verifier],
//     что Operator API).
//  4. JSON-RPC parse → Handler.Dispatch → JSON-RPC response.
//
// Любая ошибка auth → JSON-RPC error code -32600 (Invalid Request) с
// `data.code: "unauthenticated"`. Это сознательное отклонение от HTTP-style
// 401: MCP-клиент работает с JSON-RPC-уровнем, не с HTTP status code.
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

		// Парсим body как JSON-RPC request. Batch-запросы (массив) — out
		// of scope MVP (MCP-spec 2025-06 разрешает их, но MCP-клиенты
		// обычно шлют по одному; добавим в M0.7.x при реальной потребности).
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
			// JSON-RPC §4.1: для notification сервер не отвечает.
			// HTTP-аналог — 204 No Content.
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

// buildNotFoundHandler — для всех путей кроме `/mcp` отдаёт простой 404.
// MCP-spec не описывает иной URL-структуры в рамках Streamable HTTP, так
// что попадание сюда — клиентская ошибка пути. `/mcp` сюда не попадает
// (его перехватывает [buildMCPHandler] выше через ServeMux); под `/`
// попадают `/mcp/<suffix>`, `/healthz` и любые другие URL.
func buildNotFoundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONError(w, http.StatusNotFound, "no such endpoint")
	}
}

// writeJSONError пишет короткий JSON {error: "<msg>"} с указанным HTTP-кодом.
// Используется до того, как мы поняли, что это JSON-RPC request (auth-fail,
// bad method, …). После авторизации все ошибки уходят как JSON-RPC error
// поверх HTTP 200 (см. writeRPCErrorHTTP).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + jsonString(msg) + `}`))
}

// writeRPCErrorHTTP сериализует JSON-RPC error-response и пишет его в
// HTTP-ответ. status обычно 200 — это контракт JSON-RPC поверх HTTP:
// «слой ошибки — JSON-RPC, не HTTP».
func writeRPCErrorHTTP(w http.ResponseWriter, status int, id json.RawMessage, code int, message string, data any) {
	resp := newRPCError(id, code, message, data)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// isJSONArray — true, если первый non-whitespace байт `[`. Используется
// для отказа от batch-запросов (M0.7.a).
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

// jsonString — простой JSON-string-encode для коротких error-message-ей в
// writeJSONError. Не пытаемся быть универсальными (отдельный пакет json
// для этого), но эскейпим базовые символы.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
