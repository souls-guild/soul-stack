// Package cadence — реестр Cadence-расписаний в Postgres (таблица `cadences`,
// миграция 066) под ADR-046.
//
// Cadence — расписание, которое по времени спавнит обычный Voyage-прогон. Cadence
// живёт независимо от прогонов: по наступлении времени спавнит НОВЫЙ Voyage
// (Insert в `voyages`/`voyage_targets` с back-link `cadence_id`), Voyage-инвариант
// «один Voyage = один прогон» сохранён (новые строки, не воскрешение). Отношение
// «родитель Cadence → дети Voyage» (one-to-many). Cadence хранит «рецепт» прогона
// (то же множество, что VoyageCreateRequest, ADR-043) + правило повторения
// ([ScheduleKind] interval XOR cron) + политику наложения ([OverlapPolicy]).
//
// Package scope (S1): read/write CRUD против PG + валидация. Scheduler-trigger
// (Reaper-правило spawn_due_cadence, action `spawn`), пересчёт next_run_at
// (interval/cron) и overlap_policy-исполнение — S2/S3, живут отдельно. API
// `/v1/cadences` — S4, UI — S5.
//
// Стиль — parity `keeper/internal/voyage` (тот же CRUD-паттерн insertSQL/
// selectColumns/scan, sentinel-ошибки, enum-константы + Valid-проверки).
package cadence

import (
	"encoding/json"
	"errors"
	"time"
)

// ScheduleKind — вид правила повторения. Closed enum, совпадает с CHECK
// `cadences_schedule_kind_valid` миграции 066.
type ScheduleKind string

const (
	// ScheduleKindInterval — повторение каждые N секунд. Требует непустой
	// IntervalSeconds, CronExpr должен быть пуст (CHECK
	// `cadences_schedule_consistency`).
	ScheduleKindInterval ScheduleKind = "interval"
	// ScheduleKindCron — повторение по стандартному 5-полевому cron-выражению.
	// Требует непустой CronExpr, IntervalSeconds должен быть пуст. Парсинг cron и
	// пересчёт next_run_at — S2 (на S1 строка хранится as-is).
	ScheduleKindCron ScheduleKind = "cron"
)

// OverlapPolicy — поведение, когда время следующего спавна наступило, а
// предыдущий порождённый Voyage ещё не терминален. Closed enum, совпадает с
// CHECK `cadences_overlap_policy_valid` миграции 066. Исполнение политик — S3.
type OverlapPolicy string

const (
	// OverlapPolicySkip — спавн пропускается (next_run_at всё равно
	// пересчитывается); защита от накопления при периоде короче длительности
	// прогона.
	OverlapPolicySkip OverlapPolicy = "skip"
	// OverlapPolicyQueue — спавн откладывается до терминала предыдущего ребёнка
	// (строгая последовательность без потери запусков).
	OverlapPolicyQueue OverlapPolicy = "queue"
	// OverlapPolicyParallel — спавн происходит независимо от состояния предыдущих
	// (наложенные прогоны допустимы; лимит — на Voyage/Acolyte-слое).
	OverlapPolicyParallel OverlapPolicy = "parallel"
)

// Kind — режим спавнимого Voyage-прогона. Совпадает по значениям с voyage.Kind /
// CHECK `cadences_kind_valid` миграции 066. Дублируется здесь (а не импортируется
// из voyage), чтобы cadence-валидация не зависела от voyage-пакета: на S1 рецепт
// проверяется локально, спавн (voyage.Insert) — забота scheduler-а S2.
type Kind string

const (
	// KindScenario — спавнить Voyage применения named scenario к набору
	// инкарнаций. Требует непустой ScenarioName.
	KindScenario Kind = "scenario"
	// KindCommand — спавнить Voyage выполнения whitelisted-модуля на наборе
	// хостов. Требует непустой Module.
	KindCommand Kind = "command"
)

// BatchMode — режим батчинга рецепта (parity voyage.BatchMode). Closed enum,
// совпадает с CHECK `cadences_batch_mode_valid` миграции 066. NULL ⇒ barrier
// (резолв оркестратором при спавне, S2).
type BatchMode string

const (
	// BatchModeBarrier — последовательные Leg-и с барьером между пачками.
	BatchModeBarrier BatchMode = "barrier"
	// BatchModeWindow — полное скользящее окно (ширина = concurrency).
	BatchModeWindow BatchMode = "window"
)

// OnFailure — политика перехода при провале (parity voyage.OnFailure). Совпадает
// с CHECK `cadences_on_failure_valid` миграции 066.
type OnFailure string

