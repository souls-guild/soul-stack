package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/shared/api"
)

// ErrandHandler — endpoints for pull ad-hoc Errands (ADR-033):
//
//	POST /v1/souls/{sid}/exec       — run an Errand (sync 200 / async 202)
//	GET  /v1/errands/{errand_id}    — Errand state (poll)
//	GET  /v1/errands?sid=&status=…  — list Errands (RBAC filter)
//
// dispatcher / store are required on a non-nil handler; a nil handler is not
// wired to a route (see router.go, PushHandler/OracleHandler pattern).
//
// T5d-2c-full (handler-native): the errand domain is decoupled from the legacy
// generator. Typed functions accept/return NATIVE types with flat wire fields
// (ErrandRunInput / ErrandResultView / ErrandListPage / ErrandAcceptedView); the
// native wire-DTO (OpenAPI schema) is built by the api package from these fields
// (register funcs huma_errand.go / huma_soul.go). The (w,r) wrappers are gone: HTTP
// is served by huma full-typed, MCP calls errand.Dispatcher/Store directly (bypassing
// the handler).
type ErrandHandler struct {
	dispatcher *errand.Dispatcher
	store      *errand.Store

	// enforcer / gate — the console gate over the exec route (ADR-0074
	// amendment, NIM-197). The route's own middleware already checked
	// `errand.run` with [SoulSIDScopeSelector]; the second right is checked HERE
	// and not in middleware because it depends on the module, which lives in
	// the body — the same reason the Voyage route picks its permission
	// in-handler. nil enforcer → the gate is told nothing was verified.
	enforcer middleware.PermissionChecker
	gate     *shellgate.Gate

	// soulReader resolves the target host's Coven labels for that second check,
	// so it lands the SAME `{host}` + `{host, coven}` context set the route's
	// middleware built (NIM-650). Without it the in-handler half would stay
	// host-only and a `soul.console on coven=web` would deny a verb-shell
	// module on a host that IS in coven web, after `errand.run` had already
	// admitted it one layer up. nil → the `{host}` context alone.
	soulReader SoulHostContextReader

	// scoper resolves the caller's `errand` purview, which is what decides WHICH
	// hosts' command output they may read back (NIM-841). Before it existed the
	// read side was all-or-nothing: the route gate asked only whether
	// `errand.list` was held, so a bare grant returned the stdout and stderr of
	// every command run anywhere in the fleet, and the narrower spelling
	// `errand.list on coven=dev` was denied outright by the NoSelector gate.
	//
	// nil is fail-closed rather than permissive — a zero Purview renders FALSE
	// and hides everything (the [ConsoleRecordingHandler] rule).
	scoper PurviewResolver

	logger *slog.Logger
}

// NewErrandHandler constructs the handler. dispatcher/store are required for
// production calls; in a drift/unit test nil is allowed only if the routes do
// not invoke the handler. enforcer/gate carry the console gate (NIM-197) and are
// nil-safe: a nil enforcer makes the gate count the decision as `unconfigured`
// rather than silently allow or deny it. soulReader is the souls read surface
// the console gate resolves the host's covens through (NIM-650); nil-safe, and
// nil means coven-scoped `soul.console` fails closed on this route. scoper is
// the read-side scope boundary (NIM-841); nil hides every errand rather than
// showing all of them.
func NewErrandHandler(dispatcher *errand.Dispatcher, store *errand.Store, enforcer middleware.PermissionChecker, gate *shellgate.Gate, soulReader SoulHostContextReader, scoper PurviewResolver, logger *slog.Logger) *ErrandHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ErrandHandler{dispatcher: dispatcher, store: store, enforcer: enforcer, gate: gate, soulReader: soulReader, scoper: scoper, logger: logger}
}

