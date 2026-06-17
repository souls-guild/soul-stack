// Package middleware — HTTP middleware Operator API.
//
// JWT-auth middleware ([RequireJWT]) — extract `Authorization: Bearer …`,
// верифицирует через [jwt.Verifier], кладёт claims в request-context для
// downstream handler-ов (RBAC / audit / endpoints).
//
// Health/meta-endpoints (`/healthz`, `/readyz`, `/metrics`) и публичный
// shell doc-вьювера (`/docs`, `/docs/assets/*`) этот middleware **не** вешают —
// они вне `/v1/*` chain (см. operator-api.md § Health / Meta). Сама же спека
// `/openapi.yaml` ТЕПЕРЬ за `RequireJWT` (вне `/v1`, без RBAC/audit), см.
// router.go.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
)

// sseQueryTokenParam — имя query-param, через который SSE-endpoint-ы
// принимают JWT в обход `Authorization`-header-а.
//
// EXCEPTION для browser-native EventSource: спецификация EventSource НЕ
// позволяет задавать custom-заголовки, поэтому UI не может отправить
// `Authorization: Bearer …` на SSE-канал (`text/event-stream`). Это
// единственный санкционированный способ передать JWT в URL; для общего
// использования (любой не-SSE запрос) он намеренно игнорируется — токен в
// URL утёк бы в access-логи / referer / историю.
const sseQueryTokenParam = "access_token"

// claimsCtxKey — non-exported тип для context-key, чтобы исключить
// случайные коллизии между пакетами (Go-idiom для context keys).
type claimsCtxKey struct{}

// RequireJWT — middleware-фабрика: возвращает middleware, который
// извлекает Bearer-токен, валидирует через v.Verify и пропускает запрос
// дальше с claims в context. При любой ошибке возвращает 401 в форме
// problem+json.
//
// Принимается заголовок `Authorization: Bearer <token>`. Единственное
// исключение — SSE-endpoint-ы (см. [isSSERequest]): для них при отсутствии
// header-а токен берётся из query-param `access_token` (EXCEPTION для
// browser EventSource, который не умеет custom-заголовки — НЕ для общего
// использования). На любом не-SSE запросе query-param игнорируется.
func RequireJWT(v *jwt.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := extractToken(r)
			if !ok {
				problem.Write(w, problem.New(
					problem.TypeUnauthenticated,
					r.URL.Path,
					"missing or malformed Authorization header (expect: Bearer <jwt>)",
				))
				return
			}

			claims, err := v.Verify(token)
			if err != nil {
				// detail-строки строятся ТОЛЬКО через jwt.ClassifyVerifyErr —
				// никогда не пробрасывать err.Error() в HTTP-ответ (raw
				// сообщение golang-jwt/v5 = oracle-attack surface).
				problem.Write(w, problem.New(
					problem.TypeUnauthenticated,
					r.URL.Path,
					jwt.ClassifyVerifyErr(err),
				))
				return
			}

			ctx := context.WithValue(r.Context(), claimsCtxKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractToken достаёт JWT из запроса. Приоритет — `Authorization: Bearer`.
// Если header-а нет, но запрос — SSE (см. [isSSERequest]), допускается
// query-param `access_token` (browser EventSource limit). На любом не-SSE
// запросе query-param НЕ читается: токен в URL — security-floor только для
// потокового канала.
func extractToken(r *http.Request) (string, bool) {
	if tok, ok := jwt.ParseBearerToken(r.Header.Get("Authorization")); ok {
		return tok, true
	}
	if isSSERequest(r) {
		if tok := r.URL.Query().Get(sseQueryTokenParam); tok != "" {
			return tok, true
		}
	}
	return "", false
}

// isSSERequest — true для SSE-канала: GET с `Accept: text/event-stream`
// ИЛИ путь, оканчивающийся на `/events` (`GET /v1/voyages/{id}/events`).
// Query-token допустим строго на таких запросах — не на mutating-методах
// (POST/PUT/DELETE/PATCH) и не на обычных GET.
func isSSERequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return true
	}
	return strings.HasSuffix(r.URL.Path, "/events")
}

// ClaimsFromContext возвращает claims, положенные [RequireJWT] в context.
// ok=false означает, что middleware не отработал (например, handler
// прицеплен напрямую к роутеру без auth-chain) — endpoint-у в `/v1/*`
// это сигнал ошибки конфигурации сервера.
func ClaimsFromContext(ctx context.Context) (*jwt.Claims, bool) {
	c, ok := ctx.Value(claimsCtxKey{}).(*jwt.Claims)
	return c, ok
}

// InjectClaimsForTest кладёт claims в context напрямую — helper для
// unit-тестов handler-ов, которые не поднимают RequireJWT middleware.
// Не использовать вне *_test.go-кода: production-handler-ы должны
// получать claims только через RequireJWT-цепочку.
func InjectClaimsForTest(ctx context.Context, c *jwt.Claims) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, c)
}

// WithClaims кладёт claims в context. Используется in-memory invokation
// HTTP-handler-ов из MCP-tool-ов (httptest.Recorder + ClaimsFromContext —
// claims приходят не из RequireJWT, а из MCP-auth-chain).
//
// От [InjectClaimsForTest] отличается только namespace (test-only vs prod-
// доступный); реализация одна. Сохраняем оба, чтобы тестовые call-сайты не
// меняли смысл (test-helper остался test-helper-ом).
func WithClaims(ctx context.Context, c *jwt.Claims) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, c)
}
