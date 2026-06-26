// Package incarnation — runtime-инстанс Service в Postgres под ADR-009.
//
// M0.6c-1: типы + CRUD (Create / SelectByName / SelectAll / HistorySelectByName).
// Scenario-execution / migrate-executor — следующие slice-ы (M0.6c-2/3),
// блокированы Soul gRPC infrastructure (M2.x).
package incarnation

import (
	"regexp"
	"time"
)

// Status — статус incarnation. MVP-enum: четыре базовых значения + DESTROYING
// (S-D1, фаза teardown через scenario `destroy`) + DESTROY_FAILED (S-D2a,
// терминал упавшего teardown-а) + DRIFT (ADR-031, Scry, информационный).
// PROVISIONING — пост-MVP, появится в каталоге при имплементации фазы.
//
// Совпадает с CHECK-constraint incarnation_status_valid (005 + 031 + 036 + 047).
type Status string

const (
	StatusReady           Status = "ready"
	StatusApplying        Status = "applying"
	StatusErrorLocked     Status = "error_locked"
	StatusMigrationFailed Status = "migration_failed"

	// StatusDestroying — оператор инициировал destroy: запущен teardown
	// (scenario `destroy`, S-D2b) с последующим DELETE строки (S-D3). Не
	// терминальный для самой строки — при успехе строка удаляется, при фейле
	// teardown переходит в destroy_failed (S-D2b). Из этого статуса другие
	// операции (run / upgrade / повторный destroy) отвергаются.
	StatusDestroying Status = "destroying"

	// StatusDestroyFailed — teardown (scenario `destroy`) упал на хостах: инстанс
	// НЕ удалён, state остался last known-good (teardown работает с хостами, не с
	// jsonb-state). Терминал, требующий вмешательства оператора — из него (в
	// S-D2b/S-D3) оператор сможет повторить destroy, force-снести или unlock в
	// ready. Сам переход в этот статус выставляется teardown-исходом (S-D2b/S-D3);
	// S-D2a вводит только само значение. Для обычного прогона отвергается
	// fail-closed allow-list-ом scenario.lockRun (run.go).
	StatusDestroyFailed Status = "destroy_failed"

	// StatusDrift — Scry-check обнаружил расхождение реальных состояний хостов
	// с декларацией (ADR-031, on-demand-пилот). Информационный, НЕ блокирующий:
	// remediation drift-а = обычный apply из `drift` → `ready` (allow-list
	// scenario.lockRun принимает drift как стартовый статус, симметрично
	// ready). Переход в drift выставляется check-drift-handler-ом по итогу
	// сборки DriftReport (если есть hosts_drifted > 0); снятие — успешный apply
	// (commitSuccess → ready).
	StatusDrift Status = "drift"
)

// NamePattern — каноническая форма имени incarnation: kebab-case, начинается
// с букв/цифр, длина 1..63. То же, что CHECK incarnation_name_format в
// миграции.
const NamePattern = `^[a-z0-9][a-z0-9-]{0,62}$`

// ReasonMaxLen — верхняя граница свободного текста подтверждения (reason) у
// unlock / rerun-create. Единый источник для huma-тега maxLength и рантайм-
// валидатора (UnlockTyped / RerunCreateTyped 422-ят `len(reason) > ReasonMaxLen`).
// Нижняя граница (непустота) — отдельная проверка `reason == ""`.
const ReasonMaxLen = 500

var nameRe = regexp.MustCompile(NamePattern)

// ValidName проверяет соответствие name канонической форме.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Incarnation — runtime-представление строки реестра `incarnation`.
//
// jsonb-поля (`Spec` / `State` / `StatusDetails`) — `map[string]any` для
// freeform; типизация по conkrete service / scenario живёт в их manifest-ах,
// не в этом слое.
type Incarnation struct {
	Name               string         `json:"name"`
	Service            string         `json:"service"`
	ServiceVersion     string         `json:"service_version"`
	StateSchemaVersion int            `json:"state_schema_version"`
	Spec               map[string]any `json:"spec"`
	State              map[string]any `json:"state"`
	Status             Status         `json:"status"`
	StatusDetails      map[string]any `json:"status_details,omitempty"`
	CreatedByAID       *string        `json:"created_by_aid,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`

	// Covens — declared environment-теги incarnation (ADR-008 amendment a,
	// колонка incarnation.covens). Источник RBAC coven-scope incarnation-
	// операций: effective scope = Covens ∪ {Name} (имя — корневая Coven-метка).
	// Пустой массив для incarnation без env-тегов (DEFAULT '{}').
	Covens []string `json:"covens"`

	// CreatedScenario — имя стартового сценария, которым создана incarnation
	// (механизм нескольких create-сценариев, Вариант A; колонка
	// incarnation.created_scenario, миграция 089). Runtime-факт: оператор
	// выбирает его при POST /v1/incarnations (поле `create_scenario`, default
	// `create`). rerun-create перезапускает ИМЕННО этот сценарий (а не хардкод
	// `create`). DEFAULT 'create' — back-compat для строк до миграции.
	CreatedScenario string `json:"created_scenario"`

	// Traits — operator-set key-value метки incarnation (ADR-060 amend, R1,
	// колонка incarnation.traits jsonb). Источник истины Trait-ов: задаётся
	// оператором в incarnation.spec при create, проецируется МАТЕРИАЛИЗОВАННО в
	// souls.traits хостов-членов через sync-hook (incarnation create + bind хоста
	// через core.soul.registered). Значение полиморфно (scalar | list). Пустой
	// map для incarnation без traits (DEFAULT '{}').
	Traits map[string]any `json:"traits"`

	// LastDriftCheckAt — время завершения последнего dry_run-прогона converge
	// (ADR-031 Slice C, миграция 050). nil = ни разу не сканировали.
	// Заполняется через [UpdateDriftScanResult] (Slice B on-demand + Slice C
	// background).
	LastDriftCheckAt *time.Time `json:"last_drift_check_at,omitempty"`

	// LastDriftSummary — typed counts-агрегат последнего DriftReport-а
	// ([DriftScanSummary]: `hosts_*` + `total_hosts` + `scanned_at`).
	// nil = ни разу не сканировали (колонка NULL). Counts-only: полный
	// DriftReport в БД не хранится (Slice C ограничен счётчиками, полный отчёт
	// on-demand из Slice B возвращается прямо в response). Читается из колонки
	// типизированно ([scanIncarnation]), на wire уходит typed-объектом.
	LastDriftSummary *DriftScanSummary `json:"last_drift_summary,omitempty"`
}

// HistoryEntry — запись `state_history` (snapshot per-change, ADR-009 / ADR-019).
type HistoryEntry struct {
	HistoryID    string         `json:"history_id"`
	Scenario     string         `json:"scenario"`
	StateBefore  map[string]any `json:"state_before"`
	StateAfter   map[string]any `json:"state_after"`
	ChangedByAID *string        `json:"changed_by_aid,omitempty"`
	ApplyID      string         `json:"apply_id"`
	At           time.Time      `json:"at"`
}
