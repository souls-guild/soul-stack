package cadence

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser is the standard 5-field cron parser (minute hour dom month dow),
// without a seconds field and without descriptors (@every/@daily). ADR-046 §3
// fixes "standard 5-field cron". The parser is stateless and thread-safe — we
// keep one package-level instance (parity with robfig's approach:
// ParseStandard uses the same option set under the hood).
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// ParseCron parses a 5-field cron expression into a [cron.Schedule]. Used by
// validation (reject a broken cron ahead of PG) and by [NextRun] (recompute
// next_run_at). All calculations run in UTC (ADR-046: cron timezone is UTC,
// tz-aware is deferred post-MVP); the caller must pass UTC time to Schedule.Next.
func ParseCron(expr string) (cron.Schedule, error) {
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("cadence: invalid cron_expr %q: %w", expr, err)
	}
	return sched, nil
}

// NextRun is a low-level primitive: a single schedule step from point `from`.
// The basis for [NextRunAnchored] (drift-free recompute of next_run_at) and for
// validation. The spawn path (Conductor) does NOT use NextRun directly, but
// [NextRunAnchored] (anchored to the planned slot, ADR-046 §4) — otherwise
// ticker drift would accumulate in next_run_at.
//
//   - ScheduleKindInterval → from + interval_seconds (exactly one period from from).
//   - ScheduleKindCron → the next cron moment strictly after `from`, in UTC.
//
// from is normalized to UTC (the cron parser is sensitive to time locale;
// ADR-046 fixes UTC). Returns an error only for cron with a broken/unparsable
// cron_expr — in practice validate rejects such a Cadence before Insert/Update,
// so on the spawn path an error would mean a DB/validation desync.
func NextRun(c *Cadence, from time.Time) (time.Time, error) {
	from = from.UTC()
	switch c.ScheduleKind {
	case ScheduleKindInterval:
		if c.IntervalSeconds == nil || *c.IntervalSeconds <= 0 {
			return time.Time{}, fmt.Errorf("cadence: interval_seconds must be > 0 for next_run")
		}
		return from.Add(time.Duration(*c.IntervalSeconds) * time.Second), nil
	case ScheduleKindCron:
		if c.CronExpr == nil || *c.CronExpr == "" {
			return time.Time{}, fmt.Errorf("cadence: empty cron_expr for next_run")
		}
		sched, err := ParseCron(*c.CronExpr)
		if err != nil {
			return time.Time{}, err
		}
		return sched.Next(from), nil
	default:
		return time.Time{}, fmt.Errorf("cadence: invalid schedule_kind %q for next_run", c.ScheduleKind)
	}
}

// NextRunAnchored computes the next spawn slot, anchored to the PLANNED
// moment (scheduledFor — `next_run_at` before recompute) rather than the
// actual `now`, and returns the first grid slot STRICTLY `> now`. Drift-free
// recompute per ADR-046 §4: ticker drift (a tick arrives at now ≈
// scheduledFor+ε) doesn't accumulate in next_run_at, so the slot grid stays
// aligned and the ticker doesn't drift past it.
//
//	interval: next = scheduledFor + interval; then grid-skip past missed slots
//	          (for next <= now { next += interval }) — after downtime, one
//	          catch-up spawn for the current due slot, then aligned. The loop is
//	          arithmetic, NOT a spawn for every missed slot (anti-storm).
//	cron:     next = first cron moment after scheduledFor; then iteratively
//	          (for next <= now { next = NextRun(c, next) }) until the first
//	          future slot — converges monotonically.
//
// The boundary is STRICTLY `> now` (not `>=`): if after the loop next == now,
// the same tick with the same now on the scheduler's next iteration would
// again consider the row due → double-spawn. So the loop condition
// `next <= now` guarantees the result is pushed past now.
//
// Built ON TOP OF the low-level [NextRun] (a single step from a point) —
// without duplicating cron parsing. Returns an error only for a broken
// cron_expr (like [NextRun]).
func NextRunAnchored(c *Cadence, scheduledFor, now time.Time) (time.Time, error) {
	scheduledFor = scheduledFor.UTC()
	now = now.UTC()

	switch c.ScheduleKind {
	case ScheduleKindInterval:
		if c.IntervalSeconds == nil || *c.IntervalSeconds <= 0 {
			return time.Time{}, fmt.Errorf("cadence: interval_seconds must be > 0 for next_run")
		}
		interval := time.Duration(*c.IntervalSeconds) * time.Second
		next := scheduledFor.Add(interval)
		for !next.After(now) { // next <= now
			next = next.Add(interval)
		}
		return next, nil
	case ScheduleKindCron:
		next, err := NextRun(c, scheduledFor)
		if err != nil {
			return time.Time{}, err
		}
		// robfig Schedule.Next returns zero-time if there's no future slot within
		// its horizon (~5 years): e.g. "0 0 30 2 *" (February 30). Then next=zero,
		// !next.After(now) is forever true, NextRun(c, zero) is zero again → an
		// infinite loop under the PG lock (wedges the conductor-tx). We reject
		// zero as an error.
		if next.IsZero() {
			return time.Time{}, fmt.Errorf("cadence: cron %q не даёт будущего слота", *c.CronExpr)
		}
		for !next.After(now) { // next <= now
			next, err = NextRun(c, next)
			if err != nil {
				return time.Time{}, err
			}
			if next.IsZero() {
				return time.Time{}, fmt.Errorf("cadence: cron %q не даёт будущего слота", *c.CronExpr)
			}
		}
		return next, nil
	default:
		return time.Time{}, fmt.Errorf("cadence: invalid schedule_kind %q for next_run", c.ScheduleKind)
	}
}
