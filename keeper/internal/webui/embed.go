// Package webui embeds (go:embed) the built static UI snapshot (companion repo
// soul-stack-web) and serves it from keeper at /ui (ADR-055, single-binary
// keeper with UI).
//
// Mechanics mirror docsassets/docs_viewer (go:embed static, public mount
// OUTSIDE /v1, like /docs): JS/CSS/HTML are not secret; API is protected (/v1
// behind JWT+RBAC), not static files. After loading, UI fetches /v1 with the
// Bearer JWT entered by operator in UI itself. Serving static files does not
// expose the API surface.
//
// Asset folder is assets/ (NOT dist/): gitignore rule `dist/` would silently
// eat the tree, embed would get empty FS, and bundle would not enter the binary
// without a review trace (ADR-055 §a). assets/ is neutral to gitignore.
//
// Contents of assets/ are a REAL vite build snapshot of companion repo
// soul-stack-web (index.html + hashed chunks assets/*.js|css + locales/),
// vendored from dist/ through scripts/sync-webui.sh (ADR-055 slice map p.4-5).
// Drift between companion dist and this copy is caught by `make check-webui`;
// embed mechanism (this package) does not change when rebuilding UI, only
// assets/ tree is updated.
package webui

import "embed"

// FS is embedded filesystem of the UI build snapshot. Root is assets/ directory
// (available to consumers through fs.Sub(FS, "assets"), see Mount). `all:` also
// pulls dotfiles when a real bundle appears (vite may put .vite/, for example).
//
//go:embed all:assets
var FS embed.FS
