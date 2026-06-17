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

// L1 integration-тест core.beacon.inotify (Linux + build-tag `integration`).
// Длинный happy-path через scheduler с реалистичным interval (200 ms) и
// несколькими FS-операциями подряд: scheduler агрегирует события в одном
// Portent (fold-adapter), а не эмитит по одному на event. В отличие от L0
// (быстрый interval 50 ms, одно событие) — здесь проверка fold-семантики в
// настоящем потоке: kernel-events накапливаются, scheduler-tick их забирает
// одним окном.
//
// Запускать: `make test-integration` (в шапке Makefile).
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

	// Дать baseline установиться (без Portent).
	select {
	case ev := <-s.Portents():
		t.Fatalf("baseline не должен эмитить Portent: %q", ev.GetBeaconName())
	case <-time.After(400 * time.Millisecond):
	}

	// Серия из 5 FS-событий в одном dir. inotify-fold должен схлопнуть их в
	// один Portent (fold-adapter в Soul-side — α-вариант из architect-вердикта),
	// а scheduler — эмитнуть один edge-triggered Portent (quiet → events).
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
			t.Fatal("typed Inotify payload пуст в L1-сценарии")
		}
		if ino.GetCount() < 5 {
			t.Errorf("InotifyPortent.count=%d, want >= 5 (fold-семантика)", ino.GetCount())
		}
		if ino.GetPath() != dir {
			t.Errorf("InotifyPortent.path=%q, want %q", ino.GetPath(), dir)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("серия из 5 touch-ей не подняла Portent через scheduler")
	}
}
