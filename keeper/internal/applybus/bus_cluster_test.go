package applybus

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// newClusterRedis поднимает miniredis-инстанс и оборачивает в
// [keeperredis.Client]. Тот же механизм, что в grpc/outbound_cluster_test.go
// (miniredis вместо testcontainers — без Docker, детерминированно, гоняется
// под -race). Cleanup через t.Cleanup.
func newClusterRedis(t *testing.T) (*keeperredis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()})
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

func clusterTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// redisSubCount — число Redis-side subscriber-ов на shard-канал, в который
// отображается applyID (по данным miniredis PUBSUB NUMSUB). 0, если канала
// нет. После шардирования это «подписан ли bridge на shard данного applyID»;
// несколько applyID одного shard-а делят один счётчик.
func redisSubCount(mr *miniredis.Miniredis, applyID string) int {
	ch := keeperredis.ApplyBusChannel(applyID)
	return mr.PubSubNumSub(ch)[ch]
}

// collidingApplyIDs возвращает два различных applyID, отображённых в ОДИН
// shard-канал (одинаковый ApplyBusShardIndex). Используется для проверки, что
// forward-loop не путает payload коллидирующих applyID. Перебирает ULID-
// подобные строки до первой коллизии; при K=256 находится за десятки итераций.
func collidingApplyIDs(t *testing.T) (string, string) {
	t.Helper()
	seen := make(map[uint32]string)
	for i := 0; i < 100000; i++ {
		id := "01J0COLLIDE" + string(rune('A'+i%26)) + string(rune('a'+(i/26)%26)) +
			string(rune('0'+(i/676)%10)) + string(rune('0'+(i/6760)%10)) + "00000000000A"
		// Форма applyID не критична для shardIndex.
		shard := keeperredis.ApplyBusShardIndex(id)
		if prev, ok := seen[shard]; ok && prev != id {
			return prev, id
		}
		seen[shard] = id
	}
	t.Fatal("no shard collision found within budget — fnv distribution degenerate?")
	return "", ""
}

// collidingApplyIDsN возвращает n различных applyID, ВСЕ отображённых в один и
// тот же shard-канал. Перебирает ULID-подобные строки, копит по первому
// встреченному shard-у и возвращает первый shard, набравший n коллизий.
func collidingApplyIDsN(t *testing.T, n int) []string {
	t.Helper()
	buckets := make(map[uint32][]string)
	for i := 0; i < 5_000_000; i++ {
		id := "01J0FANOUT" + string(rune('A'+i%26)) + string(rune('a'+(i/26)%26)) +
			string(rune('0'+(i/676)%10)) + string(rune('0'+(i/6760)%10)) +
			string(rune('A'+(i/67600)%26)) + "0000000A"
		shard := keeperredis.ApplyBusShardIndex(id)
		b := buckets[shard]
		// Дубликаты по сгенерированной строке исключаем (разные i могут дать
		// одинаковый id при wrap-around счётчиков — здесь не происходит, но
		// держим инвариант «все различны»).
		dup := false
		for _, x := range b {
			if x == id {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		b = append(b, id)
		buckets[shard] = b
		if len(b) >= n {
			return b
		}
	}
	t.Fatalf("could not collect %d colliding applyIDs within budget", n)
	return nil
}

// waitFor — поллинг до true или fatal по таймауту.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

// recvWithin читает событие из канала или fatal по таймауту.
func recvWithin(t *testing.T, ch <-chan Event, timeout time.Duration) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("no event within timeout")
		return Event{}
	}
}

// TestCluster_NewBusWithRedis_EnablesClusterMode — non-nil redis + kid
// включают cluster-mode.
func TestCluster_NewBusWithRedis_EnablesClusterMode(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	if !b.clusterEnabled() {
		t.Fatal("clusterEnabled = false, want true (redis+kid set)")
	}
	if b.bridges == nil {
		t.Error("bridges map nil — cluster-mode не инициализировал bridges")
	}
}

// TestCluster_NilLoggerFallsBackToDefault — nil logger подменяется на
// slog.Default() (defensive-ветка конструктора).
func TestCluster_NilLoggerFallsBackToDefault(t *testing.T) {
	b := NewBusWithRedis(nil, nil, "")
	if b.logger == nil {
		t.Fatal("logger nil after NewBusWithRedis(nil, ...) — fallback не сработал")
	}
	// Smoke: шина рабочая.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx, "apply-nil-logger")
	b.Publish(Event{ApplyID: "apply-nil-logger", Kind: KindTaskExecuted})
	recvWithin(t, ch, time.Second)
}

// TestCluster_UnsubscribeUnknownIsNoop — unsubscribe subscriber-а, которого
// нет в map-е (или уже удалён), — no-op (idx<0-ветка). White-box: дёргаем
// unsubscribe напрямую с чужим subscriber-ом. Паники/двойного close нет.
func TestCluster_UnsubscribeUnknownIsNoop(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const applyID = "01J0UNKNOWNSUB00000000000A"

	// Живой subscriber, чтобы applyID присутствовал в map-е.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(applyID) == 1 },
		"subscriber not registered")

	// Чужой subscriber, которого в map-е нет → idx<0 → ранний возврат.
	ghost := &subscriber{ch: make(chan Event, 1)}
	b.unsubscribe(applyID, ghost) // не должно паниковать, ghost.ch не закрыт
	select {
	case <-ghost.ch:
		t.Error("ghost.ch unexpectedly readable — unsubscribe тронул чужой канал")
	default:
	}
	// Живой subscriber на месте.
	if got := b.Subscribers(applyID); got != 1 {
		t.Errorf("Subscribers = %d after no-op unsubscribe, want 1", got)
	}

	// Полностью неизвестный applyID — тоже no-op.
	b.unsubscribe("never-subscribed", &subscriber{ch: make(chan Event, 1)})
}

