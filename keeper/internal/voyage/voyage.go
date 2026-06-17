// Package voyage — реестр Voyage-прогонов в Postgres (таблицы `voyages` +
// `voyage_targets`, миграция 059) под ADR-043.
//
// Voyage — унифицированный батчевый прогон, поглощающий Tide (kind=scenario) +
// ErrandRun (kind=command). Дискриминатор [Kind]:
//
//   - [KindScenario] — применить named scenario к набору ИНКАРНАЦИЙ
//     (поглощает Tide + classic scenario-run); batch (Leg) = N инкарнаций,
//     per-incarnation state-commit (B1, ADR-043 пункт 3).
//   - [KindCommand] — выполнить whitelisted-модуль на наборе ХОСТОВ (поглощает
//     ErrandRun); batch (Leg) = N хостов, `incarnation.state` не трогается.
//
// Единица батча — Leg (один «отрезок пути»), идентифицируется
// [VoyageTarget.BatchIndex]. Per-target прогресс хранится в `voyage_targets`.
//
// Failover-resilient через PG-based claim+lease (parity Tide / ErrandRun,
// ADR-027(d)): pending → claimed_by_kid + claim_expires_at → running; протухший
// claim возвращается Reaper-правилом обратно в pending для пере-claim другим
// Keeper-инстансом (тираж — пост-S1).
//
// Package scope: read/write CRUD + claim/lease/finalize. Orchestrator
// (VoyageWorker) живёт отдельно в `keeper/internal/voyageorch/`. На S1 worker —
// NOOP-исполнение (config-gated OFF по умолчанию): реальный scenario/command
// прогон подключается в S2/S3.
//
// TODO(post-S1): claim+lease-хелперы дублируют tide/errandrun (architect-
// decision γ 2026-05-27 об extract в shared `claimlease/` отложен до review,
// чтобы не вводить абстракцию преждевременно). Текущий copy-pattern — явное
// место для будущего рефакторинга.
package voyage

import (
	"encoding/json"
	"errors"
	"time"
)

// Kind — режим Voyage-прогона. Closed enum, совпадает с CHECK `voyages_kind_valid`
// миграции 059.
type Kind string

const (
	// KindScenario — применить named scenario к набору инкарнаций (поглощает
	// Tide + classic scenario-run). Требует непустой ScenarioName (CHECK
	// `voyages_kind_payload_consistency`).
	KindScenario Kind = "scenario"
	// KindCommand — выполнить whitelisted-модуль на наборе хостов (поглощает
	// ErrandRun). Требует непустой Module.
	KindCommand Kind = "command"
)

// Status — статус строки `voyages`. Closed enum, совпадает с CHECK
// `voyages_status_valid` миграции 059.
type Status string

const (
	// StatusScheduled — отложенный старт (schedule_at в будущем, S4). Worker не
	// подбирает scheduled до наступления schedule_at; на S1 не используется
	// (планировщик — S4), но значение зарезервировано в CHECK.
	StatusScheduled Status = "scheduled"
	// StatusPending — Voyage создан, ждёт подбора VoyageWorker-ом. Очередь
	// pickup: ClaimNext по FIFO `created_at` (FOR UPDATE SKIP LOCKED).
	StatusPending Status = "pending"
	// StatusRunning — Voyage подобран VoyageWorker-ом (claim CAS-UPDATE
	// pending→running), claim-поля NOT NULL (CHECK
	// `voyages_running_claim_consistency`). renewal-goroutine держит lease.
	StatusRunning Status = "running"
	// StatusSucceeded — все Leg-и / targets отработали успешно. Финал.
	StatusSucceeded Status = "succeeded"
	// StatusFailed — прогон провалился целиком. Финал.
	StatusFailed Status = "failed"
	// StatusPartialFailed — часть targets succeed, часть failed/cancelled. Финал.
	StatusPartialFailed Status = "partial_failed"
	// StatusCancelled — Voyage отменён оператором. Финал.
	StatusCancelled Status = "cancelled"
)

// BatchMode — режим батчинга прогона (ADR-043 amendment 2026-06-01). Closed enum,
// совпадает с CHECK `voyages_batch_mode_valid` миграции 064. NULL-колонка
// трактуется как [BatchModeBarrier] (forward-compat: прогоны без поля работают
// как раньше).
type BatchMode string

const (
	// BatchModeBarrier — прогон бьётся на последовательные Leg-и (пачка
	// batch_size единиц), между Leg-ами барьер (дождаться терминала всех единиц
	// Leg-а). concurrency = параллелизм ВНУТРИ Leg-а. Default-поведение (NULL
	// колонка ⇒ barrier).
	BatchModeBarrier BatchMode = "barrier"
	// BatchModeWindow — полное скользящее окно: пул воркеров тянет единицы из
	// общей очереди прогона, вернулся один → запускается следующий, постоянно
	// держится concurrency активных. Барьеров между пачками нет; batch_size не
	// используется (ширина окна = concurrency). batch_index = 0 у всех единиц.
	BatchModeWindow BatchMode = "window"
)

