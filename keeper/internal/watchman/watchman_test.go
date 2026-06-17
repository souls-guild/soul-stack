package watchman

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// scriptedProbe — probe, отдающий ошибки по заранее заданному скрипту. Когда
// скрипт исчерпан, отдаёт последнее значение (удобно для «дальше всё хорошо»).
type scriptedProbe struct {
	mu     sync.Mutex
	script []error
	idx    int
	calls  int
}

func (p *scriptedProbe) Probe(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.script) == 0 {
		return nil
	}
	if p.idx >= len(p.script) {
		return p.script[len(p.script)-1]
	}
	err := p.script[p.idx]
	p.idx++
	return err
}

// countingCloser — fake StreamCloser: считает вызовы CloseAll, возвращает
// настроенное число «закрытых» стримов.
type countingCloser struct {
	mu      sync.Mutex
	calls   int
	returns int
}

func (c *countingCloser) CloseAll() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.returns
}

func (c *countingCloser) closeCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// recordingMetrics — fake Metrics: фиксирует последний isolated и сумму shed.
type recordingMetrics struct {
	mu           sync.Mutex
	isolatedSets []bool
	shedTotal    int
}

func (m *recordingMetrics) SetIsolated(isolated bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isolatedSets = append(m.isolatedSets, isolated)
}

func (m *recordingMetrics) AddStreamsShed(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shedTotal += n
}

func (m *recordingMetrics) lastIsolated() (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.isolatedSets) == 0 {
		return false, false
	}
	return m.isolatedSets[len(m.isolatedSets)-1], true
}