// TestCluster_FirstSubscribeCreatesBridge — первый Subscribe(applyID) в
// cluster-mode поднимает Redis-bridge и реально подписывается на канал
// `apply:<id>` (проверяем через miniredis-side subscriber count).
func TestCluster_FirstSubscribeCreatesBridge(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0BRIDGE000000000000000A"
	_ = b.Subscribe(ctx, applyID)

	// Subscribe в cluster-mode дожидается Ready синхронно — к моменту
	// возврата Redis-подписка зарегистрирована.
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) >= 1
	}, "miniredis did not register subscriber on apply-channel")
}

// TestCluster_SecondSubscribeRefcountsNoSecondRedisSub — второй
// Subscribe того же applyID не создаёт второй Redis-subscribe (ref-count++),
// что видно по неизменному числу subscriber-ов канала в miniredis.
func TestCluster_SecondSubscribeRefcountsNoSecondRedisSub(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0REFCOUNT00000000000000"
	_ = b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "first subscribe did not register exactly 1 redis-subscriber")

	// Второй local-subscribe того же applyID.
	_ = b.Subscribe(ctx, applyID)
	if got := b.Subscribers(applyID); got != 2 {
		t.Fatalf("local Subscribers = %d, want 2", got)
	}

	// Redis-side: всё ещё ровно один subscriber на канал (refs++, не Subscribe).
	// Даём событийному циклу немного времени на случай ошибочного второго sub.
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, applyID); n != 1 {
		t.Errorf("redis subscribers = %d, want 1 (ref-count, not second Subscribe)", n)
	}
}

// TestCluster_DeliverFromCluster_DeliversToLocalSubscribers — событие,
// опубликованное другим Keeper-ом (другой origin_kid), доходит до
// local-subscriber-а через bridge.
func TestCluster_DeliverFromCluster_DeliversToLocalSubscribers(t *testing.T) {
	c, _ := newClusterRedis(t)
	// Два bus-а на одном Redis, разные KID — busB публикует, busA получает.
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0DELIVER0000000000000XY"
	ch := busA.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busA.Subscribers(applyID) == 1 },
		"busA subscriber not registered")

	// busB публикует — у busB нет local-subscriber-ов, событие идёт в Redis,
	// bridge busA форвардит его в ch.
	busB.Publish(Event{
		ApplyID: applyID,
		Kind:    KindTaskExecuted,
		Payload: map[string]any{"sid": "host.example", "task_idx": 0},
	})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed", ev.Kind)
	}
	if ev.ApplyID != applyID {
		t.Errorf("ApplyID = %q, want %q", ev.ApplyID, applyID)
	}
	// Payload приходит как json.RawMessage (bridge не знает typed-структуру).
	raw, ok := ev.Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("Payload type = %T, want json.RawMessage", ev.Payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if decoded["sid"] != "host.example" {
		t.Errorf("payload sid = %v, want host.example", decoded["sid"])
	}
}

// TestCluster_SelfOriginNotDoubleDelivered — self-publish доставляется
// local-субскрайберу ровно один раз: local-доставка + Redis-echo с тем же
// origin_kid отфильтрован в SubscribeApplyEvent. Дубля быть не должно.
func TestCluster_SelfOriginNotDoubleDelivered(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0SELF000000000000000XYZ"
	ch := b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(applyID) == 1 },
		"subscriber not registered")

	b.Publish(Event{ApplyID: applyID, Kind: KindApplyCompleted, Payload: map[string]any{"x": 1}})

	// Первая (local) доставка.
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindApplyCompleted {
		t.Errorf("Kind = %q, want apply.completed", ev.Kind)
	}

	// Echo из Redis с тем же origin_kid отфильтрован — второго события нет.
	select {
	case dup := <-ch:
		t.Errorf("unexpected duplicate from self-echo: %+v", dup)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestCluster_DeliverFromCluster_NoLocalSubscribers — событие из Redis для
// applyID без local-subscriber-ов не паникует и не блокирует (early-return
// в deliverFromCluster при len(subs)==0). Достигается через ref-count: два
// local-subscribe держат bridge живым, оба отписываются, но bridge ещё
// форвардит до своего cancel — событие приходит «в пустоту».
func TestCluster_DeliverFromCluster_NoLocalSubscribers(t *testing.T) {
	c, _ := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	const applyID = "01J0EMPTY000000000000000AB"

	// busA: поднимаем bridge через subscribe, затем отписываемся — но bridge
	// закроется асинхронно; чтобы детерминированно покрыть «нет subscriber-ов»,
	// используем holderCtx (живёт), и параллельно отдельный subscriber,
	// которого отменяем, после чего публикуем.
	holderCtx, holderCancel := context.WithCancel(context.Background())
	defer holderCancel()
	_ = busA.Subscribe(holderCtx, applyID) // держит bridge

	subCtx, subCancel := context.WithCancel(context.Background())
	chTmp := busA.Subscribe(subCtx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busA.Subscribers(applyID) == 2 },
		"two subscribers not registered")

	// Отменяем второй subscriber и дренируем его канал.
	subCancel()
	waitFor(t, 2*time.Second, func() bool { return busA.Subscribers(applyID) == 1 },
		"second subscriber not unsubscribed")
	go func() {
		for range chTmp {
		}
	}()

	// busB публикует — bridge busA жив (holder), доставка идёт оставшемуся.
	// Это покрывает обычный deliverFromCluster с непустым snapshot.
	busB.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})

	// Теперь снимаем holder и сразу публикуем — гонка bridge-teardown vs
	// событие; deliverFromCluster обязан безопасно отработать пустой snapshot.
	holderCancel()
	busB.Publish(Event{ApplyID: applyID, Kind: KindApplyFailed})
	busB.Publish(Event{ApplyID: applyID, Kind: KindApplyCancelled})

	// Не должно паниковать; даём время.
	time.Sleep(200 * time.Millisecond)
}