// ResolveBatchMode возвращает эффективный режим: NULL/пустой → barrier
// (forward-compat). Используется оркестратором при ветвлении executor-а.
func ResolveBatchMode(m *BatchMode) BatchMode {
	if m == nil || *m == "" {
		return BatchModeBarrier
	}
	return *m
}

// ValidBatchMode сообщает, входит ли режим в [BatchMode]-enum.
func ValidBatchMode(m BatchMode) bool {
	switch m {
	case BatchModeBarrier, BatchModeWindow:
		return true
	}
	return false
}

// ResolveFailThreshold возвращает эффективный порог числа провалов, при котором
// прогон останавливается (ADR-043 amendment 2026-06-01). Семантика обобщает
// существующий abort-gate:
//   - fail_threshold задан → его значение (N>1 — промежуточная толерантность);
//   - не задан, OnFailure=abort → 1 (первый провал → стоп, backcompat);
//   - не задан, OnFailure=continue/nil → 0 (без порога, бежать до конца).
//
// Возврат 0 означает «порога нет». Используется оркестратором: достигнуто
// failCount >= threshold (threshold>0) → прекратить спавн новых единиц.
func ResolveFailThreshold(threshold *int, policy *OnFailure) int {
	if threshold != nil && *threshold > 0 {
		return *threshold
	}
	if policy != nil && *policy == OnFailureAbort {
		return 1
	}
	return 0
}

// ResolveRequireAlive возвращает эффективное значение presence-фильтра: NULL →
// false (без фильтра, forward-compat), ADR-043 amendment.
func ResolveRequireAlive(b *bool) bool {
	return b != nil && *b
}

// OnFailure — политика перехода к следующему Leg при провале. Closed-set,
// совпадает с CHECK `voyages_on_failure_valid`.
type OnFailure string

const (
	// OnFailureContinue — провал target-ов учитывается, остальные Leg-и
	// продолжают; финал — partial_failed (если был хоть один fail) либо succeeded.
	OnFailureContinue OnFailure = "continue"
	// OnFailureAbort — провал → остановка перехода к следующему Leg → финал.
	OnFailureAbort OnFailure = "abort"
)

// TargetKind — тип единицы прогона в `voyage_targets`. Closed enum, совпадает с
// CHECK `voyage_targets_target_kind_valid`.
type TargetKind string

const (
	// TargetKindIncarnation — единица для kind=scenario (одна инкарнация =
	// полноценный scenario-run, back-link ApplyID на apply_runs).
	TargetKindIncarnation TargetKind = "incarnation"
	// TargetKindSID — единица для kind=command (один хост, back-link ErrandID
	// на errands).
	TargetKindSID TargetKind = "sid"
)

// TargetStatus — статус строки `voyage_targets`. Closed enum, совпадает с CHECK
// `voyage_targets_status_valid`.
type TargetStatus string

const (
	// TargetStatusAwaiting — target в очереди своего Leg-а, ещё не запущен.
	TargetStatusAwaiting TargetStatus = "awaiting"
	// TargetStatusRunning — для target-а спавнен дочерний прогон (apply_run /
	// errand), ждём его терминал.
	TargetStatusRunning TargetStatus = "running"
	// TargetStatusSucceeded — дочерний прогон target-а завершился успехом.
	TargetStatusSucceeded TargetStatus = "succeeded"
	// TargetStatusFailed — дочерний прогон target-а провалился.
	TargetStatusFailed TargetStatus = "failed"
	// TargetStatusCancelled — target отменён (cancel-all либо on_failure: abort).
	TargetStatusCancelled TargetStatus = "cancelled"
	// TargetStatusNoMatch — target не дал ни одного совпадения при резолве
	// (parity apply_runs `no_match`): целевая инкарнация/хост вне фактического
	// scope на момент исполнения.
	TargetStatusNoMatch TargetStatus = "no_match"
)

