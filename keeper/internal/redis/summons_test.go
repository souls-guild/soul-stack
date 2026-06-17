package redis

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func TestSummonsChannel(t *testing.T) {
	if SummonsChannel != "apply:summons" {
		t.Fatalf("SummonsChannel = %q, want apply:summons", SummonsChannel)
	}
}

func TestPublishSummons_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name      string
		client    *Client
		originKID string
	}{
		{"nil client", nil, "kid"},
		{"empty originKID", c, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PublishSummons(ctx, tc.client, tc.originKID); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestSubscribeSummons_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	noop := func() {}
	cases := []struct {
		name     string
		client   *Client
		onSignal func()
		logger   bool
	}{
		{"nil client", nil, noop, true},
		{"nil onSignal", c, nil, true},
		{"nil logger", c, noop, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lg := discardLog()
			if !tc.logger {
				lg = nil
			}
			if _, err := SubscribeSummons(ctx, tc.client, tc.onSignal, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestSummons_RoundTrip — publish/subscribe полный цикл: на PublishSummons
// подписчик дёргает onSignal. Self-filter отсутствует — origin неважен.
func TestSummons_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fired := make(chan struct{}, 1)
	sub, err := SubscribeSummons(ctx, c, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	}, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSummons: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	n, err := PublishSummons(ctx, c, "keeper-sender")
	if err != nil {
		t.Fatalf("PublishSummons: %v", err)
	}
	if n != 1 {
		t.Errorf("subscribers count = %d, want 1", n)
	}

	select {
	case <-fired:
		// OK — callback вызван.
	case <-time.After(2 * time.Second):
		t.Fatal("onSignal not called within 2s")
	}
}

// TestSummons_NoSelfFilter — сигнал от того же origin, что и подписчик, всё
// равно дёргает callback (Summons НЕ фильтрует self, в отличие от applybus).
func TestSummons_NoSelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var calls atomic.Int64
	sub, err := SubscribeSummons(ctx, c, func() { calls.Add(1) }, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSummons: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Один и тот же origin дважды — оба должны дёрнуть callback.
	if _, err := PublishSummons(ctx, c, "keeper-same"); err != nil {
		t.Fatalf("PublishSummons #1: %v", err)
	}
	if _, err := PublishSummons(ctx, c, "keeper-same"); err != nil {
		t.Fatalf("PublishSummons #2: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("onSignal called %d times, want >= 2 (no self-filter)", calls.Load())
}

// TestSummons_NoSubscribers — publish без подписчиков → 0, без ошибки.
func TestSummons_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	n, err := PublishSummons(ctx, c, "kid")
	if err != nil {
		t.Fatalf("PublishSummons: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestPublishSummons_BestEffortOnClosedClient — недоступный (закрытый) Redis
// при Publish возвращает ошибку (caller её глотает), но не паникует.
func TestPublishSummons_BestEffortOnClosedClient(t *testing.T) {
	c, _ := newClientMR(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := PublishSummons(context.Background(), c, "kid"); err == nil {
		t.Error("expected error publishing on closed client, got nil")
	}
}

// TestSummons_CloseShutsDownGoroutine — Close завершает goroutine; повторный
// Close идемпотентен.
func TestSummons_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeSummons(ctx, c, func() {}, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSummons: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if err := sub.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Errorf("second Close should be idempotent: %v", err)
	}
}

// TestSummons_CloseSurvivesConcurrentReceive — гонка Close vs поток сигналов.
// -race должен пройти.
func TestSummons_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeSummons(ctx, c, func() {}, discardLog())
	if err != nil {
		t.Fatalf("SubscribeSummons: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishSummons(ctx, c, "other")
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}
}