// TestCluster_LastUnsubscribeClosesBridge — отписка последнего subscriber-а
// закрывает bridge: Redis-канал теряет подписчика (cancel + sub.Close), и
// последующий cross-Keeper publish уже не доходит до нового local-subscriber-а
// до повторного создания bridge. Проверяем через miniredis subscriber-count.
func TestCluster_LastUnsubscribeClosesBridge(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const applyID = "01J0LASTSUB0000000000000AB"
	ctx, cancel := context.WithCancel(context.Background())
	_ = b.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "bridge subscriber not registered")

	// Отписываем последнего → bridge.refs=0 → cancel + sub.Close → Redis
	// теряет подписчика на канал.
	cancel()
	waitFor(t, 3*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 0
	}, "bridge не закрылся: Redis-канал всё ещё имеет subscriber-а")

	if got := b.Subscribers(applyID); got != 0 {
		t.Errorf("local Subscribers = %d, want 0", got)
	}
}

// TestCluster_BridgeRecreatedAfterClose — после закрытия bridge новый
// Subscribe того же applyID поднимает bridge заново (refs снова с 1) и
// cross-Keeper доставка снова работает.
func TestCluster_BridgeRecreatedAfterClose(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	const applyID = "01J0RECREATE000000000000AB"

	ctx1, cancel1 := context.WithCancel(context.Background())
	_ = busA.Subscribe(ctx1, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "first bridge not up")
	cancel1()
	waitFor(t, 3*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 0
	}, "first bridge not closed")

	// Новый subscribe — bridge пересоздаётся.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ch := busA.Subscribe(ctx2, applyID)
	waitFor(t, 2*time.Second, func() bool {
		return redisSubCount(mr, applyID) == 1
	}, "recreated bridge not up")

	busB.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed after bridge recreation", ev.Kind)
	}
}

// TestCluster_BridgeReadyFails_LocalDeliveryStillWorks — если Redis-клиент
// закрыт до Subscribe, sub.Ready падает (Warn), но local-доставка продолжает
// работать, Subscribe не зависает и не паникует. Покрывает Ready-failure-ветку
// в forward-горутине ensureClusterBridgeLocked.
func TestCluster_BridgeReadyFails_LocalDeliveryStillWorks(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	// Закрываем Redis-клиент: последующий Subscribe-loop не сможет дойти до
	// Ready (initial Receive упадёт), но Subscribe всё равно вернётся (по
	// Ready-сигналу из forward-горутины, который закрывается даже при ошибке).
	_ = c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0READYFAIL00000000000AB"
	done := make(chan (<-chan Event), 1)
	go func() { done <- b.Subscribe(ctx, applyID) }()

	var ch <-chan Event
	select {
	case ch = <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("Subscribe hung past Ready-timeout when redis closed")
	}

	// Local-доставка работает несмотря на мёртвый bridge.
	b.Publish(Event{ApplyID: applyID, Kind: KindApplyStarted})
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindApplyStarted {
		t.Errorf("Kind = %q, want apply.started (local delivery)", ev.Kind)
	}
}

// TestCluster_PublishToCluster_Success — Publish в cluster-mode реально
// доставляет в Redis: второй bus на том же Redis получает событие
// (проверяет успешную ветку publishToCluster end-to-end).
func TestCluster_PublishToCluster_Success(t *testing.T) {
	c, _ := newClusterRedis(t)
	busPub := NewBusWithRedis(clusterTestLogger(), c, "keeper-pub")
	busSub := NewBusWithRedis(clusterTestLogger(), c, "keeper-sub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0PUBOK00000000000000XYZ"
	ch := busSub.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busSub.Subscribers(applyID) == 1 },
		"subscriber not registered")

	busPub.Publish(Event{ApplyID: applyID, Kind: KindApplyCompleted, Payload: map[string]any{"ok": true}})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindApplyCompleted {
		t.Errorf("Kind = %q, want apply.completed", ev.Kind)
	}
}

// TestCluster_PublishToCluster_RedisError_Swallowed — при недоступном Redis
// (клиент закрыт) Publish не паникует, не блокируется и всё равно доставляет
// local-subscriber-у. Ошибка PUBLISH глотается (warn).
func TestCluster_PublishToCluster_RedisError_Swallowed(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	// local-subscriber до закрытия Redis.
	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	ch := b.Subscribe(subCtx, "01J0PUBERR000000000000XYZ")
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers("01J0PUBERR000000000000XYZ") == 1 },
		"subscriber not registered")

	// Рвём Redis — последующий publishToCluster получит ошибку PUBLISH.
	_ = c.Close()

	done := make(chan struct{})
	go func() {
		b.Publish(Event{ApplyID: "01J0PUBERR000000000000XYZ", Kind: KindTaskExecuted})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked on closed Redis longer than clusterPublishTimeout")
	}

	// local-доставка произошла несмотря на ошибку Redis-publish.
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed (local still delivered)", ev.Kind)
	}
}