// Voyage — runtime-представление строки `voyages`. Полная проекция (включая
// claim-колонки и summary): унифицированная read-модель для CRUD-вызовов.
//
// Nullable-поля — указатели/raw-bytes. ScenarioName/Module взаимоисключающи по
// Kind (CHECK `voyages_kind_payload_consistency`). BatchSize/Concurrency
// nullable — отсутствие = «весь прогон одним Leg / дефолтная степень»
// (резолвится оркестратором, не CRUD-ом).
type Voyage struct {
	VoyageID           string
	Kind               Kind
	ScenarioName       *string
	Module             *string
	Input              []byte // jsonb (caller сериализует input один раз)
	TargetResolved     json.RawMessage
	TargetOrigin       json.RawMessage // optional declarative origin (NULL допустим)
	BatchSize          *int
	BatchPercent       *int // % от scope (XOR с BatchSize), ADR-043 amendment. nil ⇒ задан BatchSize / весь прогон одним Leg.
	Concurrency        *int
	BatchMode          *BatchMode // nil ⇒ barrier (forward-compat), ADR-043 amendment.
	DryRun             bool
	ScheduleAt         *time.Time
	InterBatchInterval *time.Duration
	InterUnitInterval  *time.Duration // per-unit пауза в window (parity InterBatchInterval), ADR-043 amendment.
	FailThreshold      *int           // порог абсолютного числа провалов → стоп. nil ⇒ без порога, ADR-043 amendment.
	RequireAlive       *bool          // presence-фильтр живых на резолве scope. nil ⇒ false, ADR-043 amendment.
	OnFailure          *OnFailure
	CadenceID          *string // back-link на породившую Cadence (ADR-046 §2). nil ⇒ ручной прогон; populated ⇒ спавн от Cadence (S2).
	TotalBatches       int
	CurrentBatchIndex  int
	Status             Status
	ClaimedByKID       *string
	LastRenewedAt      *time.Time
	ClaimExpiresAt     *time.Time
	Attempt            int
	StartedByAID       string
	CreatedAt          time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
	Summary            *Summary
}

// VoyageTarget — runtime-представление строки `voyage_targets` (единица прогона,
// Leg-разбиение). ApplyID (kind=scenario) и ErrandID (kind=command) — back-link-и
// на дочерний прогон, заполняются оркестратором при спавне (S2/S3).
type VoyageTarget struct {
	VoyageID   string
	TargetKind TargetKind
	TargetID   string
	BatchIndex int
	Status     TargetStatus
	ApplyID    *string
	ErrandID   *string
	FinishedAt *time.Time
}

// Summary — агрегированный итог прогона, складывается в jsonb-колонку `summary`
// при финализации Voyage.
type Summary struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	NoMatch   int `json:"no_match,omitempty"`
}

// Sentinel-ошибки CRUD-слоя.
//   - ErrVoyageNotFound  — нет строки по запрошенному voyage_id.
//   - ErrLeaseLost       — CAS-renewal / finalize вернул 0 строк: lease уже не
//     наш (другой Keeper подобрал через Reaper-reclaim+claim). VoyageWorker,
//     получив ErrLeaseLost, немедленно бросает работу.
//   - ErrInvalidStatus   — попытка перевода в несовместимый статус
//     (программная ошибка caller-а; CRUD не пишет терминал в терминал).
//   - ErrVoyageExists     — UNIQUE по PK (voyage_id) — повторный Insert одного
//     ULID (программная ошибка caller-а).
var (
	ErrVoyageNotFound = errors.New("voyage: not found")
	ErrLeaseLost      = errors.New("voyage: lease lost (CAS returned 0 rows)")
	ErrInvalidStatus  = errors.New("voyage: invalid status transition")
	ErrVoyageExists   = errors.New("voyage: voyage_id already exists")
)

// ValidKind сообщает, входит ли kind в [Kind]-enum.
func ValidKind(k Kind) bool {
	switch k {
	case KindScenario, KindCommand:
		return true
	}
	return false
}

// ValidStatus сообщает, входит ли статус в [Status]-enum.
func ValidStatus(s Status) bool {
	switch s {
	case StatusScheduled, StatusPending, StatusRunning,
		StatusSucceeded, StatusFailed, StatusPartialFailed, StatusCancelled:
		return true
	}
	return false
}

// ValidOnFailure — парная проверка для OnFailure.
func ValidOnFailure(p OnFailure) bool {
	switch p {
	case OnFailureContinue, OnFailureAbort:
		return true
	}
	return false
}

// ValidTargetKind — проверка для TargetKind.
func ValidTargetKind(k TargetKind) bool {
	switch k {
	case TargetKindIncarnation, TargetKindSID:
		return true
	}
	return false
}

// IsTerminal сообщает, является ли статус терминальным (финализированным).
// finished_at у таких строк всегда NOT NULL.
func IsTerminal(s Status) bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusPartialFailed, StatusCancelled:
		return true
	}
	return false
}

// marshalSummary сериализует Summary в jsonb-байты. nil → nil (NULL в БД).
func marshalSummary(s *Summary) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	return json.Marshal(s)
}

// unmarshalSummary парсит jsonb-байты в Summary. Пустой/nil-вход → nil.
func unmarshalSummary(raw []byte) (*Summary, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
