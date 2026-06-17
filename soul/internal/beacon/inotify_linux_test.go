//go:build linux

package beacon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// L0 unit-тесты core.beacon.inotify (Linux-only). Real inotify-fd на tmpdir —
// под Linux это всегда доступно (даже без root), без testcontainers.

// TestInotify_QuietThenEvents — happy-path:
//   - первый Check: lazy-init watch + return state="quiet" (буфер пуст).
//   - touch файла в watched-каталоге → kernel шлёт IN_CREATE / IN_MODIFY.
//   - второй Check: state="events", data.count > 0, data.events содержит
//     запись с type="created".
func TestInotify_QuietThenEvents(t *testing.T) {
	dir := t.TempDir()

	b := NewInotify()
	params := paramStruct(t, map[string]any{"path": dir})

	state, _, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("первый Check: %v", err)
	}
	if state != stateInotifyQuiet {
		t.Fatalf("первый Check без событий должен быть quiet, got %q", state)
	}

	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Дать readLoop поднять событие. inotifyReadIdle = 100 ms.
	waitForInotifyEvents(t, b, params, 2*time.Second)

	state, data, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("второй Check: %v", err)
	}
	if state != stateInotifyEvents {
		t.Fatalf("после touch ожидали state=events, got %q", state)
	}
	if data.GetFields()["count"].GetNumberValue() < 1 {
		t.Errorf("data.count = %v, want >= 1", data.GetFields()["count"].GetNumberValue())
	}
	if got := data.GetFields()["path"].GetStringValue(); got != dir {
		t.Errorf("data.path = %q, want %q", got, dir)
	}
	events := data.GetFields()["events"].GetListValue().GetValues()
	if len(events) == 0 {
		t.Fatal("data.events пуст после touch")
	}
	firstType := events[0].GetStructValue().GetFields()["type"].GetStringValue()
	if firstType != "created" && firstType != "modified" {
		t.Errorf("первое событие type=%q, want created/modified", firstType)
	}
}

// TestInotify_FilterCreatedOnly — фильтр `events: ["created"]` отбрасывает
// IN_MODIFY/IN_ATTRIB на kernel-уровне (mask), beacon видит только IN_CREATE.
func TestInotify_FilterCreatedOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing")
	if err := os.WriteFile(target, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewInotify()
	params := paramStruct(t, map[string]any{
		"path":   dir,
		"events": []any{"created"},
	})

	// Baseline-Check регистрирует watch (фильтр только на created).
	if _, _, err := b.Check(context.Background(), params); err != nil {
		t.Fatalf("baseline Check: %v", err)
	}

	// IN_MODIFY (не в фильтре) — НЕ должен попасть в окно.
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	state, _, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("Check после modify: %v", err)
	}
	if state != stateInotifyQuiet {
		t.Errorf("modify не в фильтре, но state=%q", state)
	}

	// IN_CREATE — должен попасть.
	if err := os.WriteFile(filepath.Join(dir, "new"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForInotifyEvents(t, b, params, 2*time.Second)

	state, data, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("Check после create: %v", err)
	}
	if state != stateInotifyEvents {
		t.Errorf("create в фильтре, но state=%q", state)
	}
	if got := data.GetFields()["count"].GetNumberValue(); got < 1 {
		t.Errorf("count=%v, want >= 1", got)
	}
}

// TestInotify_MissingPath — путь не существует: inotify_add_watch вернёт
// ENOENT, beacon — ошибку (scheduler пропустит тик, baseline не установится).
func TestInotify_MissingPath(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "does-not-exist")

	b := NewInotify()
	_, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": absent}))
	if err == nil {
		t.Fatal("ожидали ошибку на отсутствующий path")
	}
}

// TestInotify_RecursiveRejected — recursive: true в MVP не поддерживается,
// beacon отвергает на валидации params (раньше любого syscall-а).
func TestInotify_RecursiveRejected(t *testing.T) {
	b := NewInotify()
	_, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":      "/tmp",
		"recursive": true,
	}))
	if err == nil {
		t.Fatal("recursive=true должно отвергаться в MVP")
	}
}

// TestInotify_MissingParam — отсутствие обязательного path → ошибка.
func TestInotify_MissingParam(t *testing.T) {
	b := NewInotify()
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("ожидали ошибку при отсутствии param path")
	}
}