// TestCluster_PublishToCluster_PayloadMarshalError_Swallowed — payload,
// который не сериализуется в JSON (chan), не валит Publish: marshal-ошибка
// логируется и глотается, local-доставка проходит.
func TestCluster_PublishToCluster_PayloadMarshalError_Swallowed(t *testing.T) {
	c, _ := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	const applyID = "01J0MARSHALERR0000000000AB"
	ch := b.Subscribe(subCtx, applyID)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(applyID) == 1 },
		"subscriber not registered")

	// chan не сериализуется json.Marshal-ом → ветка marshal-error.
	b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted, Payload: make(chan int)})

	// local-доставка прошла (snapshot был до publishToCluster).
	ev := recvWithin(t, ch, 2*time.Second)
	if ev.Kind != KindTaskExecuted {
		t.Errorf("Kind = %q, want task.executed", ev.Kind)
	}
}

// TestCluster_PublishNilPayload_NoMarshal — nil-payload не вызывает marshal
// (ветка ev.Payload == nil в publishToCluster) и публикуется корректно.
func TestCluster_PublishNilPayload_NoMarshal(t *testing.T) {
	c, _ := newClusterRedis(t)
	busPub := NewBusWithRedis(clusterTestLogger(), c, "keeper-pub")
	busSub := NewBusWithRedis(clusterTestLogger(), c, "keeper-sub")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0NILPAYLOAD000000000AB"
	ch := busSub.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return busSub.Subscribers(applyID) == 1 },
		"subscriber not registered")

	busPub.Publish(Event{ApplyID: applyID, Kind: KindApplyStarted})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindApplyStarted {
		t.Errorf("Kind = %q, want apply.started", ev.Kind)
	}
}

// TestCluster_NoForwardLoopGoroutineLeak — после полного teardown bridge-а
// (last-unsubscribe) forward-loop-горутина завершается, утечки нет.
// Проверяем через стабилизацию runtime.NumGoroutine.
func TestCluster_NoForwardLoopGoroutineLeak(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	const rounds = 20
	for i := 0; i < rounds; i++ {
		applyID := "01J0LEAK00000000000000" + string(rune('A'+i%26)) + string(rune('a'+i%26))
		ctx, cancel := context.WithCancel(context.Background())
		_ = b.Subscribe(ctx, applyID)
		waitFor(t, 2*time.Second, func() bool {
			return redisSubCount(mr, applyID) == 1
		}, "bridge not up in leak-round")
		cancel()
		waitFor(t, 3*time.Second, func() bool {
			return redisSubCount(mr, applyID) == 0
		}, "bridge not closed in leak-round")
	}

	// Даём завершиться форвард-горутинам.
	waitFor(t, 5*time.Second, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= before+3
	}, "goroutine count did not settle — forward-loop leak suspected")
}

// TestCluster_ConcurrentSubscribeUnsubscribe — параллельные subscribe/
// unsubscribe того же applyID не ломают ref-count-инг bridge-а и не паникуют
// под -race. Race Publish-vs-unsubscribe в local-доставке починен вариантом A
// (см. TestConcurrentPublishVsUnsubscribe), поэтому здесь добавлен и
// параллельный Publish — заодно проверяем, что доставка под churn не паникует.
func TestCluster_ConcurrentSubscribeUnsubscribe(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const (
		applyID = "01J0CONC0000000000000000AB"
		workers = 16
		rounds  = 25
	)

	pubCtx, pubCancel := context.WithCancel(context.Background())
	var wgP sync.WaitGroup
	wgP.Add(1)
	go func() {
		defer wgP.Done()
		for {
			select {
			case <-pubCtx.Done():
				return
			default:
				b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				ch := b.Subscribe(ctx, applyID)
				// ch не закрывается (вариант A) — reader выходит по ctx.Done().
				go func() {
					for {
						select {
						case <-ch:
						case <-ctx.Done():
							return
						}
					}
				}()
				cancel()
			}
		}()
	}
	wg.Wait()
	pubCancel()
	wgP.Wait()

	// После полного teardown — ноль local-subscriber-ов и bridge схлопнут
	// (refs=0 → Redis-канал без subscriber-ов). Это и есть инвариант
	// ref-count-инга: никаких залипших bridge при равном числе sub/unsub.
	waitFor(t, 5*time.Second, func() bool { return b.Subscribers(applyID) == 0 },
		"subscribers not drained to 0 after concurrent churn")
	waitFor(t, 5*time.Second, func() bool { return redisSubCount(mr, applyID) == 0 },
		"bridge не схлопнулся после concurrent churn — утечка bridge/refs")
}

