package beacon

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestPortClosedOpen(t *testing.T) {
	// A real local listener — deterministic "open" with no sleep.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	b := NewPortClosed()
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"port": port,
		"host": "127.0.0.1",
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != statePortOpen {
		t.Fatalf("state = %q, want open", state)
	}
	if int(data.GetFields()["port"].GetNumberValue()) != port {
		t.Error("data.port must carry the port")
	}
}

func TestPortClosedRefused(t *testing.T) {
	// Open and immediately close the listener: the port is guaranteed free
	// and dial gets refused — deterministic "closed" without guessing a port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	b := NewPortClosed()
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"port": port}))
	if err != nil {
		t.Fatalf("Check for a closed port must not return an error: %v", err)
	}
	if state != statePortClosed {
		t.Fatalf("state = %q, want closed", state)
	}
}

func TestPortClosedDialError(t *testing.T) {
	// Fake dialer that errors — closed with no network access (host unreachable).
	b := &PortClosed{Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no route to host")
	}}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"port": 8080,
		"host": "10.255.255.1",
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != statePortClosed {
		t.Fatalf("state = %q, want closed", state)
	}
	if data.GetFields()["host"].GetStringValue() != "10.255.255.1" {
		t.Error("data.host must carry the host")
	}
}

func TestPortClosedDefaultHost(t *testing.T) {
	var gotAddr string
	b := &PortClosed{Dial: func(_ context.Context, _, address string) (net.Conn, error) {
		gotAddr = address
		return nil, errors.New("refused")
	}}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"port": 5432})); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if gotAddr != "127.0.0.1:5432" {
		t.Fatalf("default host: dial on %q, expected 127.0.0.1:5432", gotAddr)
	}
}

func TestPortClosedMissingPort(t *testing.T) {
	b := NewPortClosed()
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("expected an error when param port is missing")
	}
}

func TestPortClosedInvalidPort(t *testing.T) {
	b := NewPortClosed()
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"port": 70000})); err == nil {
		t.Fatal("expected an error when port is out of range 1..65535")
	}
}