// TestInotify_MultipleVigilsSeparateWatches — два Check-а с разными path
// получают независимые watch-и (один InotifyBeacon на процесс, per-path
// независимые kernel-fd). Изменение в dirA НЕ должно подняться в окне dirB.
func TestInotify_MultipleVigilsSeparateWatches(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	b := NewInotify()
	paramsA := paramStruct(t, map[string]any{"path": dirA})
	paramsB := paramStruct(t, map[string]any{"path": dirB})

	// Регистрируем оба watch-а.
	if _, _, err := b.Check(context.Background(), paramsA); err != nil {
		t.Fatalf("baseline A: %v", err)
	}
	if _, _, err := b.Check(context.Background(), paramsB); err != nil {
		t.Fatalf("baseline B: %v", err)
	}

	// Событие только в A.
	if err := os.WriteFile(filepath.Join(dirA, "x"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForInotifyEvents(t, b, paramsA, 2*time.Second)

	// B должен оставаться quiet.
	stateB, _, err := b.Check(context.Background(), paramsB)
	if err != nil {
		t.Fatalf("Check B: %v", err)
	}
	if stateB != stateInotifyQuiet {
		t.Errorf("watch B не должен видеть событие из A, state=%q", stateB)
	}

	// A должен поднять событие.
	stateA, _, err := b.Check(context.Background(), paramsA)
	if err != nil {
		t.Fatalf("Check A: %v", err)
	}
	if stateA != stateInotifyEvents {
		t.Errorf("watch A должен видеть своё событие, state=%q", stateA)
	}
}

// TestInotify_RegistryAndDefault — beacon зарегистрирован в Default-реестре под
// каноническим адресом из beaconaddr.Inotify. Гарантия инварианта «keeper-enum
// == soul-registry == beaconaddr» (см. shared/beaconaddr/beaconaddr_test.go).
func TestInotify_RegistryAndDefault(t *testing.T) {
	reg := Default()
	b, ok := reg.Lookup(InotifyName)
	if !ok {
		t.Fatalf("InotifyName=%q отсутствует в Default()", InotifyName)
	}
	if _, isInotify := b.(*InotifyBeacon); !isInotify {
		t.Errorf("Lookup(%q) вернул %T, want *InotifyBeacon", InotifyName, b)
	}
}

// TestInotify_SchedulerEdgeTriggered — сквозной путь через scheduler: смена
// quiet → events эмитит ровно один Portent с typed payload (V5-1 mapper) +
// data-веткой (deprecation period). Параллель TestSchedulerWithRealFileBeacon
// для file_changed.
func TestInotify_SchedulerEdgeTriggered(t *testing.T) {
	dir := t.TempDir()

	s := NewScheduler(SchedulerConfig{Registry: Default(), SID: "host.example"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	def := &keeperv1.VigilDef{
		Name:     "inotify-watch",
		Check:    InotifyName,
		Interval: "50ms",
		Params:   paramStruct(t, map[string]any{"path": dir}),
	}
	s.Apply(ctx, []*keeperv1.VigilDef{def})

	// Baseline (первый тик) — без Portent.
	select {
	case ev := <-s.Portents():
		t.Fatalf("baseline не должен эмитить Portent, got %q", ev.GetBeaconName())
	case <-time.After(200 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-s.Portents():
		if ev.GetBeaconName() != "inotify-watch" {
			t.Fatalf("beacon_name=%q, want inotify-watch", ev.GetBeaconName())
		}
		// V5-1 dual-write: data + typed payload.
		if ev.GetData() == nil {
			t.Error("data-ветка пуста (нарушение deprecation hand-off)")
		}
		ino := ev.GetInotify()
		if ino == nil {
			t.Fatal("typed payload InotifyPortent пуст")
		}
		if ino.GetPath() != dir {
			t.Errorf("InotifyPortent.path=%q, want %q", ino.GetPath(), dir)
		}
		if ino.GetCount() < 1 {
			t.Errorf("InotifyPortent.count=%d, want >= 1", ino.GetCount())
		}
		if len(ino.GetEvents()) == 0 {
			t.Error("InotifyPortent.events пуст")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("touch файла не поднял Portent через scheduler")
	}
}

// waitForInotifyEvents выполняет polling-Check до появления events ИЛИ
// timeout-а. Без сна перед Check kernel может не успеть доставить event-ы
// (readLoop spinит с inotifyReadIdle=100ms между read-syscall-ами). Не
// замусоривает основной тест-логикой sleep-ом.
func waitForInotifyEvents(t *testing.T, b *InotifyBeacon, params interface{}, timeout time.Duration) {
	t.Helper()
	// peek-Check: получаем буфер БЕЗ flush? Текущий Check всегда flush-ит, так
	// что peek-цикл портит окно теста. Вместо peek-а — пассивная пауза +
	// доверие, что 200–500 ms достаточно для kernel+readLoop. На CI-машине под
	// нагрузкой timeout может быть больше, поэтому ждём в несколько шагов.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if hasBufferedEvents(b) {
			return
		}
	}
}

// hasBufferedEvents — non-flushing peek в w.events. Используется только тестом
// для синхронизации; в production-Check всегда flush. Доступ под общим lock-ом
// beacon-а, чтобы не получить race с readLoop.
func hasBufferedEvents(b *InotifyBeacon) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.watches {
		w.mu.Lock()
		n := len(w.events)
		w.mu.Unlock()
		if n > 0 {
			return true
		}
	}
	return false
}
