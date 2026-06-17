package redis

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// crockfordULIDAlphabet — алфавит Crockford-base32 как в ULID (без I/L/O/U).
const crockfordULIDAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// randULID генерирует 26-символьную ULID-подобную строку (тот же алфавит и
// длина, что у реальных applyID). Для теста распределения важна лишь форма и
// энтропия, не монотонность времени — поэтому самодостаточно, без внешней
// зависимости (не тянем oklog/ulid в direct-импорт redis-модуля).
func randULID(t *testing.T) string {
	t.Helper()
	var raw [26]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	var b strings.Builder
	b.Grow(26)
	for _, c := range raw {
		b.WriteByte(crockfordULIDAlphabet[int(c)%len(crockfordULIDAlphabet)])
	}
	return b.String()
}

func TestApplyBusChannel(t *testing.T) {
	got := ApplyBusChannel("01JABC")
	want := fmt.Sprintf("events:shard:%d", ApplyBusShardIndex("01JABC"))
	if got != want {
		t.Fatalf("ApplyBusChannel = %q, want %q", got, want)
	}
	// Префикс — events (закрыт TODO-rename apply:→events:), индекс в диапазоне.
	if !strings.HasPrefix(got, "events:shard:") {
		t.Errorf("channel %q must use events:shard: prefix", got)
	}
	if idx := ApplyBusShardIndex("01JABC"); idx >= ApplyBusShardCount {
		t.Errorf("shard index %d out of range [0,%d)", idx, ApplyBusShardCount)
	}
}

// TestApplyBusChannel_DeterministicShard — guard на детерминизм shard-резолва
// и его равномерность по K шардам на выборке ULID. Детерминизм критичен:
// publisher и subscriber на разных Keeper-инстансах обязаны вычислить ОДИН
// shard-канал для одного applyID, иначе cross-keeper-доставка не сойдётся.
// Равномерность — обоснование выбора K=256: при перекосе hot-shard свёл бы
// шардирование на нет.
func TestApplyBusChannel_DeterministicShard(t *testing.T) {
	// (1) Детерминизм: один applyID → один и тот же канал и индекс.
	const sample = "01J0DETERMINISTICSHARD000A"
	idx0 := ApplyBusShardIndex(sample)
	ch0 := ApplyBusChannel(sample)
	for i := 0; i < 1000; i++ {
		if got := ApplyBusShardIndex(sample); got != idx0 {
			t.Fatalf("ApplyBusShardIndex non-deterministic: %d != %d", got, idx0)
		}
		if got := ApplyBusChannel(sample); got != ch0 {
			t.Fatalf("ApplyBusChannel non-deterministic: %q != %q", got, ch0)
		}
	}

	// (2) Все индексы в диапазоне [0, K) и канал согласован с индексом.
	const n = 50000
	hits := make([]int, ApplyBusShardCount)
	for i := 0; i < n; i++ {
		id := randULID(t)
		idx := ApplyBusShardIndex(id)
		if idx >= ApplyBusShardCount {
			t.Fatalf("shard index %d out of range for %q", idx, id)
		}
		if got, want := ApplyBusChannel(id), fmt.Sprintf("events:shard:%d", idx); got != want {
			t.Fatalf("channel %q inconsistent with index %d (want %q)", got, idx, want)
		}
		hits[idx]++
	}

	// (3) Равномерность: ни один shard не пустой и не «горячий». Идеал —
	// n/K попаданий на shard; допускаем ±60% коридор (запас на дисперсию
	// случайной выборки, без статистического χ², чтобы не флакать).
	ideal := float64(n) / float64(ApplyBusShardCount)
	lo, hi := ideal*0.4, ideal*1.6
	for i, h := range hits {
		if h == 0 {
			t.Errorf("shard %d got zero hits over %d samples — distribution gap", i, n)
			continue
		}
		if float64(h) < lo || float64(h) > hi {
			t.Errorf("shard %d hits = %d, outside uniform corridor [%.0f, %.0f] (ideal %.0f)",
				i, h, lo, hi, ideal)
		}
	}
}

