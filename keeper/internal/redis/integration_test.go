//go:build integration

// Integration-тест Redis-клиента и lease-семантики на реальном redis:7
// через testcontainers-go (generic container — отдельный модуль
// `testcontainers/modules/redis` не подключаем, чтобы не плодить зависимости).
//
// Запуск:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/redis/...

package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

const (
	integrationRedisImage = "redis:7-alpine"
)

var integrationAddr string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        integrationRedisImage,
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		if requireDocker() {
			log.Fatalf("redis integration: container setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("redis integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = ctr.Terminate(tctx)
	}()

	host, err := ctr.Host(ctx)
	if err != nil {
		log.Printf("redis integration: container host: %v", err)
		return 1
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		log.Printf("redis integration: mapped port: %v", err)
		return 1
	}
	integrationAddr = fmt.Sprintf("%s:%s", host, port.Port())

	return m.Run()
}

func TestIntegration_PingAndClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestIntegration_LeaseAcquireRenewRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	key := uniqueKey(t)

	l, err := Acquire(ctx, c, key, "keeper-int-a", 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if err := l.Renew(ctx); err != nil {
		t.Errorf("Renew: %v", err)
	}

	// Конкурент не должен получить ключ.
	if _, err := Acquire(ctx, c, key, "keeper-int-b", 5*time.Second); !errors.Is(err, ErrLeaseTaken) {
		t.Errorf("competing Acquire err = %v, want ErrLeaseTaken", err)
	}

	if err := l.Release(ctx); err != nil {
		t.Errorf("Release: %v", err)
	}

	// После Release конкурент проходит.
	l2, err := Acquire(ctx, c, key, "keeper-int-b", 5*time.Second)
	if err != nil {
		t.Errorf("Acquire after Release: %v", err)
	}
	defer l2.Release(ctx)
}

func TestIntegration_LeaseExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	key := uniqueKey(t)

	// Короткий TTL, без Renew — Renew после expiry должен вернуть ErrLeaseLost.
	l, err := Acquire(ctx, c, key, "keeper-int-a", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	time.Sleep(700 * time.Millisecond)

	if err := l.Renew(ctx); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("Renew after expiry err = %v, want ErrLeaseLost", err)
	}
}

func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("reaper:leader:test:%s", t.Name())
}

// TestIntegration_OutboundPubSub_RoundTrip — real Redis pub/sub:
// PublishOutbound от одного клиента, SubscribeOutbound на другом,
// FromKeeper доходит через protojson-envelope.
func TestIntegration_OutboundPubSub_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cPub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(pub): %v", err)
	}
	defer cPub.Close()

	cSub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(sub): %v", err)
	}
	defer cSub.Close()

	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	sid := fmt.Sprintf("host.%s.example.com", t.Name())

	sub, err := SubscribeOutbound(ctx, cSub, sid, "keeper-receiver", lg)
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ApplyRequest{ApplyRequest: &keeperv1.ApplyRequest{
			ApplyId: "01HABCINT",
			Tasks:   []*keeperv1.RenderedTask{{Name: "t1", Module: "core.pkg.installed"}},
		}},
	}
	n, err := PublishOutbound(ctx, cPub, sid, "keeper-sender", msg)
	if err != nil {
		t.Fatalf("PublishOutbound: %v", err)
	}
	if n < 1 {
		t.Errorf("subscribers count = %d, want >= 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		req := got.GetApplyRequest()
		if req == nil {
			t.Fatalf("payload = %T, want ApplyRequest", got.GetPayload())
		}
		if req.GetApplyId() != "01HABCINT" {
			t.Errorf("apply_id = %q, want 01HABCINT", req.GetApplyId())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive message within 5s")
	}
}

// TestIntegration_OutboundPubSub_SelfFilter — real Redis: self-origin
// сообщения отфильтровываются.
func TestIntegration_OutboundPubSub_SelfFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	sid := fmt.Sprintf("host.%s.example.com", t.Name())

	sub, err := SubscribeOutbound(ctx, c, sid, "keeper-self", lg)
	if err != nil {
		t.Fatalf("SubscribeOutbound: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelApply{CancelApply: &keeperv1.CancelApply{ApplyId: "x"}},
	}
	if _, err := PublishOutbound(ctx, c, sid, "keeper-self", msg); err != nil {
		t.Fatalf("PublishOutbound self: %v", err)
	}
	if _, err := PublishOutbound(ctx, c, sid, "keeper-other", msg); err != nil {
		t.Fatalf("PublishOutbound other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.GetCancelApply() == nil {
			t.Errorf("payload = %T, want CancelApply (other-origin)", got.GetPayload())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive other-origin message within 5s")
	}

	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: payload = %T", got.GetPayload())
		}
	case <-time.After(500 * time.Millisecond):
		// OK — self отфильтрован.
	}
}

