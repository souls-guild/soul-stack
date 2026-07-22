package middleware

// REGRESSION NIM-37: the /v1 recorders (StatusRecorder from audit.go, statusRecorder from authlimit.go)
// embed http.ResponseWriter. Without Unwrap/Flush, the unwrapFlusher/unwrapWriteDeadliner chains of
// the run's live-progress SSE handler break on them → flush=no-op → the stream does not reach the client.
// We pin the transparency of both links (including nested) for Flush and Unwrap.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushSpy — a terminal ResponseWriter that records the fact of Flush() (the role of a real socket).
type flushSpy struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushSpy) Flush() { f.flushed = true }

// walkFlusher replicates the SSE handler's unwrapFlusher: http.Flusher directly OR via the
// Unwrap() http.ResponseWriter chain. nil — the chain broke (a link exposes neither Flush nor Unwrap).
func walkFlusher(w http.ResponseWriter) http.Flusher {
	var cur any = w
	for {
		if f, ok := cur.(http.Flusher); ok {
			return f
		}
		u, ok := cur.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		cur = u.Unwrap()
	}
}

// walkToSpy walks the Unwrap() chain to the terminal *flushSpy — a parallel of
// unwrapWriteDeadliner (the same Unwrap traversal finds the real socket's SetWriteDeadline).
func walkToSpy(w http.ResponseWriter) *flushSpy {
	cur := w
	for {
		if s, ok := cur.(*flushSpy); ok {
			return s
		}
		u, ok := cur.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		cur = u.Unwrap()
	}
}

func TestStatusRecorders_SSEFlushPassthrough(t *testing.T) {
	cases := []struct {
		name string
		wrap func(spy *flushSpy) http.ResponseWriter
	}{
		{"audit.StatusRecorder", func(spy *flushSpy) http.ResponseWriter { return NewStatusRecorder(spy) }},
		{"authlimit.statusRecorder", func(spy *flushSpy) http.ResponseWriter { return &statusRecorder{ResponseWriter: spy} }},
		{"stacked audit->authlimit", func(spy *flushSpy) http.ResponseWriter {
			return NewStatusRecorder(&statusRecorder{ResponseWriter: spy})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &flushSpy{ResponseWriter: httptest.NewRecorder()}
			rec := tc.wrap(spy)

			f := walkFlusher(rec)
			if f == nil {
				t.Fatal("unwrapFlusher chain broke on the recorder (no Flush/Unwrap) - SSE not flushed")
			}
			f.Flush()
			if !spy.flushed {
				t.Fatal("Flush() did not reach the terminal socket flusher")
			}
			if walkToSpy(rec) != spy {
				t.Fatal("Unwrap() chain does not reach the wrapped ResponseWriter (unwrapWriteDeadliner would break)")
			}
		})
	}
}