// TestSubscribe_BackCompat_DefaultBridges — Subscribe(ctx,id) поведенчески
// эквивалентен SubscribeWithBridge(ctx,id,true): в cluster-mode оба поднимают
// per-applyID Redis-bridge (redisSubCount==1), и оба получают local-доставку.
// Guard на back-compat: existing caller-ы Subscribe не меняют поведение
// после введения SubscribeWithBridge (S1).
func TestSubscribe_BackCompat_DefaultBridges(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	// legacy Subscribe → bridge поднят.
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	const idA = "01J0BACKCOMPATAAAAAAAAAAAAA"
	chA := b.Subscribe(ctxA, idA)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idA) == 1 },
		"Subscribe did not raise bridge")

	// SubscribeWithBridge(...,true) → тот же эффект на другом applyID.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	const idB = "01J0BACKCOMPATBBBBBBBBBBBBB"
	chB := b.SubscribeWithBridge(ctxB, idB, true)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idB) == 1 },
		"SubscribeWithBridge(true) did not raise bridge")

	// Local-доставка работает у обоих.
	b.Publish(Event{ApplyID: idA, Kind: KindTaskExecuted})
	if ev := recvWithin(t, chA, 2*time.Second); ev.Kind != KindTaskExecuted {
		t.Errorf("Subscribe local delivery Kind = %q, want task.executed", ev.Kind)
	}
	b.Publish(Event{ApplyID: idB, Kind: KindTaskExecuted})
	if ev := recvWithin(t, chB, 2*time.Second); ev.Kind != KindTaskExecuted {
		t.Errorf("SubscribeWithBridge(true) local delivery Kind = %q, want task.executed", ev.Kind)
	}
}

// TestSubscribeWithBridge_False_NoRedisSubscribe — wantBridge=false в
// cluster-mode не поднимает per-applyID Redis-Subscribe (redisSubCount==0),
// но local-доставка через Publish того же инстанса работает. Прямой
// unit-guard на bridge-skip (S1), независимый от dispatcher-а.
func TestSubscribeWithBridge_False_NoRedisSubscribe(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const id = "01J0NOBRIDGE000000000000AB"
	ch := b.SubscribeWithBridge(ctx, id, false)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(id) == 1 },
		"local subscriber not registered")

	// Redis-Subscribe не поднят — bridge пропущен.
	for i := 0; i < 10; i++ {
		if n := redisSubCount(mr, id); n != 0 {
			t.Fatalf("redis subscribers = %d, want 0 (wantBridge=false)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// local-доставка работает.
	b.Publish(Event{ApplyID: id, Kind: KindApplyCompleted})
	if ev := recvWithin(t, ch, 2*time.Second); ev.Kind != KindApplyCompleted {
		t.Errorf("Kind = %q, want apply.completed (local delivery)", ev.Kind)
	}
}

// TestSubscribeWithBridge_MixedRefcount_NoPrematureClose — local-only
// subscriber (wantBridge=false) НЕ декрементит refs bridge-а, поднятого
// соседним subscriber-ом того же applyID. Guard на heldBridge-инвариант:
// отписка local-only sub не должна ронять чужой bridge.
func TestSubscribeWithBridge_MixedRefcount_NoPrematureClose(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const id = "01J0MIXREFCOUNT00000000AB"

	// subBridge держит bridge (wantBridge=true).
	ctxBridge, cancelBridge := context.WithCancel(context.Background())
	defer cancelBridge()
	_ = b.SubscribeWithBridge(ctxBridge, id, true)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, id) == 1 },
		"bridge not raised by wantBridge=true subscriber")

	// subLocal — local-only, refs не трогает.
	ctxLocal, cancelLocal := context.WithCancel(context.Background())
	_ = b.SubscribeWithBridge(ctxLocal, id, false)
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(id) == 2 },
		"two subscribers not registered")

	// Отписываем local-only — bridge ОБЯЗАН остаться (refs==1 от subBridge).
	cancelLocal()
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(id) == 1 },
		"local-only subscriber not unsubscribed")
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, id); n != 1 {
		t.Fatalf("redis subscribers = %d after local-only unsubscribe, want 1 (bridge must survive)", n)
	}

	// Теперь отписываем держателя bridge — bridge схлопывается.
	cancelBridge()
	waitFor(t, 3*time.Second, func() bool { return redisSubCount(mr, id) == 0 },
		"bridge did not close after last bridge-holder unsubscribed")
}