const (
	// OnFailureContinue — провал учитывается, остальные Leg-и продолжают.
	OnFailureContinue OnFailure = "continue"
	// OnFailureAbort — провал → остановка перехода к следующему Leg.
	OnFailureAbort OnFailure = "abort"
)

// Cadence — runtime-представление строки `cadences`. Полная проекция: рецепт
// прогона + правило повторения + политика наложения + расчётные тайминги.
//
// Nullable-поля рецепта — указатели/raw-bytes (резолвятся оркестратором при
// спавне, не CRUD-ом). IntervalSeconds/CronExpr взаимоисключающи по ScheduleKind
// (CHECK `cadences_schedule_consistency`); ScenarioName/Module — по Kind (CHECK
// `cadences_kind_payload_consistency`).
type Cadence struct {
	ID      string
	Name    string
	Enabled bool

	// Правило повторения.
	ScheduleKind    ScheduleKind
	IntervalSeconds *int    // для ScheduleKindInterval; nil для cron.
	CronExpr        *string // для ScheduleKindCron; nil для interval.
	OverlapPolicy   OverlapPolicy

	// Рецепт прогона (то же множество, что VoyageCreateRequest, ADR-043).
	Kind          Kind
	ScenarioName  *string
	Module        *string
	Target        json.RawMessage // выбор из RBAC-скоупа создателя (NOT NULL)
	Input         []byte          // jsonb (caller сериализует input один раз)
	BatchMode     *BatchMode      // nil ⇒ barrier (резолв при спавне).
	BatchSize     *int            // XOR с BatchPercent (handler-инвариант).
	BatchPercent  *int            // % от scope; nil ⇒ задан BatchSize / весь прогон одним Leg.
	Concurrency   *int
	FailThreshold *int // порог абсолютного числа провалов → стоп; nil ⇒ без порога.
	// FailThresholdPercent — порог провалов в процентах от spawn-scope (XOR с
	// FailThreshold, handler-инвариант). Хранится колонкой (в отличие от Voyage, где
	// max_failures="N%" резолвится в абсолют на create-scope) потому, что у Cadence
	// scope неизвестен на создании — резолвится при спавне на len(resolved) в
	// BuildVoyage, симметрично BatchPercent. nil ⇒ задан FailThreshold / без порога.
	FailThresholdPercent *int
	InterBatchInterval   *time.Duration // пауза между Leg-ами (barrier).
	InterUnitInterval    *time.Duration // per-unit пауза в window.
	RequireAlive         *bool          // presence-фильтр живых на резолве scope; nil ⇒ false.
	OnFailure            *OnFailure

	// Расчётные тайминги (scheduler — S2; на S1 CRUD пишет/читает as-is).
	NextRunAt *time.Time
	LastRunAt *time.Time

	CreatedByAID string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Sentinel-ошибки CRUD-слоя (parity voyage).
//   - ErrCadenceNotFound — нет строки по запрошенному id.
//   - ErrCadenceExists   — UNIQUE по PK (id): повторный Insert одного ULID
//     (программная ошибка caller-а).
var (
	ErrCadenceNotFound = errors.New("cadence: not found")
	ErrCadenceExists   = errors.New("cadence: id already exists")
)

// ValidScheduleKind сообщает, входит ли вид в [ScheduleKind]-enum.
func ValidScheduleKind(k ScheduleKind) bool {
	switch k {
	case ScheduleKindInterval, ScheduleKindCron:
		return true
	}
	return false
}

// ValidOverlapPolicy сообщает, входит ли политика в [OverlapPolicy]-enum.
func ValidOverlapPolicy(p OverlapPolicy) bool {
	switch p {
	case OverlapPolicySkip, OverlapPolicyQueue, OverlapPolicyParallel:
		return true
	}
	return false
}

// ValidKind сообщает, входит ли kind в [Kind]-enum.
func ValidKind(k Kind) bool {
	switch k {
	case KindScenario, KindCommand:
		return true
	}
	return false
}

// ValidBatchMode сообщает, входит ли режим в [BatchMode]-enum.
func ValidBatchMode(m BatchMode) bool {
	switch m {
	case BatchModeBarrier, BatchModeWindow:
		return true
	}
	return false
}

// ValidOnFailure — парная проверка для [OnFailure].
func ValidOnFailure(p OnFailure) bool {
	switch p {
	case OnFailureContinue, OnFailureAbort:
		return true
	}
	return false
}
