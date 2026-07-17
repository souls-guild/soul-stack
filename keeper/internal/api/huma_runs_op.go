package api

// FULL-TYPED form of the global RUNS read view (GET /v1/runs + /v1/runs/stats, the
// "All Runs" UI page; ADR-054 §Pattern). READ domain: no audit is written,
// newHumaCadenceAPI. Permission incarnation.history (reuse the read-tier per-incarnation
// runs) — a RequireAction gate on the /v1/runs chi group (router.go); Purview narrowing
// is in-handler (fail-closed, parity souls/stats). Op-Path is relative to the
// r.Route("/runs") group.

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === GET /v1/runs (list) — READ with typed query (no audit) ===

// runsListInput — huma input for GET /v1/runs. Filters are optional (value validation is
// done by AllRunsTyped: invalid status/incarnation → 422); offset/limit are int32 with a
// default (out-of-range/limit>100 → 400).
type runsListInput struct {
	Status        string `query:"status" doc:"filter by aggregate run status (applying/success/failed/cancelled); invalid -> 422"`
	Incarnation   string `query:"incarnation" doc:"filter by incarnation name; invalid name -> 422"`
	Service       string `query:"service" doc:"filter by the incarnation's owner service (exact match); longer than 128 characters -> 422"`
	Q             string `query:"q" doc:"free-text search (substring, case-insensitive) over incarnation/scenario/service/started_by; longer than 128 characters -> 422"`
	StartedAfter  string `query:"started_after" doc:"filter: run start time >= (RFC3339, inclusive); invalid -> 422"`
	StartedBefore string `query:"started_before" doc:"filter: run start time <= (RFC3339, inclusive); invalid -> 422"`
	Sort          string `query:"sort" doc:"sort field (started_at/finished_at/status/incarnation/service/scenario; default started_at); invalid -> 422"`
	SortDir       string `query:"sort_dir" doc:"sort direction (asc/desc; default desc); invalid -> 422"`
	Offset        int32  `query:"offset" default:"0" doc:"offset from start of set, ≥0 (out-of-range → 400)"`
	Limit         int32  `query:"limit" default:"50" doc:"page size 1..100 (out-of-range → 400)"`
}

// runsListOutput — huma output for GET /v1/runs (FULL-TYPED). Body — the native envelope
// runsListReply (items.$ref to GlobalRunEntry: snake_case wire).
type runsListOutput struct {
	Body runsListReply
}

func runsListOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "listRuns",
		Method:        http.MethodGet,
		Path:          "/",
		Summary:       "Global run list (paged)",
		Description:   "Folds apply_runs by apply_id ACROSS ALL incarnations: run status (applying/success/failed/cancelled), owner incarnation, time bounds, initiator. A run (apply_run) is NOT a Voyage. Sorting by column - sort/sort_dir (stable tie-break on apply_id). Visibility scoped by RBAC (ADR-047, fail-closed: empty scope -> empty list). Permission incarnation.history. Read-only.",
		Tags:          []string{"runs"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusInternalServerError},
	}
}

// === GET /v1/runs/stats (stats) — READ aggregate (no audit) ===

// runsStatsInput — huma input for GET /v1/runs/stats. No parameters: an aggregate within
// the operator's Purview scope.
type runsStatsInput struct{}

// runsStatsOutput — huma output for GET /v1/runs/stats (FULL-TYPED). Body — the native
// aggregate DTO (RunsStatsReply: all/last_24h buckets by aggregate status).
type runsStatsOutput struct {
	Body RunsStatsReply
}

func runsStatsOperation() huma.Operation {
	return huma.Operation{
		OperationID:   "getRunsStats",
		Method:        http.MethodGet,
		Path:          "/stats",
		Summary:       "Run summary counters",
		Description:   "Run counters by aggregate status (total/applying/success/failed/cancelled) for all time and for the last 24 hours, within the RBAC scope (fail-closed: empty scope -> zero aggregate). Permission incarnation.history. Read-only.",
		Tags:          []string{"runs"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusForbidden, http.StatusInternalServerError},
	}
}

// === reply-DTO ===

// GlobalRunEntry — the native items element of GET /v1/runs. Shape — RunSummaryEntry
// (per-incarnation runs) + incarnation (the run's owner: the global list is unreadable
// without it). finished_at / started_by_aid — omitempty (nil → key omitted).
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

// runsListReply — the GET /v1/runs envelope (the contract 4-field form
// items/offset/limit/total, parity with incarnationRunsReply). The type name = the
// contract schema name (huma DefaultSchemaNamer capitalizes → "RunsListReply").
type runsListReply struct {
	Items  []GlobalRunEntry `json:"items" doc:"page of runs across all incarnations (apply_runs fold)"`
	Offset int32            `json:"offset" doc:"offset from start of set"`
	Limit  int32            `json:"limit" doc:"page size"`
	Total  int32            `json:"total" doc:"total number of runs under filters/scope"`
}

// RunsStatsBucket — one run-summary bucket (all time or the last 24 hours): total +
// a counter for each aggregate status (zeros included — the enum is closed).
type RunsStatsBucket struct {
	Total     int `json:"total" doc:"total runs in the bucket"`
	Applying  int `json:"applying" doc:"runs in progress"`
	Success   int `json:"success" doc:"successful runs"`
	Failed    int `json:"failed" doc:"failed runs (including orphaned hosts)"`
	Cancelled int `json:"cancelled" doc:"cancelled runs"`
}

// RunsStatsReply — the native body of GET /v1/runs/stats: two buckets of the same shape.
type RunsStatsReply struct {
	All     RunsStatsBucket `json:"all" doc:"all time"`
	Last24h RunsStatsBucket `json:"last_24h" doc:"runs started in the last 24 hours"`
}

// newGlobalRunEntry projects the domain handlers.RunSummaryView into native.
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

// newRunsStatsReply projects the domain handlers.RunsStatsView into native.
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
