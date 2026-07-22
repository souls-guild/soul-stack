package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// pushApplier is the narrow orchestrator surface for ApplyTyped: the single
// Apply method (accept push run → apply_id). The narrowing exists for the S6
// happy-path test: `*pushorch.PushRun` holds a real `*Store` on PG, so a
// happy-path Apply through the concrete struct needs testcontainers. This
// interface lets a fake orchestrator with a static apply_id be injected and
// proves the 202→audit path (carrier→middleware) at unit level. Prod still
// passes `*pushorch.PushRun` (satisfies the interface automatically) — wire
// and MCP push are unchanged.
type pushApplier interface {
	Apply(ctx context.Context, req pushorch.ApplyRequest) (applyID string, err error)
}

// PushHandler exposes the Destiny push-run endpoints over SSH (`POST /v1/push/apply` +
// `GET /v1/push/{apply_id}`, ADR-004 push-flow + Variant C orchestrator
// docs/keeper/push.md). svc is `*pushorch.PushRun` (optional api.Deps field): when
// nil the router does not mount the routes (SigilSvc/AugurSvc pattern). All business
// logic lives in pushorch. applier is the narrow Apply surface (== svc in prod; fake in
// the S6 test); read paths (GetTyped/ListRunsTyped) hit svc directly.
//
// T5d-2c-full (handler-native): the push domain is decoupled from the legacy generator.
// The *Typed functions accept/return NATIVE types with flat wire fields (PushApplyInput /
// PushApplyResultView / PushRunListPage); the api package builds the native wire-DTO
// (OpenAPI schema) from these fields (register func huma_push.go). HTTP is served by huma
// full-typed, MCP calls pushorch.PushRun directly (bypassing the handler).
type PushHandler struct {
	svc     *pushorch.PushRun
	applier pushApplier
	logger  *slog.Logger
}

// NewPushHandler constructs the handler. svc nil → the caller (api.NewServer)
// decides not to mount the push routes (see router.go); nil is allowed here only
// for constructor unit tests. applier == svc (the orchestrator implements Apply);
// with nil svc, applier stays nil → ApplyTyped returns 500 "not configured".
func NewPushHandler(svc *pushorch.PushRun, logger *slog.Logger) *PushHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	h := &PushHandler{svc: svc, logger: logger}
	if svc != nil {
		h.applier = svc
	}
	return h
}

// NewPushHandlerWithApplier is a test-only constructor: injects a fake orchestrator
// into the Apply path (S6 RecordsOnSuccess happy-path without PG). svc stays nil →
// read paths are unavailable, but Apply goes through applier. Do NOT use in prod.
func NewPushHandlerWithApplier(applier pushApplier, logger *slog.Logger) *PushHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &PushHandler{applier: applier, logger: logger}
}

// PushSpecStub is a non-empty *PushHandler stub for generating the huma OpenAPI
// fragment (HumaPushSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its nil check (parity [RoleSpecStub]). svc nil —
// the handler never executes in spec mode.
func PushSpecStub() *PushHandler {
	return &PushHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// PushApplyInput is the NATIVE request shape of POST /v1/push/apply (handler-native T5d-2c-full).
// Replaces PushApplyRequest: the huma input (api package) binds/validates the body and projects
// it into these fields before calling ApplyTyped. Optional fields are pointers (Input/SSHProvider/
// CleanupStaleVersions); the handler dereferences them into pushorch.ApplyRequest.
type PushApplyInput struct {
	Inventory            []string
	Destiny              string
	Input                *map[string]any
	SSHProvider          *string
	CleanupStaleVersions *bool
}

// PushApplyReply is the extracted result of [PushHandler.ApplyTyped] (handler-native).
// Carries apply_id (the 202 body builds the native api.PushApplyReply from it) + the audit
// payload (set by the Variant B middleware). Apply is async: 202 Accepted, then the client
// polls GET by apply_id.
type PushApplyReply struct {
	// audit fields + 202 body (parity legacy SetAuditPayload; full SIDs are NOT included).
	ApplyID       string
	Destiny       string
	InventorySize int
	SSHProvider   string
	CleanupStale  bool
}

// AuditPayload assembles the audit fields of the 202 Apply (parity legacy).
func (r PushApplyReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"apply_id":       r.ApplyID,
		"destiny":        r.Destiny,
		"inventory_size": r.InventorySize,
		"ssh_provider":   r.SSHProvider,
		"cleanup_stale":  r.CleanupStale,
	}
}

