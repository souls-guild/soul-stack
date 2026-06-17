package clouddriver

import (
	"context"
	"time"
)

// BackoffConfig — параметры экспоненциального backoff для [Retry] и [WaitUntilReady].
// Общий канон для всех драйверов: единый шейп ретраев вместо per-provider
// диалектов. Нулевая структура НЕ валидна — используйте [DefaultBackoff].
type BackoffConfig struct {
	// Initial — задержка перед первым повтором.
	Initial time.Duration
	// Max — потолок задержки (экспонента упирается в него).
	Max time.Duration
	// Factor — множитель роста задержки между попытками (обычно 2.0).
	Factor float64
	// MaxAttempts — максимум попыток (включая первую). 0 → без лимита по
	// числу (ограничение только ctx-дедлайном); для [Retry] это means
	// «ретраить до успеха или ctx-cancel».
	MaxAttempts int
}

// DefaultBackoff — разумный дефолт для cloud API (throttling-friendly):
// 1s → 2s → 4s → … → 30s, до 8 попыток.
func DefaultBackoff() BackoffConfig {
	return BackoffConfig{
		Initial:     1 * time.Second,
		Max:         30 * time.Second,
		Factor:      2.0,
		MaxAttempts: 8,
	}
}

// next вычисляет задержку для попытки attempt (0-based: attempt=0 → Initial).
func (b BackoffConfig) next(attempt int) time.Duration {
	d := float64(b.Initial)
	for i := 0; i < attempt; i++ {
		d *= b.Factor
		if d >= float64(b.Max) {
			return b.Max
		}
	}
	if d > float64(b.Max) {
		return b.Max
	}
	return time.Duration(d)
}

// Retry выполняет op с экспоненциальным backoff, пока op возвращает ошибку,
// классифицируемую как transient (через [Classify]+classify). Не-transient
// ошибка возвращается немедленно (нет смысла ретраить auth/quota/not_found).
//
// Возвращает nil при первом успехе; последнюю ошибку — при исчерпании
// MaxAttempts; ctx.Err() — при отмене/таймауте во время ожидания backoff.
// Это общий канон для всех драйверов: idempotent-операции (DescribeImages,
// RunInstances при throttling) оборачиваются им единообразно.
func Retry(ctx context.Context, cfg BackoffConfig, classify ClassifyFunc, op func() error) error {
	attempt := 0
	for {
		err := op()
		if err == nil {
			return nil
		}
		if !Classify(classify, err).Transient() {
			return err
		}
		attempt++
		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			return err
		}
		if waitErr := sleepCtx(ctx, cfg.next(attempt-1)); waitErr != nil {
			return waitErr
		}
	}
}

// sleepCtx ждёт d либо ctx-cancel. Возвращает ctx.Err() при отмене, nil по
// истечении d. Единая точка ожидания для Retry/WaitUntilReady (ctx-aware,
// без утечки таймеров).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// Уважаем уже-отменённый ctx даже при нулевой задержке.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
