package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/sdnotify"
)

type fakeSoulCounter int

func (f fakeSoulCounter) Count() int { return int(f) }

func TestKeeperStatusLine(t *testing.T) {
	if got, want := keeperStatus("127.0.0.1:8080", fakeSoulCounter(7)), "serving: api=127.0.0.1:8080 souls=7"; got != want {
		t.Fatalf("keeperStatus = %q, want %q", got, want)
	}
	if got, want := keeperStatus("127.0.0.1:8080", nil), "serving: api=127.0.0.1:8080"; got != want {
		t.Fatalf("keeperStatus without a counter = %q, want %q", got, want)
	}
}

// Outside systemd (docker/k8s: no NOTIFY_SOCKET) the reporter must not start a
// ticker that outlives the call.
func TestStatusReporterNoopWithoutSystemd(t *testing.T) {
	if err := os.Unsetenv("NOTIFY_SOCKET"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	notifier := sdnotify.New(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runStatusReporter(context.Background(), notifier, "127.0.0.1:8080", fakeSoulCounter(1), time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runStatusReporter did not return with systemd absent")
	}
}
