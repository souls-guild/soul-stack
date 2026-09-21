package reaper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// fakeVault is an in-memory vault client replacement for reconcile tests.
// names[] is returned by ListKV; created[key_id] is created_time for
// ReadKVMetadata.
type fakeVault struct {
	names    []string
	created  map[string]time.Time
	listErr  error
	metaErr  map[string]error
	listCnt  int
	metaCnt  int
	metaSeen []string
}

func (f *fakeVault) ListKV(_ context.Context, prefix string) ([]string, error) {
	f.listCnt++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if prefix != orphanScanPrefix {
		// Regression guard: the rule must scan only the hardcoded prefix.
		return nil, errors.New("unexpected prefix: " + prefix)
	}
	return f.names, nil
}

func (f *fakeVault) ReadKVMetadata(_ context.Context, path string) (time.Time, error) {
	f.metaCnt++
	f.metaSeen = append(f.metaSeen, path)
	// path = orphanScanPrefix + "/" + key_id; extract key_id.
	keyID := path[len(orphanScanPrefix)+1:]
	if f.metaErr != nil {
		if err, ok := f.metaErr[keyID]; ok {
			return time.Time{}, err
		}
	}
	t, ok := f.created[keyID]
	if !ok {
		return time.Time{}, errors.New("metadata not found")
	}
	return t, nil
}

// fakeKeys replaces sigil.ListAllKeyIDs with a set of live key_id values.
type fakeKeys struct {
	live map[string]struct{}
	err  error
}

