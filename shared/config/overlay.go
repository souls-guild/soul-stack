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
	"os"
	"sort"
	"time"

	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// OverlayEntry is one override: a goccy yaml path plus the scalar to place
// there. Path syntax matches [PatchKeeper] (`$.toll.threshold`); an absent path
// is created ([PatchKeeperOrCreate]) — optional blocks are legal, so a key
// inside a block the file never mentions must still be overridable, and that is
// precisely where the cluster value applies: a path the file DOES set keeps its
// local value (ADR-0073(b), amended — the file wins).
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
// A nil/empty result means "no overrides": the built-in defaults show through
// wherever the file is silent.
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

// ValidateOverlay dry-runs a candidate override set: it merges the entries onto
// the file base and pushes the result through the full validation pipeline
// WITHOUT swapping the snapshot. Errors mean "this set would be rejected".
//
// The write-gate needs it because a per-field range check cannot see cross-field
// invariants (ADR-0073(i)): `poll_floor <= poll_ceiling` only exists on the
// merged config. A value that passed PUT but failed the merge would be rejected
// by every reader as a whole (all-or-nothing, ADR-0073(h)), leaving the cluster
// stuck on its last-good overlay while a restarting instance came up on the file
// base — a split the write path must prevent, not discover afterwards.
func (s *Store[T]) ValidateOverlay(entries []OverlayEntry) []diag.Diagnostic {
	src, err := os.ReadFile(s.path)
	if err != nil {
		return []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    s.path,
			Code:    "io_error",
			Message: err.Error(),
		}}
	}

	// Two questions, because the answer differs per instance once the file wins
	// (b): "what does this do HERE", where a locally-set key is skipped, and
	// "what does it do on an instance whose file is silent", which applies the
	// whole set. Judging only the first would let a value this node happens to
	// ignore be committed and then rejected — as a whole, all-or-nothing (h) —
	// by every node that does apply it.
	for _, force := range []bool{false, true} {
		if diags := s.validateMerged(src, entries, force); diag.HasErrors(diags) {
			return diags
		}
	}
	return nil
}

func (s *Store[T]) validateMerged(src []byte, entries []OverlayEntry, force bool) []diag.Diagnostic {
	if len(entries) > 0 {
		merged, odiags := applyOverlayWith(s.path, src, entries, force)
		if len(odiags) > 0 {
			return odiags
		}
		src = merged
	}
	switch s.kind {
	case storeKindKeeper:
		_, _, diags, _ := LoadKeeperFromBytes(s.path, src, s.opts)
		return diags
	case storeKindSoul:
		_, _, diags, _ := LoadSoulFromBytes(s.path, src, s.opts)
		return diags
	default:
		return []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    s.path,
			Code:    "io_error",
			Message: fmt.Sprintf("config: Store has unknown kind %d", s.kind),
		}}
	}
}

// AppliedOverlay reports the override set the current snapshot was merged from,
// keyed by yaml path. Note that an entry here was not necessarily APPLIED: one
// whose path the file also sets was skipped by the merge, because the file wins
// (ADR-0073(b), amended). The settings API answers `source` from the file
// document rather than from this map for exactly that reason.
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
//
// ★ Precedence (ADR-0073(b), amended): the LOCAL FILE WINS. A path this
// instance's `keeper.yml` sets explicitly is left alone — the cluster value
// applies only where the file is silent. A local decision on a host is a
// deliberate act by whoever administers that host, and a cluster-wide default
// must not silently override it.
func applyOverlay(path string, src []byte, entries []OverlayEntry) ([]byte, []diag.Diagnostic) {
	return applyOverlayWith(path, src, entries, false)
}

// applyOverlayWith is the body of [applyOverlay]. With force, the file-wins rule
// is suspended and every entry is applied — used only by [Store.ValidateOverlay]
// to judge a candidate the way an instance whose file is silent would see it.
func applyOverlayWith(path string, src []byte, entries []OverlayEntry, force bool) ([]byte, []diag.Diagnostic) {
	doc, err := parseDocumentOnly(path, src)
	if err != nil {
		return nil, []diag.Diagnostic{overlayDiag(path, err)}
	}
	for _, e := range entries {
		if !force && doc.HasPath(e.Path) {
			continue
		}
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
