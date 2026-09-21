// Package pgutil contains small keeper-side helpers for working with Postgres,
// shared by several CRUD layers (applyrun / oracle / ...).
package pgutil

import (
	"fmt"
	"time"
)

// Interval formats a Go duration as a PG interval literal in seconds, suitable
// as the text argument for a `$N::interval` parameter. Single source for
// applyrun (Ward-claim lease) and oracle (circuit-breaker window); previously
// each kept its own copy.
func Interval(d time.Duration) string {
	return fmt.Sprintf("%f seconds", d.Seconds())
}