// TestCluster_ConcurrentMixedBridgeChurn_HeldBridgeInvariant — параллельный
// subscribe/unsubscribe того же applyID СМЕСЬЮ wantBridge (часть local-only
// false, часть bridge true) под churn. Guard на heldBridge-инвариант
// (S1, holder==self skip):
//
//   - local-only subscriber (wantBridge=false) НЕ декрементит refs bridge-а →
//     не роняет чужой bridge (no preliminary-close);
//   - симметричный refs++/refs-- только у heldBridge-subscriber-ов → нет
//     refs-leak (по равному числу bridge-sub/unsub bridge схлопывается в 0).
//
// «Якорный» bridge-holder держится всё время churn-а: проверяем, что под
// шквалом смешанных sub/unsub он НЕ закрывается преждевременно (redisSubCount
// остаётся ≥1). После снятия якоря и полного teardown — refs=0, bridge закрыт.
// Под -race.
func TestCluster_ConcurrentMixedBridgeChurn_HeldBridgeInvariant(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	const (
		applyID = "01J0MIXCHURN00000000000AB"
		workers = 16
		rounds  = 25
	)

	// Якорь: bridge-holder, живёт всю длительность churn-а. Гарантирует, что
	// redisSubCount не должен падать до 0 в окне churn-а — преждевременный
	// close чужого bridge сразу всплывёт.
	anchorCtx, anchorCancel := context.WithCancel(context.Background())
	_ = b.SubscribeWithBridge(anchorCtx, applyID, true)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, applyID) >= 1 },
		"anchor bridge not raised")

	// Параллельный publisher — заодно проверяем, что доставка под churn не
	// паникует (Publish-vs-unsubscribe).
	pubCtx, pubCancel := context.WithCancel(context.Background())
	var wgP sync.WaitGroup
	wgP.Add(1)
	go func() {
		defer wgP.Done()
		for {
			select {
			case <-pubCtx.Done():
				return
			default:
				b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
			}
		}
	}()

	// Детектор преждевременного close-а: пока якорь жив, redisSubCount обязан
	// оставаться ≥1. Любое падение до 0 = heldBridge-инвариант нарушен
	// (local-only sub уронил чужой bridge).
	monCtx, monCancel := context.WithCancel(context.Background())
	prematureClose := make(chan struct{}, 1)
	var monWG sync.WaitGroup
	monWG.Add(1)
	go func() {
		defer monWG.Done()
		for {
			select {
			case <-monCtx.Done():
				return
			default:
				if redisSubCount(mr, applyID) == 0 {
					select {
					case prematureClose <- struct{}{}:
					default:
					}
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				// Смесь: чётные раунды — local-only (false), нечётные — bridge (true).
				wantBridge := (seed+i)%2 == 0
				ch := b.SubscribeWithBridge(ctx, applyID, wantBridge)
				go func() {
					for {
						select {
						case <-ch:
						case <-ctx.Done():
							return
						}
					}
				}()
				cancel()
			}
		}(w)
	}
	wg.Wait()

	// Останавливаем монитор до снятия якоря.
	monCancel()
	monWG.Wait()
	pubCancel()
	wgP.Wait()

	select {
	case <-prematureClose:
		t.Fatal("bridge закрылся преждевременно во время churn-а при живом anchor — heldBridge-инвариант нарушен (local-only sub уронил чужой bridge)")
	default:
	}

	// Якорь ещё держит bridge.
	if n := redisSubCount(mr, applyID); n != 1 {
		t.Fatalf("redis subscribers = %d after churn (anchor alive), want 1 (no refs-leak, no premature close)", n)
	}
	if got := b.Subscribers(applyID); got != 1 {
		t.Fatalf("local Subscribers = %d after churn, want 1 (anchor only)", got)
	}

	// Снимаем якорь — bridge обязан схлопнуться (refs=0). Доказывает отсутствие
	// refs-leak: лишний инкремент оставил бы refs>0 навсегда.
	anchorCancel()
	waitFor(t, 5*time.Second, func() bool { return redisSubCount(mr, applyID) == 0 },
		"bridge не схлопнулся после снятия anchor — refs-leak от mixed churn")
	waitFor(t, 5*time.Second, func() bool { return b.Subscribers(applyID) == 0 },
		"local subscribers не обнулились после полного teardown")
}

// TestCluster_ShardChannel_CrossKeeperErrand — cross-keeper доставка
// Errand-результата через ШАРДИРОВАННЫЙ канал. A диспатчит для SID, Soul на
// B; B публикует errand.completed (busB, иной KID), A подписан на тот же
// shard → событие доходит. Перенос cross-keeper-кейса на shard-форму
// (errand-семейство EventKind-ов).
func TestCluster_ShardChannel_CrossKeeperErrand(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const errandID = "01J0ERRANDSHARD00000000XYZ"
	ch := busA.Subscribe(ctx, errandID)
	// Bridge поднят именно на shard данного applyID.
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, errandID) >= 1 },
		"busA bridge not registered on shard")

	busB.Publish(Event{
		ApplyID: errandID,
		Kind:    KindErrandCompleted,
		Payload: map[string]any{"errand_id": errandID, "status": "success", "exit_code": 0},
	})

	ev := recvWithin(t, ch, 3*time.Second)
	if ev.Kind != KindErrandCompleted {
		t.Errorf("Kind = %q, want errand.completed", ev.Kind)
	}
	if ev.ApplyID != errandID {
		t.Errorf("ApplyID = %q, want %q", ev.ApplyID, errandID)
	}
	raw, ok := ev.Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("Payload type = %T, want json.RawMessage", ev.Payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if decoded["status"] != "success" {
		t.Errorf("payload status = %v, want success", decoded["status"])
	}
}

// TestCluster_ShardChannel_CrossKeeperSSE — cross-keeper scenario-run
// (apply.*-семейство, как SSE-стрим) через shard доходит: A держит SSE-style
// подписку, B (иной KID) публикует task.executed + apply.completed на тот же
// shard, оба доходят в правильном порядке. Перенос SSE-cross-keeper-кейса на
// shard-форму (in-memory уровень bus, без HTTP — HTTP-вариант в integration
// sse_cluster_test.go).
func TestCluster_ShardChannel_CrossKeeperSSE(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const applyID = "01J0SSESHARD0000000000XYZ1"
	ch := busA.Subscribe(ctx, applyID)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, applyID) >= 1 },
		"SSE-style bridge not registered on shard")

	busB.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted, Payload: map[string]any{"task_idx": 0}})
	busB.Publish(Event{ApplyID: applyID, Kind: KindApplyCompleted, Payload: map[string]any{"run_status": "RUN_STATUS_SUCCESS"}})

	ev1 := recvWithin(t, ch, 3*time.Second)
	if ev1.Kind != KindTaskExecuted {
		t.Errorf("first frame Kind = %q, want task.executed", ev1.Kind)
	}
	ev2 := recvWithin(t, ch, 3*time.Second)
	if ev2.Kind != KindApplyCompleted {
		t.Errorf("second frame Kind = %q, want apply.completed", ev2.Kind)
	}
}