func (f fakeKeys) ListAllKeyIDs(_ context.Context) (map[string]struct{}, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.live, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func set(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

const testGrace = 24 * time.Hour

// fixedNow is a deterministic clock for grace comparisons.
func fixedNow() time.Time { return time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC) }

// (a) key exists in both Vault and PG, so it is not an orphan.
func TestReportOrphan_KeyInBothVaultAndPG_NotOrphan(t *testing.T) {
	v := &fakeVault{
		names:   []string{"k-active"},
		created: map[string]time.Time{"k-active": fixedNow().Add(-48 * time.Hour)},
	}
	vr := NewVaultReconciler(v, fakeKeys{live: set("k-active")}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("want 0 orphans, got %d", got)
	}
	// The key was in PG, so metadata-read must not be done for it.
	if v.metaCnt != 0 {
		t.Errorf("metadata read should be skipped for live key, got %d reads", v.metaCnt)
	}
}

// (b) candidate exists, but created_time is within grace, so it is NOT counted
// because Introduce may be racing.
func TestReportOrphan_CandidateWithinGrace_NotOrphan(t *testing.T) {
	v := &fakeVault{
		names:   []string{"k-fresh"},
		created: map[string]time.Time{"k-fresh": fixedNow().Add(-time.Hour)}, // younger than 24h grace
	}
	vr := NewVaultReconciler(v, fakeKeys{live: set()}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("fresh candidate must not be orphan (Introduce race), got %d", got)
	}
}

// (c) candidate older than grace becomes an orphan, count=1.
func TestReportOrphan_OldCandidate_IsOrphan(t *testing.T) {
	v := &fakeVault{
		names:   []string{"k-orphan"},
		created: map[string]time.Time{"k-orphan": fixedNow().Add(-48 * time.Hour)}, // older than grace
	}
	vr := NewVaultReconciler(v, fakeKeys{live: set()}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("want 1 orphan, got %d", got)
	}
}

// Grace boundary: created_time exactly at cutoff. created.After(cutoff) ==
// false, so it is an orphan because >= grace is considered old.
func TestReportOrphan_ExactlyAtGrace_IsOrphan(t *testing.T) {
	v := &fakeVault{
		names:   []string{"k-edge"},
		created: map[string]time.Time{"k-edge": fixedNow().Add(-testGrace)},
	}
	vr := NewVaultReconciler(v, fakeKeys{live: set()}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("key exactly at grace cutoff should be orphan, got %d", got)
	}
}

// (d) vault nil degrades to (0, error).
func TestReportOrphan_NilVault_Degrades(t *testing.T) {
	vr := NewVaultReconciler(nil, fakeKeys{live: set()}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err == nil {
		t.Fatal("want error on nil vault, got nil")
	}
	if got != 0 {
		t.Errorf("want 0 on degrade, got %d", got)
	}
}

// (e) retired key in PG is NOT an orphan because ListAllKeyIDs includes all
// statuses.
func TestReportOrphan_RetiredKeyInPG_NotOrphan(t *testing.T) {
	v := &fakeVault{
		names:   []string{"k-retired"},
		created: map[string]time.Time{"k-retired": fixedNow().Add(-100 * 24 * time.Hour)},
	}
	// k-retired is present in the live set because retired also counts as live.
	vr := NewVaultReconciler(v, fakeKeys{live: set("k-retired")}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("retired key (still live) must not be orphan, got %d", got)
	}
	if v.metaCnt != 0 {
		t.Errorf("metadata read should be skipped for retired-but-live key, got %d", v.metaCnt)
	}
}

// Empty LIST returns 0 without calling PG.
func TestReportOrphan_EmptyList_NoPGCall(t *testing.T) {
	v := &fakeVault{names: nil}
	keys := &countingKeys{}
	vr := NewVaultReconciler(v, keys, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("want 0, got %d", got)
	}
	if keys.calls != 0 {
		t.Errorf("PG ListAllKeyIDs must be skipped on empty vault list, got %d calls", keys.calls)
	}
}

type countingKeys struct{ calls int }

func (c *countingKeys) ListAllKeyIDs(_ context.Context) (map[string]struct{}, error) {
	c.calls++
	return set(), nil
}

// batchSize limits metadata reads per run.
func TestReportOrphan_BatchSizeCapsMetadataReads(t *testing.T) {
	old := fixedNow().Add(-48 * time.Hour)
	v := &fakeVault{
		names: []string{"k1", "k2", "k3", "k4", "k5"},
		created: map[string]time.Time{
			"k1": old, "k2": old, "k3": old, "k4": old, "k5": old,
		},
	}
	vr := NewVaultReconciler(v, fakeKeys{live: set()}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.metaCnt != 2 {
		t.Errorf("batchSize=2 must cap metadata reads at 2, got %d", v.metaCnt)
	}
	if got != 2 {
		t.Errorf("want 2 orphans within batch cap, got %d", got)
	}
}

// ListKV error returns (0, error), and PG is not called.
func TestReportOrphan_ListError_Propagates(t *testing.T) {
	v := &fakeVault{listErr: errors.New("vault transport down")}
	keys := &countingKeys{}
	vr := NewVaultReconciler(v, keys, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err == nil {
		t.Fatal("want error on ListKV failure")
	}
	if got != 0 {
		t.Errorf("want 0 on error, got %d", got)
	}
	if keys.calls != 0 {
		t.Errorf("PG must not be called after ListKV error, got %d", keys.calls)
	}
}

// ListAllKeyIDs error returns (0, error).
func TestReportOrphan_PGError_Propagates(t *testing.T) {
	v := &fakeVault{names: []string{"k1"}}
	vr := NewVaultReconciler(v, fakeKeys{err: errors.New("pg down")}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err == nil {
		t.Fatal("want error on ListAllKeyIDs failure")
	}
	if got != 0 {
		t.Errorf("want 0 on error, got %d", got)
	}
}

// A metadata-read failure for one candidate does not fail the whole run: the
// candidate is skipped, and the rest are processed.
func TestReportOrphan_MetadataReadError_SkipsCandidate(t *testing.T) {
	old := fixedNow().Add(-48 * time.Hour)
	v := &fakeVault{
		names:   []string{"k-bad", "k-good"},
		created: map[string]time.Time{"k-good": old},
		metaErr: map[string]error{"k-bad": errors.New("read failed")},
	}
	vr := NewVaultReconciler(v, fakeKeys{live: set()}, discardLogger(), fixedNow)

	got, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100)
	if err != nil {
		t.Fatalf("metadata read error of one candidate must not fail the run: %v", err)
	}
	if got != 1 {
		t.Errorf("want 1 orphan (k-good), got %d", got)
	}
}

// scope-prefix is hardcoded: the rule scans only orphanScanPrefix.
func TestReportOrphan_ScanPrefixIsHardcoded(t *testing.T) {
	if orphanScanPrefix != "keeper/sigil-keys" {
		t.Fatalf("orphanScanPrefix must be hardcoded to keeper/sigil-keys, got %q", orphanScanPrefix)
	}
	old := fixedNow().Add(-48 * time.Hour)
	v := &fakeVault{
		names:   []string{"k1"},
		created: map[string]time.Time{"k1": old},
	}
	vr := NewVaultReconciler(v, fakeKeys{live: set()}, discardLogger(), fixedNow)

	if _, err := vr.ReportOrphanVaultKeys(context.Background(), testGrace, 100); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// fakeVault.ListKV returns an error when prefix != orphanScanPrefix, so a
	// successful run already proves that the rule sends exactly the hardcoded
	// prefix. Additionally, metadata-path must start with the same prefix.
	for _, p := range v.metaSeen {
		if want := orphanScanPrefix + "/k1"; p != want {
			t.Errorf("metadata path %q does not match hardcoded prefix layout %q", p, want)
		}
	}
}
