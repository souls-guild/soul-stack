// Package migrations — embedded SQL-миграций Keeper-а. Используется
// `keeper/internal/migrate.Apply` (apply-on-startup в M0.4.2; ad-hoc
// smoke в M0.4.0).
//
// Имена файлов — `NNN_<slug>.up.sql` / `NNN_<slug>.down.sql` (формат
// `golang-migrate`); порядок применения = sort-asc по `NNN`.
package migrations

import "embed"

// FS содержит все `.sql`-миграции рядом с этим файлом. Передаётся в
// `migrate.Apply(ctx, pool, migrations.FS, ".")`.
//
//go:embed *.sql
var FS embed.FS
