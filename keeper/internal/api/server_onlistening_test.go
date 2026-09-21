package api

// The keeper daemon hangs systemd readiness (NIM-157) off the onListening hook:
// READY=1 must go out only once the operator API socket is really bound, and a
// bind failure must surface as a Start error instead.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func newBareServer(addr string) *Server {
	return &Server{
		srv:        &http.Server{Handler: http.NotFoundHandler()},
		configAddr: addr,
		addr:       addr,
		logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func TestStart_OnListeningFiresWithBoundAddr(t *testing.T) {
	srv := newBareServer("127.0.0.1:0")
	bound := make(chan string, 1)
	srv.SetOnListening(func(addr string) { bound <- addr })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	select {
	case addr := <-bound:
		if addr == "" || addr == "127.0.0.1:0" {
			t.Fatalf("onListening addr = %q, want the resolved ephemeral port", addr)
		}
		// The socket accepts connections by the time the hook fires.
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial %s after onListening: %v", addr, err)
		}
		_ = conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("onListening did not fire within 5s")
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
}

func TestStart_OnListeningSilentWhenBindFails(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()

	srv := newBareServer(busy.Addr().String())
	srv.SetOnListening(func(string) { t.Error("onListening fired on a failed bind") })

	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("Start on a taken port returned nil error")
	}
}
