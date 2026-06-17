package audit

import (
	"context"
	"log/slog"
	"sync/atomic"
)

// Tap — пассивный наблюдатель audit-событий поверх primary-[Writer]-а
// (точка расширения ADR-022(f)). Получает КОПИЮ уже записанного в primary
// события ПОСЛЕ успешной primary-записи. Реализация tap-а обязана быть
// неблокирующей и best-effort: её ошибки не влияют на исход [Writer.Write]
// (см. [MultiWriter]). Конкретный notification-tap — keeper-side
// (keeper/internal/herald, ADR-052(c)).
//
// Семантика «событие уже в primary store» гарантирует, что tap не увидит
// и не просигналит наружу о незаписанном событии (порядок ADR-052(c):
// сначала audit-факт в PG, потом уведомление).
type Tap interface {
	// Observe принимает событие, гарантированно записанное в primary.
	// event — read-only для tap-а (общий указатель; tap не мутирует).
	// Реализация не должна блокировать вызывающего: тяжёлую/сетевую
	// работу — в собственную горутину/очередь. Ошибки tap проглатывает
	// сам (лог/метрика), наружу их не отдаёт.
	Observe(ctx context.Context, event *Event)
}

// MultiWriter оборачивает primary-[Writer] и набор [Tap]-ов. Контракт:
//
//   - primary-запись (PG) выполняется ПЕРВОЙ и её результат —
//     единственный, влияющий на исход [Write]. Ошибка primary возвращается
//     как есть, tap-ы при этом НЕ вызываются (нельзя сигналить наружу о
//     незаписанном событии).
//   - tap-ы вызываются строго ПОСЛЕ успешной primary-записи, в порядке
//     передачи. Любая ошибка/паника tap-а не фейлит [Write] (best-effort,
//     ADR-022(f)/ADR-052(c)).
//
// Неблокируемость и буферизация — ответственность конкретного [Tap]-а
// (см. herald.notificationTap: bounded-канал + drop-счётчик). MultiWriter
// лишь гарантирует порядок и изоляцию ошибок.
type MultiWriter struct {
	primary Writer
	taps    []Tap
	logger  *slog.Logger

	// tapPanics — счётчик паник, перехваченных при вызове Tap.Observe.
	// Паника tap-а — programmer error, но не должна валить audit write-path;
	// атомарный счётчик доступен тестам/диагностике.
	tapPanics atomic.Uint64
}

// NewMultiWriter оборачивает primary в декоратор с tap-ами. nil-tap-ы
// отбрасываются. При пустом наборе tap-ов возвращается primary как есть —
// декоратор без наблюдателей бессмыслен (нулевой оверхед на write-path).
// logger — для лога паник tap-а; nil допустим (паники молча считаются).
func NewMultiWriter(primary Writer, logger *slog.Logger, taps ...Tap) Writer {
	live := make([]Tap, 0, len(taps))
	for _, t := range taps {
		if t != nil {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return primary
	}
	return &MultiWriter{primary: primary, taps: live, logger: logger}
}

// Write записывает событие в primary; при успехе раздаёт его tap-ам.
func (m *MultiWriter) Write(ctx context.Context, event *Event) error {
	if err := m.primary.Write(ctx, event); err != nil {
		return err
	}
	for _, t := range m.taps {
		m.observe(ctx, t, event)
	}
	return nil
}

// observe вызывает tap с recover-барьером: паника tap-а считается и
// логируется, но не валит write-path.
func (m *MultiWriter) observe(ctx context.Context, t Tap, event *Event) {
	defer func() {
		if r := recover(); r != nil {
			m.tapPanics.Add(1)
			if m.logger != nil {
				m.logger.Error("audit: tap panic recovered",
					slog.String("event_type", string(event.EventType)),
					slog.Any("panic", r))
			}
		}
	}()
	t.Observe(ctx, event)
}