// ErrandSpecStub — a non-empty *ErrandHandler stub for generating the huma OpenAPI
// fragment (HumaErrandSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its no-op nil check. dispatcher/store are nil —
// the handler never executes in spec mode (parity [AugurSpecStub]).
func ErrandSpecStub() *ErrandHandler {
	return &ErrandHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ErrandRunInput — NATIVE request shape for POST /v1/souls/{sid}/exec (handler-native
// T5d-2c-full). Replaces ErrandRunRequest: the SOUL-domain huma input (huma_soul.go)
// binds/validates the body and projects it into these fields before calling ExecTyped.
// Optional fields are pointers (Input/TimeoutSeconds/DryRun); the handler dereferences
// them into errand.DispatchRequest.
type ErrandRunInput struct {
	Module         string
	Input          *map[string]any
	TimeoutSeconds *int
	DryRun         *bool
}

// ErrandResultView — FLAT wire shape of an Errand result (200 body for errand-get
// terminal / sync-exec / a list element), handler-native (replaces ErrandResult).
// Optional fields are pointers with nil-when-empty (parity with the former omitempty);
// status is a flat domain-status string; started_at/finished_at are UTC+Truncate(Second)
// (byte-exact with the former second-precision RFC3339). The api package projects
// ErrandResultView → the native ErrandResult schema (register func huma_errand.go); the
// native type pins wire field order.
type ErrandResultView struct {
	DurationMs      *int64
	ErrandID        string
	ErrorMessage    *string
	ExitCode        *int32
	FinishedAt      *time.Time
	Module          string
	Output          *map[string]interface{}
	SID             string
	StartedAt       time.Time
	StartedByAID    string
	Status          string
	Stderr          *string
	StderrTruncated *bool
	Stdout          *string
	StdoutTruncated *bool
}

// ErrandListPage — domain paged result of GET /v1/errands (handler-native). Flat
// offset/limit/total + a slice of ErrandResultView; the api package projects it into the
// native ErrandListReply envelope (register func huma_errand.go).
type ErrandListPage struct {
	Items  []ErrandResultView
	Offset int
	Limit  int
	Total  int
}

// ErrandAcceptedView — flat 202 body for async escalation (sync cap exceeded) /
// errand-get-running. errand_id + a string status (domain status errand.Status). Serialized
// on the wire by the register func (api projects into native ErrandAccepted byte-exact).
type ErrandAcceptedView struct {
	ErrandID string
	Status   string
}

// newErrandAcceptedView builds the flat 202 body from errand_id + domain status.
func newErrandAcceptedView(errandID string, status errand.Status) ErrandAcceptedView {
	return ErrandAcceptedView{ErrandID: errandID, Status: string(status)}
}

// ErrandExecReply — extracted result of [ErrandHandler.ExecTyped] (handler-native
// T5d-2c-full). Async=true → 202 Accepted body + Location header; otherwise a 200 Result
// body (terminal reached before the server cap). Carries audit fields (event errand.invoked)
// — duplicated for the security navigation-trail middleware (the dispatcher itself writes the
// audit event source=api inside Dispatch).
type ErrandExecReply struct {
	Async    bool
	ErrandID string
	Result   ErrandResultView
	Accepted ErrandAcceptedView
	// audit fields (parity with legacy SetAuditPayload).
	sid        string
	module     string
	timeoutSec int
	dryRun     bool
}

// AuditPayload collects the audit fields of the errand.invoked route (parity with legacy:
// sid/module/errand_id/timeout_seconds/dry_run). Source for huma variant B.
func (r ErrandExecReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"sid":             r.sid,
		"module":          r.module,
		"errand_id":       r.ErrandID,
		"timeout_seconds": r.timeoutSec,
		"dry_run":         r.dryRun,
	}
}

// ExecTyped — domain function for POST /v1/souls/{sid}/exec (handler-native): SID validation
// (soul.ValidSID→422) + dispatcher.Dispatch + sentinel→problem. req is the native request
// shape (the SOUL-domain huma_soul.go binds/validates the body and projects into it; huma
// rejects unknown → 400 before the call). Errors are *problemError (422 invalid sid / empty
// module / timeout out of range; 404 soul-not-connected; 400 dry_run on a verb module; 500
// dispatcher nil / DB failure); success is [ErrandExecReply] (200 sync / 202 async).
func (h *ErrandHandler) ExecTyped(ctx context.Context, claims *keeperjwt.Claims, sid string, req ErrandRunInput) (ErrandExecReply, error) {
	var zero ErrandExecReply
	if h.dispatcher == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand dispatcher is not configured")}
	}
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}
	if err := h.authorizeShell(ctx, claims.Subject, sid, req.Module); err != nil {
		return zero, err
	}

	var input map[string]any
	if req.Input != nil {
		input = *req.Input
	}
	var timeoutSec int
	if req.TimeoutSeconds != nil {
		timeoutSec = *req.TimeoutSeconds
	}
	dryRun := req.DryRun != nil && *req.DryRun

	res, err := h.dispatcher.Dispatch(ctx, errand.DispatchRequest{
		SID:          sid,
		Module:       req.Module,
		Input:        input,
		TimeoutSec:   timeoutSec,
		DryRun:       dryRun,
		StartedByAID: claims.Subject,
	})
	if err != nil {
		return zero, h.dispatchError(err)
	}

	reply := ErrandExecReply{
		Async:      res.Async,
		ErrandID:   res.ErrandID,
		sid:        sid,
		module:     req.Module,
		timeoutSec: timeoutSec,
		dryRun:     dryRun,
	}
	if res.Async {
		reply.Accepted = newErrandAcceptedView(res.ErrandID, res.Status)
	} else {
		reply.Result = dispatchResultView(&res, sid, req.Module, claims.Subject)
	}
	return reply, nil
}

