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

// Apply прогоняет все up-миграции из `fs` (sub-path `subdir`, например
// `"."`) поверх Postgres-а по `dsn`. Идемпотентно — если все миграции
// уже применены, возвращает nil (golang-migrate в этом случае отдаёт
// `migrate.ErrNoChange`, мы его глотаем).
//
// `migrate` использует свой собственный sql.DB (через registered driver
// `pgx5`), отдельный от `keeper/internal/pg.NewPool`. Это by design —
// migrate-tool требует exclusive lock через `pg_advisory_lock` на время
// прогона; смешивать pool и migrate-conn опасно (deadlock потенциал).
// Apply открывает свой conn, закрывает после Up.
func Apply(ctx context.Context, dsn string, fs embed.FS, subdir string) error {
	src, err := iofs.New(fs, subdir)
	if err != nil {
		return fmt.Errorf("migrate: iofs source: %w", err)
	}

	// golang-migrate ожидает URL вида `pgx5://...`. DSN может прийти как
	// `postgres://...` (стандарт) — заменяем scheme; ParseConfig pgx-а
	// уже принял такой DSN в NewPool.
	migrateURL, err := toMigrateURL(dsn)
	if err != nil {
		return err
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("migrate: new instance: %w", err)
	}
	defer m.Close() //nolint:errcheck // close errors не критичны после успешного Up

	// ctx-cancellation: golang-migrate/v4 API не принимает context;
	// единственный способ прервать долгий Up — сигнал на канал
	// m.GracefulStop (chan bool, signal-only). Goroutine ждёт ctx.Done и
	// шлёт сигнал; локальный done-канал глушит её после успешного Up,
	// чтобы не висеть на родительском ctx после возврата.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// non-blocking: GracefulStop — буферизованный chan bool(1),
			// повторный сигнал не нужен.
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

// toMigrateURL заменяет scheme `postgres://` / `postgresql://` на
// `pgx5://` (имя зарегистрированного database/pgx/v5 driver-а).
// Прочие схемы (включая keyvalue-формат) отвергаются — keeper.yml
// канонизирует URL-форму.
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
