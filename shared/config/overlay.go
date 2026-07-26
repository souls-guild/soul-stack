package config

// SettingsStore overlay hook — [ADR-0073](docs/adr/0073-keeper-runtime-config-pg.md) (c).
//
// A [Store] may be given a late-bound [OverlaySource] of cluster-wide overrides;
// every reload then merges them onto the file base BEFORE the standard
// validation pipeline, so `Get()` / `OnReload` consumers keep their contracts
// and need no edits. A nil source is today's behavior exactly — `soul` and
// `soul-lint` are untouched.
//
// The merge POLICY (which key maps to which yaml path, its bounds and the
// last-good rules) lives in `keeper`: [ADR-011](docs/adr/0011-go-layout.md)
// forbids a `shared → keeper` import, so this package only applies what the
// injected source returns.
//
// Invariant (ADR-0073(d) ★): the merge is in-memory only. [Store.Document] keeps
// pointing at the FILE document, so write-back never persists an overlay value
// into `keeper.yml`.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// OverlayEntry is one override: a goccy yaml path plus the scalar to place
// there. Path syntax matches [PatchKeeper] (`$.toll.threshold`); an absent path
// is created ([PatchKeeperOrCreate]) — optional blocks are legal, so a key
// inside a block the file never mentions must still be overridable.
//
// Value must be a comparable scalar (ADR-0073(j.3) — structural values are
// deferred): the idempotence guard compares entries by ==.
type OverlayEntry struct {
	Path  string
	Value any
}

// OverlaySource is the injected supplier of overrides. Called on the reload
// path, so implementations must be non-blocking (serve an in-memory snapshot,
// never a live DB query) and must return an already validated set — a source
// that cannot produce a trustworthy snapshot returns its last good one, or
// nothing at all.
//
// A nil/empty result means "no overrides": the file layer shows through.
type OverlaySource interface {
	Overlay() []OverlayEntry
}

// SetOverlaySource injects the overlay source into an already-created Store —
// late binding, exactly like [Store.SetAuditWriter]: the binary builds the Store
// before Postgres is up (Vault → pool → migrations is a deliberate order) and
// injects the source once it exists.
//
// Setting the source does NOT re-merge by itself; the caller follows with
// [Store.RefreshOverlay]. src may be nil (drops the overlay — the next reload
// returns to the pure file base).
func (s *Store[T]) SetOverlaySource(src OverlaySource) {
	s.mu.Lock()
	s.overlay = src
	s.mu.Unlock()
}

// overlayEntries snapshots the current overrides from the injected source.
// nil source → nil (pure file behavior).
func (s *Store[T]) overlayEntries() []OverlayEntry {
	s.mu.Lock()
	src := s.overlay
	s.mu.Unlock()
	if src == nil {
		return nil
	}
	return src.Overlay()
}

// RefreshOverlay re-merges the current overrides on top of the file and swaps
// the snapshot — the entry point for a cluster invalidation or a TTL poll
// (ADR-0073(g)). Audit source is `keeper_internal`: on a receiving node the swap
// has no initiating Archon, it is an autonomous reaction to a cluster event
// (ADR-0073(k)).
//
// Idempotence guard (load-bearing, not an optimization): the swap happens ONLY
// when the overrides actually differ from the ones already applied. The
// invalidation channel is shared with the service registry and the TTL poll
// fires every 10s — without the guard every unrelated wake-up would reconfigure
// every `OnReload` consumer and write an audit event.
//
// Result contract: `Swapped=false` with empty `Diagnostics` means "nothing
// changed" (no audit, no notify); `Swapped=false` WITH error diagnostics means
// the merged config failed validation and the previous snapshot stays current.
// On a real change the file is re-read as well — the merge is file-based by
// construction.
func (s *Store[T]) RefreshOverlay(ctx context.Context) ReloadResult {
	entries := s.overlayEntries()

	s.mu.Lock()
	applied := s.appliedOverlay
	s.mu.Unlock()

	changed := changedOverlayPaths(applied, entries)
	if len(changed) == 0 {
		return ReloadResult{Source: audit.SourceKeeperInternal, Timestamp: time.Now()}
	}
	return s.reloadWith(ctx, audit.SourceKeeperInternal, entries, changed)
}

// AppliedOverlay reports the overrides merged into the current snapshot, keyed
// by yaml path. Read surface for the settings API, which answers with the
// effective value plus its source ∈ {default, file, pg} — the discoverability
// that replaces seeding the table from the file (ADR-0073(f)).
func (s *Store[T]) AppliedOverlay() map[string]any {
	s.mu.Lock()
	applied := s.appliedOverlay
	s.mu.Unlock()

	out := make(map[string]any, len(applied))
	for _, e := range applied {
		out[e.Path] = e.Value
	}
	return out
}

// applyOverlay merges the entries into the file bytes and returns the merged
// bytes. All-or-nothing: a single unusable entry rejects the whole overlay
// (ADR-0073(h)) — a partial merge would leave the cluster in a state no
// operator asked for.
func applyOverlay(path string, src []byte, entries []OverlayEntry) ([]byte, []diag.Diagnostic) {
	doc, err := parseDocumentOnly(path, src)
	if err != nil {
		return nil, []diag.Diagnostic{overlayDiag(path, err)}
	}
	for _, e := range entries {
		if err := PatchKeeperOrCreate(doc, e.Path, e.Value); err != nil {
			return nil, []diag.Diagnostic{overlayDiag(path, fmt.Errorf("overlay %s: %w", e.Path, err))}
		}
	}
	// The round-trip warning of renderBytes is meaningless here: the merged
	// document is in-memory and is never written back to disk.
	out, _, err := renderBytes(doc)
	if err != nil {
		return nil, []diag.Diagnostic{overlayDiag(path, err)}
	}
	return out, nil
}

// parseDocumentOnly builds a [Document] without validation — the merge target
// (and the pristine file document kept for write-back). Validation happens once,
// over the merged bytes.
func parseDocumentOnly(path string, src []byte) (*Document, error) {
	file, err := parser.ParseBytes(stripBOM(src), parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return nil, errors.New("config file must contain exactly one non-empty YAML document")
	}
	return &Document{file: file, source: src, path: path}, nil
}

// overlayDiag wraps an overlay failure as an error diagnostic. Phase is
// `parse`: the overlay is applied before the config is parsed as a whole, and
// the failure is about producing the effective source, not about its content.
func overlayDiag(path string, err error) diag.Diagnostic {
	return diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseParse,
		File:    path,
		Code:    "overlay_error",
		Message: err.Error(),
	}
}

// changedOverlayPaths returns the sorted yaml paths whose override was added,
// removed or changed between two entry sets — the honest `changed_paths` of the
// audit event and the trigger of the idempotence guard.
func changedOverlayPaths(old, new []OverlayEntry) []string {
	oldByPath := make(map[string]any, len(old))
	for _, e := range old {
		oldByPath[e.Path] = e.Value
	}
	newByPath := make(map[string]any, len(new))
	for _, e := range new {
		newByPath[e.Path] = e.Value
	}

	seen := make(map[string]bool, len(oldByPath)+len(newByPath))
	var out []string
	for p, nv := range newByPath {
		ov, ok := oldByPath[p]
		if !ok || ov != nv {
			out = append(out, p)
			seen[p] = true
		}
	}
	for p := range oldByPath {
		if _, ok := newByPath[p]; !ok && !seen[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
