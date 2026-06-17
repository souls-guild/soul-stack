// Package pgutil — мелкие keeper-side helper-ы для работы с Postgres, общие для
// нескольких CRUD-слоёв (applyrun / oracle / …).
package pgutil

import (
	"fmt"
	"time"
)

// Interval форматирует Go-длительность как PG-interval-литерал в секундах,
// пригодный как text-аргумент к параметру `$N::interval`. Единый источник для
// applyrun (Ward-claim lease) и oracle (circuit-breaker window) — раньше каждый
// держал свою копию.
func Interval(d time.Duration) string {
	return fmt.Sprintf("%f seconds", d.Seconds())
}
