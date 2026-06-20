package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
)

// uiPrefix — URL-префикс встроенного UI. Слэш на конце обязателен для отдачи
// статики (http.FileServer резолвит пути относительно него); голый /ui
// редиректится на /ui/ (см. Mount).
const uiPrefix = "/ui/"

// indexFile — SPA-точка входа. Любой под-путь /ui/, не являющийся реальным
// файлом embed-дерева, отдаёт её (client-side роутинг), а не 404.
const indexFile = "index.html"

// Mount вешает публичные роуты встроенного UI на корневой chi-router (ВНЕ /v1,
// parity с /docs): без RequireJWT/RBAC/audit/maxBody/metrics-обвязки. Статика
// публична намеренно (JS/CSS/HTML — не секрет, ADR-055 §в); защищён API.
//
// Топология:
//   - GET /ui            → 308 redirect на /ui/ (канонизация без слэша; 308
//     сохраняет метод, хотя для GET это безразлично — симметрия с http.Redirect-
//     практикой статик-серверов).
//   - GET /ui/           → index.html (корень SPA).
//   - GET /ui/<file>     → реальный файл embed-дерева (assets/<file>), напр.
//     /ui/assets/app.js.
//   - GET /ui/<route>    → index.html, если <route> не резолвится в файл
//     (SPA-fallback для deep-link client-side роутера вроде /ui/incarnations/42).
//
// Directory-listing отключён (sub-FS не отдаётся как индекс — fallback на
// index.html), path-traversal невозможен (раздача из embed.FS, не disk-FS).
func Mount(r chi.Router) {
	// fs.Sub снимает корневую папку assets/: embed-дерево держит ассеты под
	// assets/, но раздаём их под /ui/ напрямую (assets/index.html → /ui/).
	// Ошибка тут невозможна при валидном go:embed-дереве (assets/ существует);
	// при пустом дереве (забытый снапшот) Open вернёт ошибку на запросе, а не
	// на mount-е — отловится guard-тестом/ревью.
	sub, err := fs.Sub(FS, "assets")
	if err != nil {
		panic("webui: fs.Sub(assets): " + err.Error())
	}

	r.Get("/ui", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, uiPrefix, http.StatusPermanentRedirect)
	})
	r.Get(uiPrefix+"*", spaHandler(sub))
}

// spaHandler отдаёт статику из встроенного дерева с SPA-fallback на index.html.
// Реальный файл → как файл; несуществующий под-путь → index.html (200).
func spaHandler(sub fs.FS) http.HandlerFunc {
	fileServer := http.StripPrefix(uiPrefix, http.FileServer(http.FS(sub)))
	return func(w http.ResponseWriter, req *http.Request) {
		// rel — путь внутри embed-дерева (без /ui/-префикса). "" / "/" → корень
		// SPA: отдаём index.html напрямую (FileServer на "" попытался бы выдать
		// directory-listing).
		rel := strings.TrimPrefix(req.URL.Path, uiPrefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			serveIndex(w, req, sub)
			return
		}

		// Реальный файл embed-дерева → отдаём http.FileServer-ом (корректный
		// Content-Type по расширению). Иначе (под-путь client-side роутера) →
		// SPA-fallback на index.html. path.Clean нормализует ./.. до проверки;
		// embed.FS сам по себе устойчив к traversal, но чистый rel убирает
		// ложные промахи Stat на "a/./b".
		if isFile(sub, path.Clean(rel)) {
			fileServer.ServeHTTP(w, req)
			return
		}
		serveIndex(w, req, sub)
	}
}

// isFile сообщает, существует ли в дереве обычный файл по rel-пути. Каталог
// файлом не считается (его запрос уходит в SPA-fallback, не в directory-listing).
func isFile(sub fs.FS, rel string) bool {
	info, err := fs.Stat(sub, rel)
	return err == nil && !info.IsDir()
}

// serveIndex отдаёт index.html (200) — корень SPA и fallback для client-side
// маршрутов. http.ServeContent выставляет Content-Type/Length по содержимому и
// уважает conditional-заголовки.
func serveIndex(w http.ResponseWriter, req *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, indexFile)
	if err != nil {
		http.Error(w, "ui index not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
