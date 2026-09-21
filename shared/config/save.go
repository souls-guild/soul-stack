package config

// Write-back YAML per [ADR-021](docs/architecture.md). Saves
// `keeper.yml`/`soul.yml` preserving comments/order/anchors via the
// goccy/go-yaml AST + an atomic rename (tmp + chmod before write + rename + fsync).
//
// Known limitations (reported as `round_trip_warning` diagnostics on mutation;
// for an unmutated Document, `Save*ToBytes` returns the original bytes as-is so
// the round-trip is byte-identical):
//
//   - Flow-style mappings (`{ a: b, c: d }`) lose key alignment and inner spaces
//     after the goccy AST render (`{a: b, c: d}`). In an unmutated document this
//     is avoided by returning `doc.source`; after Patch it is a known cosmetic
//     drift and raises a warning.
//   - An inline comment on a scalar node that is itself a Patch target is
//     preserved (snapshot+restore via `path.FilterFile`+`SetComment`), but
//     multi-space between the value and `#` collapses to one space
//     (`"value"  # cmt` → `"value" # cmt`) — a goccy stringer quirk.
//   - Anchor mutation (`&anchor` / `*alias`) when patching an anchored node may
//     split alias references (best-effort, no explicit validation).
//   - Multi-line scalar style is best-effort: a `|` or `>` block literal may be
//     rewritten to flow when patching short values.
//   - Numeric literals (`0755`, `0xFF`, `0o755`) are not preserved in literal
//     form after Patch — written as decimal.
//   - A BOM (`EF BB BF`) is not restored on write: stripped at Load per YAML 1.2.
//
// Atomic-rename I/O pipeline (9 steps, see `writeFileAtomically`):
//   1. Stat dst → mode/uid/gid; reject symlink at the Lstat step.
//   2. CreateTemp in the same directory.
//   3. Chmod tmp before Write (avoid the 0600-default window on read).
//   4. Chown tmp to the source uid/gid (best-effort on permission denied).
//   5. Write rendered bytes.
//   6. tmp.Sync() — fsync contents.
//   7. tmp.Close().
//   8. Rename(tmp, dst).
//   9. fsync the parent directory (best-effort).
//
// On any error after CreateTemp — `os.Remove(tmp.Name())` cleanup.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// SaveKeeper is a round-trip write-back of KeeperConfig to file `path`.
//
// Contract:
//   - Returned `[]diag.Diagnostic` at warning level signal round-trip
//     degradation (see known limitations above).
//   - An error is returned on fatal I/O (write/rename/stat/chmod/symlink-target)
//     or a programming error (nil Document, empty `path`). Validation errors on
//     `Load*` arrive as `[]diag.Diagnostic` (see ADR-021 d).
//   - The source file's permissions and uid/gid carry over to the new file
//     (best-effort for chown — not fatal if the process lacks CAP_CHOWN).
//   - If dst does not exist, it is created with mode 0o644. An existing file's
//     permissions/uid/gid are preserved. If `os.Stat(dst)` fails for a reason
//     other than `IsNotExist` (permission denied, etc.), Save refuses the write
//     rather than silently overwriting the mode with 0o644.
//   - If `path` is a symlink, it is rejected with an `error` and the
//     `symlink_write_not_supported` diagnostic; the file is not modified.
//   - The Document is mutated read-only; the mutated flag is honored in the
//     render (see SaveKeeperToBytes).
//   - Thread-safe: concurrent `Save*` / `Patch*` on the same `*Document` are
//     serialized via `doc.mu`.
func SaveKeeper(path string, doc *Document) ([]diag.Diagnostic, error) {
	return saveTo(path, doc)
}

// SaveSoul is a round-trip write-back of SoulConfig. See `SaveKeeper` for the
// contract, including thread-safety semantics and the set of returned errors.
func SaveSoul(path string, doc *Document) ([]diag.Diagnostic, error) {
	return saveTo(path, doc)
}

// SaveKeeperToBytes renders without writing to disk (for tests and in-memory
// pipelines where the caller handles I/O).
//
// For an unmutated document it returns `doc.source` as-is — guaranteeing a
// byte-identical round-trip with Load (golden tests). For a mutated one it
// renders `doc.file.String()` and raises `round_trip_warning` if the render
// differs from the source.
func SaveKeeperToBytes(doc *Document) ([]byte, []diag.Diagnostic, error) {
	return renderBytes(doc)
}

// SaveSoulToBytes is the same for SoulConfig.
func SaveSoulToBytes(doc *Document) ([]byte, []diag.Diagnostic, error) {
	return renderBytes(doc)
}