// ApplyTyped is the domain function for POST /v1/push/apply (handler-native): an orchestrator
// call without the http boundary. claims/req are arguments; req is the native request shape (the
// huma api package binds/validates the body and projects into it; huma rejects unknown → 400
// before the call). Errors are *problemError (422 empty inventory / bad destiny-ref; 500 svc nil /
// PG failure); success is [PushApplyReply] (apply_id + audit fields).
func (h *PushHandler) ApplyTyped(ctx context.Context, claims *jwt.Claims, req PushApplyInput) (PushApplyReply, error) {
	var zero PushApplyReply
	if h.applier == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push orchestrator is not configured")}
	}
	if len(req.Inventory) == 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'inventory' is required and must be non-empty")}
	}
	if req.Destiny == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'destiny' is required")}
	}

	var sshProvider string
	if req.SSHProvider != nil {
		sshProvider = *req.SSHProvider
	}
	var input map[string]any
	if req.Input != nil {
		input = *req.Input
	}
	cleanupStale := req.CleanupStaleVersions != nil && *req.CleanupStaleVersions

	applyID, err := h.applier.Apply(ctx, pushorch.ApplyRequest{
		InventorySIDs: req.Inventory,
		DestinyRef:    req.Destiny,
		SSHProvider:   sshProvider,
		Input:         input,
		CleanupStale:  cleanupStale,
		StartedByAID:  claims.Subject,
	})
	if err != nil {
		if errors.Is(err, pushorch.ErrInvalidDestinyRef) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
		h.logger.Error("push.apply: orchestrator accept failed",
			slog.String("destiny", req.Destiny),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push apply failed")}
	}

	return PushApplyReply{
		ApplyID:       applyID,
		Destiny:       req.Destiny,
		InventorySize: len(req.Inventory),
		SSHProvider:   sshProvider,
		CleanupStale:  cleanupStale,
	}, nil
}

// GetTyped is the domain function for GET /v1/push/{apply_id}. Errors are *problemError (422 empty
// id / 404 no record / 500 svc nil / PG failure); success is [PushApplyResultView].
func (h *PushHandler) GetTyped(ctx context.Context, applyID string) (PushApplyResultView, error) {
	var zero PushApplyResultView
	if h.svc == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push orchestrator is not configured")}
	}
	if applyID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path parameter 'apply_id' is required")}
	}
	row, err := h.svc.GetRow(ctx, applyID)
	if err != nil {
		if errors.Is(err, pushorch.ErrNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "push run "+applyID+" not found")}
		}
		h.logger.Error("push.get: orchestrator read failed", slog.String("apply_id", applyID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push get failed")}
	}
	return rowToPushApplyResultView(row), nil
}

