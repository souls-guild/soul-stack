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

// TestSchedulerDropOnOverflow — drop-on-overflow через РЕАЛЬНЫЙ loop Vigil-а (qa
// coverage_gap #3): буфер Portents заполнен, последующая смена State в loop-е
// дропает событие + warn + soul_beacon_portents_dropped_total++; Vigil-горутина
// не залипает (продолжает Check-и), паники нет.
//
// Отличие от TestEmit_DropIncrementsMetric (тот зовёт s.emit напрямую): здесь
// дроп идёт по edge-triggered пути loop→emit при заполненном канале, и тест
// утверждает живучесть горутины после дропа.
func TestSchedulerDropOnOverflow(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBeaconMetrics(reg)

	fb := newFakeBeacon("up")
	s := NewScheduler(SchedulerConfig{
		Registry:      regWith("core.beacon.x", fb),
		SID:           "host.example",
		PortentBuffer: 1, // ёмкость 1 — второй неслитый Portent дропается
		Metrics:       m,
	})
	mt := NewManualTicker()
	s.SetTicker(func(time.Duration) Ticker { return mt })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	s.Apply(ctx, []*keeperv1.VigilDef{vigil("v1", "core.beacon.x", "1s")})

	// baseline (up) — без Portent.
	mt.Tick()
	waitChecked(t, fb)

	// 1-я смена up→down: emit занимает единственный слот буфера (НЕ дроп). Канал
	// намеренно НЕ читаем — он остаётся полным.
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)

	// 2-я смена down→up: буфер полон → emit уходит в drop-ветку (warn + метрика).
	fb.SetState("up")
	mt.Tick()
	waitChecked(t, fb)

	// 3-я смена up→down: горутина не залипла после дропа — Check снова вызывается
	// (живучесть Vigil-а; буфер всё ещё полон, тоже дроп).
	fb.SetState("down")
	mt.Tick()
	waitChecked(t, fb)

	// Барьер синхронизации: no-change тик (state остаётся down). emit на нём не
	// зовётся, но чтобы консьюмнуть этот тик, горутина обязана вернуться в select
	// — а значит emit предыдущего (3-го) тика гарантированно завершён. Без этого
	// барьера scrape мог бы прочитать метрику до 2-го дропа (emit идёт ПОСЛЕ
	// сигнала checked).
	mt.Tick()
	waitChecked(t, fb)

	// Слитый слот — это ПЕРВЫЙ Portent (down). Две последующие смены дропнуты.
	first := expectPortent(t, s)
	if first.GetBeaconName() != "v1" {
		t.Fatalf("ожидали первый (неслитый) Portent v1, got %q", first.GetBeaconName())
	}

	body := obstest.Scrape(t, reg.Gatherer())
	// Ровно два дропа (2-я и 3-я смены при полном буфере; 1-я смена слита в буфер).
	if !strings.Contains(body, "soul_beacon_portents_dropped_total 2") {
		t.Errorf("ожидали 2 дропа через loop; got=\n%s", body)
	}
}

// TestSchedulerFileFlapMissingHash — edge-triggered flap появление/исчезновение
// файла через scheduler (qa coverage_gap #6): реальный core.beacon.file_changed
// под ManualTicker, переход "missing"↔hash на каждой смене даёт Portent.
//
// Последовательность: missing (baseline) → present (Portent) → удалён (Portent) →
// present снова (Portent). Каждая смена State edge-triggered, дублей нет.
func TestSchedulerFileFlapMissingHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flap.conf")
	// Файл изначально отсутствует — baseline State = "missing".

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

	// baseline: missing — без Portent.
	mt.Tick()
	expectNoPortent(t, s)

	// missing → present (создан): Portent с хешем в data.
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt.Tick()
	ev := expectPortent(t, s)
	if ev.GetBeaconName() != "flap-watch" {
		t.Fatalf("present: beacon_name = %q, want flap-watch", ev.GetBeaconName())
	}
	if ev.GetData().GetFields()["sha256"].GetStringValue() == "" {
		t.Error("present-Portent должен нести sha256 в data")
	}

	// present → missing (удалён): переход hash→"missing" — тоже смена State.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mt.Tick()
	ev = expectPortent(t, s)
	if ev.GetData().GetFields()["state"].GetStringValue() != string(stateFileMissing) {
		t.Errorf("missing-Portent data.state = %q, want missing", ev.GetData().GetFields()["state"].GetStringValue())
	}

	// missing → present снова: edge-triggered поднимает Portent на каждой смене.
	if err := os.WriteFile(path, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt.Tick()
	if ev := expectPortent(t, s); ev.GetData().GetFields()["sha256"].GetStringValue() == "" {
		t.Error("повторное появление должно дать Portent с sha256")
	}

	// Стабильное состояние (тот же файл) — без нового Portent (no-change guard).
	mt.Tick()
	expectNoPortent(t, s)
}