func newTestWatchman(t *testing.T, probe HealthProbe, closer StreamCloser, m Metrics, threshold int) *Watchman {
	t.Helper()
	w, err := New(probe, closer, Config{Interval: time.Millisecond, FailThreshold: threshold}, m, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

var errDep = errors.New("dependency unreachable")

func TestWatchman_IsolationAfterThreshold(t *testing.T) {
	closer := &countingCloser{returns: 4}
	m := &recordingMetrics{}
	w := newTestWatchman(t, &scriptedProbe{}, closer, m, 3)

	// Два провала — изоляции ещё нет (debounce).
	w.tick(errDep)
	w.tick(errDep)
	if closer.closeCalls() != 0 {
		t.Fatalf("CloseAll called before threshold: %d", closer.closeCalls())
	}
	if w.isolated {
		t.Fatal("isolated set before threshold")
	}

	// Третий провал — порог достигнут → shedding.
	w.tick(errDep)
	if closer.closeCalls() != 1 {
		t.Fatalf("CloseAll calls = %d, want 1", closer.closeCalls())
	}
	if !w.isolated {
		t.Fatal("isolated not set after threshold")
	}
	if got, _ := m.lastIsolated(); !got {
		t.Fatal("metric isolated not set to true")
	}
	if m.shedTotal != 4 {
		t.Fatalf("streams_shed total = %d, want 4", m.shedTotal)
	}
}

func TestWatchman_SingleFailDoesNotTrigger(t *testing.T) {
	closer := &countingCloser{returns: 2}
	w := newTestWatchman(t, &scriptedProbe{}, closer, nil, 3)

	w.tick(errDep)
	if closer.closeCalls() != 0 {
		t.Fatalf("single fail triggered CloseAll: %d", closer.closeCalls())
	}
	if w.consecutiveFails != 1 {
		t.Fatalf("consecutiveFails = %d, want 1", w.consecutiveFails)
	}
}

func TestWatchman_RecoveryResetsCounterBeforeIsolation(t *testing.T) {
	closer := &countingCloser{returns: 1}
	w := newTestWatchman(t, &scriptedProbe{}, closer, nil, 3)

	// Два провала, затем успех — счётчик сброшен, изоляции не было.
	w.tick(errDep)
	w.tick(errDep)
	w.tick(nil)
	if w.consecutiveFails != 0 {
		t.Fatalf("consecutiveFails after recovery = %d, want 0", w.consecutiveFails)
	}
	if w.isolated {
		t.Fatal("isolated set despite recovery before threshold")
	}
	if closer.closeCalls() != 0 {
		t.Fatalf("CloseAll called despite recovery: %d", closer.closeCalls())
	}

	// Теперь три новых провала подряд — порог считается с нуля → shedding.
	w.tick(errDep)
	w.tick(errDep)
	if closer.closeCalls() != 0 {
		t.Fatalf("CloseAll too early: %d", closer.closeCalls())
	}
	w.tick(errDep)
	if closer.closeCalls() != 1 {
		t.Fatalf("CloseAll calls = %d, want 1", closer.closeCalls())
	}
}

func TestWatchman_RecoveryAfterIsolationClearsFlag(t *testing.T) {
	closer := &countingCloser{returns: 3}
	m := &recordingMetrics{}
	w := newTestWatchman(t, &scriptedProbe{}, closer, m, 2)

	w.tick(errDep)
	w.tick(errDep) // изоляция
	if !w.isolated {
		t.Fatal("not isolated after threshold")
	}

	w.tick(nil) // recovery
	if w.isolated {
		t.Fatal("isolated flag not cleared after recovery")
	}
	if got, _ := m.lastIsolated(); got {
		t.Fatal("metric isolated not reset to false on recovery")
	}
}

func TestWatchman_NoSecondShedWhileIsolated(t *testing.T) {
	closer := &countingCloser{returns: 5}
	w := newTestWatchman(t, &scriptedProbe{}, closer, nil, 2)

	w.tick(errDep)
	w.tick(errDep) // shedding #1
	if closer.closeCalls() != 1 {
		t.Fatalf("CloseAll calls = %d, want 1", closer.closeCalls())
	}
	// Ещё провалы в изоляции — повторного CloseAll быть не должно.
	w.tick(errDep)
	w.tick(errDep)
	if closer.closeCalls() != 1 {
		t.Fatalf("second shed while isolated: CloseAll calls = %d, want 1", closer.closeCalls())
	}
}

// TestWatchman_RunLoopShedsAndStops прогоняет реальный Run-loop: probe отдаёт
// устойчивые провалы → после порога shedding → cancel ctx останавливает loop.
func TestWatchman_RunLoopShedsAndStops(t *testing.T) {
	probe := &scriptedProbe{script: []error{errDep}} // всегда провал
	closer := &countingCloser{returns: 2}
	m := &recordingMetrics{}
	w, err := New(probe, closer, Config{Interval: time.Millisecond, FailThreshold: 2}, m, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if closer.closeCalls() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run loop did not shed within timeout")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run loop did not stop after ctx cancel")
	}

	if got, ok := m.lastIsolated(); !ok || !got {
		t.Fatal("metric isolated not true after run-loop shedding")
	}
}

func TestNew_RequiresDeps(t *testing.T) {
	closer := &countingCloser{}
	probe := &scriptedProbe{}
	if _, err := New(nil, closer, Config{}, nil, discardLogger()); err == nil {
		t.Fatal("New accepted nil probe")
	}
	if _, err := New(probe, nil, Config{}, nil, discardLogger()); err == nil {
		t.Fatal("New accepted nil closer")
	}
	if _, err := New(probe, closer, Config{}, nil, nil); err == nil {
		t.Fatal("New accepted nil logger")
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	w, err := New(&scriptedProbe{}, &countingCloser{}, Config{}, nil, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w.cfg.Interval != DefaultInterval {
		t.Errorf("Interval = %v, want default %v", w.cfg.Interval, DefaultInterval)
	}
	if w.cfg.FailThreshold != DefaultFailThreshold {
		t.Errorf("FailThreshold = %d, want default %d", w.cfg.FailThreshold, DefaultFailThreshold)
	}
	if w.probeTO != defaultProbeTimeout {
		t.Errorf("probeTO = %v, want default %v", w.probeTO, defaultProbeTimeout)
	}
}
