package config

// Mutate a single scalar value by yaml.Path, preserving the inline comment,
// under [ADR-021](docs/architecture.md). Library API; the consumer (Operator
// API `config.set`) will land in a separate slice in M0.3.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
)

// ErrPathNotFound — the yaml path does not exist in the document. The caller
// decides: return it as a 404, or wrap it in a create-on-write pipeline
// (deferred to M0.2.5).
var ErrPathNotFound = errors.New("yaml path not found")

// PatchKeeper mutates the value at `yamlPath` in a KeeperConfig document.
//
// Contract:
//   - `yamlPath` — goccy/go-yaml.PathString format (e.g. `$.kid`,
//     `$.listen.grpc.addr`, `$.services[0].ref`). Resolved via
//     `yaml.PathString(...)`; syntax errors are returned as-is.
//   - `value` is marshaled via `yaml.Marshal` — the caller is responsible for
//     type compatibility with the schema (PatchKeeper does NOT re-validate
//     KeeperConfig after the mutation; that is the caller's job).
//   - On a nonexistent path returns `ErrPathNotFound`. No create-on-write in MVP.
//   - The inline comment of the scalar node targeted by the patch is preserved
//     (snapshot+restore via `path.FilterFile` + `SetComment`).
//   - Thread-safe: concurrent `PatchKeeper`/`PatchSoul`/`Save*` on the same
//     `*Document` are serialized through `doc.mu`. A single Document can be
//     safely shared between goroutines; no external synchronization is needed.
//
// An error is returned on fatal I/O (`Save*` — write/rename/stat) or a
// programming error (nil Document, empty yaml path, path without a `$` prefix,
// non-scalar target, marshaling a value of an unsupported type — chan/func/
// cyclic structure). Validation errors from `Load*` arrive as
// `[]diag.Diagnostic` (see ADR-021 d).
func PatchKeeper(doc *Document, yamlPath string, value any) error {
	return patchOne(doc, yamlPath, value)
}

// PatchSoul — the same for a SoulConfig document. The contract is identical to
// PatchKeeper, including thread-safety and the set of returned errors.
func PatchSoul(doc *Document, yamlPath string, value any) error {
	return patchOne(doc, yamlPath, value)
}

func patchOne(doc *Document, yamlPath string, value any) error {
	if doc == nil {
		return errors.New("config: Document is nil")
	}
	// doc.file is set only in parseAndValidate; immutable after construction.
	if doc.file == nil {
		return errors.New("config: Document has no AST file (parse failed; cannot patch)")
	}

	// Pre-validate yamlPath BEFORE `yaml.PathString`: for an empty / whitespace-only
	// string goccy returns a non-nil Path with a nil error, and the subsequent
	// `FilterFile` SIGSEGVs (`path.go:491`). Likewise a path without a `$` prefix
	// is a syntactically invalid YAMLPath — better to reject it with an explicit
	// message than to trust goccy.
	if strings.TrimSpace(yamlPath) == "" {
		return errors.New("config: yaml path is empty")
	}
	if !strings.HasPrefix(yamlPath, "$") {
		return fmt.Errorf("config: yaml path must start with '$': got %q", yamlPath)
	}

	p, err := yaml.PathString(yamlPath)
	if err != nil {
		return fmt.Errorf("config: invalid yaml path %q: %w", yamlPath, err)
	}

	doc.mu.Lock()
	defer doc.mu.Unlock()

	// (a) Existence check + capture the inline comment of the current scalar node.
	//
	// Use FilterFile (not ReadNode): ReadNode takes an io.Reader and re-sorts the
	// AST internally, losing references to the original nodes; FilterFile returns a
	// live pointer to the node inside `doc.file`, whose inline comment we preserve.
	target, err := p.FilterFile(doc.file)
	if err != nil {
		if yaml.IsNotFoundNodeError(err) {
			return fmt.Errorf("%w: %s", ErrPathNotFound, yamlPath)
		}
		return fmt.Errorf("config: cannot resolve path %q: %w", yamlPath, err)
	}

	// Reject non-scalar target (mapping/sequence/anchor/...). The PatchKeeper/
	// PatchSoul contract is a scalar replace; silently replacing a whole subtree
	// risks silent data loss (`$.listen.grpc` → would drop
	// tls.cert/tls.key/tls.ca). To change a mapping, write a separate Patch per
	// scalar field.
	if _, isScalar := target.(ast.ScalarNode); !isScalar {
		return fmt.Errorf("config: yaml path %q points to non-scalar node (kind=%s); "+
			"only scalars are patchable (non_scalar_patch_target)", yamlPath, target.Type().String())
	}
	oldComment := target.GetComment()

	// (b) Marshal the value. yaml.Marshal appends a trailing `\n`; trim it —
	// ReplaceWithReader needs a single line / single node.
	raw, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Errorf("config: cannot marshal value for %q: %w", yamlPath, err)
	}
	// Marshaling a scalar yields `value\n`; mapping/sequence — multi-line with a
	// trailing `\n`. The parser tolerates a trailing `\n`, but trim for cleanliness.
	fragment := bytes.TrimRight(raw, "\n")

	// (c) Replace in the AST.
	if err := p.ReplaceWithReader(doc.file, bytes.NewReader(fragment)); err != nil {
		if yaml.IsNotFoundNodeError(err) {
			return fmt.Errorf("%w: %s", ErrPathNotFound, yamlPath)
		}
		return fmt.Errorf("config: cannot replace at %q: %w", yamlPath, err)
	}

	// (d) Restore the inline comment.
	if oldComment != nil {
		newNode, err := p.FilterFile(doc.file)
		if err == nil && newNode != nil {
			_ = newNode.SetComment(oldComment)
		}
	}

	doc.mutated = true
	return nil
}
