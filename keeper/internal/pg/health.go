package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Ping checks Postgres availability via pool.Ping. Called by keeper on startup
// after [NewPool] — if pg is unavailable, the binary fails early before reaching
// open listeners.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pg: ping: %w", err)
	}
	return nil
}
