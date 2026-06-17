package herald

// Mini-reaper осиротевших job-ов доставки (ADR-052(d)): worker, клеймивший job
// (BRPOPLPUSH в processing-LIST), мог умереть до Ack/Requeue — job застрял в
// processing с истёкшим lease-ключом. Reaper периодически возвращает такие
// job-ы обратно в pending (at-least-once). Лёгкая фоновая горутина (одна на
// инстанс; конкурентные reaper-ы на N инстансов безопасны — перенос идемпотентен
// по LREM).

import (
	"context"
	"log/slog"
	"time"
)

// DefaultReaperInterval — период скана processing-LIST на осиротевшие job-ы.
// Реже leaseTTL (5m): осиротевший job ждёт максимум interval+leaseTTL до
// возврата — приемлемо для уведомлений (не критичный по latency путь).
const DefaultReaperInterval = time.Minute

// RunDeliveryReaper крутит mini-reaper до отмены ctx. На каждом тике зовёт
// RequeueExpired backend-а; число возвращённых job-ов логирует. Ошибка скана
// best-effort (логируется, не валит loop — следующий тик повторит).
func RunDeliveryReaper(ctx context.Context, queue queueBackend, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = DefaultReaperInterval
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := queue.RequeueExpired(ctx, jobIDFromPayload)
			if err != nil {
				logger.Warn("herald: delivery reaper scan failed", slog.Any("error", err))
				continue
			}
			if n > 0 {
				logger.Info("herald: requeued orphaned delivery jobs", slog.Int("count", n))
			}
		}
	}
}