// ErrandGetReply — extracted result of [ErrandHandler.GetTyped] (handler-native).
// Running=true → 202 Accepted body (Errand still running, async poll); otherwise a 200
// Result body (terminal). One of the two fields is meaningful per the Running flag.
type ErrandGetReply struct {
	Running  bool
	Accepted ErrandAcceptedView
	Result   ErrandResultView
}

// GetTyped — domain function for GET /v1/errands/{errand_id} (READ with path, no audit):
// path-id validation + store.Get + scope + sentinel→problem. running → 202 Accepted,
// terminal → 200 Result. Errors are *problemError (404/422/500); store nil → 500.
//
// An errand on a host outside the caller's `errand.list` purview answers with the
// SAME 404 an unknown id gets (NIM-841), deliberately: a 403 would confirm that an
// errand with that id exists, turning the route into an oracle over which hosts have
// been commanded and when. [ConsoleRecordingHandler.load] answers the same way for
// the same reason.
func (h *ErrandHandler) GetTyped(ctx context.Context, claims *keeperjwt.Claims, errandID string) (ErrandGetReply, error) {
	var zero ErrandGetReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand store is not configured")}
	}
	if errandID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'errand_id' is required")}
	}
	row, err := h.store.Get(ctx, errandID)
	if err != nil {
		if errors.Is(err, errand.ErrNotFound) {
			return zero, errandNotFound(errandID)
		}
		h.logger.Error("errand.get: store failed",
			slog.String("errand_id", errandID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get errand failed")}
	}
	if !h.inScope(claims, "list", row) {
		return zero, errandNotFound(errandID)
	}
	if row.Status == errand.StatusRunning {
		return ErrandGetReply{Running: true, Accepted: newErrandAcceptedView(row.ErrandID, row.Status)}, nil
	}
	return ErrandGetReply{Result: rowToErrandResultView(row)}, nil
}

// ErrandListInput — validated input for GET /v1/errands. sid/status are string filters
// (format/enum validation in ListTyped → 422); StartedAfter is time.Time (zero = no filter,
// a bad value is already rejected at the bind phase → 400); Modules is a multi-value
// exact-match OR; Offset/Limit are pagination.
type ErrandListInput struct {
	SID          string
	Status       string
	StartedAfter time.Time
	Modules      []string
	Offset       int
	Limit        int
}

// ListTyped — domain function for GET /v1/errands (READ with typed query, no audit).
// offset/limit are validated by huma bind; the range is enforced by CheckPageBounds → 400
// (parity with ParsePage). sid format / status enum → 422. StartedAfter — a bad value is
// already rejected by huma-bind date-time (400). A read error → *problemError (500); store
// nil → 500.
//
// The caller's `errand.list` purview is pushed into the query, not applied to the
// rows that come back (NIM-841): `total` is counted under the same WHERE, so a
// post-filter would hide the out-of-scope errands and still publish how many of
// them exist. An operator entitled to no host gets an empty page with total=0.
func (h *ErrandHandler) ListTyped(ctx context.Context, claims *keeperjwt.Claims, in ErrandListInput) (ErrandListPage, error) {
	var zero ErrandListPage
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand store is not configured")}
	}
	if err := api.CheckPageBounds(in.Offset, in.Limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	var filter errand.ListFilter
	if in.SID != "" {
		if !soul.ValidSID(in.SID) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"query 'sid' must match "+soul.SIDPattern)}
		}
		filter.SID = in.SID
	}
	if in.Status != "" {
		if !validErrandStatus(in.Status) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"query 'status' must be one of running/success/failed/timed_out/cancelled/module_not_allowed")}
		}
		filter.Status = errand.Status(in.Status)
	}
	filter.StartedAfter = in.StartedAfter
	if len(in.Modules) > 0 {
		// Exact-match OR, no regex/glob (by spec). Duplicates are allowed — they pass
		// into the IN predicate as-is, PG normalizes.
		filter.Modules = in.Modules
	}
	scope := h.scopeFor(claims, "list")
	filter.Scope = func(startIdx int) (string, []any, int) {
		return scope.WhereSQL(errandScopeColumns, startIdx)
	}

	rows, total, err := h.store.List(ctx, filter, in.Offset, in.Limit)
	if err != nil {
		h.logger.Error("errand.list: store failed",
			slog.Any("filter", filter), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list errands failed")}
	}

	items := make([]ErrandResultView, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToErrandResultView(r))
	}
	return ErrandListPage{
		Items:  items,
		Offset: in.Offset,
		Limit:  in.Limit,
		Total:  total,
	}, nil
}

