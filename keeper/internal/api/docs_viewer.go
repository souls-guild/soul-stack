package api

// Визуальный OpenAPI-вьювер GET /docs — механизм A (ADR-054 doc-viewer):
//
//   - GET /docs — ПУБЛИЧНЫЙ shell (вне /v1, без auth). Отдаёт HTML-страницу с
//     полем ввода Archon JWT. Неавторизованный видит ТОЛЬКО поле ввода —
//     API-поверхность не раскрыта (она приходит лишь после успешного fetch
//     спеки за JWT). XSS-гигиена: токен держим в sessionStorage (вкладка),
//     НЕ в localStorage (переживает закрытие/доступен другим вкладкам).
//   - GET /docs/assets/* — ПУБЛИЧНАЯ статика RapiDoc (web-component rapidoc-min.js,
//     go:embed из пакета docsassets; стили у RapiDoc в Shadow DOM, отдельного CSS
//     нет). Только GET, неизменна за процесс.
//   - Сама спека GET /openapi.json — ЗА JWT (router.go), фетчится страницей с
//     Bearer-заголовком и рендерится RapiDoc ИНЛАЙН через метод loadSpec(obj):
//     RapiDoc.loadSpec трактует СТРОКУ как spec-URL (а url-фетч RapiDoc не несёт
//     наш Bearer и упрётся в 401), поэтому отдаём JSON и подаём РАЗОБРАННЫЙ
//     объект. Метод зовём после customElements.whenDefined('rapi-doc'). Тот же
//     JWT прокидывается в <rapi-doc> для «Try It» через setApiKey(bearerAuth, jwt)
//     — ЧИСТЫЙ jwt без префикса (RapiDoc сам добавит 'Bearer ' для http/bearer).
//
// БЕЗОПАСНОСТЬ: shell и ассеты публичны намеренно — они не содержат ни данных,
// ни описания API; всё чувствительное (спека + Try It) требует валидный JWT,
// проверяемый тем же RequireJWT, что и /v1. Mount ВНЕ /v1 → без RBAC/audit/
// maxBody/metrics (как /healthz/openapi.yaml).

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/docsassets"
)

// docsAssetsPrefix — URL-префикс, под которым раздаётся вшитая статика RapiDoc.
// Совпадает с путём в docsPage (<script src>).
const docsAssetsPrefix = "/docs/assets/"

// mountDocsViewer вешает публичные роуты вьювера на корневой router (ВНЕ /v1):
// GET /docs (shell) и GET /docs/assets/* (вшитые ассеты). Вызывается из
// buildRouter рядом с health/meta-mount-ом.
func mountDocsViewer(r chi.Router) {
	r.Get("/docs", docsShellHandler)
	// http.FileServer на embed.FS отдаёт rapidoc-min.js с корректным Content-Type
	// (по расширению). StripPrefix снимает /docs/assets/.
	assetServer := http.StripPrefix(docsAssetsPrefix, http.FileServer(http.FS(docsassets.FS)))
	r.Get(docsAssetsPrefix+"*", func(w http.ResponseWriter, req *http.Request) {
		assetServer.ServeHTTP(w, req)
	})
}

// docsShellHandler отдаёт HTML shell-страницу вьювера (200, text/html).
func docsShellHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsPage))
}

// docsPage — статичный HTML shell вьювера. Самодостаточен: тянет вшитую статику
// RapiDoc с /docs/assets/, спеку — fetch'ем /openapi.json с Bearer-заголовком.
// Inline-render через loadSpec(ОБЪЕКТ): RapiDoc трактует СТРОКУ как spec-URL и
// сам фетчит её БЕЗ нашего Bearer (→ 401), поэтому отдаём JSON и подаём
// разобранный объект. setApiKey несёт тот же JWT в "Try It" (bearerAuth).
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
  <!-- show-header=false прячет встроенную шапку RapiDoc с полем spec-url (грузим
       спеку сами, инлайн). allow-advanced-search — это и есть full-text поиск.
       allow-spec-url-load/allow-spec-file-load=false — оператор не должен
       подгружать чужие спеки в наш вьювер. -->
  <rapi-doc id="viewer" show-header="false" theme="light" render-style="read"
            allow-search="true" allow-advanced-search="true" allow-try="true"
            allow-authentication="true" allow-spec-url-load="false"
            allow-spec-file-load="false" style="display:none;height:100vh"></rapi-doc>

  <!-- Бандл подключаем В КОНЦЕ body (не в <head>): ~840КБ web-component не
       блокирует отрисовку gate, поле JWT видно сразу. defer для скрипта в конце
       body избыточен (исполнение и так после парса DOM) — оставлен явным
       маркером неблокирующей загрузки. -->
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
            // XSS-гигиена: токен только на время вкладки (per-tab), не персистентно.
            sessionStorage.setItem('soulstack_jwt', jwt);
            // RapiDoc.loadSpec ждёт ОБЪЕКТ (строку он трактует как spec-URL и
            // фетчит её БЕЗ нашего Bearer → 401), поэтому подаём уже разобранный
            // JSON. Ждём регистрацию <rapi-doc> бандлом (whenDefined) — иначе
            // вызов сядет на ещё-неапгрейженный элемент. setApiKey несёт тот же
            // JWT в "Try It"; передаём ЧИСТЫЙ jwt — RapiDoc сам добавит префикс
            // 'Bearer ' для http/bearer-схемы bearerAuth.
            customElements.whenDefined('rapi-doc').then(function () {
              viewer.loadSpec(specObj);
              try {
                viewer.setApiKey('bearerAuth', jwt);
              } catch (e) { /* схема не найдена — Try It без префилла, рендер не страдает */ }
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

      // Авто-восстановление из sessionStorage (тот же таб после reload).
      var saved = sessionStorage.getItem('soulstack_jwt');
      if (saved) { input.value = saved; load(saved); }
    })();
  </script>
</body>
</html>
`