// renderBytes is the common part of both *ToBytes: SoulConfig vs KeeperConfig
// makes no difference to the AST render (YAML is one format); the functions are
// typed separately for API symmetry and future divergence.
func renderBytes(doc *Document) ([]byte, []diag.Diagnostic, error) {
	if doc == nil {
		return nil, nil, errors.New("config: Document is nil")
	}
	doc.mu.Lock()
	defer doc.mu.Unlock()

	if !doc.mutated {
		// No Patch happened — return the source as-is, without rendering the
		// AST. This is the only path where the round-trip is guaranteed
		// byte-identical (the goccy stringer normalizes flow-style and trims
		// trivial spaces).
		out := make([]byte, len(doc.source))
		copy(out, doc.source)
		return out, nil, nil
	}

	if doc.file == nil {
		return nil, nil, errors.New("config: Document has no AST file (parse failed)")
	}

	rendered := doc.file.String()
	if len(rendered) == 0 || rendered[len(rendered)-1] != '\n' {
		rendered += "\n"
	}
	out := []byte(rendered)

	var diags []diag.Diagnostic
	if !bytes.Equal(out, doc.source) {
		diags = append(diags, diag.Diagnostic{
			Level:   diag.LevelWarning,
			Phase:   diag.PhaseWriteBack,
			File:    doc.path,
			Code:    "round_trip_warning",
			Message: "rendered YAML differs from source (flow-style restyled / inline-comment whitespace normalized / similar AST-stringer artifact)",
			Hint:    "if format preservation is critical, restrict mutations to block-style values",
		})
	}
	return out, diags, nil
}

// saveTo is the common part of SaveKeeper/SaveSoul. Renders bytes, checks for a
// symlink, writes atomically.
func saveTo(path string, doc *Document) ([]diag.Diagnostic, error) {
	if path == "" {
		return nil, errors.New("config: empty save path")
	}
	out, diags, err := renderBytes(doc)
	if err != nil {
		return diags, err
	}

	writeDiags, writeErr := writeFileAtomically(path, out)
	diags = append(diags, writeDiags...)
	return diags, writeErr
}

// writeFileAtomically implements the 9-step pipeline (see the file doc-comment).
func writeFileAtomically(path string, data []byte) ([]diag.Diagnostic, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// (1+8) Symlink check: Lstat detects a symlink without following it. Reject
	// before creating tmp — the user must explicitly decide what to do with a
	// symlink (create-on-write / follow — deferred to M0.2.5).
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return []diag.Diagnostic{{
				Level:   diag.LevelError,
				Phase:   diag.PhaseWriteBack,
				File:    path,
				Code:    "symlink_write_not_supported",
				Message: "refusing to write through a symlink",
				Hint:    "resolve the symlink target manually or replace it with a regular file",
			}}, fmt.Errorf("config: refusing to write through symlink %q", path)
		}
	}

	// (1) Stat dst → mode/uid/gid (if the file exists). If absent, use mode 0644
	// (a typical POSIX default for configs). Any stat error other than
	// `IsNotExist` (e.g. EACCES on the parent directory) is fatal: silently
	// falling back to 0o644 is not allowed — a potential privilege widening.
	var (
		dstMode os.FileMode = 0o644
		dstStat os.FileInfo
	)
	s, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		dstStat = s
		dstMode = s.Mode().Perm()
	case os.IsNotExist(statErr):
		// creating the file anew — keep the default 0o644
	default:
		return []diag.Diagnostic{atomicRenameDiag(path, statErr, "stat dst")}, fmt.Errorf("config: cannot stat dst %q: %w", path, statErr)
	}

	// (2) CreateTemp in the same directory — otherwise rename could cross
	// filesystems (EXDEV) and lose atomicity.
	tmp, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return []diag.Diagnostic{atomicRenameDiag(path, err, "create temp")}, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = os.Remove(tmpName)
	}

	// (3) Chmod tmp BEFORE Write. CreateTemp makes the file mode 0600 — if a
	// reader opens tmp between Write and Rename it would get narrower permissions
	// than the source. Change the mode immediately.
	if err := os.Chmod(tmpName, dstMode); err != nil {
		_ = tmp.Close()
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "chmod temp")}, err
	}

	// (4) Chown tmp to the source uid/gid if the process can (best-effort).
	if dstStat != nil {
		if uid, gid, ok := statOwner(dstStat); ok {
			if err := os.Chown(tmpName, uid, gid); err != nil {
				// EPERM without CAP_CHOWN is not fatal: tmp gets the process's
				// effective uid. Otherwise raise a warning.
				if !errors.Is(err, os.ErrPermission) {
					_ = tmp.Close()
					cleanup()
					return []diag.Diagnostic{atomicRenameDiag(path, err, "chown temp")}, err
				}
			}
		}
	}

	// (5) Write.
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "write temp")}, err
	}

	// (6) Sync contents.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "fsync temp")}, err
	}

	// (7) Close.
	if err := tmp.Close(); err != nil {
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "close temp")}, err
	}

	// (8) Symlink recheck before Rename — TOCTOU protection: between the first
	// Lstat check and Rename an attacker could have planted a symlink.
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			cleanup()
			return []diag.Diagnostic{{
				Level:   diag.LevelError,
				Phase:   diag.PhaseWriteBack,
				File:    path,
				Code:    "symlink_write_not_supported",
				Message: "refusing to write through a symlink (TOCTOU race detected)",
			}}, fmt.Errorf("config: symlink appeared at %q between checks", path)
		}
	}

	// (8.b) Rename.
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return []diag.Diagnostic{atomicRenameDiag(path, err, "rename")}, err
	}

	// (9) fsync the parent directory (best-effort: some filesystems/OSes do not
	// support directory fsync).
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	return nil, nil
}

// atomicRenameDiag is the single helper for the `atomic_rename_failed` error
// code, recording the specific pipeline stage in Hint.
func atomicRenameDiag(path string, err error, stage string) diag.Diagnostic {
	return diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseWriteBack,
		File:    path,
		Code:    "atomic_rename_failed",
		Message: err.Error(),
		Hint:    "stage: " + stage,
	}
}
