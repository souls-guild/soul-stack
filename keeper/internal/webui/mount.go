package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
)

// uiPrefix is URL prefix of embedded UI. Trailing slash is required for serving
// static files (http.FileServer resolves paths relative to it); bare /ui
// redirects to /ui/ (see Mount).
const uiPrefix = "/ui/"

// indexFile is SPA entry point. Any /ui/ subpath that is not a real file in the
// embed tree serves it (client-side routing), not 404.
const indexFile = "index.html"

// Mount attaches public embedded UI routes to root chi-router (OUTSIDE /v1,
// parity with /docs): without RequireJWT/RBAC/audit/maxBody/metrics wrapping.
// Static files are intentionally public (JS/CSS/HTML are not secret, ADR-055
// §c); API is protected.
//
// Topology:
//   - GET /ui            -> 308 redirect to /ui/ (canonicalization without slash;
//     308 preserves method, although for GET it is irrelevant; symmetry with
//     http.Redirect practice of static servers).
//   - GET /ui/           -> index.html (SPA root).
//   - GET /ui/<file>     -> real embed-tree file (assets/<file>), e.g.
//     /ui/assets/app.js.
//   - GET /ui/<route>    -> index.html if <route> does not resolve to file
//     (SPA fallback for client-side router deep-link like /ui/incarnations/42).
//
// Directory listing is disabled (sub-FS is not served as index; fallback to
// index.html), path traversal is impossible (serving from embed.FS, not disk-FS).
func Mount(r chi.Router) {
	// fs.Sub strips root assets/ directory: embed tree keeps assets under assets/,
	// but we serve them under /ui/ directly (assets/index.html -> /ui/). Error is
	// impossible with a valid go:embed tree (assets/ exists); with an empty tree
	// (forgotten snapshot), Open returns error on request, not on mount, and a
	// guard test/review catches it.
	sub, err := fs.Sub(FS, "assets")
	if err != nil {
		panic("webui: fs.Sub(assets): " + err.Error())
	}

	r.Get("/ui", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, uiPrefix, http.StatusPermanentRedirect)
	})
	r.Get(uiPrefix+"*", spaHandler(sub))
}

// spaHandler serves static files from the embedded tree with SPA fallback to
// index.html. Real file -> as file; missing subpath -> index.html (200).
func spaHandler(sub fs.FS) http.HandlerFunc {
	fileServer := http.StripPrefix(uiPrefix, http.FileServer(http.FS(sub)))
	return func(w http.ResponseWriter, req *http.Request) {
		// rel is path inside embed tree (without /ui/ prefix). "" / "/" -> SPA
		// root: serve index.html directly (FileServer on "" would try to return
		// directory-listing).
		rel := strings.TrimPrefix(req.URL.Path, uiPrefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			serveIndex(w, req, sub)
			return
		}

		// Real embed-tree file -> serve with http.FileServer (correct Content-Type
		// by extension). Otherwise (client-side router subpath) -> SPA fallback to
		// index.html. path.Clean normalizes ./.. before check; embed.FS is
		// traversal-resistant by itself, but clean rel removes false Stat misses on
		// "a/./b".
		if isFile(sub, path.Clean(rel)) {
			fileServer.ServeHTTP(w, req)
			return
		}
		serveIndex(w, req, sub)
	}
}

// isFile reports whether a regular file exists in the tree by rel path.
// Directory is not a file (its request goes to SPA fallback, not directory
// listing).
func isFile(sub fs.FS, rel string) bool {
	info, err := fs.Stat(sub, rel)
	return err == nil && !info.IsDir()
}

// serveIndex serves index.html (200): SPA root and fallback for client-side
// routes. http.ServeContent sets Content-Type/Length by content and respects
// conditional headers.
func serveIndex(w http.ResponseWriter, req *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, indexFile)
	if err != nil {
		http.Error(w, "ui index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
