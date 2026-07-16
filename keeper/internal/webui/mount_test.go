package webui

// Guard tests for embed-UI mechanism (ADR-055, slice map p.2):
//
//   - /ui      -> redirect to /ui/ (canonicalization without slash).
//   - /ui/     -> 200 + real vite build index.html marker (SPA root).
//   - /ui/<client-route> (not file) -> 200 + index.html (SPA fallback, NOT 404).
//   - /ui/assets/<hashed>.js (real hash chunk) -> 200 (file serving, not fallback).
//
// Tests run Mount on a bare chi router; embed mechanism and SPA fallback are
// isolated from buildRouter wrapping (toggle-off and /v1 regression are checked
// in api/webui_routes_test.go through real buildRouter).
//
// Markers are content-stable across UI rebuilds: hash chunk names change on
// each build, so concrete asset is resolved DYNAMICALLY through ReadDir
// (firstAssetFile), while index.html is asserted by structural vite-SPA markers
// (`<div id="root"` + base prefix `/ui/assets/` in injected links), which
// survive copy and rebuild changes.

import (
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// uiRouter builds a bare chi router with only webui mount.
func uiRouter() http.Handler {
	r := chi.NewRouter()
	Mount(r)
	return r
}

// indexMarkers are structural markers of real index.html (vite-SPA):
//   - SPA mount node (`<div id="root"`) exists in every vite React build;
//   - base prefix `/ui/assets/` in injected <script>/<link> follows from
//     vite base:'/ui/' and proves the bundle is meant to be served under /ui.
//
// Both are independent of copy/hashes -> survive UI rebuilds.
var indexMarkers = []string{`<div id="root"`, `/ui/assets/`}

// firstAssetFile finds the first real asset file in embed tree (assets/<f>) so
// the "real file served as file" test does not depend on a concrete hash chunk
// name (changes on every vite build). It returns rel path inside embed tree
// (including root assets/ directory), suitable for URL /ui/<rel>.
func firstAssetFile(t *testing.T) string {
	t.Helper()
	sub, err := fs.Sub(FS, "assets")
	if err != nil {
		t.Fatalf("fs.Sub(assets): %v", err)
	}
	entries, err := fs.ReadDir(sub, "assets")
	if err != nil {
		t.Fatalf("ReadDir assets/assets: %v (is real dist vendored? see scripts/sync-webui.sh)", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			return "assets/" + e.Name()
		}
	}
	t.Fatal("assets/assets/ has no files; real vite build is not vendored")
	return ""
}

// firstAssetWithExt finds the first asset file with the given extension (e.g.
// .js) in assets/assets/. It returns rel path inside embed tree, or "" if the
// bundle has no such extension (test case then skips, see call).
func firstAssetWithExt(t *testing.T, ext string) string {
	t.Helper()
	sub, err := fs.Sub(FS, "assets")
	if err != nil {
		t.Fatalf("fs.Sub(assets): %v", err)
	}
	entries, err := fs.ReadDir(sub, "assets")
	if err != nil {
		t.Fatalf("ReadDir assets/assets: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() && path.Ext(e.Name()) == ext {
			return "assets/" + e.Name()
		}
	}
	return ""
}

// assertHasAllMarkers verifies that body contains all structural index.html markers.
func assertHasAllMarkers(t *testing.T, body, what string) {
	t.Helper()
	for _, m := range indexMarkers {
		if !strings.Contains(body, m) {
			t.Errorf("%s does not contain index.html marker %q", what, m)
		}
	}
}

// TestUI_BareRedirectsToSlash verifies GET /ui redirects to /ui/ (3xx).
func TestUI_BareRedirectsToSlash(t *testing.T) {
	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui", http.NoBody))

	if rec.Code < 300 || rec.Code >= 400 {
		t.Fatalf("GET /ui = %d, want 3xx redirect to /ui/", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("Location = %q, want /ui/", loc)
	}
}

// TestUI_RootServesIndex verifies GET /ui/ returns 200 + index.html markers.
func TestUI_RootServesIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/ = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type /ui/ = %q, want text/html…", ct)
	}
	assertHasAllMarkers(t, rec.Body.String(), "/ui/")
}

// TestUI_SPAFallback verifies that missing /ui/ subpath (client-side route)
// returns index.html (200), NOT 404. Regression (404 on deep-link) would break
// SPA routing.
func TestUI_SPAFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/incarnations/42", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/incarnations/42 (client-route) = %d, want 200 SPA-fallback; body=%s",
			rec.Code, rec.Body.String())
	}
	assertHasAllMarkers(t, rec.Body.String(), "SPA-fallback")
}

