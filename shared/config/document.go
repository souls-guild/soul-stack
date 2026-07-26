package config

import (
	"sync"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
)

// Document is an opaque handle over the AST + source bytes for round-trip
// write-back under [ADR-021](docs/architecture.md).
//
// Fields are private: external packages must not depend on the internal layout.
// All mutations go through the package's free functions (`PatchKeeper`/`PatchSoul`),
// all writes through `SaveKeeper`/`SaveSoul` / `*ToBytes`.
//
// `mutated` records that at least one Patch* has successfully run over this
// document: for an unmutated document `Save*ToBytes` returns the source bytes
// (byte-identical round-trip guarantee), for a mutated one it renders the AST
// via `file.String()` with a `round_trip_warning` attached.
type Document struct {
	file    *ast.File
	source  []byte
	path    string
	mu      sync.Mutex
	mutated bool
}

// HasPath reports whether the document explicitly sets `yamlPath`. The settings
// API answers `source ∈ {default, file, pg}` from evidence, and this is the
// evidence for `file` — an effective value that merely equals the built-in
// default is not the same as one the operator wrote down.
//
// A syntactically invalid path or an unparsed document is simply "not set".
func (d *Document) HasPath(yamlPath string) bool {
	if d == nil || d.file == nil {
		return false
	}
	p, err := yaml.PathString(yamlPath)
	if err != nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	node, err := p.FilterFile(d.file)
	return err == nil && node != nil
}
