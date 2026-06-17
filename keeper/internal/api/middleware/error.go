// Convenience-обёртки для записи RFC 7807 problem+json в HTTP-ответ.
// Маршрутизация type→status вынесена в [problem]; здесь — short-cut-ы
// для наиболее частых случаев (auth/404/500/malformed).
package middleware

import (
	"net/http"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// WriteUnauthenticated пишет 401 problem+json для случаев, когда
// аутентификация требуется, но не предоставлена/невалидна. Не
// использовать для отказа в authorization — для этого WriteForbidden.
func WriteUnauthenticated(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeUnauthenticated, r.URL.Path, detail))
}

// WriteForbidden пишет 403 problem+json. Используется RBAC-middleware
// (M0.6b) для permission-failure.
func WriteForbidden(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
}

// WriteNotFound пишет 404 problem+json. Используется для несуществующих
// path-ов (default chi-handler) и для несуществующих ресурсов в endpoint-ах.
func WriteNotFound(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeNotFound, r.URL.Path, detail))
}

// WriteMalformed пишет 400 problem+json (плохой JSON, неверные query-
// params, и т.п.).
func WriteMalformed(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, detail))
}

// Write405 пишет 405 problem+json с заголовком `Allow` (RFC 7231 §6.5.5
// требует Allow для 405). allowedMethods — список допустимых методов
// для данного path-а; пустой допустим, если caller не знает (тогда
// Allow не пишется).
//
// Используется chi.MethodNotAllowed-handler-ом: семантически POST на
// GET-only endpoint — это «метод недопустим», а не «синтаксическая
// ошибка запроса» (4xx ≠ 400).
func Write405(w http.ResponseWriter, r *http.Request, allowedMethods ...string) {
	if len(allowedMethods) > 0 {
		w.Header().Set("Allow", strings.Join(allowedMethods, ", "))
	}
	problem.Write(w, problem.New(
		problem.TypeMethodNotAllowed,
		r.URL.Path,
		"method "+r.Method+" is not allowed for this endpoint",
	))
}

// WriteInternal пишет 500 problem+json. Никакой детальной диагностики в
// `detail` — клиент получает generic-сообщение, реальная причина уезжает
// в логи/OTel-trace (M0.6+).
func WriteInternal(w http.ResponseWriter, r *http.Request) {
	problem.Write(w, problem.New(
		problem.TypeInternalError,
		r.URL.Path,
		"internal error; see server logs for trace details",
	))
}
