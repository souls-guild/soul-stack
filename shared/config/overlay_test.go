package config

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// fakeOverlaySource is a settable [OverlaySource] standing in for the keeper
// SettingsStore.
type fakeOverlaySource struct {
	mu      sync.Mutex
	entries []OverlayEntry
}

func (f *fakeOverlaySource) Overlay() []OverlayEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]OverlayEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

func (f *fakeOverlaySource) set(entries ...OverlayEntry) {
	f.mu.Lock()
	f.entries = entries
	f.mu.Unlock()
}

// overlayStore builds a keeper Store over the golden fixture plus `extra` YAML,
// with an attached overlay source and audit writer.
func overlayStore(t *testing.T, extra string) (*Store[KeeperConfig], *fakeOverlaySource, *mockWriter) {
	t.Helper()
	path := fixtureKeeperPath(t)
	if extra != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		if err := os.WriteFile(path, append(data, []byte(extra)...), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	store, diags, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("LoadKeeperStore: err=%v diags=%v", err, diags)
	}
	w := &mockWriter{}
	store.SetAuditWriter(w)
	src := &fakeOverlaySource{}
	store.SetOverlaySource(src)
	return store, src, w
}

// ★ The local file beats the cluster for a key it sets, and the layering is
// per key, not per block: a sibling the file leaves out still takes the cluster
// value (ADR-0073(b), amended).
func TestOverlay_FileWinsOverPostgres(t *testing.T) {
	store, src, _ := overlayStore(t, "\ntoll:\n  threshold: 0.9\n")
	if got := store.Get().Toll.Threshold; got != 0.9 {
		t.Fatalf("file value not loaded: %v", got)
	}

	src.set(
		OverlayEntry{Path: "$.toll.threshold", Value: 0.5},
		OverlayEntry{Path: "$.toll.window_size", Value: "45s"},
	)
	res := store.RefreshOverlay(context.Background())
	if !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}
	if got := store.Get().Toll.Threshold; got != 0.9 {
		t.Errorf("threshold = %v, want the local 0.9 (the file wins)", got)
	}
	if got := store.Get().Toll.WindowSize; got != "45s" {
		t.Errorf("window_size = %q, want 45s (the file is silent here, so the cluster applies)", got)
	}
}

// Removing the key from the file hands the decision back to the cluster: the
// same override that was shadowed a moment ago now takes effect, with no change
// on the Postgres side at all.
func TestOverlay_ClusterTakesOverWhenTheFileStopsSettingTheKey(t *testing.T) {
	store, src, _ := overlayStore(t, "\ntoll:\n  threshold: 0.9\n")
	src.set(OverlayEntry{Path: "$.toll.threshold", Value: 0.5})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}
	if got := store.Get().Toll.Threshold; got != 0.9 {
		t.Fatalf("precondition: file value should win, got %v", got)
	}

	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	trimmed := strings.Replace(string(data), "\ntoll:\n  threshold: 0.9\n", "\n", 1)
	if trimmed == string(data) {
		t.Fatal("fixture edit did not match")
	}
	if err := os.WriteFile(store.Path(), []byte(trimmed), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if res := store.Reload(context.Background(), audit.SourceSignal); !res.Swapped {
		t.Fatalf("reload after the file edit: %+v", res.Diagnostics)
	}
	if got := store.Get().Toll.Threshold; got != 0.5 {
		t.Errorf("threshold = %v, want the cluster 0.5 once the file stopped setting it", got)
	}
}

// The pilot's own case: neither `toll:` nor `tempo:` exists in the golden file,
// so the overlay has to create the whole block (create-on-write).
func TestOverlay_CreatesAbsentBlocks(t *testing.T) {
	store, src, _ := overlayStore(t, "")
	if store.Get().Toll != nil {
		t.Fatalf("fixture unexpectedly carries a toll block")
	}

	src.set(
		OverlayEntry{Path: "$.toll.threshold", Value: 0.5},
		OverlayEntry{Path: "$.tempo.voyage_create.rate", Value: 1.0},
		OverlayEntry{Path: "$.tempo.voyage_create.burst", Value: 2},
	)
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}

	cfg := store.Get()
	if cfg.Toll == nil || cfg.Toll.Threshold != 0.5 {
		t.Errorf("toll.threshold not applied: %+v", cfg.Toll)
	}
	rate, burst := cfg.Tempo.ResolvedVoyageCreate()
	if rate != 1.0 || burst != 2 {
		t.Errorf("tempo voyage_create = (%v, %d), want (1, 2)", rate, burst)
	}
}

