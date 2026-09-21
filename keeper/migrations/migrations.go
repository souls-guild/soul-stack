// Package migrations -- embedded SQL migrations for Keeper. Used by
// `keeper/internal/migrate.Apply` (apply-on-startup in M0.4.2; ad-hoc
// smoke in M0.4.0).
//
// File names are `NNN_<slug>.up.sql` / `NNN_<slug>.down.sql` (`golang-migrate`
// format); apply order = sort-asc by `NNN`.
package migrations

import "embed"

// FS contains all `.sql` migrations next to this file. Passed to
// `migrate.Apply(ctx, pool, migrations.FS, ".")`.
//
//go:embed *.sql
var FS embed.FS
