//go:build linux && integration

package beacon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// L1 integration test for core.beacon.inotify (Linux + `integration` build
// tag). A long happy-path through the scheduler with a realistic interval
// (200 ms) and several FS operations in a row: the scheduler aggregates
// events into one Portent (fold-adapter), rather than emitting one per
// event. Unlike L0 (fast 50 ms interval, one event) — this checks fold
// semantics under a real stream: kernel events accumulate, the scheduler
// tick collects them in one window.
//
// Run with: `make test-integration` (see the Makefile header).
func TestL1InotifyFold_BatchOfFsEvents(t *testing.T) {
	dir := t.TempDir()

	s := NewScheduler(SchedulerConfig{Registry: Default(), SID: "soul-l1.example.com"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	def := &keeperv1.VigilDef{
		Name:     "inotify-batch",
		Check:    InotifyName,
		Interval: "200ms",
		Params:   paramStruct(t, map[string]any{"path": dir}),
	}
	s.Apply(ctx, []*keeperv1.VigilDef{def})

	// Let the baseline settle (no Portent).
	select {
	case ev := <-s.Portents():
		t.Fatalf("baseline should not emit a Portent: %q", ev.GetBeaconName())
	case <-time.After(400 * time.Millisecond):
	}

	// A series of 5 FS events in one dir. inotify-fold should collapse them
	// into one Portent (Soul-side fold-adapter — the α variant from the
	// architect's verdict), and the scheduler emits one edge-triggered
	// Portent (quiet → events).
	for i := 0; i < 5; i++ {
		fname := filepath.Join(dir, "f-"+string(rune('a'+i)))
		if err := os.WriteFile(fname, []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case ev := <-s.Portents():
		ino := ev.GetInotify()
		if ino == nil {
			t.Fatal("typed Inotify payload empty in the L1 scenario")
		}
		if ino.GetCount() < 5 {
			t.Errorf("InotifyPortent.count=%d, want >= 5 (fold semantics)", ino.GetCount())
		}
		if ino.GetPath() != dir {
			t.Errorf("InotifyPortent.path=%q, want %q", ino.GetPath(), dir)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a series of 5 touches did not raise a Portent via the scheduler")
	}
}
