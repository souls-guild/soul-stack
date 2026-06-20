package webui

// Guard-тесты механизма embed-UI (ADR-055, §Слайс-карта п.2):
//
//   - /ui      → редирект на /ui/ (канонизация без слэша).
//   - /ui/     → 200 + маркер index.html реального vite-build (корень SPA).
//   - /ui/<client-route> (не файл) → 200 + index.html (SPA-fallback, НЕ 404).
//   - /ui/assets/<hashed>.js (реальный хеш-чанк) → 200 (отдача файла, не fallback).
//
// Тесты гоняют Mount на голом chi-роутере — embed-механизм и SPA-fallback
// изолированы от buildRouter-обвязки (тоггл-off и /v1-регресс проверяются
// интеграционно в api/webui_routes_test.go через реальный buildRouter).
//
// Маркеры контентно-устойчивы к пересборке UI: имена хеш-чанков меняются на
// каждом build, поэтому конкретный ассет резолвится ДИНАМИЧЕСКИ через ReadDir
// (firstAssetFile), а index.html ассертится по структурным маркерам vite-SPA
// (`<div id="root"` + base-префикс `/ui/assets/` в инжектированных ссылках),
// которые переживают смену копирайтинга и пересборку.

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

// uiRouter собирает голый chi-роутер только с webui-mount-ом.
func uiRouter() http.Handler {
	r := chi.NewRouter()
	Mount(r)
	return r
}

// indexMarkers — структурные маркеры реального index.html (vite-SPA):
//   - mount-узел SPA (`<div id="root"`) — есть в любом vite-React-build-е;
//   - base-префикс `/ui/assets/` в инжектированных <script>/<link> — следствие
//     vite base:'/ui/', доказывает что бандл рассчитан на отдачу под /ui.
//
// Оба независимы от копирайтинга/хешей → переживают пересборку UI.
var indexMarkers = []string{`<div id="root"`, `/ui/assets/`}

// firstAssetFile находит первый реальный файл-ассет в embed-дереве (assets/<f>),
// чтобы тест «реальный файл отдаётся как файл» не зависел от конкретного
// хеш-имени чанка (меняется на каждом vite-build). Возвращает rel-путь внутри
// embed-дерева (включая корневую папку assets/), пригодный для URL /ui/<rel>.
func firstAssetFile(t *testing.T) string {
	t.Helper()
	sub, err := fs.Sub(FS, "assets")
	if err != nil {
		t.Fatalf("fs.Sub(assets): %v", err)
	}
	entries, err := fs.ReadDir(sub, "assets")
	if err != nil {
		t.Fatalf("ReadDir assets/assets: %v (завендорен ли реальный dist? см. scripts/sync-webui.sh)", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			return "assets/" + e.Name()
		}
	}
	t.Fatal("в assets/assets/ нет файлов — реальный vite-build не завендорен")
	return ""
}

// firstAssetWithExt находит первый файл-ассет с заданным расширением (напр. .js)
// в assets/assets/. Возвращает rel-путь внутри embed-дерева либо "" если такого
// расширения в бандле нет (тест-кейс тогда skip-ается, см. вызов).
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

// assertHasAllMarkers — body содержит все структурные маркеры index.html.
func assertHasAllMarkers(t *testing.T, body, what string) {
	t.Helper()
	for _, m := range indexMarkers {
		if !strings.Contains(body, m) {
			t.Errorf("%s не содержит маркер index.html %q", what, m)
		}
	}
}

// TestUI_BareRedirectsToSlash — GET /ui редиректится на /ui/ (3xx).
func TestUI_BareRedirectsToSlash(t *testing.T) {
	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui", http.NoBody))

	if rec.Code < 300 || rec.Code >= 400 {
		t.Fatalf("GET /ui = %d, want 3xx redirect на /ui/", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("Location = %q, want /ui/", loc)
	}
}

// TestUI_RootServesIndex — GET /ui/ отдаёт 200 + содержит маркеры index.html.
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

// TestUI_SPAFallback — несуществующий под-путь /ui/ (client-side роут) отдаёт
// index.html (200), а НЕ 404. Регресс (404 на deep-link) сломал бы SPA-роутинг.
func TestUI_SPAFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/incarnations/42", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/incarnations/42 (client-route) = %d, want 200 SPA-fallback; body=%s",
			rec.Code, rec.Body.String())
	}
	assertHasAllMarkers(t, rec.Body.String(), "SPA-fallback")
}

