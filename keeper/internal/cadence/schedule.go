package cadence

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser — стандартный 5-полевой cron-парсер (minute hour dom month dow), без
// поля секунд и без descriptor-ов (@every/@daily). ADR-046 §3 фиксирует
// «стандартный 5-полевой cron». Парсер stateless и потокобезопасен — держим один
// package-level экземпляр (parity подхода robfig: ParseStandard под капотом — тот
// же набор опций).
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// ParseCron разбирает 5-полевое cron-выражение в [cron.Schedule]. Используется
// валидацией (отвергнуть битый cron до PG) и [NextRun] (пересчёт next_run_at).
// Все расчёты ведутся в UTC (ADR-046: timezone cron — UTC, tz-aware — отложено
// post-MVP); вызывающий обязан передавать UTC-время в Schedule.Next.
func ParseCron(expr string) (cron.Schedule, error) {
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("cadence: invalid cron_expr %q: %w", expr, err)
	}
	return sched, nil
}

// NextRun — low-level примитив: один шаг расписания от точки `from`. Базис для
// [NextRunAnchored] (drift-free пересчёт next_run_at) и для валидации. Спавн-путь
// (Conductor) использует НЕ NextRun напрямую, а [NextRunAnchored] (anchored к
// плановому слоту, ADR-046 §4) — иначе дрейф тика накапливался бы в next_run_at.
//
//   - ScheduleKindInterval → from + interval_seconds (ровно один период от from).
//   - ScheduleKindCron → следующий cron-момент строго после `from`, в UTC.
//
// from нормализуется к UTC (cron-парсер чувствителен к локали времени; ADR-046
// фиксирует UTC). Возвращает ошибку только для cron с битым/непарсящимся
// cron_expr — на практике validate отвергает такой Cadence ещё до Insert/Update,
// поэтому в spawn-пути ошибка означала бы рассинхрон БД и валидации.
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

// NextRunAnchored вычисляет следующий слот спавна, anchored к ПЛАНОВОМУ моменту
// (scheduledFor — `next_run_at` до пересчёта), а не к фактическому `now`, и
// возвращает первый слот сетки СТРОГО `> now`. Drift-free пересчёт ADR-046 §4:
// дрейф тикера (тик приходит на now ≈ scheduledFor+ε) не накапливается в
// next_run_at, поэтому сетка слотов остаётся выровненной и тикер не промахивается
// мимо неё.
//
//	interval: next = scheduledFor + interval; затем грид-скип пропущенных слотов
//	          (for next <= now { next += interval }) — после downtime один catch-up
//	          спавн за текущий due, дальше выровнено. Loop — арифметика, НЕ спавн на
//	          каждый пропущенный слот (anti-storm).
//	cron:     next = first cron-момент после scheduledFor; затем итеративно
//	          (for next <= now { next = NextRun(c, next) }) до первого будущего —
//	          монотонно сходится.
//
// Граница СТРОГО `> now` (не `>=`): если after-loop next == now, тот же тик с тем
// же now на следующей итерации scheduler-а снова счёл бы строку due → задвоение.
// Поэтому условие loop-а `next <= now` гарантированно выталкивает результат за now.
//
// Построено ПОВЕРХ low-level [NextRun] (один шаг от точки) — без дублирования
// cron-парсинга. Ошибку возвращает только при битом cron_expr (как [NextRun]).
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
		// robfig Schedule.Next возвращает zero-time, если будущего слота нет в его
		// горизонте (~5 лет): напр. "0 0 30 2 *" (30 февраля). Тогда next=zero,
		// !next.After(now) навсегда true, NextRun(c, zero) снова zero → бесконечный
		// цикл под PG-lock (заклинит conductor-tx). Отсекаем zero как ошибку.
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
