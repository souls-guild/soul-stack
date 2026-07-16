// Package migrate applies embedded SQL migrations from `keeper/migrations/`
// against the Keeper's Postgres. Integrated into `keeper/cmd/keeper/main.go`
// (apply-on-startup) since M0.4.2; in M0.4.0 this package was invoked
// ad-hoc (smoke test).
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers driver "pgx5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Apply runs all up migrations from `fs` (sub-path `subdir`, e.g. `"."`) over
// Postgres by `dsn`. It is idempotent: if all migrations are already applied,
// it returns nil (golang-migrate returns `migrate.ErrNoChange` in that case,
// and we swallow it).
//
// `migrate` uses its own sql.DB (through registered driver `pgx5`), separate
// from `keeper/internal/pg.NewPool`. This is by design: migrate tool requires
// an exclusive lock through `pg_advisory_lock` while running; mixing pool and
// migrate connection is dangerous (deadlock potential). Apply opens its own
// conn and closes it after Up.
func Apply(ctx context.Context, dsn string, fs embed.FS, subdir string) error {
	src, err := iofs.New(fs, subdir)
	if err != nil {
		return fmt.Errorf("migrate: iofs source: %w", err)
	}

	// golang-migrate expects URL like `pgx5://...`. DSN may arrive as
	// `postgres://...` (standard), so replace scheme; pgx ParseConfig already
	// accepted such DSN in NewPool.
	migrateURL, err := toMigrateURL(dsn)
	if err != nil {
		return err
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("migrate: new instance: %w", err)
	}
	defer m.Close() //nolint:errcheck // close errors are not critical after successful Up

	// ctx-cancellation: golang-migrate/v4 API does not accept context; the only
	// way to interrupt a long Up is a signal to m.GracefulStop (chan bool,
	// signal-only). Goroutine waits for ctx.Done and sends signal; local done
	// channel silences it after successful Up so it does not hang on parent ctx
	// after return.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// non-blocking: GracefulStop is buffered chan bool(1), repeat signal
			// is not needed.
			select {
			case m.GracefulStop <- true:
			default:
			}
		case <-done:
		}
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate: up: %w", err)
	}
	return nil
}

// toMigrateURL replaces scheme `postgres://` / `postgresql://` with
// `pgx5://` (name of registered database/pgx/v5 driver). Other schemes
// (including keyvalue format) are rejected; keeper.yml canonicalizes URL form.
func toMigrateURL(dsn string) (string, error) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://"), nil
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://"), nil
	case strings.HasPrefix(dsn, "pgx5://"):
		return dsn, nil
	default:
		return "", fmt.Errorf("migrate: dsn must be postgres://... or postgresql://...: %q", dsn)
	}
}