// TestUI_RealAssetServedAsFile — реальный файл embed-дерева отдаётся как файл
// (а не подменяется index.html). Конкретный хеш-чанк резолвится динамически.
func TestUI_RealAssetServedAsFile(t *testing.T) {
	asset := firstAssetFile(t) // напр. assets/index-XXXX.js

	rec := httptest.NewRecorder()
	uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/"+asset, http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/%s = %d, want 200; body=%s", asset, rec.Code, rec.Body.String())
	}
	// Реальный ассет — НЕ index.html: SPA-fallback не должен съесть существующий
	// файл. Проверяем отсутствием root-узла index.html (ассет — JS/CSS, не HTML).
	if strings.Contains(rec.Body.String(), `<div id="root"`) {
		t.Errorf("/ui/%s вернул index.html вместо реального ассета (SPA-fallback съел существующий файл)", asset)
	}
	// Content-Type соответствует расширению файла (http.FileServer-mime), а НЕ
	// text/html: подмена text/html на ассет ломает MIME-strict загрузку модулей
	// в браузере. Ожидаемый тип выводим из расширения отданного ассета (имя
	// хеш-чанка меняется на каждом build-е, расширение — нет).
	gotCT := rec.Header().Get("Content-Type")
	if strings.HasPrefix(gotCT, "text/html") {
		t.Errorf("/ui/%s отдан с Content-Type %q (text/html) — ассет подменён index.html", asset, gotCT)
	}
	if wantCT := mime.TypeByExtension(path.Ext(asset)); wantCT != "" && gotCT != wantCT {
		t.Errorf("Content-Type /ui/%s = %q, want %q (по расширению)", asset, gotCT, wantCT)
	}
}

// TestUI_RealJSAssetContentType — реальный .js-ассет отдаётся как text/javascript
// (а не text/html / octet-stream). Регресс MIME-типа ломает <script type=module>
// (браузер отказывает строгой проверке MIME). Если в дереве нет .js — skip
// (структурный инвариант, не требующий конкретного бандла).
func TestUI_RealJSAssetContentType(t *testing.T) {
	asset := firstAssetWithExt(t, ".js")
	if asset == "" {
		t.Skip("в assets/ нет .js-ассета — MIME-инвариант проверять не на чем")
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

// TestUI_NonGetMethodNotAllowed — /ui/ принимает только GET (метод-allowlist).
// POST/PUT/DELETE → 405. Регресс (случайный r.Post/r.Handle на /ui/*) открыл бы
// мутирующий метод на публичной статике — статика read-only, mutation-методам
// тут не место.
func TestUI_NonGetMethodNotAllowed(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		uiRouter().ServeHTTP(rec, httptest.NewRequest(m, "/ui/", http.NoBody))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /ui/ = %d, want 405 (только GET на статике)", m, rec.Code)
		}
	}
}

// TestUI_PathTraversalContained — path-traversal НЕ выводит за пределы embed.FS.
// `../`, вложенный `../`, %2e%2e-encoded — все нормализуются и резолвятся внутри
// дерева: 200 + index.html (SPA-fallback), НИКОГДА не содержимое внешней FS
// (/etc/passwd). ADR-055 §security называет contained-traversal инвариантом:
// регресс (embed.FS → http.Dir / отдача по сырому пути) молча открыл бы leak.
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
		// Никакого disk-leak: маркер /etc/passwd не появляется ни при каком коде.
		if strings.Contains(body, "root:") || strings.Contains(body, "/bin/bash") {
			t.Fatalf("GET %s слил содержимое внешней FS (code=%d): %.80q", p, rec.Code, body)
		}
		// Traversal безопасно сворачивается в embed-дерево: либо index.html (200),
		// либо отказ (3xx/4xx) — но не 200 с НЕ-UI-содержимым. Достаточно проверить,
		// что 200-ответ — это именно index.html SPA, а не нечто из disk-FS.
		if rec.Code == http.StatusOK {
			assertHasAllMarkers(t, body, "traversal "+p)
		}
	}
}

// TestUI_DirectoryNoListing — запрос каталога (/ui/assets/) НЕ отдаёт
// directory-listing, а сворачивается в SPA-index (200 + index.html). Регресс
// (raw http.FileServer на каталоге) раскрыл бы дерево файлов бандла.
func TestUI_DirectoryNoListing(t *testing.T) {
	for _, p := range []string{"/ui/assets/", "/ui/assets"} {
		rec := httptest.NewRecorder()
		uiRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, http.NoBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (SPA-index, не листинг)", p, rec.Code)
		}
		body := rec.Body.String()
		assertHasAllMarkers(t, body, "directory "+p)
		// Признак go-style directory-listing — заголовок autoindex-страницы.
		if strings.Contains(body, "<title>") && strings.Contains(strings.ToLower(body), "index of") {
			t.Errorf("GET %s вернул directory-listing вместо SPA-index", p)
		}
	}
}