// ErrandCancelReply — extracted result of [ErrandHandler.CancelTyped] (handler-native).
// Carries audit fields (the HTTP response is an empty 204 body).
type ErrandCancelReply struct {
	ErrandID string
}

// AuditPayload collects the audit payload of the errand.cancel route (parity with legacy:
// errand_id). The dispatcher already writes the audit event source=api itself — the payload
// is duplicated for the security navigation-trail middleware. Source for huma variant B.
func (r ErrandCancelReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"errand_id": r.ErrandID}
}

// CancelTyped — domain function for DELETE /v1/errands/{errand_id} (handler-native):
// path-id validation + scope + dispatcher.Cancel + sentinel→problem. Errors are
// *problemError; success is [ErrandCancelReply]. dispatcher nil → 500.
//
// The target host is resolved and checked against the caller's `errand.cancel`
// purview BEFORE anything is dispatched (NIM-841): the route gate can only ask
// whether the right is held at all, because the SID is not in the path. An errand
// outside the boundary answers with the same 404 an unknown id gets, so the refusal
// cannot be used to probe what is running where.
//
// The row is read here even though [errand.Dispatcher.Cancel] reads it again: this
// read decides authorization, its read decides the running→terminal race, and
// collapsing them would make one of the two answer the wrong question.
func (h *ErrandHandler) CancelTyped(ctx context.Context, claims *keeperjwt.Claims, errandID string) (ErrandCancelReply, error) {
	var zero ErrandCancelReply
	if h.dispatcher == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand dispatcher is not configured")}
	}
	if errandID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'errand_id' is required")}
	}
	if h.store == nil {
		// Fail-closed: without the store the target host is unknowable, and
		// cancelling a command on an unknown host is exactly what the scope check
		// exists to prevent.
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand store is not configured")}
	}
	row, err := h.store.Get(ctx, errandID)
	if err != nil {
		if errors.Is(err, errand.ErrNotFound) {
			return zero, errandNotFound(errandID)
		}
		h.logger.Error("errand.cancel: store failed",
			slog.String("errand_id", errandID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand cancel failed")}
	}
	if !h.inScope(claims, "cancel", row) {
		return zero, errandNotFound(errandID)
	}
	if err := h.dispatcher.Cancel(ctx, errand.CancelRequest{
		ErrandID:    errandID,
		RequestedBy: claims.Subject,
	}); err != nil {
		return zero, h.cancelError(errandID, err)
	}
	return ErrandCancelReply{ErrandID: errandID}, nil
}

// cancelError maps errand.Dispatcher.Cancel sentinels to *problemError (delivered by the
// huma wrapper via AsProblemDetails).
func (h *ErrandHandler) cancelError(errandID string, err error) error {
	switch {
	case errors.Is(err, errand.ErrEmptyErrandID):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "path 'errand_id' is required")}
	case errors.Is(err, errand.ErrNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "errand "+errandID+" not found")}
	case errors.Is(err, errand.ErrErrandTerminal):
		return &problemError{problem.New(problem.TypeErrandNotCancellable, "",
			"errand "+errandID+" is already in a terminal state")}
	case errors.Is(err, errand.ErrSoulNotConnected):
		return &problemError{problem.New(problem.TypeNotFound, "", "target soul is not connected to the cluster")}
	default:
		h.logger.Error("errand.cancel: dispatcher failed",
			slog.String("errand_id", errandID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "errand cancel failed")}
	}
}