// ListRunsTyped is the domain function for GET /v1/push-runs (handler-native). The status[]
// (multi-value OR) / ssh_provider (exact) filters arrive as arguments; the offset/limit range is
// enforced by CheckPageBounds → 400 (NOT huma min/max — parity legacy ParsePage). Errors are
// *problemError (400 out-of-range / 422 invalid status / 500 svc nil / PG failure); success is
// [PushRunListPage].
func (h *PushHandler) ListRunsTyped(ctx context.Context, statuses []string, sshProvider string, offset, limit int) (PushRunListPage, error) {
	var zero PushRunListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	var filter pushorch.ListFilter
	if len(statuses) > 0 {
		filter.Statuses = make([]pushorch.PushRunStatus, 0, len(statuses))
		for _, s := range statuses {
			st := pushorch.PushRunStatus(s)
			if !pushorch.ValidStatus(st) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
					"invalid 'status' filter: must be one of pending/running/success/partial_failed/failed/cancelled")}
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}
	if sshProvider != "" {
		filter.SSHProvider = sshProvider
	}

	// nil check AFTER validation (deterministic 400/422 regardless of svc).
	if h.svc == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push orchestrator is not configured")}
	}

	rows, total, err := h.svc.ListRows(ctx, filter, offset, limit)
	if err != nil {
		h.logger.Error("push.list: orchestrator read failed", slog.Any("filter", filter), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list push runs failed")}
	}
	items := make([]PushRunListEntryView, 0, len(rows))
	for _, row := range rows {
		items = append(items, rowToPushRunListEntryView(row))
	}
	return PushRunListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// PushApplyResultView is the FLAT wire shape of GET /v1/push/{apply_id} (handler-native,
// replaces PushApplyView). Mirror of the push_runs row + summary as opaque jsonb.
// Optional fields (ssh_provider/input/started_by_aid/summary/finished_at) are nil when empty;
// status is a flat domain-status string; started_at/finished_at are UTC+Truncate(Second)
// (byte-exact with the former second-granularity RFC3339). The api package projects it into the
// native PushApplyView (register func huma_push.go); the native type pins the wire field order.
type PushApplyResultView struct {
	ApplyID       string
	CleanupStale  bool
	DestinyRef    string
	FinishedAt    *time.Time
	Input         *map[string]interface{}
	InventorySids []string
	SSHProvider   *string
	StartedAt     time.Time
	StartedByAID  *string
	Status        string
	Summary       *map[string]interface{}
}

// PushSummaryCountsView is the FLAT counts aggregate (PushRunListEntryView.SummaryCounts).
// All fields are `*int` (nil → key omitted). Projected into the native PushSummaryCounts.
type PushSummaryCountsView struct {
	FailCount    *int
	SuccessCount *int
	Total        *int
}

// PushRunListEntryView is the FLAT compact push_runs row (element of PushRunListPage.Items),
// handler-native (replaces PushRunListEntry). Compact shape: heavy fields are stripped
// (`input` / `summary.hosts[]`), summary is reduced to aggregated summary_counts.
type PushRunListEntryView struct {
	ApplyID       string
	CleanupStale  bool
	DestinyRef    string
	FinishedAt    *time.Time
	InventorySids []string
	SSHProvider   *string
	StartedAt     time.Time
	StartedByAID  *string
	Status        string
	SummaryCounts *PushSummaryCountsView
}

// PushRunListPage is the domain paged result of GET /v1/push-runs (handler-native). Flat
// offset/limit/total + a slice of PushRunListEntryView; the api package projects it into the
// native envelope PushRunListReply (register func huma_push.go).
type PushRunListPage struct {
	Items  []PushRunListEntryView
	Offset int
	Limit  int
	Total  int
}

// rowToPushApplyResultView projects [pushorch.PushRunRow] into the flat view of
// GET /v1/push/{apply_id}. date-time: the former wire was second-granularity (RFC3339 truncated
// to seconds), so Truncate(Second) keeps it byte-for-byte. Optional fields
// (ssh_provider/input/started_by_aid/summary) are nil when the value is empty (parity with the
// former omitempty).
func rowToPushApplyResultView(row *pushorch.PushRunRow) PushApplyResultView {
	view := PushApplyResultView{
		ApplyID:       row.ApplyID,
		InventorySids: row.InventorySIDs,
		DestinyRef:    row.DestinyRef,
		CleanupStale:  row.CleanupStale,
		Status:        string(row.Status),
		StartedAt:     row.StartedAt.UTC().Truncate(time.Second),
		SSHProvider:   ptrIfNotEmpty(row.SSHProvider),
		StartedByAID:  ptrIfNotEmpty(row.StartedByAID),
		Input:         ptrMapIfNotEmpty(row.Input),
		Summary:       ptrMapIfNotEmpty(row.Summary),
	}
	if row.FinishedAt != nil {
		fin := row.FinishedAt.UTC().Truncate(time.Second)
		view.FinishedAt = &fin
	}
	return view
}

// ptrIfNotEmpty returns nil for an empty string (parity with json omitempty over
// string), otherwise a pointer to the value.
func ptrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ptrMapIfNotEmpty returns nil for an empty/nil map (parity with the former
// omitempty over map[string]any), otherwise a pointer to the map in wire shape.
func ptrMapIfNotEmpty(m map[string]any) *map[string]interface{} {
	if len(m) == 0 {
		return nil
	}
	out := map[string]interface{}(m)
	return &out
}

// extractSummaryCounts pulls success_count/fail_count/total out of the summary jsonb
// into a flat [PushSummaryCountsView], returning nil when no field is present (empty
// or pending/running with {}). jsonb numbers arrive as float64 after json.Unmarshal —
// we convert to int with floor semantics (the orchestrator always writes integers). All fields
// are *int so 0 (a legitimate count) is distinct from "no record written".
func extractSummaryCounts(summary map[string]any) *PushSummaryCountsView {
	if len(summary) == 0 {
		return nil
	}
	var counts PushSummaryCountsView
	any := false
	if v, ok := summary["total"].(float64); ok {
		i := int(v)
		counts.Total = &i
		any = true
	}
	if v, ok := summary["success_count"].(float64); ok {
		i := int(v)
		counts.SuccessCount = &i
		any = true
	}
	if v, ok := summary["fail_count"].(float64); ok {
		i := int(v)
		counts.FailCount = &i
		any = true
	}
	if !any {
		return nil
	}
	return &counts
}

// rowToPushRunListEntryView is the boundary domain→view converter for the list endpoint
// `GET /v1/push-runs` (UI-4). Maps [pushorch.PushRunRow] into the flat [PushRunListEntryView],
// keeping the native time.Time. Compact shape: heavy fields are stripped (`input` /
// `summary.hosts[]`), summary is reduced to aggregated summary_counts
// (extractSummaryCounts). date-time: the former wire was second-granularity, so Truncate(Second)
// keeps it byte-for-byte. Optional fields are nil when the value is empty (parity with the former
// omitempty). Full record via `GET /v1/push/{apply_id}` (GetTyped).
func rowToPushRunListEntryView(row *pushorch.PushRunRow) PushRunListEntryView {
	entry := PushRunListEntryView{
		ApplyID:       row.ApplyID,
		InventorySids: row.InventorySIDs,
		DestinyRef:    row.DestinyRef,
		CleanupStale:  row.CleanupStale,
		Status:        string(row.Status),
		StartedAt:     row.StartedAt.UTC().Truncate(time.Second),
		SSHProvider:   ptrIfNotEmpty(row.SSHProvider),
		StartedByAID:  ptrIfNotEmpty(row.StartedByAID),
		SummaryCounts: extractSummaryCounts(row.Summary),
	}
	if row.FinishedAt != nil {
		fin := row.FinishedAt.UTC().Truncate(time.Second)
		entry.FinishedAt = &fin
	}
	return entry
}
