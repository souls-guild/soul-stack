package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

// TestAcceptAttempt_FirstAttemptAccepted — первый ApplyRequest по apply_id
// принимается и фиксирует seen (ADR-027(g)).
func TestAcceptAttempt_FirstAttemptAccepted(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	if !r.AcceptAttempt("apply-1", 1) {
		t.Fatalf("AcceptAttempt(apply-1, 1) = false, want true (первый attempt)")
	}
	if got := r.lastSeenAttempt["apply-1"]; got != 1 {
		t.Errorf("seen[apply-1] = %d, want 1", got)
	}
}

// TestAcceptAttempt_HigherAttemptAccepted — пере-claim с бОльшим attempt
// (recovery вернул Ward → ClaimNext инкрементил) принимается и сдвигает seen.
func TestAcceptAttempt_HigherAttemptAccepted(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 1)
	if !r.AcceptAttempt("apply-1", 2) {
		t.Fatalf("AcceptAttempt(apply-1, 2) = false, want true (больший attempt)")
	}
	if got := r.lastSeenAttempt["apply-1"]; got != 2 {
		t.Errorf("seen[apply-1] = %d, want 2", got)
	}
}

// TestAcceptAttempt_EqualAttemptAccepted — равный attempt принимается
// (повторная доставка того же epoch — не stale; «==» фенсить нельзя, SID-lease
// отсекает истинный дубль того же attempt).
func TestAcceptAttempt_EqualAttemptAccepted(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 2)
	if !r.AcceptAttempt("apply-1", 2) {
		t.Errorf("AcceptAttempt(apply-1, 2) повторно = false, want true (==seen не stale)")
	}
}

// TestAcceptAttempt_StaleRejected — attempt < seen отвергается (stale-дубль:
// протухший Ward, чей apply ещё в полёте, а пере-claim с большим attempt уже
// принят).
func TestAcceptAttempt_StaleRejected(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 3) // оригинальный (больший) уже принят
	if r.AcceptAttempt("apply-1", 1) {
		t.Fatalf("AcceptAttempt(apply-1, 1) = true, want false (stale < seen=3)")
	}
	// seen НЕ откатился назад.
	if got := r.lastSeenAttempt["apply-1"]; got != 3 {
		t.Errorf("seen[apply-1] = %d, want 3 (stale не должен сдвигать seen)", got)
	}
}

// TestAcceptAttempt_ZeroNeverFenced — attempt=0 (старый Keeper без fencing-поля,
// forward-compat) всегда принимается и НЕ записывается в кеш, чтобы не «отравить»
// seen для последующих fencing-запросов.
func TestAcceptAttempt_ZeroNeverFenced(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	// Даже после виденного attempt=5 нулевой принимается (старый Keeper).
	r.AcceptAttempt("apply-1", 5)
	if !r.AcceptAttempt("apply-1", 0) {
		t.Errorf("AcceptAttempt(apply-1, 0) = false, want true (старый Keeper не фенсится)")
	}
	// attempt=0 не сдвинул seen вниз.
	if got := r.lastSeenAttempt["apply-1"]; got != 5 {
		t.Errorf("seen[apply-1] = %d, want 5 (0 не пишется в кеш)", got)
	}

	// Чистый apply без виденного: 0 принят, кеш пуст (0 не пишется).
	if !r.AcceptAttempt("apply-fresh", 0) {
		t.Errorf("AcceptAttempt(apply-fresh, 0) = false, want true")
	}
	if _, ok := r.lastSeenAttempt["apply-fresh"]; ok {
		t.Errorf("seen[apply-fresh] записан для attempt=0, ожидалось отсутствие")
	}
}

// TestAcceptAttempt_PerApplyIDIsolation — кеш ведётся per apply_id: stale на
// одном прогоне не влияет на другой.
func TestAcceptAttempt_PerApplyIDIsolation(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-a", 5)
	// apply-b видит attempt=1 впервые — принимается (изоляция от apply-a).
	if !r.AcceptAttempt("apply-b", 1) {
		t.Errorf("AcceptAttempt(apply-b, 1) = false, want true (другой apply_id)")
	}
}

// TestAcceptAttempt_RejectedIncrementsMetric — отвергнутый stale-дубль
// инкрементирует soul_apply_fenced_total (B1: метрика — единственный наружный
// след отказа).
func TestAcceptAttempt_RejectedIncrementsMetric(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)
	r := NewApplyRunner(mapRegistry{}, m)

	r.AcceptAttempt("apply-1", 2)
	// Два stale-дубля подряд → счётчик 2.
	if r.AcceptAttempt("apply-1", 1) {
		t.Fatal("первый stale принят, want отвергнут")
	}
	if r.AcceptAttempt("apply-1", 0+1) { // attempt=1 < seen=2
		t.Fatal("второй stale принят, want отвергнут")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "soul_apply_fenced_total 2") {
		t.Errorf("expected soul_apply_fenced_total 2; got=\n%s", body)
	}
}