// TestCluster_ShardCollision_NoPayloadMix — ★критичный guard: ДВА applyID,
// коллидящих в ОДИН shard, публикуются одновременно. Каждый local-subscriber
// получает ТОЛЬКО события своего applyID (forward-loop фильтрует по
// envelope.ApplyID, не по shard). Без фильтра payload одного прогона утёк бы
// в SSE-стрим другого — нарушение изоляции.
func TestCluster_ShardCollision_NoPayloadMix(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	idX, idY := collidingApplyIDs(t)
	if keeperredis.ApplyBusShardIndex(idX) != keeperredis.ApplyBusShardIndex(idY) {
		t.Fatalf("test setup broken: %q and %q are not on the same shard", idX, idY)
	}
	if idX == idY {
		t.Fatal("test setup broken: colliding IDs must be distinct")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chX := busA.Subscribe(ctx, idX)
	chY := busA.Subscribe(ctx, idY)
	// Оба applyID на одном shard → одна Redis-подписка (refs=2).
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idX) == 1 },
		"shared shard bridge not registered as single redis-subscriber")
	if got := redisSubCount(mr, idY); got != 1 {
		t.Fatalf("redis subscribers for shared shard = %d, want 1 (idX and idY share one bridge)", got)
	}

	// Публикуем оба одновременно с другого keeper-а (busB).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		busB.Publish(Event{ApplyID: idX, Kind: KindApplyCompleted, Payload: map[string]any{"who": "X"}})
	}()
	go func() {
		defer wg.Done()
		busB.Publish(Event{ApplyID: idY, Kind: KindApplyFailed, Payload: map[string]any{"who": "Y"}})
	}()
	wg.Wait()

	// chX обязан получить ТОЛЬКО X-событие; chY — только Y. Считываем по
	// одному и сверяем payload + applyID.
	assertOwnEvent := func(name string, ch <-chan Event, wantID, wantWho string, wantKind EventKind) {
		ev := recvWithin(t, ch, 3*time.Second)
		if ev.ApplyID != wantID {
			t.Errorf("%s: ApplyID = %q, want %q (cross-applyID leak on shared shard)", name, ev.ApplyID, wantID)
		}
		if ev.Kind != wantKind {
			t.Errorf("%s: Kind = %q, want %q", name, ev.Kind, wantKind)
		}
		raw, ok := ev.Payload.(json.RawMessage)
		if !ok {
			t.Fatalf("%s: Payload type = %T, want json.RawMessage", name, ev.Payload)
		}
		var dec map[string]any
		if err := json.Unmarshal(raw, &dec); err != nil {
			t.Fatalf("%s: payload unmarshal: %v", name, err)
		}
		if dec["who"] != wantWho {
			t.Errorf("%s: payload who = %v, want %q (payload mixed across colliding applyIDs)", name, dec["who"], wantWho)
		}
	}
	assertOwnEvent("chX", chX, idX, "X", KindApplyCompleted)
	assertOwnEvent("chY", chY, idY, "Y", KindApplyFailed)

	// Никаких лишних (чужих) событий ни у одного из каналов.
	select {
	case extra := <-chX:
		t.Errorf("chX got extra event after its own: %+v (cross-applyID leak)", extra)
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case extra := <-chY:
		t.Errorf("chY got extra event after its own: %+v (cross-applyID leak)", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestCluster_ShardBridge_RefcountAcrossApplyIDs — два РАЗНЫХ applyID на одном
// shard делят одну Redis-подписку (refs по shard-у). Отписка первого НЕ
// закрывает bridge (второй applyID ещё держит shard); отписка второго —
// закрывает. Guard на per-shard refcount (S2): bridge живёт пока на shard есть
// хоть один held-subscriber, независимо от applyID.
func TestCluster_ShardBridge_RefcountAcrossApplyIDs(t *testing.T) {
	c, mr := newClusterRedis(t)
	b := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")

	idX, idY := collidingApplyIDs(t)
	if keeperredis.ApplyBusShardIndex(idX) != keeperredis.ApplyBusShardIndex(idY) {
		t.Fatalf("test setup broken: %q and %q not on same shard", idX, idY)
	}

	ctxX, cancelX := context.WithCancel(context.Background())
	_ = b.Subscribe(ctxX, idX)
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, idX) == 1 },
		"first applyID did not raise shard bridge")

	ctxY, cancelY := context.WithCancel(context.Background())
	defer cancelY()
	_ = b.Subscribe(ctxY, idY)
	// Тот же shard → refs++, не вторая Redis-подписка.
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(idY) == 1 },
		"second applyID subscriber not registered")
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, idY); n != 1 {
		t.Fatalf("redis subscribers = %d for shared shard, want 1 (refs++, not second subscribe)", n)
	}

	// Отписываем первый applyID — bridge ОБЯЗАН выжить (idY ещё держит shard).
	cancelX()
	waitFor(t, 2*time.Second, func() bool { return b.Subscribers(idX) == 0 },
		"first applyID not unsubscribed")
	time.Sleep(50 * time.Millisecond)
	if n := redisSubCount(mr, idX); n != 1 {
		t.Fatalf("redis subscribers = %d after first unsubscribe, want 1 (bridge must survive for idY)", n)
	}

	// Отписываем второй — теперь refs shard-а = 0 → bridge закрывается.
	cancelY()
	waitFor(t, 3*time.Second, func() bool { return redisSubCount(mr, idY) == 0 },
		"shard bridge did not close after last applyID unsubscribed")
}