func TestPublishApplyEvent_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	payload := json.RawMessage(`{"k":"v"}`)
	cases := []struct {
		name      string
		client    *Client
		applyID   string
		originKID string
		kind      string
	}{
		{"nil client", nil, "id", "kid", "task.executed"},
		{"empty applyID", c, "", "kid", "task.executed"},
		{"empty originKID", c, "id", "", "task.executed"},
		{"empty kind", c, "id", "kid", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PublishApplyEvent(ctx, tc.client, tc.applyID, tc.originKID, tc.kind, time.Time{}, payload); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestSubscribeApplyEvent_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		client  *Client
		applyID string
		selfKID string
		hasLog  bool
	}{
		{"nil client", nil, "id", "kid", true},
		{"empty applyID", c, "", "kid", true},
		{"empty selfKID", c, "id", "", true},
		{"nil logger", c, "id", "kid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lg = discardLog()
			if !tc.hasLog {
				lg = nil
			}
			if _, err := SubscribeApplyEvent(ctx, tc.client, tc.applyID, tc.selfKID, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestApplyBus_RoundTrip — publish/subscribe полный цикл. Подписчик
// должен получить ApplyEvent с восстановленным kind/applyID/payload.
func TestApplyBus_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "01JABC", "keeper-receiver", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	payload := json.RawMessage(`{"sid":"host.example","task_idx":1}`)
	n, err := PublishApplyEvent(ctx, c, "01JABC", "keeper-sender", "task.executed", time.Time{}, payload)
	if err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	if n != 1 {
		t.Errorf("subscribers count = %d, want 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		if got.Kind != "task.executed" {
			t.Errorf("kind = %q, want task.executed", got.Kind)
		}
		if got.ApplyID != "01JABC" {
			t.Errorf("apply_id = %q, want 01JABC", got.ApplyID)
		}
		if got.OriginKID != "keeper-sender" {
			t.Errorf("origin_kid = %q, want keeper-sender", got.OriginKID)
		}
		if got.At.IsZero() {
			t.Error("At is zero — publish must stamp default")
		}
		var dec map[string]any
		if err := json.Unmarshal(got.Payload, &dec); err != nil {
			t.Fatalf("payload unmarshal: %v", err)
		}
		if dec["sid"] != "host.example" {
			t.Errorf("payload.sid = %v, want host.example", dec["sid"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message within 2s")
	}
}

// TestApplyBus_SelfFilter — publish с тем же origin_kid, что и selfKID
// подписчика → сообщение игнорируется.
func TestApplyBus_SelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	payload := json.RawMessage(`{"x":1}`)
	// Self-origin сначала.
	if _, err := PublishApplyEvent(ctx, c, "id", "keeper-self", "task.executed", time.Time{}, payload); err != nil {
		t.Fatalf("PublishApplyEvent self: %v", err)
	}
	// Потом other-origin — оно должно прийти.
	if _, err := PublishApplyEvent(ctx, c, "id", "keeper-other", "apply.completed", time.Time{}, payload); err != nil {
		t.Fatalf("PublishApplyEvent other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.Kind != "apply.completed" {
			t.Errorf("kind = %q, want apply.completed (other-origin)", got.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive other-origin message within 2s")
	}

	// Self не должен прийти дополнительно.
	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: kind = %q", got.Kind)
		}
	case <-time.After(150 * time.Millisecond):
		// OK — self отфильтрован.
	}
}

// TestApplyBus_NoSubscribers — PublishApplyEvent без подписчиков
// возвращает 0 без ошибки.
func TestApplyBus_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	n, err := PublishApplyEvent(ctx, c, "id", "kid", "task.executed", time.Time{}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestApplyBus_CloseShutsDownGoroutine — Close корректно завершает
// goroutine и закрывает out-канал.
func TestApplyBus_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if err := sub.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	select {
	case _, ok := <-sub.Channel():
		if ok {
			t.Error("Channel returned value after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Channel not closed within 2s after Close")
	}

	if err := sub.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestApplyBus_CloseSurvivesConcurrentReceive — гонка между внешним
// Close и потоком данных. -race должен пройти.
func TestApplyBus_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishApplyEvent(ctx, c, "id", "other", "task.executed", time.Time{}, json.RawMessage(`{}`))
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}

	for range sub.Channel() {
	}
}

// TestPublishApplyEvent_StampsAtIfZero — at.IsZero подменяется на
// time.Now().UTC().
func TestPublishApplyEvent_StampsAtIfZero(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	before := time.Now().UTC()
	if _, err := PublishApplyEvent(ctx, c, "id", "other", "task.executed", time.Time{}, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	select {
	case got := <-sub.Channel():
		if got.At.Before(before.Add(-time.Second)) {
			t.Errorf("stamped At=%v is before Publish call (=%v)", got.At, before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// TestApplyBus_ForwardBufferOverflowDropsOldest — guard на shard-side
// forward-буфер (`s.out`, размер applyEventSubBufferSize). Подписчик НЕ
// дренирует Channel(); в shard-канал заливается >applyEventSubBufferSize
// cross-origin сообщений с монотонным seq в payload. Инварианты:
//
//	(а) publisher (PublishApplyEvent) НЕ блокируется на полном буфере;
//	(б) forward-loop не паникует;
//	(в) лишние события дропаются (в дренаже не больше buffer событий);
//	(г) drop-OLDEST: сохраняется NEWEST — последний seq доезжает,
//	    первые отброшены (симметрично applybus TestBufferOverflowDropsOldest).
//
// origin отличен от selfKID, иначе self-filter отбросил бы всё ещё в
// forward-loop-е (см. doc-comment про echo собственных публикаций).
func TestApplyBus_ForwardBufferOverflowDropsOldest(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "01JOVERFLOW", "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Заливаем buffer + запас, НЕ читая Channel(). seq монотонно растёт —
	// после drop-oldest должен сохраниться хвост (наибольшие seq).
	total := applyEventSubBufferSize + 32
	for i := 0; i < total; i++ {
		payload := json.RawMessage(fmt.Sprintf(`{"seq":%d}`, i))
		// (а) publisher не должен блокироваться, даже когда forward-буфер полон.
		if _, err := PublishApplyEvent(ctx, c, "01JOVERFLOW", "keeper-other", "task.executed", time.Time{}, payload); err != nil {
			t.Fatalf("PublishApplyEvent seq=%d: %v (publisher blocked or errored on full buffer)", i, err)
		}
	}

	// Даём forward-loop-у разгрести Redis-очередь в s.out (с дропами).
	// Поллим до стабилизации: длина канала перестаёт расти и достигла предела.
	waitForStable(t, 3*time.Second, func() int { return len(sub.Channel()) })

	// (в)+(г) Дренируем без блокировки. Не больше buffer событий; последний
	// seq присутствует (newest сохранён), а начальные seq отброшены.
	var got []int
drain:
	for {
		select {
		case ev, ok := <-sub.Channel():
			if !ok {
				break drain
			}
			var dec struct {
				Seq int `json:"seq"`
			}
			if err := json.Unmarshal(ev.Payload, &dec); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			got = append(got, dec.Seq)
		case <-time.After(150 * time.Millisecond):
			break drain
		}
	}

	if len(got) == 0 {
		t.Fatal("no events forwarded — overflow path dropped everything")
	}
	if len(got) > applyEventSubBufferSize {
		t.Fatalf("forwarded %d events, want <= %d (buffer not bounded)", len(got), applyEventSubBufferSize)
	}
	// (г) newest сохранён: последний опубликованный seq доехал.
	last := got[len(got)-1]
	if last != total-1 {
		t.Errorf("newest forwarded seq = %d, want %d (drop-oldest semantics: freshest must survive)", last, total-1)
	}
	// (г) старейшие отброшены: seq=0 не должен пережить overflow при buffer<total.
	for _, s := range got {
		if s == 0 {
			t.Errorf("oldest seq=0 survived overflow — drop-newest leaked instead of drop-oldest")
			break
		}
	}
	// (б) Подписка ещё жива (forward-loop не упал паникой): новый publish доходит.
	if _, err := PublishApplyEvent(ctx, c, "01JOVERFLOW", "keeper-other", "apply.completed", time.Time{}, json.RawMessage(`{"seq":-1}`)); err != nil {
		t.Fatalf("post-overflow PublishApplyEvent: %v", err)
	}
	select {
	case ev, ok := <-sub.Channel():
		if !ok {
			t.Fatal("Channel closed after overflow — forward-loop died")
		}
		if ev.Kind != "apply.completed" {
			t.Errorf("post-overflow event kind = %q, want apply.completed", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forward-loop stopped delivering after overflow (likely panicked)")
	}
}

// waitForStable поллит size() пока значение не перестанет расти между двумя
// замерами (или таймаут). Для async Redis-pubsub: ждём, пока forward-loop
// дойдёт до своего bounded-предела на полном буфере.
func waitForStable(t *testing.T, timeout time.Duration, size func() int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	prev := -1
	stable := 0
	for time.Now().Before(deadline) {
		cur := size()
		if cur == prev {
			stable++
			if stable >= 3 {
				return
			}
		} else {
			stable = 0
		}
		prev = cur
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPublishApplyEvent_PreservesNonZeroAt — переданный at сохраняется.
func TestPublishApplyEvent_PreservesNonZeroAt(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	sub, err := SubscribeApplyEvent(ctx, c, "id", "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	fixed := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := PublishApplyEvent(ctx, c, "id", "other", "apply.completed", fixed, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	select {
	case got := <-sub.Channel():
		if !got.At.Equal(fixed) {
			t.Errorf("At = %v, want preserved %v", got.At, fixed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}
