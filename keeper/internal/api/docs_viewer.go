package api

// Visual OpenAPI viewer GET /docs — mechanism A (ADR-054 doc-viewer):
//
//   - GET /docs — PUBLIC shell (outside /v1, no auth). Serves an HTML page with
//     an Archon JWT input field. Unauthenticated users see ONLY the input — the
//     API surface is not exposed (it arrives only after a successful spec fetch
//     behind the JWT). XSS hygiene: the token lives in sessionStorage (per-tab),
//     NOT in localStorage (which survives close / is shared across tabs).
//   - GET /docs/assets/* — PUBLIC RapiDoc static assets (web-component
//     rapidoc-min.js, go:embed from package docsassets; RapiDoc styles live in
//     Shadow DOM, there is no separate CSS). GET only, immutable per process.
//   - The spec itself GET /openapi.json — BEHIND the JWT (router.go), fetched by
//     the page with a Bearer header and rendered by RapiDoc INLINE via
//     loadSpec(obj): RapiDoc.loadSpec treats a STRING as a spec-URL (and a
//     RapiDoc url-fetch does not carry our Bearer and hits 401), so we return
//     JSON and hand it the PARSED object. Call it after
//     customElements.whenDefined('rapi-doc'). The same JWT is passed into
//     <rapi-doc> for "Try It" via setApiKey(bearerAuth, jwt) — the RAW jwt with
//     no prefix (RapiDoc adds 'Bearer ' itself for http/bearer).
//
// SECURITY: the shell and assets are public on purpose — they carry neither data
// nor an API description; everything sensitive (spec + Try It) requires a valid
// JWT, checked by the same RequireJWT as /v1. Mounted OUTSIDE /v1 → no RBAC/audit/
// maxBody/metrics (like /healthz/openapi.yaml).

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/docsassets"
)

// docsAssetsPrefix — the URL prefix under which the embedded RapiDoc static
// assets are served. Matches the path in docsPage (<script src>).
const docsAssetsPrefix = "/docs/assets/"

// mountDocsViewer wires the viewer's public routes onto the root router (OUTSIDE
// /v1): GET /docs (shell) and GET /docs/assets/* (embedded assets). Called from
// buildRouter next to the health/meta mount.
func mountDocsViewer(r chi.Router) {
	r.Get("/docs", docsShellHandler)
	// http.FileServer over embed.FS serves rapidoc-min.js with the correct
	// Content-Type (by extension). StripPrefix strips /docs/assets/.
	assetServer := http.StripPrefix(docsAssetsPrefix, http.FileServer(http.FS(docsassets.FS)))
	r.Get(docsAssetsPrefix+"*", func(w http.ResponseWriter, req *http.Request) {
		assetServer.ServeHTTP(w, req)
	})
}

// docsShellHandler serves the viewer's HTML shell page (200, text/html).
func docsShellHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsPage))
}

// docsPage — the viewer's static HTML shell. Self-contained: pulls the embedded
// RapiDoc assets from /docs/assets/, and the spec via a fetch of /openapi.json
// with a Bearer header. Inline render via loadSpec(OBJECT): RapiDoc treats a
// STRING as a spec-URL and fetches it WITHOUT our Bearer (→ 401), so we return
// JSON and hand it the parsed object. setApiKey carries the same JWT into
// "Try It" (bearerAuth).
const docsPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Soul Stack Keeper — Operator API</title>
  <style>
    body { margin: 0; font-family: system-ui, sans-serif; }
    #gate { padding: 24px; max-width: 720px; }
    #gate h1 { font-size: 18px; margin: 0 0 12px; }
    #gate p { color: #555; font-size: 14px; margin: 0 0 16px; }
    #gate input { width: 100%; box-sizing: border-box; padding: 8px 10px;
      font-family: monospace; font-size: 13px; border: 1px solid #ccc; border-radius: 4px; }
    #gate button { margin-top: 12px; padding: 8px 18px; font-size: 14px; cursor: pointer;
      border: 1px solid #2563eb; background: #2563eb; color: #fff; border-radius: 4px; }
    #gate button:hover { background: #1d4ed8; }
    #err { color: #b91c1c; font-size: 13px; margin-top: 12px; min-height: 18px; }
    #viewer { display: none; height: 100vh; }
  </style>
