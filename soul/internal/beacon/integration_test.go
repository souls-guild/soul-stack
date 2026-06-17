package beacon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TestSchedulerWithRealFileBeacon — сквозной путь VigilSnapshot → реальный
// core.beacon.file_changed → edge-triggered Portent на реальном (не fake)
// тикере. Покрывает production newTicker + Default-реестр + parse interval.
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

	// baseline (первый тик) не должен дать Portent.
	select {
	case ev := <-s.Portents():
		t.Fatalf("baseline не должен эмитить Portent, получили %q", ev.GetBeaconName())
	case <-time.After(80 * time.Millisecond):
	}

	// Меняем файл — следующий тик должен поднять Portent.
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

// TestSchedulerReplaceAllSwapsSet — ReplaceAll заменяет набор: после второго
// snapshot с другим Vigil первый перестаёт наблюдаться, второй наблюдает.
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
	time.Sleep(60 * time.Millisecond) // дать baseline установиться

	// ReplaceAll: только watch-b. watch-a останавливается.
	vigilB := &keeperv1.VigilDef{Name: "watch-b", Check: FileChangedName, Interval: "20ms", Params: paramStruct(t, map[string]any{"path": pathB})}
	s.Apply(ctx, []*keeperv1.VigilDef{vigilB})
	time.Sleep(60 * time.Millisecond) // baseline для watch-b

	// Меняем A — Portent НЕ должен прийти (watch-a остановлен).
	if err := os.WriteFile(pathA, []byte("a-changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Меняем B — Portent должен прийти от watch-b.
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
