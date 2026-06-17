package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Ping проверяет доступность Postgres-а через pool.Ping. Вызывается
// keeper-ом на старте после [NewPool] — если pg недоступен, бинарь
// падает рано, не доходя до open listener-ов.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pg: ping: %w", err)
	}
	return nil
}