// rowToErrandResultView projects [errand.Row] into a flat [ErrandResultView]
// (GET /v1/errands/{id} terminal + a list element).
//
// date-time: the former wire was second-precision (`.Format(time.RFC3339)`), so
// `.UTC().Truncate(time.Second)` keeps it byte-for-byte. Optional fields
// (stdout/stderr/error_message — string omitempty; truncated flags — bool omitempty
// in the spec) are projected via nil-when-empty (parity with the former omitempty).
func rowToErrandResultView(row *errand.Row) ErrandResultView {
	res := ErrandResultView{
		ErrandID:        row.ErrandID,
		SID:             row.SID,
		Module:          row.Module,
		Status:          string(row.Status),
		ExitCode:        row.ExitCode,
		Stdout:          ptrIfNotEmpty(row.Stdout),
		Stderr:          ptrIfNotEmpty(row.Stderr),
		StdoutTruncated: ptrBoolIfTrue(row.StdoutTruncated),
		StderrTruncated: ptrBoolIfTrue(row.StderrTruncated),
		DurationMs:      row.DurationMs,
		ErrorMessage:    ptrIfNotEmpty(row.ErrorMessage),
		Output:          ptrMapIfNotEmpty(row.Output),
		StartedByAID:    row.StartedByAID,
		StartedAt:       row.StartedAt.UTC().Truncate(time.Second),
	}
	if row.FinishedAt != nil {
		fin := row.FinishedAt.UTC().Truncate(time.Second)
		res.FinishedAt = &fin
	}
	return res
}

