package pluginhost

import "sync"

// tailBuffer is a concurrency-safe ring buffer of the last N bytes. It collects
// a plugin's stderr tail: on handshake/RPC failure the last ~4KB of stderr reach
// diagnostics (ADR-020 → plugins.md).
type tailBuffer struct {
	mu   sync.Mutex
	buf  []byte
	max  int
	full bool
	head int
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{buf: make([]byte, 0, max), max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.max {
		// Overwrite the buffer: keep the last t.max bytes of input.
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		t.full = true
		t.head = 0
		return n, nil
	}
	if !t.full && len(t.buf)+n <= t.max {
		t.buf = append(t.buf, p...)
		return n, nil
	}
	// Ring: write mod max, mark full=true.
	if !t.full {
		// Grow up to max; the rest goes into the ring.
		need := t.max - len(t.buf)
		t.buf = append(t.buf, p[:need]...)
		p = p[need:]
		t.full = true
		t.head = 0
	}
	for _, b := range p {
		t.buf[t.head] = b
		t.head = (t.head + 1) % t.max
	}
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.full {
		return string(t.buf)
	}
	out := make([]byte, 0, t.max)
	out = append(out, t.buf[t.head:]...)
	out = append(out, t.buf[:t.head]...)
	return string(out)
}