// ★ ADR-0073(d): the merge is in-memory. The document handed to write-back must
// still be the file, or a later Save would persist Postgres values into
// keeper.yml.
func TestOverlay_NeverLeaksIntoTheFileDocument(t *testing.T) {
	store, src, _ := overlayStore(t, "\ntoll:\n  threshold: 0.9\n")
	src.set(OverlayEntry{Path: "$.toll.threshold", Value: 0.5})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}

	out, _, err := SaveKeeperToBytes(store.Document())
	if err != nil {
		t.Fatalf("SaveKeeperToBytes: %v", err)
	}
	if strings.Contains(string(out), "threshold: 0.5") {
		t.Errorf("overlay value leaked into the file document:\n%s", out)
	}
	if !strings.Contains(string(out), "threshold: 0.9") {
		t.Errorf("file value missing from the file document:\n%s", out)
	}
	// On disk, untouched.
	onDisk, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(onDisk), "threshold: 0.5") {
		t.Errorf("overlay value written to disk:\n%s", onDisk)
	}
}

// Hot-apply without a restart: an overlay swap notifies the same OnReload
// subscribers a SIGHUP would (that is what makes Toll reconfigure itself).
func TestOverlay_NotifiesReloadSubscribers(t *testing.T) {
	store, src, _ := overlayStore(t, "")

	got := make(chan float64, 1)
	store.OnReload(func(_, newCfg *KeeperConfig) {
		if newCfg != nil && newCfg.Toll != nil {
			got <- newCfg.Toll.Threshold
		}
	})

	src.set(OverlayEntry{Path: "$.toll.threshold", Value: 0.5})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}
	select {
	case v := <-got:
		if v != 0.5 {
			t.Errorf("subscriber saw %v, want 0.5", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber was not notified of the overlay swap")
	}
}

// ADR-0073(g): the invalidation channel is shared and the TTL poll fires every
// 10s — an unchanged overlay must resolve to a no-op, not to a config swap plus
// an audit event every tick.
func TestOverlay_IdempotenceGuard(t *testing.T) {
	store, src, w := overlayStore(t, "")
	src.set(OverlayEntry{Path: "$.toll.threshold", Value: 0.5})

	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("first refresh did not swap: %+v", res.Diagnostics)
	}
	events := w.len()

	notified := make(chan struct{}, 1)
	store.OnReload(func(_, _ *KeeperConfig) { notified <- struct{}{} })

	res := store.RefreshOverlay(context.Background())
	if res.Swapped {
		t.Errorf("second refresh swapped on an unchanged overlay")
	}
	if len(res.Diagnostics) != 0 {
		t.Errorf("no-op refresh produced diagnostics: %+v", res.Diagnostics)
	}
	if w.len() != events {
		t.Errorf("no-op refresh wrote an audit event")
	}
	select {
	case <-notified:
		t.Errorf("no-op refresh notified subscribers")
	case <-time.After(200 * time.Millisecond):
	}
}

// Read path, fail-closed on content: a value that passes the write-gate type
// check but breaks the merged config is rejected WHOLE, and the previous
// snapshot survives (ADR-0073(h)).
func TestOverlay_InvalidMergeKeepsLastGood(t *testing.T) {
	store, src, w := overlayStore(t, "")
	src.set(OverlayEntry{Path: "$.toll.threshold", Value: 0.5})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("first refresh did not swap: %+v", res.Diagnostics)
	}

	// threshold must be in (0, 1] — 5 is out of range for the schema validator.
	src.set(
		OverlayEntry{Path: "$.toll.threshold", Value: 5.0},
		OverlayEntry{Path: "$.tempo.voyage_create.rate", Value: 3.0},
	)
	res := store.RefreshOverlay(context.Background())
	if res.Swapped {
		t.Fatalf("a broken overlay was swapped in")
	}
	if !diag.HasErrors(res.Diagnostics) {
		t.Errorf("expected error diagnostics, got %+v", res.Diagnostics)
	}
	if got := store.Get().Toll.Threshold; got != 0.5 {
		t.Errorf("last-good lost: threshold = %v, want 0.5", got)
	}
	// All-or-nothing: the healthy entry of the same batch is not applied either.
	if rate, _ := store.Get().Tempo.ResolvedVoyageCreate(); rate == 3.0 {
		t.Errorf("partial merge: an entry from a rejected overlay was applied")
	}
	if evs := w.snapshot(); len(evs) == 0 || evs[len(evs)-1].EventType != audit.EventConfigReloadFailed {
		t.Errorf("expected a config.reload_failed audit event, got %+v", evs)
	}
}

