package beacon

import "time"

// SetTicker swaps the ticker factory for a deterministic one (test-only).
// No need to restore production form — the scheduler is single-use in tests.
func (s *Scheduler) SetTicker(f func(time.Duration) ticker) { s.newTicker = f }

// SetNow swaps the time source (for checking PortentEvent.collected_at).
func (s *Scheduler) SetNow(f func() time.Time) { s.now = f }

// Ticker is a public alias for the private ticker interface, for test fakes.
type Ticker = ticker

// NewManualTicker returns a ticker whose tick is triggered manually via
// Tick(). Replaces time-based waits in scheduler tests with a deterministic step.
func NewManualTicker() *ManualTicker {
	return &ManualTicker{c: make(chan time.Time, 1)}
}

// ManualTicker is a controllable test ticker.
type ManualTicker struct {
	c chan time.Time
}

func (m *ManualTicker) C() <-chan time.Time { return m.c }
func (m *ManualTicker) Stop()               {}

// Tick sends one tick (non-blocking — buffer of 1, extra ticks are dropped).
func (m *ManualTicker) Tick() {
	select {
	case m.c <- time.Now():
	default:
	}
}
