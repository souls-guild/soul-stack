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

// TestNextRun_Interval_NonUTC: from в локали → расчёт всё равно от UTC-стенки.
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
	// Каждые 6 часов от полуночи UTC: 00,06,12,18 → после 10:30 → 12:00 UTC.
	want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextRun = %v, want %v", got.UTC(), want)
	}
}

// TestNextRun_Cron_NonUTCFromNormalized: cron-момент считается в UTC даже если
// from в другой локали (ADR-046: cron в UTC).
func TestNextRun_Cron_NonUTCFromNormalized(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("0 0 * * *")} // ежедневно в полночь UTC
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
		t.Fatal("ожидалась ошибка для битого cron_expr")
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

// TestValidate_RejectsBrokenCron: битый cron отвергается validate (до PG).
func TestValidate_RejectsBrokenCron(t *testing.T) {
	t.Parallel()
	c := cronCadence()
	c.CronExpr = strptr("not valid")
	if err := validate(c); err == nil {
		t.Fatal("validate должна отвергнуть битый cron_expr")
	}
}

// TestValidate_AcceptsValidCron: корректный cron проходит validate.
func TestValidate_AcceptsValidCron(t *testing.T) {
	t.Parallel()
	c := cronCadence()
	c.CronExpr = strptr("*/15 * * * *")
	if err := validate(c); err != nil {
		t.Fatalf("validate валидного cron: %v", err)
	}
}

// --- NextRunAnchored: drift-free пересчёт от планового слота ---

// TestNextRunAnchored_Interval_GridAligned — стендовый регресс (ADR-046 §4).
// interval=10s, плановый слот scheduledFor=t, спавн произошёл на тике t+ε (ε
// мелкий дрейф тикера). Якорить надо к ПЛАНОВОМУ слоту, не к фактическому now:
// next = t+10s РОВНО, а не t+ε+10s. Старый NextRun(c, now) давал t+ε+10s → тикер,
// бьющий ровно по сетке 10s, видел t+ε+10s > t+10s и пропускал слот → 15s вместо
// 10s. Этот тест ловит регресс.
func TestNextRunAnchored_Interval_GridAligned(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(10)}
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	scheduledFor := t0
	now := t0.Add(2 * time.Millisecond) // тик пришёл с дрейфом ε
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := t0.Add(10 * time.Second) // grid-aligned, НЕ now+10s
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (grid-aligned, не now-anchored %v)",
			got, want, now.Add(10*time.Second))
	}
}

// TestNextRunAnchored_Interval_MissedSlot — downtime/missed-slot: плановый слот в
// далёком прошлом → результат РОВНО один = первый слот сетки строго > now (НЕ
// старый слот, НЕ N доспавнов). Loop в helper-е — арифметика grid-skip, не спавн:
// один catch-up за текущий due, дальше выровнено. Проверяем: будущий, grid-aligned
// к scheduledFor, единственный.
func TestNextRunAnchored_Interval_MissedSlot(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(600)} // 10 мин
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := scheduledFor.Add(time.Hour + 7*time.Minute) // 11:07 — 6 целых слотов прошло
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := time.Date(2026, 6, 1, 11, 10, 0, 0, time.UTC) // первый слот > 11:07, выровнен к :00
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (первый будущий grid-слот)", got, want)
	}
	if !got.After(now) {
		t.Errorf("NextRunAnchored = %v не строго > now %v", got, now)
	}
	// grid-aligned: разница (got - scheduledFor) кратна интервалу.
	if d := got.Sub(scheduledFor) % (600 * time.Second); d != 0 {
		t.Errorf("результат не grid-aligned к scheduledFor: остаток %v", d)
	}
}

// TestNextRunAnchored_Interval_BoundaryStrictlyAfter — граница СТРОГО > now: если
// плановый слот + interval == now ровно, шагнуть дальше (иначе тот же тик с тем же
// now снова сочтёт due → задвоение).
func TestNextRunAnchored_Interval_BoundaryStrictlyAfter(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindInterval, IntervalSeconds: intptr(10)}
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	scheduledFor := t0
	now := t0.Add(10 * time.Second) // scheduledFor+interval == now ровно
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := t0.Add(20 * time.Second) // шагнуть дальше: строго > now
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (строго > now при равенстве)", got, want)
	}
	if !got.After(now) {
		t.Errorf("результат %v не строго > now %v", got, now)
	}
}

// TestNextRunAnchored_Cron_Anchored — cron-аналог (a): anchored к плановому слоту,
// первый cron-момент строго после scheduledFor, когда now чуть позже слота.
func TestNextRunAnchored_Cron_Anchored(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("*/5 * * * *")} // каждые 5 мин
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

// TestNextRunAnchored_Cron_MissedSlot — cron-аналог (b): плановый слот в прошлом,
// now далеко → ровно один результат = первый cron-момент строго > now (loop
// итеративно сходится, не зацикливается, не доспавнивает каждый пропущенный).
func TestNextRunAnchored_Cron_MissedSlot(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("0 * * * *")} // ежечасно в :00
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC) // 4.5 часа простоя
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC) // первый :00 > 14:30
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (первый будущий cron-момент)", got, want)
	}
}

// TestNextRunAnchored_Cron_BoundaryStrictlyAfter — cron-аналог (c): если cron-момент
// от scheduledFor совпал с now ровно, шагнуть дальше (строго > now).
func TestNextRunAnchored_Cron_BoundaryStrictlyAfter(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("*/5 * * * *")}
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC) // ровно следующий cron-момент
	got, err := NextRunAnchored(c, scheduledFor, now)
	if err != nil {
		t.Fatalf("NextRunAnchored: %v", err)
	}
	want := time.Date(2026, 6, 1, 10, 10, 0, 0, time.UTC) // шагнуть дальше
	if !got.Equal(want) {
		t.Errorf("NextRunAnchored = %v, want %v (строго > now при равенстве)", got, want)
	}
}

// TestNextRunAnchored_BrokenCron — битый cron пробрасывает ошибку (parity NextRun).
func TestNextRunAnchored_BrokenCron(t *testing.T) {
	t.Parallel()
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("not a cron")}
	now := time.Now().UTC()
	if _, err := NextRunAnchored(c, now, now); err == nil {
		t.Fatal("ожидалась ошибка для битого cron_expr")
	}
}

// TestNextRunAnchored_Cron_NoFutureSlot — cron валиден синтаксически, но не имеет
// будущего слота в горизонте robfig (~5 лет): "0 0 30 2 *" = 30 февраля, которого
// никогда не бывает → Schedule.Next возвращает zero-time. Без IsZero-guard первый же
// NextRun(scheduledFor) дал бы zero, цикл `for !next.After(now)` крутился бы вечно под
// PG-lock (wedged conductor-tx, ADR-046 §4). Guard должен вернуть ОШИБКУ, не зависнуть.
func TestNextRunAnchored_Cron_NoFutureSlot(t *testing.T) {
	t.Parallel()
	// Sanity: выражение парсится валидно (zero приходит из Next, не из Parse).
	if _, err := ParseCron("0 0 30 2 *"); err != nil {
		t.Fatalf("0 0 30 2 * должно парситься валидно: %v", err)
	}
	c := &Cadence{ScheduleKind: ScheduleKindCron, CronExpr: strptr("0 0 30 2 *")}
	scheduledFor := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	now := scheduledFor.Add(time.Minute)

	// Сам тест не должен зависнуть: гоним в горутине с дедлайном.
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
			t.Fatalf("ожидалась ошибка для cron без будущего слота, получили %v", r.t)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NextRunAnchored зависла на cron без будущего слота (нет IsZero-guard)")
	}
}
