// Package webui вшивает (go:embed) собранный статический build-снапшот UI
// (companion-репо soul-stack-web) и раздаёт его keeper-ом на маршруте /ui
// (ADR-055, single-binary keeper с UI).
//
// Механика — калька docsassets/docs_viewer (go:embed-статика, публичный mount
// ВНЕ /v1, как /docs): JS/CSS/HTML — не секрет, защищён API (/v1 за JWT+RBAC),
// а не статика. UI после загрузки фетчит /v1 с Bearer-JWT, который оператор
// вводит в самом UI. Раздача статики API-поверхность не раскрывает.
//
// Папка ассетов — assets/ (НЕ dist/): gitignore-правило `dist/` молча съело бы
// дерево, embed получил бы пустую FS и бандл не попал бы в бинарь без следа
// в ревью (ADR-055 §а). assets/ нейтрально к gitignore.
//
// Содержимое assets/ — РЕАЛЬНЫЙ vite-build-снапшот companion-репо soul-stack-web
// (index.html + хеш-чанки assets/*.js|css + locales/), завендоренный из dist/
// через scripts/sync-webui.sh (ADR-055 §Слайс-карта п.4–5). Drift между
// companion-dist и этой копией ловит `make check-webui`; embed-механизм
// (этот пакет) при пересборке UI не меняется — обновляется только дерево assets/.
package webui

import "embed"

// FS — вшитая файловая система build-снапшота UI. Корень — папка assets/
// (доступна потребителю через fs.Sub(FS, "assets"), см. Mount). `all:` тянет и
// dot-файлы при появлении реального бандла (vite кладёт, например, .vite/).
//
//go:embed all:assets
var FS embed.FS
