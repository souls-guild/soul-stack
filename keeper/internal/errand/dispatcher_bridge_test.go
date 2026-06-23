package errand

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
)

// Guard-тесты S1 (applybus-bottleneck): holder==self → пропуск per-applyID
// Redis-Subscribe; holder!=self → bridge поднимается; holder-flip после
// skip-решения → завершение по wait-timer-у (timed_out), НЕ зависание.
//
// busBridge — адаптер real *applybus.EventBus к узкой errand.ApplyBus
// (симметрично прод-обёртке errandApplyBusBridge из cmd/keeper/daemon.go).
// Через него dispatcher гоняет НАСТОЯЩУЮ шину поверх miniredis, и
// Redis-Subscribe-вызовы реально измеряются (PUBSUB NUMSUB).
type busBridge struct{ bus *applybus.EventBus }

func (b busBridge) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.bus.Subscribe(ctx, applyID)
}

func (b busBridge) SubscribeWithBridge(ctx context.Context, applyID string, wantBridge bool) <-chan applybus.Event {
	return b.bus.SubscribeWithBridge(ctx, applyID, wantBridge)
}

func bridgeTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newBridgeRedis — miniredis + keeperredis.Client (без Docker, как в
// applybus/bus_cluster_test.go).
func newBridgeRedis(t *testing.T) (*keeperredis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := keeperredis.NewClient(ctx, keeperredis.Config{Addr: mr.Addr()}, nil)
	if err != nil {
		t.Fatalf("redis NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// redisSubCountFor — число Redis-side подписчиков на канал apply:<id> по
// данным miniredis. 0 если канала/подписки нет. Это и есть прямой измеритель
// «поднял ли bus per-applyID Redis-Subscribe».
func redisSubCountFor(mr *miniredis.Miniredis, applyID string) int {
	ch := keeperredis.ApplyBusChannel(applyID)
	return mr.PubSubNumSub(ch)[ch]
}

const bridgeTestKID = "kid-self"

// buildBridgeDispatcher — Dispatcher поверх real applybus-bus и fakeLease.
// KID = bridgeTestKID совпадает с KID шины (self-publish фильтруется как в
// проде). lease управляет тем, какой holder вернёт resolveHolder.
func buildBridgeDispatcher(t *testing.T, bus *applybus.EventBus, lease *fakeLease, ob *fakeOutbound, cap time.Duration) *Dispatcher {
	t.Helper()
	return &Dispatcher{deps: Deps{
		Store:       newFakeStore(),
		Outbound:    ob,
		Publisher:   ob,
		LeaseLookup: lease,
		ApplyBus:    busBridge{bus: bus},
		Logger:      bridgeTestLogger(),
		KID:         bridgeTestKID,
		ServerCap:   cap,
		Clock:       time.Now,
	}}
}

// TestErrand_SingleHolderSelf_NoRedisSubscribe — holder == self-KID ⇒
// dispatcher просит wantBridge=false ⇒ per-applyID Redis-Subscribe НЕ
// поднимается (redisSubCount==0 в течение всей жизни подписки), а Errand
// доставляется через local-bus того же инстанса. Прямой guard на устранение
// applybus-bottleneck.
func TestErrand_SingleHolderSelf_NoRedisSubscribe(t *testing.T) {
	c, mr := newBridgeRedis(t)
	bus := applybus.NewBusWithRedis(bridgeTestLogger(), c, bridgeTestKID)

	const sid = "host.local"
	// holder == self → skip-решение.
	lease := &fakeLease{holders: map[string]string{sid: bridgeTestKID}}
	ob := &fakeOutbound{}

	d := buildBridgeDispatcher(t, bus, lease, ob, 2*time.Second)

	// Запускаем Dispatch в горутине; пока он ждёт event, измеряем
	// Redis-Subscribe и доставляем результат локально (через тот же bus →
	// local-доставка без bridge).
	type out struct {
		res DispatchResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := d.Dispatch(context.Background(), DispatchRequest{
			SID: sid, Module: "core.cmd.shell", TimeoutSec: 2, StartedByAID: "archon-alice",
		})
		done <- out{res, err}
	}()

	// Ждём, пока local-subscriber появится (подписка зарегистрирована).
	var errandID string
	waitUntil(t, 2*time.Second, func() bool {
		errandID = ob.firstSentErrandID()
		return errandID != "" && bus.Subscribers(errandID) == 1
	}, "errand not subscribed locally")

	// Окно проверки: подписка жива, Redis-Subscribe должен оставаться 0.
	for i := 0; i < 10; i++ {
		if n := redisSubCountFor(mr, errandID); n != 0 {
			t.Fatalf("redis subscribers = %d while holder==self, want 0 (bridge must be skipped)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Доставка результата через local-bus (publish того же инстанса).
	bus.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    applybus.KindErrandCompleted,
		Payload: ResultEvent{ErrandID: errandID, Status: StatusSuccess, ExitCode: ptrInt32(0), Stdout: "ok"},
	})

	o := recvDispatch(t, done, 3*time.Second)
	if o.err != nil {
		t.Fatalf("Dispatch: %v", o.err)
	}
	if o.res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success (local-bus delivery)", o.res.Status)
	}
	if o.res.Async {
		t.Fatalf("Async=true, want sync success")
	}
}

// TestErrand_HolderOther_StillBridges — holder != self ⇒ dispatcher просит
// wantBridge=true ⇒ per-applyID Redis-bridge поднят (redisSubCount>=1), и
// cross-keeper доставка работает: событие, опубликованное «другим» keeper-ом
// (busOther, иной KID), доходит до dispatcher-а через bridge.
func TestErrand_HolderOther_StillBridges(t *testing.T) {
	c, mr := newBridgeRedis(t)
	busSelf := applybus.NewBusWithRedis(bridgeTestLogger(), c, bridgeTestKID)
	busOther := applybus.NewBusWithRedis(bridgeTestLogger(), c, "kid-other")

	const sid = "host.remote"
	lease := &fakeLease{holders: map[string]string{sid: "kid-other"}}
	ob := &fakeOutbound{}

	d := buildBridgeDispatcher(t, busSelf, lease, ob, 3*time.Second)

	type out struct {
		res DispatchResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := d.Dispatch(context.Background(), DispatchRequest{
			SID: sid, Module: "core.cmd.shell", TimeoutSec: 3, StartedByAID: "archon-alice",
		})
		done <- out{res, err}
	}()

	var errandID string
	waitUntil(t, 3*time.Second, func() bool {
		errandID = ob.firstSentErrandID()
		return errandID != "" && busSelf.Subscribers(errandID) == 1
	}, "errand not subscribed")

	// holder!=self → bridge обязан подняться (Redis-Subscribe на канал).
	waitUntil(t, 3*time.Second, func() bool {
		return redisSubCountFor(mr, errandID) >= 1
	}, "bridge not raised for holder!=self")

	// Cross-keeper доставка: busOther публикует, bridge busSelf форвардит.
	busOther.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    applybus.KindErrandCompleted,
		Payload: ResultEvent{ErrandID: errandID, Status: StatusSuccess, ExitCode: ptrInt32(0)},
	})

	o := recvDispatch(t, done, 4*time.Second)
	if o.err != nil {
		t.Fatalf("Dispatch: %v", o.err)
	}
	if o.res.Status != StatusSuccess {
		t.Fatalf("status = %q, want success (cross-keeper bridge delivery)", o.res.Status)
	}
}

// TestErrand_HolderFlipAfterSkip_TimesOut — holder==self на skip-решении
// (bridge пропущен), но фактически событие приходит только на «другой» bus
// (holder сменился после skip — реальный исполнитель оказался на другом
// keeper-е). Dispatch НЕ получает event через свою local-only подписку и
// завершается timed_out по wait-timer-у. Гарантия: потеря события при
// holder-flip ≠ зависание.
func TestErrand_HolderFlipAfterSkip_TimesOut(t *testing.T) {
	c, mr := newBridgeRedis(t)
	busSelf := applybus.NewBusWithRedis(bridgeTestLogger(), c, bridgeTestKID)
	busOther := applybus.NewBusWithRedis(bridgeTestLogger(), c, "kid-other")

	const sid = "host.flip"
	// Решение на момент Dispatch: holder==self → skip (wantBridge=false).
	lease := &fakeLease{holders: map[string]string{sid: bridgeTestKID}}
	ob := &fakeOutbound{}

	// Короткий timeout: TimeoutSec=1, ServerCap=2s → sync-wait 1с, нет event
	// → timed_out (sync, не async).
	d := buildBridgeDispatcher(t, busSelf, lease, ob, 2*time.Second)

	type out struct {
		res DispatchResult
		err error
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		res, err := d.Dispatch(context.Background(), DispatchRequest{
			SID: sid, Module: "core.cmd.shell", TimeoutSec: 1, StartedByAID: "archon-alice",
		})
		done <- out{res, err}
	}()

	var errandID string
	waitUntil(t, 2*time.Second, func() bool {
		errandID = ob.firstSentErrandID()
		return errandID != "" && busSelf.Subscribers(errandID) == 1
	}, "errand not subscribed locally")

	// bridge пропущен — Redis-Subscribe не поднят.
	if n := redisSubCountFor(mr, errandID); n != 0 {
		t.Fatalf("redis subscribers = %d, want 0 (skip-decision)", n)
	}

	// Событие приходит ТОЛЬКО на «другой» bus (holder-flip). Поскольку busSelf
	// без bridge — оно не доставится в подписку Dispatch-а.
	busOther.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    applybus.KindErrandCompleted,
		Payload: ResultEvent{ErrandID: errandID, Status: StatusSuccess, ExitCode: ptrInt32(0)},
	})

	o := recvDispatch(t, done, 4*time.Second)
	if o.err != nil {
		t.Fatalf("Dispatch: %v", o.err)
	}
	if o.res.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed_out (event lost on flip → timer fires)", o.res.Status)
	}
	if o.res.Async {
		t.Fatalf("Async=true, want sync timed_out")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("elapsed = %v, want ≥1s (timer must fire, not hang or instant)", elapsed)
	}
}

// --- helpers (file-local) ---

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
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

func recvDispatch[T any](t *testing.T, ch <-chan T, timeout time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatal("Dispatch did not return within timeout (possible hang)")
		var zero T
		return zero
	}
}
