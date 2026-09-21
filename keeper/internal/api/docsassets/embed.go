// Package docsassets embeds (go:embed) the static assets of the visual OpenAPI
// viewer at /docs — the RapiDoc web component. The asset is vendored from the npm
// rapidoc package (rapidoc-min.js, version 9.3.8) and served by keeper from the
// /docs page (mechanism A, ADR-054 doc-viewer): the shell page is public, the asset
// is static, the OpenAPI spec itself is behind JWT.
//
// There is no separate CSS: RapiDoc keeps all styles in the web component's Shadow
// DOM, so exactly one file is vendored (Stoplight used to carry a separate
// styles.min.css — no longer needed).
//
// License: RapiDoc is distributed under MIT. Vendoring an MIT dependency into this
// Apache-2.0 repository is compatible. The bundle header references the accompanying
// rapidoc-min.js.LICENSE.txt (attribution of transitive MIT/BSD dependencies); the
// file sits next to the bundle on disk. It is intentionally NOT included in go:embed —
// on-disk attribution is enough, only the asset itself ships in the binary.
//
// Why embed, not CDN: keeper in an air-gapped/closed environment must not pull a
// third-party CDN on every view (security first + offline installs). One binary
// carries everything.
//
// Updating the vendor: re-download the file from
// https://unpkg.com/rapidoc@<version>/dist/rapidoc-min.js
// (mirror: https://cdn.jsdelivr.net/npm/rapidoc@<version>/dist/rapidoc-min.js).
package docsassets

import "embed"

// FS is the embedded filesystem with the RapiDoc asset. Contains rapidoc-min.js at
// the root; served under /docs/assets/.
//
//go:embed rapidoc-min.js
var FS embed.FS
