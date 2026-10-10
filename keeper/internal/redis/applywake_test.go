package redis

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// ★ GUARD (NIM-984). A wake reaches every other instance, and not the one that
// published it: the origin has woken its own barriers already, and its echo would
// make each of them read its rows a second time for nothing.
//
// Mutation: drop the self-filter in the forward loop → the origin hears itself.
func TestApplyWake_DeliversToOtherInstancesAndSkipsTheOrigin(t *testing.T) {
	c, _ := newClientMR(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logger := slog.New(slog.DiscardHandler)

	peer, err := SubscribeApplyWake(ctx, c, "kid-peer", logger)
	if err != nil {
		t.Fatalf("SubscribeApplyWake(peer): %v", err)
	}
	defer func() { _ = peer.Close() }()
	origin, err := SubscribeApplyWake(ctx, c, "kid-origin", logger)
	if err != nil {
		t.Fatalf("SubscribeApplyWake(origin): %v", err)
	}
	defer func() { _ = origin.Close() }()
	if err := peer.Ready(ctx); err != nil {
		t.Fatalf("peer.Ready: %v", err)
	}
	if err := origin.Ready(ctx); err != nil {
		t.Fatalf("origin.Ready: %v", err)
	}

	if err := PublishApplyWake(ctx, c, "run-a", "kid-origin"); err != nil {
		t.Fatalf("PublishApplyWake: %v", err)
	}

	select {
	case got := <-peer.Channel():
		if got != "run-a" {
			t.Fatalf("peer got a wake for %q, want run-a", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the other instance never heard the wake")
	}
	select {
	case got := <-origin.Channel():
		t.Fatalf("the publishing instance heard its own wake for %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}