// TestConcurrentPublishVsUnsubscribe — регресс-гард на "send on closed
// channel": Publish снимает snapshot subscriber-ов под RLock и доставляет
// ВНЕ lock-а, параллельно unsubscribe снимает subscriber-а. По варианту A
// (done-канал, s.ch не закрывается) deliver в select-е на done тихо
// прекращает доставку — ни паники, ни гонки записи в закрытый канал.
// Воспроизводится и на local-шине (без Redis): это НЕ cluster-специфика.
//
// Должен зелёным проходить под `go test -race ./internal/applybus/...` без
// спец-флагов. reader выходит по ctx.Done() (ch не закрывается).
func TestConcurrentPublishVsUnsubscribe(t *testing.T) {
	b := NewBus(clusterTestLogger())
	const applyID = "01J0PROBE000000000000000AB"
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				ch := b.Subscribe(ctx, applyID)
				go func() {
					// ch не закрывается — выходим по отмене ctx.
					for {
						select {
						case <-ch:
						case <-ctx.Done():
							return
						}
					}
				}()
				b.Publish(Event{ApplyID: applyID, Kind: KindTaskExecuted})
				cancel()
			}
		}()
	}
	wg.Wait()
}

// TestCluster_ShardFanout_NoDropNoMix — веер: N>10 различных applyID,
// коллидящих в ОДИН shard, публикуются с другого keeper-а при разумной
// скорости (читатели дренируют параллельно). Один forward-loop на shard
// обслуживает все applyID: каждый канал получает РОВНО свои события (без
// drop при разумной скорости) и без перемешивания (порядок per-applyID
// сохранён, payload не утекает между applyID). Расширение
// TestCluster_ShardCollision_NoPayloadMix на N>2 каналов.
func TestCluster_ShardFanout_NoDropNoMix(t *testing.T) {
	c, mr := newClusterRedis(t)
	busA := NewBusWithRedis(clusterTestLogger(), c, "keeper-A")
	busB := NewBusWithRedis(clusterTestLogger(), c, "keeper-B")

	const (
		fanout       = 16 // N>10 applyID на один shard
		perID        = 20 // событий на каждый applyID
		drainTimeout = 5 * time.Second
	)
	ids := collidingApplyIDsN(t, fanout)
	shard := keeperredis.ApplyBusShardIndex(ids[0])
	for _, id := range ids {
		if keeperredis.ApplyBusShardIndex(id) != shard {
			t.Fatalf("setup broken: %q not on shard %d", id, shard)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Подписки на все applyID одного shard → одна общая Redis-подписка (refs=N).
	chans := make(map[string]<-chan Event, fanout)
	for _, id := range ids {
		chans[id] = busA.Subscribe(ctx, id)
	}
	waitFor(t, 2*time.Second, func() bool { return redisSubCount(mr, ids[0]) == 1 },
		"fanout subscriptions did not collapse to single shard bridge")

	// Параллельные читатели: собирают seq по своему applyID. Разумная скорость
	// дренажа — буфер не переполняется, drop не ожидается.
	type collected struct {
		mu  sync.Mutex
		seq []int
	}
	got := make(map[string]*collected, fanout)
	var rwg sync.WaitGroup
	for _, id := range ids {
		got[id] = &collected{}
		ch := chans[id]
		col := got[id]
		wantID := id
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			idle := time.NewTimer(drainTimeout)
			defer idle.Stop()
			for {
				select {
				case ev := <-ch:
					if ev.ApplyID != wantID {
						t.Errorf("cross-applyID leak: got ApplyID=%q on channel for %q", ev.ApplyID, wantID)
					}
					var dec struct {
						Seq int `json:"seq"`
					}
					raw, ok := ev.Payload.(json.RawMessage)
					if !ok {
						t.Errorf("payload type %T, want json.RawMessage", ev.Payload)
						continue
					}
					if err := json.Unmarshal(raw, &dec); err != nil {
						t.Errorf("payload unmarshal: %v", err)
						continue
					}
					col.mu.Lock()
					col.seq = append(col.seq, dec.Seq)
					col.mu.Unlock()
					if !idle.Stop() {
						<-idle.C
					}
					idle.Reset(500 * time.Millisecond)
				case <-idle.C:
					return
				}
			}
		}()
	}

	// Публикуем с busB (другой KID) per-applyID последовательно: один applyID
	// — монотонный seq 0..perID-1; разумная скорость (без burst-а в один tick).
	var pwg sync.WaitGroup
	for _, id := range ids {
		id := id
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			for s := 0; s < perID; s++ {
				busB.Publish(Event{
					ApplyID: id,
					Kind:    KindTaskExecuted,
					Payload: map[string]any{"seq": s},
				})
				time.Sleep(time.Millisecond)
			}
		}()
	}
	pwg.Wait()
	rwg.Wait()

	// Каждый applyID получил ровно свои perID событий, в порядке возрастания
	// seq (forward-loop не перемешал per-applyID-порядок), без drop.
	for _, id := range ids {
		col := got[id]
		col.mu.Lock()
		seq := col.seq
		col.mu.Unlock()
		if len(seq) != perID {
			t.Errorf("applyID %q: got %d events, want %d (drop or loss under fanout)", id, len(seq), perID)
			continue
		}
		for i, s := range seq {
			if s != i {
				t.Errorf("applyID %q: event[%d] seq = %d, want %d (per-applyID order scrambled)", id, i, s, i)
				break
			}
		}
	}
}
