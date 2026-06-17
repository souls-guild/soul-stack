package pluginhost

import "sync"

// tailBuffer — конкурентно-безопасный кольцевой буфер последних N байт.
// Используется для сбора stderr-tail плагина: при сбое handshake/RPC
// последние ~4KB stderr попадают в диагностику (ADR-020 → plugins.md →
// Поведение host-а после handshake).
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
		// Перетираем буфер: запоминаем последние t.max байт входа.
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		t.full = true
		t.head = 0
		return n, nil
	}
	if !t.full && len(t.buf)+n <= t.max {
		t.buf = append(t.buf, p...)
		return n, nil
	}
	// Кольцо: пишем по mod max, фиксируем full=true.
	if !t.full {
		// «Доращиваем» до max, остальное идёт в кольцо.
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
