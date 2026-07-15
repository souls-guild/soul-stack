package beacon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestSchedulerWithRealFileBeacon — end-to-end path VigilSnapshot → real
// core.beacon.file_changed → edge-triggered Portent on a real (not fake)
// ticker. Covers production newTicker + Default registry + interval parsing.
func TestSchedulerWithRealFileBeacon(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watched")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewScheduler(SchedulerConfig{Registry: Default(), SID: "host.example"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	def := &keeperv1.VigilDef{
		Name:     "conf-watch",
		Check:    FileChangedName,
		Interval: "20ms",
		Params:   paramStruct(t, map[string]any{"path": path}),
	}
	s.Apply(ctx, []*keeperv1.VigilDef{def})

	// baseline (first tick) must not emit a Portent.
	select {
	case ev := <-s.Portents():
		t.Fatalf("baseline не должен эмитить Portent, получили %q", ev.GetBeaconName())
	case <-time.After(80 * time.Millisecond):
	}

	// Change the file — the next tick should raise a Portent.
	if err := os.WriteFile(path, []byte("v2-changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-s.Portents():
		if ev.GetBeaconName() != "conf-watch" {
			t.Fatalf("beacon_name = %q, want conf-watch", ev.GetBeaconName())
		}
		if ev.GetData().GetFields()["path"].GetStringValue() != path {
			t.Error("Portent.data должен нести путь файла")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("смена файла не подняла Portent")
	}
}

// TestSchedulerReplaceAllSwapsSet — ReplaceAll swaps the set: after a second
// snapshot with a different Vigil, the first stops being watched and the
// second is watched.
func TestSchedulerReplaceAllSwapsSet(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a")
	pathB := filepath.Join(dir, "b")
	if err := os.WriteFile(pathA, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewScheduler(SchedulerConfig{Registry: Default(), SID: "host.example"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	vigilA := &keeperv1.VigilDef{Name: "watch-a", Check: FileChangedName, Interval: "20ms", Params: paramStruct(t, map[string]any{"path": pathA})}
	s.Apply(ctx, []*keeperv1.VigilDef{vigilA})
	time.Sleep(60 * time.Millisecond) // let baseline settle

	// ReplaceAll: only watch-b. watch-a is stopped.
	vigilB := &keeperv1.VigilDef{Name: "watch-b", Check: FileChangedName, Interval: "20ms", Params: paramStruct(t, map[string]any{"path": pathB})}
	s.Apply(ctx, []*keeperv1.VigilDef{vigilB})
	time.Sleep(60 * time.Millisecond) // baseline for watch-b

	// Change A — a Portent must NOT arrive (watch-a is stopped).
	if err := os.WriteFile(pathA, []byte("a-changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Change B — a Portent should arrive from watch-b.
	if err := os.WriteFile(pathB, []byte("b-changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-s.Portents():
		if ev.GetBeaconName() != "watch-b" {
			t.Fatalf("после ReplaceAll ожидали Portent только от watch-b, получили %q", ev.GetBeaconName())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch-b не поднял Portent после ReplaceAll")
	}
}
