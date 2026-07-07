package api

// FULL-TYPED форма глобального RUNS-read-view (GET /v1/runs + /v1/runs/stats,
// страница «All Runs» UI; ADR-054 §Pattern). READ-домен: audit НЕ пишется,
// newHumaCadenceAPI. Permission incarnation.history (reuse read-tier per-incarnation
// runs) — RequireAction-гейт на chi-группе /v1/runs (router.go); сужение по
// Purview — in-handler (fail-closed, parity souls/stats). Op-Path относителен
// группе r.Route("/runs").

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === GET /v1/runs (list) — READ-with-typed-query (БЕЗ audit) ===

// runsListInput — huma-input GET /v1/runs. Фильтры опциональны (валидацию значений
// ведёт AllRunsTyped: невалидный status/incarnation → 422); offset/limit — int32 с
// default (out-of-range/limit>100 → 400).
type runsListInput struct {
	Status        string `query:"status" doc:"фильтр по агрегатному статусу прогона (applying/success/failed/cancelled); невалидный → 422"`
	Incarnation   string `query:"incarnation" doc:"фильтр по имени инкарнации; невалидное имя → 422"`
	Service       string `query:"service" doc:"фильтр по сервису инкарнации-владельца (точное совпадение); длиннее 128 символов → 422"`
	Q             string `query:"q" doc:"свободный поиск (substring, регистронезависимо) по incarnation/scenario/service/started_by; длиннее 128 символов → 422"`
	StartedAfter  string `query:"started_after" doc:"фильтр: время старта прогона ≥ (RFC3339, inclusive); невалидное → 422"`
	StartedBefore string `query:"started_before" doc:"фильтр: время старта прогона ≤ (RFC3339, inclusive); невалидное → 422"`
	Sort          string `query:"sort" doc:"поле сортировки (started_at/finished_at/status/incarnation/service/scenario; дефолт started_at); невалидное → 422"`
	SortDir       string `query:"sort_dir" doc:"направление сортировки (asc/desc; дефолт desc); невалидное → 422"`
	Offset        int32  `query:"offset" default:"0" doc:"сдвиг от начала набора, ≥0 (out-of-range → 400)"`
	Limit         int32  `query:"limit" default:"50" doc:"размер страницы 1..100 (out-of-range → 400)"`
}

// runsListOutput — huma-output GET /v1/runs (FULL-TYPED). Body — native envelope
// runsListReply (items.$ref на GlobalRunEntry: snake_case-wire).
type runsListOutput struct {
	Body runsListReply
}

func runsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listRuns",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Глобальный список прогонов (paged)",
		Description:   "Свёртка apply_runs по apply_id ЧЕРЕЗ ВСЕ инкарнации: статус прогона (applying/success/failed/cancelled), инкарнация-владелец, границы времени, инициатор. Прогон (apply_run) — НЕ Voyage. Сортировка колонок — sort/sort_dir (стабильный tie-break apply_id). Видимость scoped по RBAC (ADR-047, fail-closed: пустой scope → пустой список). Permission incarnation.history. Read-only.",
		Tags:          []string{"runs"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/runs/stats (stats) — READ-агрегат (БЕЗ audit) ===

// runsStatsInput — huma-input GET /v1/runs/stats. Параметров нет: агрегат в
// границах Purview-scope оператора.
type runsStatsInput struct{}

// runsStatsOutput — huma-output GET /v1/runs/stats (FULL-TYPED). Body — native
// агрегат-DTO (RunsStatsReply: корзины all/last_24h по агрегатным статусам).
type runsStatsOutput struct {
	Body RunsStatsReply
}

func runsStatsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getRunsStats",
		Method:        http.MethodGet,
		Path:          "/stats",
		Summary:       "Сводные счётчики прогонов",
		Description:   "Счётчики прогонов по агрегатному статусу (total/applying/success/failed/cancelled) за всё время и за последние 24 часа, в границах RBAC-scope (fail-closed: пустой scope → нулевой агрегат). Permission incarnation.history. Read-only.",
		Tags:          []string{"runs"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === reply-DTO ===

// GlobalRunEntry — native элемент items GET /v1/runs. Форма — RunSummaryEntry
// (per-incarnation runs) + incarnation (владелец прогона: глобальный список без
// него нечитаем). finished_at / started_by_aid — omitempty (nil → ключ опущен).
type GlobalRunEntry struct {
	ApplyID      string     `json:"apply_id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Incarnation  string     `json:"incarnation"`
	Service      string     `json:"service"`
	Scenario     string     `json:"scenario"`
	Status       string     `json:"status" enum:"applying,success,failed,cancelled"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	StartedByAID *string    `json:"started_by_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"`
}

// runsListReply — envelope GET /v1/runs (контрактная 4-полевая форма
// items/offset/limit/total, parity incarnationRunsReply). Имя типа = контрактное
// имя схемы (huma DefaultSchemaNamer капитализирует → "RunsListReply").
type runsListReply struct {
	Items  []GlobalRunEntry `json:"items" doc:"страница прогонов через все инкарнации (свёртка apply_runs)"`
	Offset int32            `json:"offset" doc:"сдвиг от начала набора"`
	Limit  int32            `json:"limit" doc:"размер страницы"`
	Total  int32            `json:"total" doc:"общее число прогонов под фильтрами/scope"`
}

// RunsStatsBucket — одна корзина сводки прогонов (за всё время либо за 24 часа):
// total + счётчик каждого агрегатного статуса (нули включены — enum закрыт).
type RunsStatsBucket struct {
	Total     int `json:"total" doc:"всего прогонов в корзине"`
	Applying  int `json:"applying" doc:"прогоны в процессе"`
	Success   int `json:"success" doc:"успешные прогоны"`
	Failed    int `json:"failed" doc:"упавшие прогоны (включая orphaned-хосты)"`
	Cancelled int `json:"cancelled" doc:"отменённые прогоны"`
}

// RunsStatsReply — native тело GET /v1/runs/stats: две корзины одинаковой формы.
type RunsStatsReply struct {
	All     RunsStatsBucket `json:"all" doc:"за всё время"`
	Last24h RunsStatsBucket `json:"last_24h" doc:"прогоны, стартовавшие за последние 24 часа"`
}

// newGlobalRunEntry проецирует доменный handlers.RunSummaryView в native.
func newGlobalRunEntry(v handlers.RunSummaryView) GlobalRunEntry {
	return GlobalRunEntry{
		ApplyID:      v.ApplyID,
		Incarnation:  v.Incarnation,
		Service:      v.Service,
		Scenario:     v.Scenario,
		Status:       v.Status,
		StartedAt:    v.StartedAt,
		FinishedAt:   v.FinishedAt,
		StartedByAID: v.StartedByAID,
	}
}

// newRunsStatsReply проецирует доменный handlers.RunsStatsView в native.
func newRunsStatsReply(v handlers.RunsStatsView) RunsStatsReply {
	return RunsStatsReply{
		All:     newRunsStatsBucket(v.All),
		Last24h: newRunsStatsBucket(v.Last24h),
	}
}

func newRunsStatsBucket(v handlers.RunsStatsBucketView) RunsStatsBucket {
	return RunsStatsBucket{
		Total:     v.Total,
		Applying:  v.Applying,
		Success:   v.Success,
		Failed:    v.Failed,
		Cancelled: v.Cancelled,
	}
}
