package beacon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// TestSchedulerDropOnOverflow — drop-on-overflow through the REAL Vigil loop
// (qa coverage_gap #3): the Portents buffer is full, a subsequent State
// change in the loop drops the event + warn + soul_beacon_portents_dropped_total++;
// the Vigil goroutine doesn't stick (keeps doing Checks), no panic.
//
// Differs from TestEmit_DropIncrementsMetric (which calls s.emit directly):
// here the drop happens via the edge-triggered loop→emit path when the
// channel is full, and the test asserts the goroutine survives the drop.
func TestSchedulerDropOnOverflow(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBeaconMetrics(reg)

	fb := newFakeBeacon("up")
	s := NewScheduler(SchedulerConfig{
		Registry:      regWith("core.beacon.x", fb),
		SID:           "host.example",
		PortentBuffer: 1, // capacity 1 — the second unbuffered Portent gets dropped
		Metrics:       m,
	})
	mt := NewManualTicker()
	s.SetTicker(func(time.Duration) Ticker { return mt })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	// baseline (up) — no Portent.
	mt.Tick()
	waitChecked(t, fb)

	// 1st transition up→down: emit takes the buffer's only slot (NOT a drop).
	// The channel is deliberately NOT drained — it stays full.
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)

	// 2nd transition down→up: buffer full → emit takes the drop branch (warn + metric).
	fb.SetState("up")
	mt.Tick()
	waitChecked(t, fb)

	// 3rd transition up→down: the goroutine didn't stick after the drop — Check
	// is called again (Vigil survives; buffer is still full, also dropped).
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)

	// Sync barrier: a no-change tick (state stays down). emit isn't called for
	// it, but to consume this tick the goroutine must return to select — which
	// guarantees the previous (3rd) tick's emit has completed. Without this
	// barrier, scrape could read the metric before the 2nd drop (emit runs
	// AFTER the checked signal).
	mt.Tick()
	waitChecked(t, fb)

	// The buffered slot is the FIRST Portent (down). The next two transitions are dropped.
	first := expectPortent(t, s)
	if first.GetBeaconName() != "v1" {
		t.Fatalf("expected the first (unmerged) Portent v1, got %q", first.GetBeaconName())
	}

	body := obstest.Scrape(t, reg.Gatherer())
	// Exactly two drops (2nd and 3rd transitions while the buffer was full; the 1st was buffered).
	if !strings.Contains(body, "soul_beacon_portents_dropped_total 2") {
		t.Errorf("expected 2 drops through the loop; got=\n%s", body)
	}
}

// TestSchedulerFileFlapMissingHash — edge-triggered flap of a file
// appearing/disappearing via the scheduler (qa coverage_gap #6): real
// core.beacon.file_changed under ManualTicker, each "missing"↔hash
// transition produces a Portent.
//
// Sequence: missing (baseline) → present (Portent) → removed (Portent) →
// present again (Portent). Every State transition is edge-triggered, no
// duplicates.
func TestSchedulerFileFlapMissingHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flap.conf")
	// The file is initially absent — baseline State = "missing".

	reg := &Registry{beacons: map[string]Beacon{FileChangedName: NewFileChanged()}}
	s := NewScheduler(SchedulerConfig{Registry: reg, SID: "host.example"})
	mt := NewManualTicker()
	s.SetTicker(func(time.Duration) Ticker { return mt })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	def := &keeperv1.VigilDef{
		Name:     "flap-watch",
		Check:    FileChangedName,
		Interval: "1s",
		Params:   paramStruct(t, map[string]any{"path": path}),
	}
	s.Apply(ctx, []*keeperv1.VigilDef{def})

	// baseline: missing — no Portent.
	mt.Tick()
	expectNoPortent(t, s)

	// missing → present (created): Portent carries a hash in data.
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt.Tick()
	ev := expectPortent(t, s)
	if ev.GetBeaconName() != "flap-watch" {
		t.Fatalf("present: beacon_name = %q, want flap-watch", ev.GetBeaconName())
	}
	if ev.GetData().GetFields()["sha256"].GetStringValue() == "" {
		t.Error("present Portent must carry sha256 in data")
	}

	// present → missing (removed): the hash→"missing" transition is also a State change.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mt.Tick()
	ev = expectPortent(t, s)
	if ev.GetData().GetFields()["state"].GetStringValue() != string(stateFileMissing) {
		t.Errorf("missing-Portent data.state = %q, want missing", ev.GetData().GetFields()["state"].GetStringValue())
	}

	// missing → present again: edge-triggered raises a Portent on every transition.
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt.Tick()
	if ev := expectPortent(t, s); ev.GetData().GetFields()["sha256"].GetStringValue() == "" {
		t.Error("reappearance should produce a Portent with sha256")
	}

	// Stable state (same file) — no new Portent (no-change guard).
	mt.Tick()
	expectNoPortent(t, s)
}