</head>
<body>
  <div id="gate">
    <h1>Soul Stack Keeper — Operator API</h1>
    <p>Paste your Archon JWT to load the API reference. The token is kept only
       in this browser tab (per-tab session storage) and sent as a Bearer header
       to fetch the spec; it is never stored persistently.</p>
    <input id="jwt" type="password" autocomplete="off" spellcheck="false"
           placeholder="Paste your Archon JWT">
    <button id="load" type="button">Load</button>
    <div id="err"></div>
  </div>
  <!-- show-header=false hides RapiDoc's built-in header with spec-url field (we load
       the spec ourselves, inline). allow-advanced-search enables full-text search.
       allow-spec-url-load/allow-spec-file-load=false prevents the operator from
       loading external specs into our viewer. -->
  <rapi-doc id="viewer" show-header="false" theme="light" render-style="read"
            allow-search="true" allow-advanced-search="true" allow-try="true"
            allow-authentication="true" allow-spec-url-load="false"
            allow-spec-file-load="false" style="display:none;height:100vh"></rapi-doc>

  <!-- Bundle is loaded at the END of body (not in <head>): ~840KB web-component
       does not block gate rendering, JWT field is visible immediately. defer is
       redundant for a script at the end of body (execution is after DOM parse),
       kept as explicit non-blocking marker. -->
  <script defer src="/docs/assets/rapidoc-min.js"></script>

  <script>
    (function () {
      var gate = document.getElementById('gate');
      var viewer = document.getElementById('viewer');
      var input = document.getElementById('jwt');
      var err = document.getElementById('err');

      function load(jwt) {
        err.textContent = '';
        fetch('/openapi.json', { headers: { 'Authorization': 'Bearer ' + jwt, 'Accept': 'application/json' } })
          .then(function (resp) {
            if (resp.status === 401) {
              throw new Error('invalid or expired JWT — paste a fresh Archon token');
            }
            if (!resp.ok) {
              throw new Error('failed to load spec (HTTP ' + resp.status + ')');
            }
            return resp.json();
          })
          .then(function (specObj) {
            // XSS hygiene: token stored per-tab only (sessionStorage), not persistent.
            sessionStorage.setItem('soulstack_jwt', jwt);
            // RapiDoc.loadSpec expects an OBJECT (treats a string as spec-URL and
            // fetches it WITHOUT our Bearer → 401). We provide parsed JSON instead.
            // Wait for <rapi-doc> registration by bundle (whenDefined), else the call
            // will hit an unupgraded element. setApiKey carries the same JWT into
            // "Try It"; pass raw jwt — RapiDoc adds 'Bearer ' prefix itself for
            // http/bearer auth scheme.
            customElements.whenDefined('rapi-doc').then(function () {
              viewer.loadSpec(specObj);
              try {
                viewer.setApiKey('bearerAuth', jwt);
              } catch (e) { /* schema not found — Try It without prefill, rendering unaffected */ }
              gate.style.display = 'none';
              viewer.style.display = 'block';
            });
          })
          .catch(function (e) {
            err.textContent = e.message;
            sessionStorage.removeItem('soulstack_jwt');
          });
      }

      document.getElementById('load').addEventListener('click', function () {
        var jwt = input.value.trim();
        if (jwt) { load(jwt); }
      });
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Enter') {
          var jwt = input.value.trim();
          if (jwt) { load(jwt); }
        }
      });

      // Auto-restore from sessionStorage (same tab after reload).
      var saved = sessionStorage.getItem('soulstack_jwt');
      if (saved) { input.value = saved; load(saved); }
    })();
  </script>
</body>
</html>
`
