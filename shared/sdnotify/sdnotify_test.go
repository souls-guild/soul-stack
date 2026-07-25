package sdnotify

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// listen starts a unixgram socket standing in for systemd's notify socket and
// exports its path via NOTIFY_SOCKET. The datagrams are delivered on the
// returned channel.
func listen(t *testing.T) <-chan string {
	t.Helper()

	// os.MkdirTemp instead of t.TempDir: the test name goes into t.TempDir's
	// path and a unix socket address is capped at ~108 bytes.
	dir, err := os.MkdirTemp("", "sdnotify")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	addr := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenPacket("unixgram", addr)
	if err != nil {
		t.Fatalf("listen unixgram %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	msgs := make(chan string, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				close(msgs)
				return
			}
			msgs <- string(buf[:n])
		}
	}()

	t.Setenv(notifySocketEnv, addr)
	return msgs
}

func recv(t *testing.T, msgs <-chan string) string {
	t.Helper()
	select {
	case m, ok := <-msgs:
		if !ok {
			t.Fatal("notify socket closed before a datagram arrived")
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram within 2s")
		return ""
	}
}

// Without NOTIFY_SOCKET (docker, k8s, bare run) the notifier must stay silent
// and harmless — no panic, no error, no goroutine left behind.
func TestNoopWithoutNotifySocket(t *testing.T) {
	t.Setenv(notifySocketEnv, "")
	t.Setenv("WATCHDOG_USEC", "1000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))

	n := New(nil)
	if n.Enabled() {
		t.Fatal("notifier is enabled without NOTIFY_SOCKET")
	}
	if got := n.WatchdogInterval(); got != 0 {
		t.Fatalf("watchdog interval = %v, want 0 without NOTIFY_SOCKET", got)
	}

	n.Ready("serving")
	n.Status("still here")
	n.Reloading()
	n.Stopping("bye")

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.RunWatchdog(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunWatchdog did not return with the watchdog disabled")
	}

	// The nil notifier is the same no-op: call sites do not guard for it.
	var nilNotifier *Notifier
	nilNotifier.Ready("")
	nilNotifier.Status("")
	nilNotifier.Stopping("")
	nilNotifier.RunWatchdog(context.Background())
}

func TestReadyStatusStoppingDelivered(t *testing.T) {
	msgs := listen(t)

	n := New(nil)
	if !n.Enabled() {
		t.Fatal("notifier is disabled with NOTIFY_SOCKET set")
	}

	n.Ready("connected to keeper")
	if got, want := recv(t, msgs), "READY=1\nSTATUS=connected to keeper"; got != want {
		t.Fatalf("ready datagram = %q, want %q", got, want)
	}

	n.Status("applying destiny")
	if got, want := recv(t, msgs), "STATUS=applying destiny"; got != want {
		t.Fatalf("status datagram = %q, want %q", got, want)
	}

	n.Reloading()
	if got, want := recv(t, msgs), "RELOADING=1"; got != want {
		t.Fatalf("reloading datagram = %q, want %q", got, want)
	}

	n.Stopping("shutting down")
	if got, want := recv(t, msgs), "STOPPING=1\nSTATUS=shutting down"; got != want {
		t.Fatalf("stopping datagram = %q, want %q", got, want)
	}

	// An empty status carries the state field alone.
	n.Ready("")
	if got, want := recv(t, msgs), "READY=1"; got != want {
		t.Fatalf("bare ready datagram = %q, want %q", got, want)
	}

	n.ExtendTimeout(90 * time.Second)
	if got, want := recv(t, msgs), "EXTEND_TIMEOUT_USEC=90000000"; got != want {
		t.Fatalf("extend-timeout datagram = %q, want %q", got, want)
	}

	// A non-positive extension is not a protocol message.
	n.ExtendTimeout(0)
	n.Status("after the no-op extension")
	if got, want := recv(t, msgs), "STATUS=after the no-op extension"; got != want {
		t.Fatalf("datagram after ExtendTimeout(0) = %q, want %q", got, want)
	}
}

// A status is host data (sid, endpoint, error text): it must never forge a
// second protocol field.
func TestStatusFoldsToOneLine(t *testing.T) {
	msgs := listen(t)

	New(nil).Status("dial failed\nSTOPPING=1\nWATCHDOG=1")
	got := recv(t, msgs)
	if strings.Contains(got, "\n") {
		t.Fatalf("status datagram is multi-line: %q", got)
	}
	if got != "STATUS=dial failed STOPPING=1 WATCHDOG=1" {
		t.Fatalf("status datagram = %q", got)
	}

	New(nil).Status(strings.Repeat("x", statusMaxLen+50))
	if got, want := len(recv(t, msgs)), len("STATUS=")+statusMaxLen; got != want {
		t.Fatalf("truncated status length = %d, want %d", got, want)
	}
}

func TestWatchdogPingsUntilContextDone(t *testing.T) {
	msgs := listen(t)
	t.Setenv("WATCHDOG_USEC", "200000") // 200ms -> ping every second (floored)
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))

	n := New(nil)
	if got, want := n.WatchdogInterval(), 200*time.Millisecond; got != want {
		t.Fatalf("watchdog interval = %v, want %v", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		n.RunWatchdog(ctx)
	}()

	if got := recv(t, msgs); got != "WATCHDOG=1" {
		t.Fatalf("watchdog datagram = %q", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunWatchdog did not return after ctx cancel")
	}
}

// WATCHDOG_PID guards a child that inherited the environment from pinging on
// the daemon's behalf.
func TestWatchdogIgnoresForeignPid(t *testing.T) {
	listen(t)
	t.Setenv("WATCHDOG_USEC", "5000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()+1))

	if got := New(nil).WatchdogInterval(); got != 0 {
		t.Fatalf("watchdog interval = %v, want 0 for a foreign WATCHDOG_PID", got)
	}
}
