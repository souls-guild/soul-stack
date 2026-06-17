package beacon

import "time"

// SetTicker подменяет фабрику тикеров на детерминированную (тест-only). Возврат
// в production-форме не нужен — scheduler одноразовый в тесте.
func (s *Scheduler) SetTicker(f func(time.Duration) ticker) { s.newTicker = f }

// SetNow подменяет источник времени (для проверки PortentEvent.collected_at).
func (s *Scheduler) SetNow(f func() time.Time) { s.now = f }

// Ticker — публичный псевдоним приватного интерфейса ticker для тестовых fake-ов.
type Ticker = ticker

// NewManualTicker возвращает тикер, тик которого дёргается вручную через Tick().
// Заменяет time-based ожидание в тестах scheduler-а на детерминированный шаг.
func NewManualTicker() *ManualTicker {
	return &ManualTicker{c: make(chan time.Time, 1)}
}

// ManualTicker — управляемый тест-тикер.
type ManualTicker struct {
	c chan time.Time
}

func (m *ManualTicker) C() <-chan time.Time { return m.c }
func (m *ManualTicker) Stop()               {}

// Tick посылает один тик (неблокирующе — буфер 1, лишний тик игнорируется).
func (m *ManualTicker) Tick() {
	select {
	case m.c <- time.Now():
	default:
	}
}
