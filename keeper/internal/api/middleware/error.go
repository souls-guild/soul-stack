// Convenience wrappers for writing RFC 7807 problem+json to an HTTP response.
// type→status routing lives in [problem]; here — shortcuts for the most
// common cases (auth/404/500/malformed).
package middleware

import (
	"net/http"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// WriteUnauthenticated writes 401 problem+json for cases where
// authentication is required but not provided/invalid. Do not
// use for an authorization denial — use WriteForbidden for that.
func WriteUnauthenticated(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeUnauthenticated, r.URL.Path, detail))
}

// WriteForbidden writes 403 problem+json. Used by the RBAC middleware
// (M0.6b) for permission failures.
func WriteForbidden(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeForbidden, r.URL.Path, detail))
}

// WriteNotFound writes 404 problem+json. Used for nonexistent
// paths (default chi handler) and for nonexistent resources in endpoints.
func WriteNotFound(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeNotFound, r.URL.Path, detail))
}

// WriteMalformed writes 400 problem+json (bad JSON, invalid query
// params, etc.).
func WriteMalformed(w http.ResponseWriter, r *http.Request, detail string) {
	problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, detail))
}

// Write405 writes 405 problem+json with an `Allow` header (RFC 7231 §6.5.5
// requires Allow for 405). allowedMethods — the list of allowed methods
// for the given path; empty is acceptable if the caller does not know (then
// Allow is not written).
//
// Used by the chi.MethodNotAllowed handler: semantically a POST to a
// GET-only endpoint is "method not allowed", not a "request syntax
// error" (4xx ≠ 400).
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

// WriteInternal writes 500 problem+json. No detailed diagnostics in
// `detail` — the client gets a generic message, the real cause goes
// to logs/OTel-trace (M0.6+).
func WriteInternal(w http.ResponseWriter, r *http.Request) {
	problem.Write(w, problem.New(
		problem.TypeInternalError,
		r.URL.Path,
		"internal error; see server logs for trace details",
	))
}
