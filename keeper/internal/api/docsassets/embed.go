// Package docsassets вшивает (go:embed) статику визуального OpenAPI-вьювера
// /docs — web-компонент RapiDoc. Ассет завендорен из npm-пакета rapidoc
// (rapidoc-min.js, версия 9.3.8) и отдаётся keeper-ом со страницы /docs
// (механизм A, ADR-054 doc-viewer): shell-страница публична, ассет статичен,
// сама OpenAPI-спека — за JWT.
//
// Отдельного CSS нет: RapiDoc держит все стили в Shadow DOM web-компонента,
// поэтому вендорится ровно один файл (раньше Stoplight нёс отдельный
// styles.min.css — больше не требуется).
//
// Лицензия: RapiDoc распространяется под MIT. Вендоринг MIT-зависимости в этот
// Apache-2.0 репозиторий совместим. Шапка бандла ссылается на сопутствующий
// rapidoc-min.js.LICENSE.txt (атрибуция транзитивных MIT/BSD-зависимостей);
// файл лежит рядом с бандлом на диске. В go:embed он НЕ включён намеренно —
// атрибуция на диске достаточна, в бинарь раздаётся только сам ассет.
//
// Почему embed, а не CDN: keeper в air-gapped/закрытом контуре не должен тянуть
// сторонний CDN на каждый просмотр (безопасность на первом месте + офлайн-
// инсталляции). Один бинарь несёт всё.
//
// Обновление вендора: перекачать файл из
// https://unpkg.com/rapidoc@<version>/dist/rapidoc-min.js
// (зеркало: https://cdn.jsdelivr.net/npm/rapidoc@<version>/dist/rapidoc-min.js).
package docsassets

import "embed"

// FS — вшитая файловая система с ассетом RapiDoc. Содержит rapidoc-min.js в
// корне; раздаётся под /docs/assets/.
//
//go:embed rapidoc-min.js
var FS embed.FS