// TestUI_RealAssetServedAsFile verifies that real embed-tree file is served as
// file (not replaced with index.html). Concrete hash chunk is resolved
// dynamically.
func TestUI_RealAssetServedAsFile(t *testing.T) {
	asset := firstAssetFile(t) // e.g. assets/index-XXXX.js

	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/"+asset, http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/%s = %d, want 200; body=%s", asset, rec.Code, rec.Body.String())
	}
	// Real asset is NOT index.html: SPA fallback must not consume an existing
	// file. Verify by absence of index.html root node (asset is JS/CSS, not HTML).
	if strings.Contains(rec.Body.String(), `<div id="root"`) {
		t.Errorf("/ui/%s returned index.html instead of real asset (SPA fallback consumed existing file)", asset)
	}
	// Content-Type matches file extension (http.FileServer mime), NOT text/html:
	// substituting text/html for asset breaks MIME-strict module loading in
	// browser. Expected type is derived from served asset extension (hash chunk
	// name changes on every build, extension does not).
	gotCT := rec.Header().Get("Content-Type")
	if strings.HasPrefix(gotCT, "text/html") {
		t.Errorf("/ui/%s served with Content-Type %q (text/html); asset was replaced with index.html", asset, gotCT)
	}
	if wantCT := mime.TypeByExtension(path.Ext(asset)); wantCT != "" && gotCT != wantCT {
		t.Errorf("Content-Type /ui/%s = %q, want %q (by extension)", asset, gotCT, wantCT)
	}
}

// TestUI_RealJSAssetContentType verifies that real .js asset is served as
// text/javascript (not text/html / octet-stream). MIME type regression breaks
// <script type=module> (browser rejects strict MIME check). If tree has no .js,
// skip (structural invariant, no concrete bundle required).
func TestUI_RealJSAssetContentType(t *testing.T) {
	asset := firstAssetWithExt(t, ".js")
	if asset == "" {
		t.Skip("assets/ has no .js asset; nothing to verify MIME invariant against")
	}

	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/"+asset, http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/%s = %d, want 200; body=%s", asset, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type /ui/%s = %q, want text/javascript… (MIME-strict <script type=module>)", asset, ct)
	}
}

// TestUI_NonGetMethodNotAllowed verifies /ui/ accepts only GET (method
// allowlist). POST/PUT/DELETE -> 405. Regression (accidental r.Post/r.Handle on
// /ui/*) would open a mutating method on public static files; static is
// read-only, mutation methods do not belong here.
func TestUI_NonGetMethodNotAllowed(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		uiRouter().ServeHTTP(rec, httptest.NewRequest(m, "/ui/", http.NoBody))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /ui/ = %d, want 405 (GET only on static)", m, rec.Code)
		}
	}
}

// TestUI_PathTraversalContained verifies path traversal does NOT leave embed.FS.
// `../`, nested `../`, %2e%2e-encoded are all normalized and resolved inside
// tree: 200 + index.html (SPA fallback), NEVER external FS content (/etc/passwd).
// ADR-055 §security names contained traversal as invariant: regression
// (embed.FS -> http.Dir / serving by raw path) would silently open a leak.
func TestUI_PathTraversalContained(t *testing.T) {
	for _, p := range []string{
		"/ui/../../etc/passwd",
		"/ui/assets/../../../etc/passwd",
		"/ui/%2e%2e/%2e%2e/etc/passwd",
		"/ui/..%2f..%2fetc%2fpasswd",
	} {
		rec := httptest.NewRecorder()
		uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, http.NoBody))

		body := rec.Body.String()
		// No disk leak: /etc/passwd marker does not appear for any code.
		if strings.Contains(body, "root:") || strings.Contains(body, "/bin/bash") {
			t.Fatalf("GET %s leaked external FS content (code=%d): %.80q", p, rec.Code, body)
		}
		// Traversal safely collapses into embed tree: either index.html (200), or
		// refusal (3xx/4xx), but not 200 with NON-UI content. It is enough to
		// check that 200 response is exactly SPA index.html, not something from
		// disk-FS.
		if rec.Code == http.StatusOK {
			assertHasAllMarkers(t, body, "traversal "+p)
		}
	}
}

// TestUI_DirectoryNoListing verifies directory request (/ui/assets/) does NOT
// return directory listing, but collapses to SPA index (200 + index.html).
// Regression (raw http.FileServer on directory) would expose bundle file tree.
func TestUI_DirectoryNoListing(t *testing.T) {
	for _, p := range []string{"/ui/assets/", "/ui/assets"} {
		rec := httptest.NewRecorder()
		uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, http.NoBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (SPA index, not listing)", p, rec.Code)
		}
		body := rec.Body.String()
		assertHasAllMarkers(t, body, "directory "+p)
		// Go-style directory listing marker is autoindex page header.
		if strings.Contains(body, "<title>") && strings.Contains(strings.ToLower(body), "index of") {
			t.Errorf("GET %s returned directory listing instead of SPA index", p)
		}
	}
}