// TestIntegration_ApplyBusPubSub_RoundTrip — real Redis pub/sub:
// PublishApplyEvent от одного клиента, SubscribeApplyEvent на другом,
// событие доходит через JSON-envelope.
func TestIntegration_ApplyBusPubSub_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cPub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(pub): %v", err)
	}
	defer cPub.Close()

	cSub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(sub): %v", err)
	}
	defer cSub.Close()

	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	applyID := fmt.Sprintf("01J%sINT", t.Name())

	sub, err := SubscribeApplyEvent(ctx, cSub, applyID, "keeper-receiver", lg)
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	payload := json.RawMessage(`{"sid":"host.example","task_idx":0,"task_status":"TASK_STATUS_OK"}`)
	n, err := PublishApplyEvent(ctx, cPub, applyID, "keeper-sender", "task.executed", time.Time{}, payload)
	if err != nil {
		t.Fatalf("PublishApplyEvent: %v", err)
	}
	if n < 1 {
		t.Errorf("subscribers count = %d, want >= 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		if got.Kind != "task.executed" {
			t.Errorf("kind = %q, want task.executed", got.Kind)
		}
		if got.ApplyID != applyID {
			t.Errorf("apply_id = %q, want %q", got.ApplyID, applyID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive message within 5s")
	}
}

// TestIntegration_ApplyBusPubSub_SelfFilter — real Redis: self-origin
// сообщения отфильтровываются.
func TestIntegration_ApplyBusPubSub_SelfFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	applyID := fmt.Sprintf("01J%sFLT", t.Name())

	sub, err := SubscribeApplyEvent(ctx, c, applyID, "keeper-self", lg)
	if err != nil {
		t.Fatalf("SubscribeApplyEvent: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	payload := json.RawMessage(`{"sid":"host.example"}`)
	if _, err := PublishApplyEvent(ctx, c, applyID, "keeper-self", "task.executed", time.Time{}, payload); err != nil {
		t.Fatalf("PublishApplyEvent self: %v", err)
	}
	if _, err := PublishApplyEvent(ctx, c, applyID, "keeper-other", "apply.completed", time.Time{}, payload); err != nil {
		t.Fatalf("PublishApplyEvent other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.Kind != "apply.completed" {
			t.Errorf("kind = %q, want apply.completed (other-origin)", got.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive other-origin message within 5s")
	}

	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: kind = %q", got.Kind)
		}
	case <-time.After(500 * time.Millisecond):
		// OK — self отфильтрован.
	}
}

// TestIntegration_Summons_RoundTrip — real Redis pub/sub:
// PublishSummons от одного клиента, SubscribeSummons на другом, onSignal
// дёрнут. Self-filter отсутствует (origin неважен).
func TestIntegration_Summons_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cPub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(pub): %v", err)
	}
	defer cPub.Close()

	cSub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(sub): %v", err)
	}
	defer cSub.Close()

	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	fired := make(chan struct{}, 1)

	sub, err := SubscribeSummons(ctx, cSub, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	}, lg)
	if err != nil {
		t.Fatalf("SubscribeSummons: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	n, err := PublishSummons(ctx, cPub, "keeper-sender")
	if err != nil {
		t.Fatalf("PublishSummons: %v", err)
	}
	if n < 1 {
		t.Errorf("subscribers count = %d, want >= 1", n)
	}

	select {
	case <-fired:
		// OK — callback вызван.
	case <-time.After(5 * time.Second):
		t.Fatal("onSignal not called within 5s")
	}
}

// TestIntegration_RBACInvalidate_RoundTrip — real Redis pub/sub:
// PublishRBACInvalidate с ноды A (origin_kid=A), SubscribeRBACInvalidate на
// ноде B (kid=B) получает сигнал «перечитай RBAC-снимок». Origin отличается от
// подписчика → self-filter не срабатывает, сообщение доходит до Channel().
func TestIntegration_RBACInvalidate_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cPub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(pub): %v", err)
	}
	defer cPub.Close()

	cSub, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient(sub): %v", err)
	}
	defer cSub.Close()

	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	const originKID, subKID = "keeper-A", "keeper-B"

	sub, err := SubscribeRBACInvalidate(ctx, cSub, subKID, lg)
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	n, err := PublishRBACInvalidate(ctx, cPub, originKID)
	if err != nil {
		t.Fatalf("PublishRBACInvalidate: %v", err)
	}
	if n < 1 {
		t.Errorf("subscribers count = %d, want >= 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		if got.OriginKID != originKID {
			t.Errorf("origin_kid = %q, want %q", got.OriginKID, originKID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive cross-node invalidate within 5s")
	}
}

// TestIntegration_RBACInvalidate_SelfFilter — real Redis: подписчик с kid=A
// собственный publish (origin_kid=A) ИГНОРИРУЕТ (self-filter по KID), а
// cross-node сигнал (origin_kid=B) пропускает. Ключевой инвариант: нода не
// рефрешит снимок по своему же publish-у (полагается на TTL-poll, B1-fallback).
func TestIntegration_RBACInvalidate_SelfFilter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	const selfKID, otherKID = "keeper-self", "keeper-other"

	sub, err := SubscribeRBACInvalidate(ctx, c, selfKID, lg)
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Сначала self-origin (должен быть отфильтрован), затем other-origin (должен
	// дойти). Порядок гарантирует: если бы self НЕ фильтровался, он пришёл бы
	// первым и тест упал бы на проверке origin_kid.
	if _, err := PublishRBACInvalidate(ctx, c, selfKID); err != nil {
		t.Fatalf("PublishRBACInvalidate self: %v", err)
	}
	if _, err := PublishRBACInvalidate(ctx, c, otherKID); err != nil {
		t.Fatalf("PublishRBACInvalidate other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.OriginKID != otherKID {
			t.Errorf("origin_kid = %q, want %q (self must be filtered)", got.OriginKID, otherKID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive other-origin invalidate within 5s")
	}

	// Лишних сообщений быть не должно — self отфильтрован.
	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: origin_kid = %q", got.OriginKID)
		}
	case <-time.After(500 * time.Millisecond):
		// OK — self-origin отфильтрован.
	}
}

// TestIntegration_ReadSoulLeaseHolder_RoundTrip — real Redis: lease
// hold-value читается через ReadSoulLeaseHolder.
func TestIntegration_ReadSoulLeaseHolder_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := NewClient(ctx, Config{Addr: integrationAddr}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	sid := fmt.Sprintf("host.%s.example.com", t.Name())

	v, err := ReadSoulLeaseHolder(ctx, c, sid)
	if err != nil {
		t.Fatalf("ReadSoulLeaseHolder (empty): %v", err)
	}
	if v != "" {
		t.Errorf("holder before acquire = %q, want empty", v)
	}

	l, err := AcquireSoulLease(ctx, c, sid, "keeper-int-1", 10*time.Second)
	if err != nil {
		t.Fatalf("AcquireSoulLease: %v", err)
	}
	defer l.Release(ctx)

	v, err = ReadSoulLeaseHolder(ctx, c, sid)
	if err != nil {
		t.Fatalf("ReadSoulLeaseHolder: %v", err)
	}
	if v != "keeper-int-1" {
		t.Errorf("holder = %q, want keeper-int-1", v)
	}
}
