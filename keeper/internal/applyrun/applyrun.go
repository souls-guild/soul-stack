// Package applyrun — реестр apply-прогонов в Postgres (таблица `apply_runs`,
// миграция 018) под M2.x scenario-runner.
//
// Назначение — correlation `apply_id` ↔ incarnation/scenario: при получении
// `RunResult` от Soul-а Keeper из proto не знает, к какой incarnation
// относится прогон. scenario-runner пишет строку при dispatch-е
// `ApplyRequest` ([Insert]); RunResult-handler читает её по `(apply_id, sid)`
// ([SelectIncarnationByApplyID]) и коммитит state в нужную incarnation.
//
// apply_id-model A (PM-decision): один `apply_id` на scenario, разный `sid`
// на каждый хост fan-out-а → composite PK `(apply_id, sid)`.
package applyrun

import "time"

// Status — статус строки apply_runs. Closed enum (PM-decision 1),
// совпадает с CHECK-constraint `apply_runs_status_valid` в миграции 018.
type Status string

const (
	// StatusPlanned — задание Ward-claim ещё не захвачено Acolyte-ом
	// (work-queue вход, ADR-027). scenario-runner пишет его на dispatch-е;
	// Acolyte клеймит planned-задания через [ClaimNext]. Входит в [ValidStatus]
	// с Phase 1.
	StatusPlanned Status = "planned"
	// StatusClaimed — Ward захвачен Acolyte-ом ([ClaimNext]), задание
	// резолвится/рендерится just-in-time перед переходом в `dispatched` через
	// [MarkDispatched] (ADR-027 amend). Входит в [ValidStatus] с Phase 1.
	StatusClaimed Status = "claimed"
	// StatusRunning — прогон стартован (строка вставлена на dispatch-е),
	// терминального RunResult ещё нет. Vestigial после GATE-1 передизайна
	// recovery (ADR-027 amend): Acolyte-флоу больше его не пишет (claimed →
	// [StatusDispatched] → терминал), но значение остаётся валидным для
	// wire-compat (старые/ad-hoc строки) — см. [ValidStatus].
	StatusRunning Status = "running"
	// StatusDispatched — задание отдано Soul-у (фаза lifecycle, ADR-027 amend
	// S2). claimed → dispatched отмечается АТОМАРНО ПЕРЕД SendApply
	// ([MarkDispatched]) — deliver-once intent-маркер. Как только строка
	// dispatched, recovery-reclaim её НЕ трогает (reclaim сужен до
	// `status='claimed'`, S4): после отдачи прогон ведёт Soul, пере-claim =
	// двойной apply. Терминал приходит на dispatched-строку через RunResult.
	StatusDispatched Status = "dispatched"
	// StatusSuccess — RunResult со status=SUCCESS.
	StatusSuccess Status = "success"
	// StatusFailed — RunResult со status=FAILED / ERROR_LOCKED.
	StatusFailed Status = "failed"
	// StatusCancelled — RunResult со status=CANCELLED.
	StatusCancelled Status = "cancelled"
	// StatusOrphaned — терминал Soul-reconcile (ADR-027(g), S6): строка осталась
	// в `dispatched` после «Keeper и Soul оба мертвы после отдачи», а Soul на
	// reconnect НЕ объявил этот apply_id в [WardRoster] (in-flight физически нет
	// — например, Soul-процесс был перезапущен). RunResult по ней не придёт
	// никогда — [OrphanDispatched] терминалит её в `orphaned`. Барьер
	// классифицирует orphaned как терминальный не-успех (incarnation →
	// error_locked). Добавлен миграцией 044.
	StatusOrphaned Status = "orphaned"
	// StatusNoMatch — терминал «хосту ничего не досталось» (FINDING-01, вариант
	// (б)). Acolyte-путь пишет planned-задание на КАЖДЫЙ roster-хост incarnation
	// ДО per-host резолва `on:`/`where:` (резолв позже, при claim). Хост, у
	// которого после `on:`/`where:` осталось 0 задач, закрывается в `no_match`
	// (НЕ `success`): apply_runs больше не over-reports «успех там, где ничего не
	// применялось». Барьер классифицирует no_match как ТЕРМИНАЛ и НЕ-провал
	// (benign, как success): прогон, где целевые success + не-целевые no_match,
	// ведёт incarnation в ready (НЕ error_locked). Несёт finished_at — подлежит
	// retention-purge как прочие терминалы. Добавлен миграцией 045.
	StatusNoMatch Status = "no_match"
)