// TestAcceptAttempt_AcceptedDoesNotIncrementMetric — принятый (не-stale) запрос
// НЕ трогает fenced-счётчик.
func TestAcceptAttempt_AcceptedDoesNotIncrementMetric(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterApplyMetrics(reg)
	r := NewApplyRunner(mapRegistry{}, m)

	r.AcceptAttempt("apply-1", 1)
	r.AcceptAttempt("apply-1", 2) // больший — принят
	r.AcceptAttempt("apply-2", 0) // старый Keeper — принят

	body := obstest.Scrape(t, reg.Gatherer())
	// CounterVec/Counter без Inc не публикуется — fenced-серии в body быть не
	// должно (или 0). Проверяем отсутствие положительного значения.
	if strings.Contains(body, "soul_apply_fenced_total 1") ||
		strings.Contains(body, "soul_apply_fenced_total 2") {
		t.Errorf("fenced-счётчик инкрементирован на принятых запросах; got=\n%s", body)
	}
}

// TestAcceptAttempt_CachePersistsAcrossRunnerLifetime — кеш живёт в ApplyRunner
// (per-process) и переживает reconnect-swap стрима: в cmd/soul при failback/
// reconnect пересоздаётся StreamSession, но runner ОДИН на процесс. Эмулируем
// swap прогоном двух разных sink-ов (≈ двух сессий) на одном runner — seen
// сохраняется между ними, поэтому stale после «swap» отвергается.
func TestAcceptAttempt_CachePersistsAcrossRunnerLifetime(t *testing.T) {
	reg := mapRegistry{
		"core.pkg": &fakeModule{
			applyFunc: func(_ *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
				return stream.Send(&pluginv1.ApplyEvent{})
			},
		},
	}
	r := NewApplyRunner(reg, nil)

	// Сессия №1: принимаем attempt=2 и исполняем.
	if !r.AcceptAttempt("apply-x", 2) {
		t.Fatal("attempt=2 на сессии №1 отвергнут")
	}
	sink1 := &recordingSink{}
	if err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "apply-x",
		Tasks:   []*keeperv1.RenderedTask{{Module: "core.pkg.installed"}},
		Attempt: 2,
	}, sink1); err != nil {
		t.Fatalf("Run сессии №1: %v", err)
	}

	// «reconnect-swap»: тот же runner, новая сессия (sink2). Прилетел stale
	// attempt=1 (recovery вернул в очередь ещё-живой Ward; оригинал с attempt=2
	// уже отработал). Кеш per-process помнит seen=2 → отвергаем.
	if r.AcceptAttempt("apply-x", 1) {
		t.Fatal("stale attempt=1 после swap принят — кеш не пережил swap (баг)")
	}
}

// TestAcceptAttempt_RaceOnGuardMap — конкурентные AcceptAttempt по разным и
// одинаковым apply_id-ам не дают data race на lastSeenAttempt (-race).
func TestAcceptAttempt_RaceOnGuardMap(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ids := []string{"a", "b", "c"}
			for i := int32(1); i <= 100; i++ {
				_ = r.AcceptAttempt(ids[int(i)%len(ids)], i)
				_ = r.AcceptAttempt("shared", i)
			}
		}(w)
	}
	wg.Wait()
}

// TestAcceptAttempt_B1_NoSideChannelOnReject — B1-инвариант на уровне guard-а:
// отвергнутый stale ничего не делает кроме метрики/лога — RunResult/TaskEvent НЕ
// рождаются здесь (их шлёт только Run, который при отказе caller-ом не
// вызывается). Проверяем, что отказ — чистый bool без записи в active-реестр
// (нет зарегистрированного cancel, который мог бы намекнуть на запуск Run).
func TestAcceptAttempt_B1_NoSideChannelOnReject(t *testing.T) {
	r := NewApplyRunner(mapRegistry{}, nil)
	r.AcceptAttempt("apply-1", 2)
	_ = r.AcceptAttempt("apply-1", 1) // stale, отвергнут

	// Отвергнутый запрос не запускал Run → не регистрировал cancel в active.
	if r.Cancel("apply-1") {
		t.Errorf("Cancel(apply-1) = true: отвергнутый stale не должен регистрировать active-apply")
	}
}
