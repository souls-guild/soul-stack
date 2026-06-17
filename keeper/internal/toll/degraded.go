package toll

import (
	"context"
	"time"
)

// degradedWriter — узкая поверхность мутации cluster:degraded флага, нужная
// Leader-у. Сужение делает Leader тестируемым без живого Redis-а (см.
// leader_test.go::fakeDegradedWriter). Daemon оборачивает
// [keeperredis.TollSetDegraded] / [keeperredis.TollClearDegraded] в реализацию.
type degradedWriter interface {
	// SetDegraded — выставляет ключ cluster:degraded со значением holder (KID
	// leader-а) и TTL. Использует SET без NX — на каждый тик leader освежает
	// TTL (re-arm). Возврат err — Redis-проблема.
	SetDegraded(ctx context.Context, holder string, ttl time.Duration) error
	// ClearDegraded — DEL cluster:degraded. Idempotent (DEL на отсутствующий
	// ключ — no-op).
	ClearDegraded(ctx context.Context) error
}