// ValidStatus — проверка status, разрешённого CRUD-слою ([Insert] /
// [UpdateStatus]). Соответствует полному множеству CHECK apply_runs_status_valid
// после миграции 045 ([StatusNoMatch], FINDING-01 вариант (б); ранее 044 —
// [StatusOrphaned], Soul-reconcile ADR-027(g)). [StatusPlanned] / [StatusClaimed] добавлены в Phase 1
// (claim-логика, ADR-027): scenario-runner на dispatch-е пишет `planned`,
// Acolyte клеймит в `claimed` ([ClaimNext]). [StatusDispatched] добавлен GATE-1
// передизайном recovery (ADR-027 amend, S2): Acolyte переводит claimed →
// dispatched ([MarkDispatched]) ПЕРЕД SendApply. [StatusRunning] остаётся
// валидным (vestigial — Acolyte-флоу его не пишет, но wire-compat сохранён).
func ValidStatus(s Status) bool {
	switch s {
	case StatusPlanned, StatusClaimed, StatusRunning, StatusDispatched, StatusSuccess, StatusFailed, StatusCancelled, StatusOrphaned, StatusNoMatch:
		return true
	}
	return false
}

// ApplyRun — runtime-представление строки реестра `apply_runs`.
//
// TaskIdx / ErrorSummary / FinishedAt / StartedByAID — nullable-колонки
// (PM-decision 2/3): TaskIdx неизвестен на dispatch-е; FinishedAt/ErrorSummary
// заполняются терминальным RunResult; StartedByAID — `NULL` для прогонов,
// инициированных Soul-ом без identity Архонта.
//
// ClaimByKID / ClaimAt / ClaimExpiresAt / Attempt — Ward-claim колонки
// (ADR-027, миграция 025). Phase 0: добавлены в структуру для синхронности с
// схемой (полное представление строки), но НИКЕМ не пишутся и не читаются —
// CRUD-слой (Insert/SelectByApplyID) их не маппит. claim-логика, заполняющая
// эти поля, — Phase 1.
//
// Recipe — render-инструкция для just-in-time-рендера задания Acolyte-ом при
// claim (ADR-027(c)(f), nullable-колонка recipe, миграция 029). nil для строк
// старого пути Insert(running) — рецепт несёт только planned-задание под claim.
// Парсится из/в jsonb через [UnmarshalRecipe] / [MarshalRecipe] на границе
// CRUD-слоя (симметрично nullable-указателям TaskIdx / StartedByAID). Запись
// рецепта на dispatch-е — Phase 1.4.2, чтение при claim — Phase 1.4.3.
type ApplyRun struct {
	ApplyID         string     `json:"apply_id"`
	SID             string     `json:"sid"`
	IncarnationName string     `json:"incarnation_name"`
	Scenario        string     `json:"scenario"`
	TaskIdx         *int       `json:"task_idx,omitempty"`
	Status          Status     `json:"status"`
	ErrorSummary    *string    `json:"error_summary,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	StartedByAID    *string    `json:"started_by_aid,omitempty"`

	ClaimByKID     *string    `json:"claim_by_kid,omitempty"`
	ClaimAt        *time.Time `json:"claim_at,omitempty"`
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty"`
	Attempt        int        `json:"attempt"`

	Recipe *Recipe `json:"recipe,omitempty"`
}

// ActiveApply — одна запись набора ведомых Soul-ом apply-прогонов из WardRoster
// (Soul-reconcile, ADR-027(g), S6). Доменный аналог proto-сообщения
// keeperv1.ActiveApply; маппинг proto → домен делает gRPC-handler, чтобы CRUD-слой
// (этот пакет) не зависел от proto-генерации (как и весь applyrun). [OrphanDispatched]
// читает только ApplyID; Attempt сохранён для будущего epoch-разъезда (в MVP
// присутствие apply_id в наборе защищает строку от orphan с любым attempt).
type ActiveApply struct {
	ApplyID string
	Attempt int32
}