// dispatchResultView builds the sync 200 body of POST /v1/souls/{sid}/exec from
// [errand.DispatchResult] (terminal reached before the server cap). sid/module/aid
// come from the request — DispatchResult does not carry them. StartedAt is the real
// moment of the errands-row INSERT (DispatchResult.StartedAt, the same Clock()-now
// persisted in Row.StartedAt), not the response-build time. This way the sync
// `started_at` matches what a subsequent GET /v1/errands/{id} returns.
func dispatchResultView(res *errand.DispatchResult, sid, module, startedByAID string) ErrandResultView {
	return ErrandResultView{
		ErrandID:        res.ErrandID,
		SID:             sid,
		Module:          module,
		Status:          string(res.Status),
		ExitCode:        res.ExitCode,
		Stdout:          ptrIfNotEmpty(res.Stdout),
		Stderr:          ptrIfNotEmpty(res.Stderr),
		StdoutTruncated: ptrBoolIfTrue(res.StdoutTruncated),
		StderrTruncated: ptrBoolIfTrue(res.StderrTruncated),
		DurationMs:      res.DurationMs,
		ErrorMessage:    ptrIfNotEmpty(res.ErrorMessage),
		Output:          ptrMapIfNotEmpty(res.Output),
		StartedByAID:    startedByAID,
		StartedAt:       res.StartedAt.UTC().Truncate(time.Second),
	}
}

// ptrBoolIfTrue returns nil for false (parity with omitempty over bool — the field is
// omitted), otherwise a pointer to true. truncated flags are optional in the spec.
func ptrBoolIfTrue(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

// dispatchError maps dispatcher.Dispatch sentinel errors to *problemError
// (delivered by the huma wrapper via AsProblemDetails). Path in problem.Details is
// empty — filled on output.
func (h *ErrandHandler) dispatchError(err error) error {
	// Carries the offending module, so the refusal can name it without the mapper
	// re-reading the request (the same wording serves MCP and Voyage).
	var verbShell *errand.DryRunVerbShellError

	switch {
	case errors.Is(err, errand.ErrSIDEmpty):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "sid is empty")}
	case errors.Is(err, errand.ErrModuleEmpty):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "field 'module' is required")}
	case errors.Is(err, errand.ErrTimeoutOutOfRange):
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'timeout_seconds' must be in ["+strconv.Itoa(errand.MinTimeoutSeconds)+", "+strconv.Itoa(errand.MaxTimeoutSeconds)+"]")}
	case errors.Is(err, errand.ErrSoulNotConnected):
		return &problemError{problem.New(problem.TypeNotFound, "", "target soul is not connected to the cluster")}
	case errors.As(err, &verbShell):
		// 400, not one of the 409 capability types: nothing about the cluster or the
		// target can make this pair work, so it is the request that is wrong
		// (ADR-033 contract row, NIM-489).
		return &problemError{problem.New(problem.TypeMalformedRequest, "", verbShell.Detail())}
	case errors.Is(err, errand.ErrDryRunNotAnnounced):
		return &problemError{problem.New(problem.TypeSoulCapabilityUnsupported, "",
			"the target soul's announced capability set does not include 'dry_run', so keeper cannot rule out a binary "+
				"that ignores the flag and applies for real; refused before dispatch. Usually the agent predates the "+
				"flag and needs updating; if it is current, its announcement never reached keeper's presence store and "+
				"reconnecting the agent republishes it")}
	case errors.Is(err, errand.ErrDryRunUnverifiable):
		return &problemError{problem.New(problem.TypeSoulCapabilityUnsupported, "",
			"cannot confirm the target soul honors 'dry_run' (the presence source is unavailable), and dispatching "+
				"unconfirmed would risk a real apply on a host you asked only to read - refused fail-closed")}
	default:
		h.logger.Error("errand.exec: dispatcher failed", slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "errand dispatch failed")}
	}
}

