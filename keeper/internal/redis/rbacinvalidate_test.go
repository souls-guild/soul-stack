package redis

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRBACInvalidateChannel(t *testing.T) {
	if RBACInvalidateChannel != "rbac:invalidate" {
		t.Fatalf("RBACInvalidateChannel = %q, want rbac:invalidate", RBACInvalidateChannel)
	}
}

func TestPublishRBACInvalidate_RejectsBadArgs(t *testing.T) {
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
			if _, err := PublishRBACInvalidate(ctx, tc.client, tc.originKID); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

func TestSubscribeRBACInvalidate_RejectsBadArgs(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()
	cases := []struct {
		name    string
		client  *Client
		selfKID string
		hasLog  bool
	}{
		{"nil client", nil, "kid", true},
		{"empty selfKID", c, "", true},
		{"nil logger", c, "kid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lg = discardLog()
			if !tc.hasLog {
				lg = nil
			}
			if _, err := SubscribeRBACInvalidate(ctx, tc.client, tc.selfKID, lg); err == nil {
				t.Errorf("expected validation error, got nil")
			}
		})
	}
}

// TestRBACInvalidate_RoundTrip — publish/subscribe полный цикл: подписчик с
// чужим KID получает invalidate-сообщение со штампом At.
func TestRBACInvalidate_RoundTrip(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeRBACInvalidate(ctx, c, "keeper-receiver", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	before := time.Now().UTC()
	n, err := PublishRBACInvalidate(ctx, c, "keeper-sender")
	if err != nil {
		t.Fatalf("PublishRBACInvalidate: %v", err)
	}
	if n != 1 {
		t.Errorf("subscribers count = %d, want 1", n)
	}

	select {
	case got, ok := <-sub.Channel():
		if !ok {
			t.Fatal("subscription channel closed before message")
		}
		if got.OriginKID != "keeper-sender" {
			t.Errorf("origin_kid = %q, want keeper-sender", got.OriginKID)
		}
		if got.At.Before(before.Add(-time.Second)) {
			t.Errorf("At=%v is before Publish call (=%v)", got.At, before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive message within 2s")
	}
}

// TestRBACInvalidate_SelfFilter — publish с тем же origin_kid, что и selfKID
// подписчика → игнорируется; чужой проходит.
func TestRBACInvalidate_SelfFilter(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := SubscribeRBACInvalidate(ctx, c, "keeper-self", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	defer sub.Close()
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Self-origin сначала — должно быть отфильтровано.
	if _, err := PublishRBACInvalidate(ctx, c, "keeper-self"); err != nil {
		t.Fatalf("PublishRBACInvalidate self: %v", err)
	}
	// Потом чужой — должен прийти.
	if _, err := PublishRBACInvalidate(ctx, c, "keeper-other"); err != nil {
		t.Fatalf("PublishRBACInvalidate other: %v", err)
	}

	select {
	case got := <-sub.Channel():
		if got.OriginKID != "keeper-other" {
			t.Errorf("origin_kid = %q, want keeper-other (self filtered)", got.OriginKID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive other-origin message within 2s")
	}

	// Self не должен прийти дополнительно.
	select {
	case got, ok := <-sub.Channel():
		if ok {
			t.Errorf("unexpected extra message: origin_kid = %q", got.OriginKID)
		}
	case <-time.After(150 * time.Millisecond):
		// OK — self отфильтрован.
	}
}

// TestRBACInvalidate_NoSubscribers — publish без подписчиков → 0, без ошибки.
func TestRBACInvalidate_NoSubscribers(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	n, err := PublishRBACInvalidate(ctx, c, "kid")
	if err != nil {
		t.Fatalf("PublishRBACInvalidate: %v", err)
	}
	if n != 0 {
		t.Errorf("subscribers count = %d, want 0", n)
	}
}

// TestRBACInvalidate_CloseShutsDownGoroutine — Close завершает goroutine и
// закрывает out-канал; повторный Close идемпотентен.
func TestRBACInvalidate_CloseShutsDownGoroutine(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeRBACInvalidate(ctx, c, "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
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

// TestRBACInvalidate_CloseSurvivesConcurrentReceive — гонка Close vs поток
// данных. -race должен пройти.
func TestRBACInvalidate_CloseSurvivesConcurrentReceive(t *testing.T) {
	c, _ := newClientMR(t)
	ctx := context.Background()

	sub, err := SubscribeRBACInvalidate(ctx, c, "kid", discardLog())
	if err != nil {
		t.Fatalf("SubscribeRBACInvalidate: %v", err)
	}
	if err := sub.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	go func() {
		for i := 0; i < 5; i++ {
			_, _ = PublishRBACInvalidate(ctx, c, "other")
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if err := sub.Close(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("Close: %v", err)
	}
	for range sub.Channel() {
	}
}
