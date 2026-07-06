package middleware

// РЕГРЕСС NIM-37: рекордеры /v1 (StatusRecorder из audit.go, statusRecorder из authlimit.go)
// встраивают http.ResponseWriter. Без Unwrap/Flush цепочки unwrapFlusher/unwrapWriteDeadliner
// SSE-хендлера live-хода прогона рвутся на них → flush=no-op → поток не долетает клиенту.
// Пиним прозрачность обоих звеньев (в т.ч. вложенных) для Flush и Unwrap.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// flushSpy — терминальный ResponseWriter, фиксирующий факт Flush() (роль реального сокета).
type flushSpy struct {
	http.ResponseWriter
	flushed bool
}

func (f *flushSpy) Flush() { f.flushed = true }

// walkFlusher повторяет unwrapFlusher SSE-хендлера: http.Flusher напрямую ИЛИ по цепочке
// Unwrap() http.ResponseWriter. nil — цепочка порвалась (звено не отдаёт ни Flush, ни Unwrap).
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

// walkToSpy идёт по Unwrap()-цепочке до терминального *flushSpy — параллель
// unwrapWriteDeadliner (тот же Unwrap-обход ищет SetWriteDeadline реального сокета).
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
				t.Fatal("unwrapFlusher-цепочка порвалась на рекордере (нет Flush/Unwrap) — SSE не флашится")
			}
			f.Flush()
			if !spy.flushed {
				t.Fatal("Flush() не дошёл до терминального сокет-флашера")
			}
			if walkToSpy(rec) != spy {
				t.Fatal("Unwrap()-цепочка не доходит до вложенного ResponseWriter (unwrapWriteDeadliner порвётся)")
			}
		})
	}
}