// errandScopeColumns maps an errand onto the RBAC scope dimensions. The aliases
// come from the store rather than being repeated here: a predicate naming a
// column the query does not have is a runtime SQL error on a security path, and
// two copies of the same strings is how that happens.
//
// Service and incarnation stay empty — an errand carries neither, so a condition
// on them renders FALSE (fail-closed).
var errandScopeColumns = rbac.ScopeColumns{
	Coven:  errand.ScopeCovenColumn,
	Host:   errand.ScopeHostColumn,
	Traits: errand.ScopeTraitsColumn,
}

// scopeFor resolves the caller's `errand.<action>` purview.
//
// The action is the one being performed — `list` for the two reads, `cancel` for
// the abort — rather than one boundary shared by both. They are separate rights
// in the catalogue, so folding them together would let whichever is granted more
// widely decide for the other.
//
// Purview, never [rbac.Enforcer.Check]: Check takes a context map and looks like
// it honours it, but a role's `default_scope` lives outside the permission it
// matches, so a bare `errand.list` passes Check for ANY host (ADR-047 — Check is
// the route gate, scope is resolved here). No claims / no scoper → a zero
// Purview, which renders FALSE and hides everything.
func (h *ErrandHandler) scopeFor(claims *keeperjwt.Claims, action string) soulpurview.Scope {
	if claims == nil || h.scoper == nil {
		return soulpurview.Scope{}
	}
	return soulpurview.Resolve(h.scoper.ResolvePurview(claims.Subject, "errand", action))
}

// inScope is the single-object half of [ErrandHandler.scopeFor] — the gate for
// the two routes that reach one errand by id. It must keep answering alike with
// the list pushdown: a single-object gate reading a wider set than the list
// predicate would 200 on an errand the list never showed.
func (h *ErrandHandler) inScope(claims *keeperjwt.Claims, action string, row *errand.Row) bool {
	return soulpurview.InScope(h.scopeFor(claims, action), row.SID, row.Covens,
		soulpurview.TraitsFromJSON(row.TraitsRaw))
}

// errandNotFound — the single refusal for "absent" and "out of scope".
func errandNotFound(errandID string) error {
	return &problemError{problem.New(problem.TypeNotFound, "", "errand "+errandID+" not found")}
}

// validErrandStatus — closed enum for the query filter. Matches the CHECK
// errands_status_valid list + the StatusXxx constants of the errand package.
// Single source of truth (validated right at query parsing).
func validErrandStatus(s string) bool {
	switch errand.Status(s) {
	case errand.StatusRunning, errand.StatusSuccess, errand.StatusFailed,
		errand.StatusTimedOut, errand.StatusCancelled, errand.StatusModuleNotAllowed:
		return true
	}
	return false
}

// authorizeShell applies the console gate to one exec call (ADR-0074 amendment,
// NIM-197). A non-verb-shell module returns nil without touching the enforcer;
// a verb-shell one additionally requires `soul.console` over the same shape of
// context set the route's `errand.run` middleware used — `host=<sid>` plus one
// context per Coven label of the host, granted if ANY ONE of them matches
// (NIM-650). The set is resolved again here rather than carried over: the
// middleware runs before the module is known, so it cannot decide whether this
// second right will be asked for at all.
//
// The covens are read INSIDE the probe, not before it: an ordinary Errand is not
// narrowed by this gate at all, so it must not pay a SECOND round-trip on top of
// the one the route's gate already paid, for a check that never runs.
//
// nil enforcer → no probe is supplied, and the gate records `unconfigured`
// rather than assuming an answer.
func (h *ErrandHandler) authorizeShell(ctx context.Context, aid, sid, module string) error {
	var check func() error
	if h.enforcer != nil {
		check = func() error {
			return soul.AllowAnyContext(h.enforcer, aid, "soul", "console",
				soul.HostContextsBySID(ctx, h.soulReader, sid))
		}
	}
	if err := h.gate.Authorize(shellgate.SurfaceREST, module, check); err != nil {
		return &problemError{problem.New(problem.TypeForbidden, "",
			"module "+module+" runs an arbitrary command line; it additionally requires permission soul.console")}
	}
	return nil
}
