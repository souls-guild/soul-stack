package cadence

import (
	"testing"
	"time"
)

func TestNextRun_Interval(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(300)}
	from := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	got, err := NextRun(c, from)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := from.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Errorf("NextRun = %v, want %v", got, want)
	}
}

// TestNextRun_Interval_NonUTC: from in a locale → the calculation still runs off the UTC wall-clock.
func TestNextRun_Interval_NonUTC(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(60)}
	loc := time.FixedZone("MSK", 3*3600)
	from := time.Date(2026, 6, 1, 13, 0, 0, 0, loc) // = 10:00 UTC
	got, err := NextRun(c, from)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 6, 1, 10, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextRun = %v (UTC %v), want %v", got, got.UTC(), want)
	}
}

func TestNextRun_Cron_UTC(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("0 */6 * * *")}
	from := time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC)
	got, err := NextRun(c, from)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	// Every 6 hours from midnight UTC: 00,06,12,18 → after 10:30 → 12:00 UTC.
	want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextRun = %v, want %v", got.UTC(), want)
	}
}

// TestNextRun_Cron_NonUTCFromNormalized: the cron moment is computed in UTC
// even if from is in another locale (ADR-046: cron is UTC).
func TestNextRun_Cron_NonUTCFromNormalized(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("0 0 * * *")} // daily at midnight UTC
	loc := time.FixedZone("MSK", 3*3600)
	from := time.Date(2026, 6, 1, 23, 0, 0, 0, loc) // = 20:00 UTC
	got, err := NextRun(c, from)
	if err != nil {
		t.Fatalf("NextRun: %v", err)
	}
	want := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextRun = %v, want %v", got.UTC(), want)
	}
}

func TestNextRun_Cron_BrokenExpr(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("not a cron")}
	if _, err := NextRun(c, time.Now()); err == nil {
		t.Fatal("expected error for broken cron_expr")
	}
}

func TestParseCron_ValidAndInvalid(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"0 */6 * * *", "*/5 * * * *", "0 0 1 1 *", "0 9-17 * * 1-5"} {
		if _, err := ParseCron(ok); err != nil {
			t.Errorf("ParseCron(%q) = err %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "garbage", "* * * *", "60 * * * *", "@every 5m"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("ParseCron(%q) = nil err, want error", bad)
		}
	}
}

// TestValidate_RejectsBrokenCron: a broken cron is rejected by validate (ahead of PG).
func TestValidate_RejectsBrokenCron(t *testing.T) {
	t.Parallel()
	c := cronCadence()
	c.CronExpr = strptr("not valid")
	if err := validate(c); err == nil {
		t.Fatal("validate must reject broken cron_expr")
	}
}

// TestValidate_AcceptsValidCron: a valid cron passes validate.
func TestValidate_AcceptsValidCron(t *testing.T) {
	t.Parallel()
	c := cronCadence()
	c.CronExpr = strptr("*/15 * * * *")
	if err := validate(c); err != nil {
		t.Fatalf("validate valid cron: %v", err)
	}
}

// --- NextRunAnchored: drift-free recompute from the planned slot ---

// TestNextRunAnchored_Interval_GridAligned is a stand regression test (ADR-046 §4).
// interval=10s, planned slot scheduledFor=t, spawn happened on tick t+ε (ε a
// small ticker drift). Must anchor to the PLANNED slot, not the actual now:
// next = t+10s EXACTLY, not t+ε+10s. The old NextRun(c, now) gave t+ε+10s → a
// ticker hitting exactly on the 10s grid would see t+ε+10s > t+10s and skip a
// slot → 15s instead of 10s. This test catches that regression.
func TestNextRunAnchored_Interval_GridAligned(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(10)}
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	scheduledFor := t0
	now := t0.Add(2 * time.Millisecond) // tick arrived with drift ε
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := t0.Add(10 * time.Second) // grid-aligned, NOT now+10s
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (grid-aligned, not now-anchored %v)",
			got, want, now.Add(10*time.Second))
	}
}

