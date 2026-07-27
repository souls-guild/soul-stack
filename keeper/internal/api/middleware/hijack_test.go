package middleware

// REGRESSION NIM-143: the /v1 chain wraps the ResponseWriter (StatusRecorder
// from audit.go, statusRecorder from authlimit.go, obs.statusRecorder from the
// HTTP-metrics middleware). A WebSocket upgrade needs http.Hijacker on the
// writer it is handed; a wrapper that embeds http.ResponseWriter without
// forwarding Hijack silently hides it, and gorilla's Upgrader answers
// 500 "not a hijacker" — with no way to tell from the handler. We pin the
// passthrough on every link, including stacked.

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hijackSpy — a terminal ResponseWriter that plays the role of the real socket.
type hijackSpy struct {
	http.ResponseWriter
	hijacked bool
}

func (h *hijackSpy) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, errors.New("spy: no real conn")
}

func TestStatusRecorders_HijackPassthrough(t *testing.T) {
	cases := []struct {
		name string
		wrap func(spy *hijackSpy) http.ResponseWriter
	}{
		{"audit.StatusRecorder", func(spy *hijackSpy) http.ResponseWriter { return NewStatusRecorder(spy) }},
		{"authlimit.statusRecorder", func(spy *hijackSpy) http.ResponseWriter { return &statusRecorder{ResponseWriter: spy} }},
		{"stacked audit->authlimit", func(spy *hijackSpy) http.ResponseWriter {
			return NewStatusRecorder(&statusRecorder{ResponseWriter: spy})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &hijackSpy{ResponseWriter: httptest.NewRecorder()}
			rec := tc.wrap(spy)

			hj, ok := rec.(http.Hijacker)
			if !ok {
				t.Fatal("recorder does not expose http.Hijacker - the WebSocket upgrade would fail with 500")
			}
			if _, _, err := hj.Hijack(); err == nil {
				t.Fatal("Hijack returned no error - the spy should have reported one")
			}
			if !spy.hijacked {
				t.Fatal("Hijack() did not reach the terminal socket hijacker")
			}
		})
	}
}

// A writer with no Hijack under it must report the plain "not supported" error
// rather than panicking on the type assertion.
func TestStatusRecorder_HijackUnsupported(t *testing.T) {
	var rec http.ResponseWriter = NewStatusRecorder(httptest.NewRecorder())
	hj, ok := rec.(http.Hijacker)
	if !ok {
		t.Fatal("StatusRecorder must always expose http.Hijacker")
	}
	if _, _, err := hj.Hijack(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("Hijack error = %v, want http.ErrNotSupported", err)
	}
}
