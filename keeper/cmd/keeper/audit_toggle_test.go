package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditgate"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// toggleRecorder stands in for the Postgres writer at the bottom of the gated
// chain assembled by setupAudit.
type toggleRecorder struct {
	mu     sync.Mutex
	events []audit.EventType
}

func (r *toggleRecorder) Write(_ context.Context, ev *audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev.EventType)
	return nil
}

func (r *toggleRecorder) seen() []audit.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.EventType(nil), r.events...)
}

func (r *toggleRecorder) indexOf(want audit.EventType) int {
	for i, et := range r.seen() {
		if et == want {
			return i
		}
	}
	return -1
}

// gatedStand wires the same chain as setupAudit — recorder under the master gate
// — over a fixture store whose file can be edited and reloaded (a SIGHUP
// equivalent).
func gatedStand(t *testing.T) (*config.Store[config.KeeperConfig], audit.Writer, *toggleRecorder, string) {
	t.Helper()
	store, path := keeperFixtureStore(t)
	rec := &toggleRecorder{}
	writer := auditgate.New(auditgate.Config{
		Next:    rec,
		Enabled: func() bool { return store.Get().AuditEnabled() },
		KID:     store.Get().KID,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	store.SetAuditWriter(writer)
	return store, writer, rec, path
}

// setAuditEnabled rewrites the `enabled:` line of the fixture's audit block —
// the file-edit half of the SIGHUP path.
func setAuditEnabled(t *testing.T, path string, on bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	from, to := "audit:\n  enabled: true", "audit:\n  enabled: false"
	if on {
		from, to = to, from
	}
	edited := strings.Replace(string(data), from, to, 1)
	if edited == string(data) {
		t.Fatalf("audit block not found in fixture (looking for %q)", from)
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func appendToConfig(t *testing.T, path, block string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(path, append(data, []byte(block)...), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func reloadOrFail(t *testing.T, store *config.Store[config.KeeperConfig]) {
	t.Helper()
	if res := store.Reload(context.Background(), config.ReloadSourceSignal); !res.Swapped {
		t.Fatalf("reload did not swap: %+v", res.Diagnostics)
	}
}

// The acceptance criterion of NIM-194, end to end over a real config swap:
// `audit.enabled: false` stops ordinary writes, and the reload that turned it
// off is itself still journaled (the bypass) — so the trail explains its own
// gap rather than simply stopping.
func TestAuditToggle_DisablingStopsWritesButIsJournaled(t *testing.T) {
	store, writer, rec, path := gatedStand(t)

	if !store.Get().AuditEnabled() {
		t.Fatal("fixture starts with audit disabled")
	}
	setAuditEnabled(t, path, false)
	reloadOrFail(t, store)

	if store.Get().AuditEnabled() {
		t.Fatal("audit still enabled after the reload")
	}
	if rec.indexOf(audit.EventConfigReloadSucceeded) < 0 {
		t.Errorf("the reload that disabled audit was not journaled; log holds %v", rec.seen())
	}

	// An ordinary event now must not reach the writer.
	before := len(rec.seen())
	if err := writer.Write(context.Background(),
		&audit.Event{EventType: audit.EventOperatorCreated}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := rec.seen(); len(got) != before {
		t.Errorf("operator.created reached the writer with audit disabled: %v", got)
	}
}

// The marker precedes the gap it explains: `audit.disabled` is written in-line,
// before the first event that gets dropped. This is what an OnReload subscriber
// could not guarantee — config.Store.notify runs callbacks in their own
// goroutines, so a marker written there races the drops it must precede.
func TestAuditToggle_MarkerPrecedesTheFirstDrop(t *testing.T) {
	store, writer, rec, path := gatedStand(t)

	setAuditEnabled(t, path, false)
	reloadOrFail(t, store)

	for _, et := range []audit.EventType{audit.EventOperatorCreated, audit.EventOperatorRevoked} {
		if err := writer.Write(context.Background(), &audit.Event{EventType: et}); err != nil {
			t.Fatalf("Write(%s): %v", et, err)
		}
	}

	markerAt := rec.indexOf(audit.EventAuditDisabled)
	if markerAt < 0 {
		t.Fatalf("audit.disabled never written; log holds %v", rec.seen())
	}
	// The gate notices the flip on the reload event itself, so the marker leads
	// the trail from the swap onward — nothing under the closed gate precedes it.
	if markerAt != 0 {
		t.Errorf("audit.disabled at %d, want first of %v", markerAt, rec.seen())
	}
	if rec.indexOf(audit.EventOperatorCreated) >= 0 {
		t.Error("a dropped event was recorded")
	}
}

// The other end of the blind window: re-enabling is journaled too, so the trail
// bounds the gap rather than only starting it.
func TestAuditToggle_ReEnablingIsJournaled(t *testing.T) {
	store, writer, rec, path := gatedStand(t)

	setAuditEnabled(t, path, false)
	reloadOrFail(t, store)

	setAuditEnabled(t, path, true)
	reloadOrFail(t, store)

	if err := writer.Write(context.Background(),
		&audit.Event{EventType: audit.EventOperatorCreated}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if rec.indexOf(audit.EventAuditEnabled) < 0 {
		t.Errorf("audit.enabled not written; log holds %v", rec.seen())
	}
	if rec.indexOf(audit.EventOperatorCreated) < 0 {
		t.Errorf("ordinary events still dropped after re-enabling; log holds %v", rec.seen())
	}
}

// A reload that leaves the toggle alone must not emit a transition marker —
// otherwise every SIGHUP would litter the trail.
func TestAuditToggle_UnchangedReloadIsSilent(t *testing.T) {
	store, writer, rec, path := gatedStand(t)

	appendToConfig(t, path, "\nwatchman_fail_threshold: 4\n")
	reloadOrFail(t, store)
	if err := writer.Write(context.Background(),
		&audit.Event{EventType: audit.EventOperatorCreated}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, et := range rec.seen() {
		if et == audit.EventAuditDisabled || et == audit.EventAuditEnabled {
			t.Errorf("emitted %s on a reload that did not touch the toggle", et)
		}
	}
}