// TestNextRunAnchored_Interval_MissedSlot — downtime/missed-slot: the planned
// slot is far in the past → the result is EXACTLY one = the first grid slot
// strictly > now (NOT the old slot, NOT N catch-up spawns). The loop in the
// helper is grid-skip arithmetic, not a spawn: one catch-up for the current
// due slot, then aligned. We verify: future, grid-aligned to scheduledFor,
// singular.
func TestNextRunAnchored_Interval_MissedSlot(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(600)} // 10 min
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := scheduledFor.Add(time.Hour + 7*time.Minute) // 11:07 — 6 whole slots have passed
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := time.Date(2026, 6, 1, 11, 10, 0, 0, time.UTC) // first slot > 11:07, aligned to :00
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (first future grid slot)", got, want)
	}
	if !got.After(now) {
		t.Errorf("NextRunAnchored = %v is not strictly > now %v", got, now)
	}
	// grid-aligned: the difference (got - scheduledFor) is a multiple of the interval.
	if d := got.Sub(scheduledFor) % (600 * time.Second); d != 0 {
		t.Errorf("result is not grid-aligned to scheduledFor: remainder %v", d)
	}
}

// TestNextRunAnchored_Interval_BoundaryStrictlyAfter — boundary STRICTLY > now:
// if planned slot + interval == now exactly, step further (otherwise the same
// tick with the same now would again consider it due → double-spawn).
func TestNextRunAnchored_Interval_BoundaryStrictlyAfter(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(10)}
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	scheduledFor := t0
	now := t0.Add(10 * time.Second) // scheduledFor+interval == now exactly
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := t0.Add(20 * time.Second) // step further: strictly > now
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (strictly > now on equality)", got, want)
	}
	if !got.After(now) {
		t.Errorf("result %v is not strictly > now %v", got, now)
	}
}

// TestNextRunAnchored_Cron_Anchored — cron analog (a): anchored to the planned
// slot, the first cron moment strictly after scheduledFor, when now is
// slightly past the slot.
func TestNextRunAnchored_Cron_Anchored(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("*/5 * * * *")} // every 5 min
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := scheduledFor.Add(2 * time.Millisecond)
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v", got, want)
	}
}

// TestNextRunAnchored_Cron_MissedSlot — cron analog (b): the planned slot is in
// the past, now is far away → exactly one result = the first cron moment
// strictly > now (the loop converges iteratively, doesn't loop forever,
// doesn't catch-up spawn every missed slot).
func TestNextRunAnchored_Cron_MissedSlot(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("0 * * * *")} // hourly at :00
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC) // 4.5 hours of downtime
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC) // first :00 > 14:30
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (first future cron moment)", got, want)
	}
}

// TestNextRunAnchored_Cron_BoundaryStrictlyAfter — cron analog (c): if the
// cron moment from scheduledFor coincides with now exactly, step further
// (strictly > now).
func TestNextRunAnchored_Cron_BoundaryStrictlyAfter(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("*/5 * * * *")}
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC) // exactly the next cron moment
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := time.Date(2026, 6, 1, 10, 10, 0, 0, time.UTC) // step further
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (strictly > now on equality)", got, want)
	}
}

// TestNextRunAnchored_BrokenCron — a broken cron propagates an error (parity NextRun).
func TestNextRunAnchored_BrokenCron(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("not a cron")}
	now := time.Now().UTC()
	if _, err := NextRunAnchored(c, now, now); err == nil {
		t.Fatal("expected error for broken cron_expr")
	}
}

// TestNextRunAnchored_Cron_NoFutureSlot — the cron is syntactically valid but
// has no future slot within robfig's horizon (~5 years): "0 0 30 2 *" =
// February 30, which never occurs → Schedule.Next returns zero-time. Without
// the IsZero guard, the very first NextRun(scheduledFor) call would give zero,
// and the `for !next.After(now)` loop would spin forever under the PG lock
// (wedged conductor-tx, ADR-046 §4). The guard must return an ERROR, not hang.
func TestNextRunAnchored_Cron_NoFutureSlot(t *testing.T) {
	t.Parallel()
	// Sanity: the expression parses validly (zero comes from Next, not from Parse).
	if _, err := ParseCron("0 0 30 2 *"); err != nil {
		t.Fatalf("0 0 30 2 * must parse as valid: %v", err)
	}
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("0 0 30 2 *")}
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := scheduledFor.Add(time.Minute)

	// The test itself must not hang: run it in a goroutine with a deadline.
	type res struct {
		t   time.Time
		err error
	}
	done := make(chan res, 1)
	go func() {
		got, err := NextRunAnchored(c, scheduledFor, now)
		done <- res{got, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("expected error for cron without a future slot, got %v", r.t)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NextRunAnchored hung on cron without a future slot (missing IsZero guard)")
	}
}
