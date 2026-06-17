package toll

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
)

// retryAfterSeconds — Retry-After-заголовок для 503-ответа degraded-middleware.
// Совпадает с DegradedTTL по дефолту (60s): клиент может ретраить после
// estimated max-окна. ADR-038 не фиксирует точное значение — берём симметрично
// окну.
const retryAfterSeconds = 60

// DegradedMiddleware возвращает chi/net-http-совместимый middleware. На каждом
// blocked-запросе (определяется через [BlockedRoute]) middleware читает
// cluster:degraded через [DegradedReader] и при выставленном флаге отвечает
// 503 + Retry-After + application/problem+json, иначе пропускает дальше.
//
// reader=nil → middleware no-op (просто passthrough). Это удобно для wire-up-а
// в daemon: при отсутствии Redis (single-instance/dev) middleware не блокирует
// ничего, и тесты роутера, которым Toll не нужен, не получают сюрпризов.
//
// Логика блокировки:
//  1. Если route НЕ blocked (read-API, RBAC, destroy, Errand) → passthrough.
//     Cheap check FIRST, чтобы не дёргать Redis на каждом GET.
//  2. Read cluster:degraded через DegradedReader. Ошибка → fail-OPEN (пропускаем
//     запрос): доступность важнее перестраховки, флаг гаснет через DegradedTTL
//     если leader умер, а блокировать на флапе Redis — хуже false-negative.
//  3. degraded=true → 503 + Retry-After 60 + problem+json.
//
// Middleware сам решает blocked-route по path+method. Точнее (route-pattern
// matcher) делать НЕЛЬЗЯ на уровне middleware — chi RouteContext доступен
// только под `r.Route(...)`. Поэтому используем явное оборачивание точных
// блокируемых routes в router-е (см. wire-up в api/router.go).
func DegradedMiddleware(reader DegradedReader, logger *slog.Logger) func(http.Handler) http.Handler {
	if reader == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			degraded, err := reader.IsDegraded(r.Context())
			if err != nil {
				// Fail-OPEN: лог debug (Redis-флап — частое явление), пропускаем.
				logger.Debug("toll: degraded check failed — fail-open",
					slog.Any("error", err),
					slog.String("path", r.URL.Path),
				)
				next.ServeHTTP(w, r)
				return
			}
			if !degraded {
				next.ServeHTTP(w, r)
				return
			}
			writeDegraded503(w, r)
		})
	}
}

// writeDegraded503 пишет RFC 7807 problem+json с Retry-After. Format строго
// согласован с keeper/internal/api/problem/TypeClusterDegraded (там же
// зафиксирован status 503 и title). Локальная сборка без зависимости на пакет
// problem — middleware живёт в `keeper/internal/toll/` и не должна тянуть
// `keeper/internal/api/problem/` (циклов нет, но direction зависимости
// `api → toll`, не наоборот).
func writeDegraded503(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	body := map[string]any{
		"type":     "https://soul-stack.io/errors/cluster-degraded",
		"title":    "Cluster is in degraded mode",
		"status":   http.StatusServiceUnavailable,
		"detail":   "Too many Souls disconnected recently; write-API blocked. Retry after 60s.",
		"instance": r.URL.Path,
	}
	_ = json.NewEncoder(w).Encode(body)
}
