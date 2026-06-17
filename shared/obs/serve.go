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

// BasicAuth — уже зарезолвленные креды для HTTP Basic-auth на `/metrics`.
//
// Источник-агностично: helper не знает, откуда взялись Username/Password
// (Vault KV, env, статика конфига) — caller передаёт готовые значения.
// Это держит границу ADR-011: shared/obs не тянет vault-client (на Soul
// его и нет по ADR-012). nil *BasicAuth → listener без auth.
type BasicAuth struct {
	Username string
	Password string
}

// MetricsServer — дескриптор поднятого metrics-listener-а. Owns http.Server
// + net.Listener; closed через [MetricsServer.Shutdown] в defer-цепочке main.
//
// Listener создаётся синхронно в [ServeMetrics] (fail-fast при занятом порту),
// Serve уходит в фоновую goroutine. Addr() отдаёт фактический адрес — важно
// для тестов с `:0` (ephemeral-port).
type MetricsServer struct {
	srv *http.Server
	ln  net.Listener
}

// ServeMetrics поднимает выделенный HTTP-listener для `GET /metrics` под
// переданный Registry. Используется обоими server-бинарями (keeper — на
// listen.metrics.addr с опц. basic-auth; soul — на loopback без auth).
//
// reg обязателен (без него нечего экспонировать). auth == nil → endpoint
// открыт; non-nil → каждый запрос проходит Basic-auth (constant-time
// сравнение, см. [basicAuthMiddleware]).
//
// Listener биндится синхронно — занятый порт даёт ошибку немедленно, до
// старта Serve-goroutine (fail-fast при конфликте портов).
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
		// ReadHeaderTimeout — anti-Slowloris; scrape-запрос крошечный,
		// 5s с запасом. WriteTimeout не ставим: exposition больших registry
		// может занять время, обрезать по таймауту = битый scrape.
		ReadHeaderTimeout: 5 * time.Second,
	}

	ms := &MetricsServer{srv: srv, ln: ln}
	go func() {
		// Serve возвращает ErrServerClosed на штатном Shutdown — не ошибка.
		// Прочие ошибки (accept-loop crash) ниоткуда не наблюдаются, но и
		// крэшить процесс из-за упавшего metrics-порта не стоит — метрики
		// вторичны к основному трафику.
		_ = srv.Serve(ln)
	}()
	return ms, nil
}

// Addr возвращает фактический адрес listener-а (`host:port`). При `:0` в
// конфиге отдаёт ephemeral-port, выданный ядром, — нужно для тестов.
func (m *MetricsServer) Addr() string {
	if m == nil || m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// Shutdown gracefully останавливает metrics-listener. Безопасен на nil-е.
// Вешается в defer-цепочку main с timeout-context (паттерн reaper/renewer).
func (m *MetricsServer) Shutdown(ctx context.Context) error {
	if m == nil || m.srv == nil {
		return nil
	}
	if err := m.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("obs: shutdown metrics server: %w", err)
	}
	return nil
}

// basicAuthMiddleware закрывает next за HTTP Basic-auth. Сравнение и
// username, и password — constant-time ([subtle.ConstantTimeCompare]),
// чтобы таймингом не утекала длина/совпадение credential-а.
//
// ConstantTimeCompare на разной длине возвращает 0 быстро (без сравнения
// байт), что само по себе timing-leak длины. Чтобы оба сравнения всегда
// исполнялись и комбинировались в один результат, флаги объединяются
// через `&`, а не short-circuit `&&`.
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