// A malformed entry (a path that cannot be created) is rejected the same way,
// before the config is even parsed.
func TestOverlay_UnusableEntryIsRejectedWhole(t *testing.T) {
	store, src, _ := overlayStore(t, "\ntoll:\n  threshold: 0.9\n")
	src.set(OverlayEntry{Path: "$.kid.nested", Value: "x"})

	res := store.RefreshOverlay(context.Background())
	if res.Swapped {
		t.Fatalf("an unusable overlay was swapped in")
	}
	if !diag.HasErrors(res.Diagnostics) || res.Diagnostics[0].Code != "overlay_error" {
		t.Errorf("expected an overlay_error diagnostic, got %+v", res.Diagnostics)
	}
	if got := store.Get().Toll.Threshold; got != 0.9 {
		t.Errorf("snapshot changed on a rejected overlay: %v", got)
	}
}

// A plain file reload (SIGHUP/API) must not drop the overrides — the precedence
// is a property of the store, not of the trigger.
func TestOverlay_SurvivesFileReload(t *testing.T) {
	store, src, _ := overlayStore(t, "\ntoll:\n  threshold: 0.9\n")
	src.set(OverlayEntry{Path: "$.toll.window_size", Value: "45s"})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}

	if res := store.Reload(context.Background(), ReloadSourceSignal); !res.Swapped {
		t.Fatalf("file reload did not swap: %+v", res.Diagnostics)
	}
	if got := store.Get().Toll.WindowSize; got != "45s" {
		t.Errorf("overlay dropped by a file reload: %q", got)
	}
	if got := store.Get().Toll.Threshold; got != 0.9 {
		t.Errorf("threshold = %v, want the local 0.9 across the reload", got)
	}
}

// Dropping the row is a clean revert to the file value (ADR-0073(f)) — the
// DELETE semantics of the settings endpoint.
func TestOverlay_RemovedEntryRevertsToFile(t *testing.T) {
	store, src, _ := overlayStore(t, "\ntoll:\n  threshold: 0.9\n")
	src.set(OverlayEntry{Path: "$.toll.threshold", Value: 0.5})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}

	src.set()
	res := store.RefreshOverlay(context.Background())
	if !res.Swapped {
		t.Fatalf("removal did not swap: %+v", res.Diagnostics)
	}
	if got := store.Get().Toll.Threshold; got != 0.9 {
		t.Errorf("threshold = %v, want the file value 0.9 after removal", got)
	}
	if len(res.ChangedPaths) != 1 || res.ChangedPaths[0] != "$.toll.threshold" {
		t.Errorf("changed_paths = %v, want the removed path", res.ChangedPaths)
	}
}

// ADR-0073(k): the swap on a receiving node is a keeper_internal event carrying
// the paths that actually changed.
func TestOverlay_AuditRecordsChangedPaths(t *testing.T) {
	store, src, w := overlayStore(t, "")
	src.set(
		OverlayEntry{Path: "$.toll.threshold", Value: 0.5},
		OverlayEntry{Path: "$.tempo.voyage_create.rate", Value: 1.0},
	)
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}

	evs := w.snapshot()
	if len(evs) == 0 {
		t.Fatalf("no audit event written")
	}
	ev := evs[len(evs)-1]
	if ev.EventType != audit.EventConfigReloadSucceeded {
		t.Errorf("event_type = %s", ev.EventType)
	}
	if ev.Source != audit.SourceKeeperInternal {
		t.Errorf("source = %s, want keeper_internal", ev.Source)
	}
	paths, _ := ev.Payload["changed_paths"].([]string)
	if len(paths) != 2 {
		t.Errorf("changed_paths = %v, want both overlay paths", ev.Payload["changed_paths"])
	}
}

// Without a source the store behaves exactly as before — `soul`/`soul-lint` are
// untouched by the overlay.
func TestOverlay_NilSourceIsFileOnly(t *testing.T) {
	path := fixtureKeeperPath(t)
	store, _, err := LoadKeeperStore(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperStore: %v", err)
	}
	if res := store.RefreshOverlay(context.Background()); res.Swapped {
		t.Errorf("refresh swapped without an overlay source")
	}
	if res := store.Reload(context.Background(), ReloadSourceSignal); !res.Swapped {
		t.Errorf("plain reload broken by the overlay hook: %+v", res.Diagnostics)
	}
	if len(store.AppliedOverlay()) != 0 {
		t.Errorf("AppliedOverlay is not empty without a source")
	}
}
