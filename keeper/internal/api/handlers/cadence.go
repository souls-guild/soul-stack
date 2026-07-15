package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// CadenceStore is the [cadence] package surface for the S4 HTTP/MCP handler.
//
//   - cadence.ExecQueryRower — read + single-step writes (Get/List/Update/Delete/
//     SetEnabled without a transaction: one cadences row, FK voyages.cadence_id on
//     the child row).
//   - voyage.ExecQueryRower — `GET /v1/cadences/{id}/runs` does a read-only
//     voyage.List over the same pool (CopyFrom is not called on the read path but
//     is part of the voyage interface).
//   - BeginTx — atomic Create with a notify block (ADR-052 §m): Insert Cadence +
//     InsertTiding of the permanent rules from notify[] in ONE PG tx (both rows or
//     rollback — otherwise an orphaned rule/schedule missing its other half).
//
// A real *pgxpool.Pool satisfies all of them; unit tests use a fake.
type CadenceStore interface {
	cadence.ExecQueryRower
	voyage.ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// CadenceHandler serves the Cadence endpoints (ADR-046 §6, S4):
//
//	POST   /v1/cadences              — create a Cadence (two-tier RBAC by kind).
//	GET    /v1/cadences              — paged list (filter enabled/kind).
//	GET    /v1/cadences/{id}         — detail.
//	PATCH  /v1/cadences/{id}         — update (recipe/schedule/enabled toggle).
//	DELETE /v1/cadences/{id}         — remove the schedule (child Voyages remain).
//	POST   /v1/cadences/{id}/enable  — enable (pause/resume, ADR-046 §6).
//	POST   /v1/cadences/{id}/disable — disable.
//	GET    /v1/cadences/{id}/runs    — child Voyages (reuse Voyage DTO).
//
// Two-tier RBAC (ADR-046 §7, security-critical fail-closed): `cadence.*` governs
// the schedule itself, but the recipe spawns a Voyage, so on CREATE the creator
// must also hold the Voyage permission for the recipe `kind`
// (scenario→incarnation.run, command→errand.run, ADR-043 §6) — otherwise a Cadence
// would be a privilege-escalation bypass of RBAC. `cadence.create` is gated in the
// router (middleware); the Voyage permission by kind is visible only from the body
// → checked INSIDE Create. list/get/update/delete/enable/disable are gated by the
// middleware route (cadence.list / cadence.update / cadence.delete). `/runs` is
// incarnation.history (read runtime run state, parity with Voyage list).
//
// store/enforcer are required for production routes; auditW may be nil (dev
// without audit). Resolving next_run_at is pure [cadence.NextRun] (no DB).
//
// scenarioResolver/incReader — per-target coven scope-check for a kind=scenario
// recipe (ADR-046 §7, security-critical fail-closed): the Cadence target must lie
// in the creator's RBAC scope at create/edit time, otherwise a scoped Archon "run
// on coven=A" would create a Cadence on coven=B (out of scope) and the background
// spawn (as created_by_aid) would execute out of scope = RBAC bypass. Full parity
// with VoyageHandler.createScenario: the same incarnation resolve + per-incarnation
// scope loop. incReader=nil → fail-closed (like Voyage): scoped roles are rejected,
// only a bare/`*` right from checkKindPermission passes. command-kind scope is a
// bare-check (parity with Voyage errand.run NoSelector in MVP).
type CadenceHandler struct {
	store            CadenceStore
	scenarioResolver VoyageScenarioResolver
	incReader        IncarnationContextReader
	enforcer         middleware.PermissionChecker
	auditW           audit.Writer
	// tidingInvalidator resets the dispatcher's TTL snapshot of Tiding rules after
	// the cadence-tx with a notify block commits (ADR-052 §m, the same race-fix as
	// Voyage-ephemeral): the permanent rules are inserted via a direct
	// herald.InsertTiding, bypassing herald.Service CRUD (and its invalidation), so
	// the dispatcher holds them behind a TTL snapshot (15s) — without an explicit
	// reset a fast/cross-keeper spawn dispatches the terminal against a stale
	// snapshot and the notification silently misses. Same *herald.Service instance
	// as VoyageHandler. nil (dev without herald) → no-op.
	tidingInvalidator TidingInvalidator
	// pollFloorSeconds is the lower bound on the period of an interval Cadence
	// (floor limit, ADR-046 Pass B). Single source with the Conductor's adaptive
	// poll (cfg.CadenceScheduler.ResolvedPollFloor()). 0 → floor check disabled.
	pollFloorSeconds int
	logger           *slog.Logger
}

// NewCadenceHandler assembles the handler. logger=nil → discard. store/enforcer
// are required for production routes; auditW may be nil. scenarioResolver/incReader
// are the same instances as [VoyageHandler], for the per-target scope-check of a
// kind=scenario recipe (ADR-046 §7). incReader=nil → fail-closed: scoped roles are
// rejected for scenario create/patch (like Voyage). tidingInvalidator is the same
// *herald.Service as VoyageHandler: it resets the dispatcher's TTL snapshot after
// the cadence-tx with a notify block commits (ADR-052 §m race-fix); nil → no-op
// (dev without herald).
func NewCadenceHandler(
	store CadenceStore,
	scenarioResolver VoyageScenarioResolver,
	incReader IncarnationContextReader,
	enforcer middleware.PermissionChecker,
	auditW audit.Writer,
	tidingInvalidator TidingInvalidator,
	pollFloorSeconds int,
	logger *slog.Logger,
) *CadenceHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &CadenceHandler{
		store:             store,
		scenarioResolver:  scenarioResolver,
		incReader:         incReader,
		enforcer:          enforcer,
		auditW:            auditW,
		tidingInvalidator: tidingInvalidator,
		pollFloorSeconds:  pollFloorSeconds,
		logger:            logger,
	}
}

// --- POST /v1/cadences ---

// cadenceCreateRequest — POST body. snake_case, unknown fields rejected. The run
// recipe is the same set as [voyageCreateRequest] (kind/scenario_name|module/
// target/input/batch settings) + a repeat rule + overlap_policy. target is
// serialized into jsonb as-is (resolved on spawn, not on create, ADR-046 §3).
type cadenceCreateRequest struct {
	Name            string `json:"name"`
	Enabled         *bool  `json:"enabled,omitempty"`
	ScheduleKind    string `json:"schedule_kind"`
	IntervalSeconds *int   `json:"interval_seconds,omitempty"`
	CronExpr        string `json:"cron_expr,omitempty"`
	OverlapPolicy   string `json:"overlap_policy"`

	// Run recipe (parity with voyageCreateRequest).
	Kind         string               `json:"kind"`
	ScenarioName string               `json:"scenario_name,omitempty"`
	Module       string               `json:"module,omitempty"`
	Input        map[string]any       `json:"input,omitempty"`
	Target       *voyageTargetRequest `json:"target"`
	// Batch — string batch size ("N" hosts/incarnations / "N%" of spawn-scope),
	// parity with voyageCreateRequest.Batch (ADR-043 amendment). Maps onto the
	// batch_size|batch_percent columns (see applyCadenceBatchSpec): "N%" →
	// batch_percent (resolved on spawn-scope in BuildVoyage), "N" → batch_size.
	// Conflicts with batch_size/batch_percent → 422.
	Batch        *string `json:"batch,omitempty"`
	BatchSize    *int    `json:"batch_size,omitempty"`
	BatchPercent *int    `json:"batch_percent,omitempty"`
	Concurrency  *int    `json:"concurrency,omitempty"`
	BatchMode    string  `json:"batch_mode,omitempty"`
	// MaxFailures — string failure threshold ("N" absolute / "N%" percent of
	// spawn-scope), parity with voyageCreateRequest.MaxFailures (ADR-043 amendment
	// 2026-06-09). Maps onto the fail_threshold|fail_threshold_percent columns (see
	// applyCadenceMaxFailures): "N%" → fail_threshold_percent (resolved on
	// spawn-scope in BuildVoyage — a Cadence's scope is unknown at create, unlike
	// Voyage), "N" → fail_threshold. Conflicts with fail_threshold → 422.
	MaxFailures          *string `json:"max_failures,omitempty"`
	FailThreshold        *int    `json:"fail_threshold,omitempty"`
	InterBatchIntervalMS *int    `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int    `json:"inter_unit_interval_ms,omitempty"`
	RequireAlive         *bool   `json:"require_alive,omitempty"`
	OnFailure            string  `json:"on_failure,omitempty"`

	// Notify — subscriptions for notifications about runs of THIS schedule (ADR-052
	// §m). Unlike voyage.notify[] (a one-off ephemeral rule for a single run), here
	// each element materializes into a PERMANENT Tiding (ephemeral=false), bound by
	// the schedule ULID (cadences.id) — with a Cadence selector (filter "only send
	// for runs of this schedule") + an origin marker created_from_cadence_id
	// (cascade removal when the Cadence is deleted, ADR-046 §9). Rules are inserted
	// in the SAME tx as the Cadence. nil/empty ⇒ no notifications. Element shape is
	// the same [voyageNotifyRequest] (herald/on/only_failures/only_changes/
	// annotations/projection), reusing validation/RBAC.
	Notify []voyageNotifyRequest `json:"notify,omitempty"`

	// failThresholdPercent — the bare percent from max_failures="N%", filled by
	// applyCadenceMaxFailures for writing into the fail_threshold_percent column
	// (resolved to an absolute on spawn-scope in BuildVoyage). Not a JSON field:
	// set only via max_failures.
	failThresholdPercent *int
}

// cadenceCreateReply — native 201 body (handler-native T5d). Flat shape 1:1 with
// the former CadenceCreateReply (all scalars; next_run_at is *time.Time with
// omitempty). Serialized directly and projected into api.CadenceCreateReply (huma
// schema).
type cadenceCreateReply struct {
	CadenceID string     `json:"cadence_id"`
	Enabled   bool       `json:"enabled"`
	Location  string     `json:"location"`
	Name      string     `json:"name"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

// CadenceCreateReply / CadenceCreateRequest — exported aliases of the domain
// forms of POST /v1/cadences for the FULL-TYPED huma envelope (ADR-054 §Pattern):
// the api package (huma_cadence.go) assembles CadenceCreateRequest from the typed
// huma body and calls [CadenceHandler.CreateTyped], receiving CadenceCreateReply.
// Aliases (not new types) — the same shape decoded by the legacy (w,r) Create;
// nested target/notify are [VoyageTargetRequest]/[VoyageNotifyRequest] (shared
// with Voyage).
type (
	CadenceCreateReply   = cadenceCreateReply
	CadenceCreateRequest = cadenceCreateRequest
	VoyageTargetRequest  = voyageTargetRequest
	VoyageNotifyRequest  = voyageNotifyRequest
	// CadencePatchRequest / CadenceDTO / CadenceEnabledReply — exported aliases of
	// the domain forms of the cadence REST routes (PATCH/DELETE/enable/disable) for
	// the FULL-TYPED huma envelope (ADR-054, batch-2f self-audit): the api package
	// (huma_cadence_op.go) assembles CadencePatchRequest from the typed huma body
	// and calls [CadenceHandler.PatchTyped], receiving CadenceDTO; SetEnabledTyped
	// returns CadenceEnabledReply. Aliases (not new types) — the same shape decoded
	// by the legacy (w,r) Patch/setEnabled.
	CadencePatchRequest = cadencePatchRequest
	CadenceDTO          = cadenceDTO
	CadenceEnabledReply = cadenceEnabledReply
)

// cadenceEnabledReply — native 200 body for POST /v1/cadences/{id}/enable|/disable
// (handler-native T5d). Flat shape 1:1 with the former CadenceEnabledReply
// (cadence_id + enabled). Serialized directly and projected into api.CadenceEnabledReply.
type cadenceEnabledReply struct {
	CadenceID string `json:"cadence_id"`
	Enabled   bool   `json:"enabled"`
}

// CadenceListReply — typed output of GET /v1/cadences: paged cadenceDTO of the
// same wire shape as the legacy (w,r) List (items/offset/limit/total via
// sharedapi.PagedResponse → byte-exact). Alias (not a new type) — exported for the
// FULL-TYPED huma envelope (huma_cadence_list_op.go).
type CadenceListReply = sharedapi.PagedResponse[cadenceDTO]

// problemError — typed wrapper over [problem.Details] returned from the extracted
// domain functions (FULL-TYPED unfolding, ADR-054 §Pattern (b)). A domain function
// (e.g. [CadenceHandler.CreateTyped]) returns (zeroReply, &problemError{<details>})
// instead of problem.Write(w, …); the caller decides how to deliver it:
//
//   - the huma envelope (api package) extracts .Details and returns it via
//     humaProblemError (the single problem+json huma error override);
//   - the thin (w,r) handler shell (for the strict bridge / other callers) writes
//     problem.Write(w, pe.Details) with instance=r.URL.Path set.
//
// The humaProblemError family type (also carrying problem.Details) shares the
// contract "error = problem.Details" on both sides of the boundary.
type problemError struct {
	Details problem.Details
}

func (e *problemError) Error() string { return e.Details.Detail }

// asProblemError extracts [problem.Details] from a domain-function error. ok=false
// — the error is not a domain problem (unexpected path): the caller maps it to 500.
func asProblemError(err error) (problem.Details, bool) {
	var pe *problemError
	if errors.As(err, &pe) {
		return pe.Details, true
	}
	return problem.Details{}, false
}

// AsProblemDetails — exported extractor of [problem.Details] from an extracted
// domain-function error (FULL-TYPED ADR-054 §Pattern): the huma envelope (api
// package) maps a domain *problemError into problem+json. ok=false — a non-problem
// error → the caller returns 500.
func AsProblemDetails(err error) (problem.Details, bool) {
	return asProblemError(err)
}

// CadenceSpecStub — a non-nil *CadenceHandler stub for generating the huma OpenAPI
// fragment (HumaCadenceSpecYAML): on dump the domain handler is not called, but
// huma.Register requires non-nil for its no-op check. All dependencies nil — the
// handler never executes in spec mode.
func CadenceSpecStub() *CadenceHandler {
	return &CadenceHandler{}
}

// CreateTyped — extracted domain function for POST /v1/cadences (FULL-TYPED
// unfolding, ADR-054 §Pattern (b)): all business logic without
// http.ResponseWriter/*http.Request. claims and req arrive as arguments
// (decode/auth on the caller); errors are returned as *problemError (problem.Write
// → return), success as cadenceCreateReply.
//
// Steps (parity with the former Create(w,r)): RBAC by kind + per-target scope →
// buildCadence → floor limit → next_run_at → notify[] → persist (tx + notify +
// invalidation) → audit emit (self-audit INSIDE the function, before returning the
// reply — the huma wrapper does not touch it, §Audit). ctx is the request context
// (persist/scope-check read it; audit is written on a background ctx inside
// emitWrite, as before).
func (h *CadenceHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req CadenceCreateRequest) (CadenceCreateReply, error) {
	var zero CadenceCreateReply
	if h.store == nil || h.enforcer == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if req.Target == nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'target' is required")}
	}

	// String batch fields (ADR-043 amendment): translate `batch`/`max_failures` into
	// batch_size|batch_percent / fail_threshold|fail_threshold_percent BEFORE RBAC
	// and buildCadence. A string-format conflict with the numeric columns and
	// malformed input → 422.
	if bErr := applyCadenceBatchSpec(&req); bErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bErr)}
	}
	if mfErr := applyCadenceMaxFailures(&req); mfErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", mfErr)}
	}

	// Two-tier guard (ADR-046 §7): Voyage permission by recipe kind.
	if err := h.checkKindPermissionErr(claims.Subject, req.Kind); err != nil {
		return zero, err
	}
	// Per-target coven scope-check (ADR-046 §7, fail-closed).
	if err := h.checkTargetScopeErr(ctx, claims.Subject, req.Kind, req.Target); err != nil {
		return zero, err
	}

	c := h.buildCadence(&req, audit.NewULID(), claims.Subject)

	// Floor limit on the interval Cadence period (ADR-046 Pass B).
	if err := cadence.ValidateIntervalFloor(c, h.pollFloorSeconds); err != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	}

	// next_run_at — a pure function of the schedule (ADR-046 §4).
	if next, err := cadence.NextRun(c, time.Now().UTC()); err == nil {
		c.NextRunAt = &next
	}

	// notify[] → permanent Tiding templates (ADR-052 §m): validation + herald
	// read-guard BEFORE opening the tx.
	notifyTidings, err := h.prepareNotifyErr(ctx, claims, &req, c)
	if err != nil {
		return zero, err
	}

	if err := h.persistErr(ctx, c, notifyTidings); err != nil {
		return zero, err
	}

	h.emitWrite(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventCadenceCreated, c)

	location := "/v1/cadences/" + c.ID
	return cadenceCreateReply{
		CadenceID: c.ID,
		Name:      c.Name,
		Enabled:   c.Enabled,
		NextRunAt: c.NextRunAt,
		Location:  location,
	}, nil
}

// Create — POST /v1/cadences (ADR-046 §6/§7).
//
// Contract:
//   - 201 + {cadence_id, name, enabled, next_run_at, location}.
//   - 400 — invalid JSON.
//   - 403 — two-tier RBAC deny: no Voyage permission for the recipe kind
//     (scenario without incarnation.run / command without errand.run).
//   - 422 — invalid recipe/schedule (XOR interval/cron, enum overlap/kind/
//     batch_mode/on_failure, kind↔scenario_name/module, broken cron, sane bounds) —
//     run through [cadence.validate] (Insert).
//   - 500 — store/enforcer not configured / DB failure.
//
// next_run_at is computed at create ([cadence.NextRun] from now). created_by_aid
// = JWT.sub. audit cadence.created.
func (h *CadenceHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}

	var req cadenceCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, "invalid JSON body: "+err.Error()))
		return
	}

	reply, err := h.CreateTyped(r.Context(), claims, req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}

	middleware.SetAuditPayload(r, middleware.AuditPayload{
		"cadence_id": reply.CadenceID,
		"name":       reply.Name,
		"kind":       req.Kind,
	})
	w.Header().Set("Location", reply.Location)
	writeJSON(w, http.StatusCreated, reply, h.logger)
}

// writeProblemError delivers an extracted domain-function error via problem.Write
// (thin (w,r) shell, FULL-TYPED ADR-054 §Pattern). A domain *problemError → its
// Details with instance=r.URL.Path set (the domain function leaves instance empty
// — it does not know the path). A non-problem error (unexpected path) → 500
// internal.
func writeProblemError(w http.ResponseWriter, r *http.Request, err error) {
	if d, ok := asProblemError(err); ok {
		d.Instance = r.URL.Path
		problem.Write(w, d)
		return
	}
	problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "internal error"))
}

// prepareNotifyErr validates and authorizes the Cadence notify[] block (ADR-052
// §m), assembling the templates for the PERMANENT Tiding rules BEFORE opening the
// transaction (FULL-TYPED ADR-054 §Pattern; on rejection → *problemError, the
// cadence is NOT created). Cap/store checks are explicit *problemError; the notify[]
// shape/validation/RBAC is delegated to the shared extracted core
// [prepareNotifyTidingsErr] (single source with the Voyage-ephemeral path).
// Differences from Voyage: ephemeral=false, binding IMMEDIATELY by the schedule
// ULID (c.ID — Cadence selector + origin marker created_from_cadence_id) and a
// deterministic name <name>-notify[-N]. kind is taken from the Cadence recipe
// (scenario/command — the same values as voyage.Kind).
func (h *CadenceHandler) prepareNotifyErr(
	ctx context.Context, claims *jwt.Claims, req *cadenceCreateRequest, c *cadence.Cadence,
) ([]herald.Tiding, error) {
	if len(req.Notify) == 0 {
		return nil, nil
	}
	// Cap on notify[] length (ADR-052 §m): the permanent rule name is
	// <prefix>-notify-<N> (permanentNotifyName), with prefix truncated by
	// cappedNotifyPrefix leaving room for -NNN (3 digits). Without an explicit cap
	// an array ≥1000 would give the suffix -1000 (4 digits) → name > 63 chars of
	// NamePattern → validateTiding rejects it inside the tx → a murky rollback-500.
	// An explicit cap is a clean 422 BEFORE opening the tx.
	if len(req.Notify) > maxNotifyChannels {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("field 'notify' exceeds %d channels", maxNotifyChannels))}
	}
	if h.store == nil {
		return nil, &problemError{problem.New(problem.TypeInternalError, "",
			"cadence registry is not configured")}
	}
	tidings, perr := prepareNotifyTidingsErr(prepareNotifyDeps{
		store:    h.store,
		enforcer: h.enforcer,
		logName:  "cadence.notify",
		logger:   h.logger,
	}, ctx, claims, req.Notify, voyage.Kind(c.Kind), notifyTidingShape{
		ephemeral:  false,
		cadenceID:  c.ID,
		namePrefix: cappedNotifyPrefix(c.Name),
	})
	if perr != nil {
		return nil, &problemError{*perr}
	}
	return tidings, nil
}

// persistErr writes the Cadence + the permanent Tidings (notify block) in ONE PG
// tx (ADR-052 §m, the same atomic pattern as voyage.persist; FULL-TYPED ADR-054
// §Pattern — errors via *problemError). Cadence first (FK
// tidings.created_from_cadence_id → cadences(id)), then the rules. Any failure
// (including an FK / Tiding name collision) rolls back the whole Create — neither
// Cadence nor rules (atomic by construction). After a commit with notify — a
// two-level invalidation of the dispatcher's TTL snapshot (in-process +
// cross-keeper): the permanent rules are inserted via a direct InsertTiding
// bypassing herald.Service CRUD, and without a reset a fast/cross-keeper spawn
// dispatches the terminal against a stale snapshot (DefaultRuleCacheTTL=15s) → the
// notification silently misses.
//
// notify=empty → one tx with a single Insert Cadence (behavior equivalent to the
// former direct cadence.Insert over the pool — the same 422/404/500 classification
// via writeWriteErrorPtr). ctx is the request context (rollback on a background ctx).
func (h *CadenceHandler) persistErr(ctx context.Context, c *cadence.Cadence, notifyTidings []herald.Tiding) error {
	tx, err := h.store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("cadence.create: begin tx failed",
			slog.String("cadence_id", c.ID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "cadence create failed")}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	// Insert Cadence first (FK tidings.created_from_cadence_id → cadences). Its
	// validate errors (422) and PG failures (500) are classified by
	// writeWriteErrorPtr — the same semantics as the former direct Insert over the
	// pool.
	if err := cadence.Insert(ctx, tx, c); err != nil {
		return &problemError{h.writeWriteErrorPtr("create", c.ID, err)}
	}
	// The permanent Tidings of the notify block — in the SAME tx. A failure
	// (FK / name collision / validation) rolls back the whole Create.
	// herald.InsertTiding accepts the same pgx.Tx (herald.ExecQueryRower ⊂ the tx
	// interface).
	for i := range notifyTidings {
		if err := herald.InsertTiding(ctx, tx, &notifyTidings[i]); err != nil {
			h.logger.Error("cadence.create: insert notify tiding failed",
				slog.String("cadence_id", c.ID),
				slog.String("herald", notifyTidings[i].Herald),
				slog.Any("error", err))
			return &problemError{problem.New(problem.TypeInternalError, "", "cadence create notify failed")}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("cadence.create: commit failed",
			slog.String("cadence_id", c.ID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "cadence create failed")}
	}
	committed = true
	// The permanent rules are inserted via a direct InsertTiding bypassing
	// herald.Service CRUD — reset the dispatcher's TTL snapshot EXPLICITLY, strictly
	// after commit (two-level: in-process + cross-keeper, a Cadence spawn may run on
	// any keeper). Without notify no rules were created — nothing to invalidate.
	// nil-safe: dev without herald → no-op.
	if len(notifyTidings) > 0 && h.tidingInvalidator != nil {
		h.tidingInvalidator.InvalidateTidings(ctx, c.ID)
	}
	return nil
}

// maxNotifyChannels — the ceiling on the number of notify[] channels per schedule
// (ADR-052 §m). The cap keeps the permanent rule name suffix (-<N>, see
// permanentNotifyName) from bloating: with 64 channels the max is 2 digits (-64),
// for which cappedNotifyPrefix reserves room. Exceeding it → 422 BEFORE opening the
// tx (without the cap the name of the ≥1000th rule would exceed NamePattern and
// fail with a murky rollback-500 inside the transaction).
const maxNotifyChannels = 64

// cappedNotifyPrefix reduces a human-readable schedule name to a safe Tiding rule
// name prefix (NamePattern ^[a-z0-9-]{1,63}$): lowercase, disallowed chars → `-`,
// collapse repeated `-`, trim edges. Truncated leaving room for the suffix
// `-notify` (7) + `-<N>` (≤3, index < maxNotifyChannels), so permanentNotifyName
// fits in 63 chars. An empty / degraded-to-nothing result → "cadence" (a
// deterministic fallback; a name collision is still resolved by the -<N> suffix,
// and a UNIQUE-PK race by a rollback of the whole tx).
func cappedNotifyPrefix(name string) string {
	const maxPrefix = 52 // 63 - len("-notify") - len("-NNN")
	var b strings.Builder
	prevDash := false
	for _, ru := range strings.ToLower(name) {
		switch {
		case (ru >= 'a' && ru <= 'z') || (ru >= '0' && ru <= '9'):
			b.WriteRune(ru)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxPrefix {
		out = strings.Trim(out[:maxPrefix], "-")
	}
	if out == "" {
		return "cadence"
	}
	return out
}

// buildCadence assembles a *cadence.Cadence from the request (no validation — that
// is cadence.Insert/Update's job). target is serialized into jsonb as-is; input via
// a separate json.Marshal; ms intervals → time.Duration. enabled defaults to true
// (a schedule without a scheduler is pointless, ADR-046 §4 default-ON). createdByAID
// is pinned to created_by_aid.
func (h *CadenceHandler) buildCadence(req *cadenceCreateRequest, id, createdByAID string) *cadence.Cadence {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	targetJSON, _ := json.Marshal(req.Target)
	var inputJSON []byte
	if req.Input != nil {
		inputJSON, _ = json.Marshal(req.Input)
	}

	c := &cadence.Cadence{
		ID:                   id,
		Name:                 req.Name,
		Enabled:              enabled,
		ScheduleKind:         cadence.ScheduleKind(req.ScheduleKind),
		IntervalSeconds:      req.IntervalSeconds,
		OverlapPolicy:        cadence.OverlapPolicy(req.OverlapPolicy),
		Kind:                 cadence.Kind(req.Kind),
		Target:               targetJSON,
		Input:                inputJSON,
		BatchSize:            req.BatchSize,
		BatchPercent:         req.BatchPercent,
		Concurrency:          req.Concurrency,
		FailThreshold:        req.FailThreshold,
		FailThresholdPercent: req.failThresholdPercent,
		RequireAlive:         req.RequireAlive,
		CreatedByAID:         createdByAID,
	}
	if req.CronExpr != "" {
		c.CronExpr = &req.CronExpr
	}
	if req.ScenarioName != "" {
		c.ScenarioName = &req.ScenarioName
	}
	if req.Module != "" {
		c.Module = &req.Module
	}
	if req.BatchMode != "" {
		bm := cadence.BatchMode(req.BatchMode)
		c.BatchMode = &bm
	}
	if req.OnFailure != "" {
		of := cadence.OnFailure(req.OnFailure)
		c.OnFailure = &of
	}
	if req.InterBatchIntervalMS != nil && *req.InterBatchIntervalMS > 0 {
		d := time.Duration(*req.InterBatchIntervalMS) * time.Millisecond
		c.InterBatchInterval = &d
	}
	if req.InterUnitIntervalMS != nil && *req.InterUnitIntervalMS > 0 {
		d := time.Duration(*req.InterUnitIntervalMS) * time.Millisecond
		c.InterUnitInterval = &d
	}
	return c
}

// applyCadenceBatchSpec translates the string field `batch` of a Cadence recipe
// into req.BatchSize / req.BatchPercent (parity with handlers.applyBatchSpec for
// Voyage, ADR-043 amendment). Grammar — fail-closed [voyage.ParseBatchSpec]
// (`N`|`N%`). "N%" → BatchPercent (resolved on spawn-scope in cadence.BuildVoyage);
// "N" → BatchSize. Returns the detail of a 422 error or "" (ok / field not set).
//
// Semantics:
//   - req.Batch == nil → no-op (old batch_size/batch_percent path).
//   - trim == "" → "not set" (no-op, not 422).
//   - non-empty + batch_size|batch_percent set → conflict (the same error code
//     voyage_batch_spec_conflict as Voyage).
//   - malformed → a human-readable detail showing the source string.
func applyCadenceBatchSpec(req *cadenceCreateRequest) (detail string) {
	if req.Batch == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.Batch)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.BatchSize != nil || req.BatchPercent != nil {
		return "voyage_batch_spec_conflict: field 'batch' is mutually exclusive with 'batch_size'/'batch_percent' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'batch' must be N or N%% (1-100); got %q", *req.Batch)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.BatchPercent = &value
	default: // BatchSpecHosts
		req.BatchSize = &value
	}
	return ""
}

// applyCadenceMaxFailures translates the string field `max_failures` of a Cadence
// recipe into req.FailThreshold (absolute) / req.failThresholdPercent (percent),
// parity with handlers.applyMaxFailures (ADR-043 amendment 2026-06-09). Key
// difference from Voyage: the percent is NOT resolved to an absolute on create —
// the Cadence scope is unknown, so the percent is stashed in
// req.failThresholdPercent → the fail_threshold_percent column and resolved on
// spawn-scope in cadence.BuildVoyage (effectiveFailThreshold).
//
// Semantics:
//   - req.MaxFailures == nil → no-op (old fail_threshold path).
//   - trim == "" → "not set" (no-op, not 422).
//   - non-empty + fail_threshold set → conflict (the same error code
//     voyage_batch_spec_conflict as Voyage/batch).
//   - "N" → absolute: req.FailThreshold = N.
//   - "N%" → percent: req.failThresholdPercent = N (fail_threshold_percent column).
//   - malformed → a human-readable detail showing the source string.
func applyCadenceMaxFailures(req *cadenceCreateRequest) (detail string) {
	if req.MaxFailures == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.MaxFailures)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.FailThreshold != nil {
		return "voyage_batch_spec_conflict: field 'max_failures' is mutually exclusive with 'fail_threshold' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'max_failures' must be N or N%% (1-100); got %q", *req.MaxFailures)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.failThresholdPercent = &value
	default: // BatchSpecHosts → absolute failure count
		req.FailThreshold = &value
	}
	return ""
}

// applyCadencePatchBatchSpec — the PATCH variant of [applyCadenceBatchSpec] over
// cadencePatchRequest (same grammar/conflict semantics, ADR-043 amendment).
func applyCadencePatchBatchSpec(req *cadencePatchRequest) (detail string) {
	if req.Batch == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.Batch)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.BatchSize != nil || req.BatchPercent != nil {
		return "voyage_batch_spec_conflict: field 'batch' is mutually exclusive with 'batch_size'/'batch_percent' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'batch' must be N or N%% (1-100); got %q", *req.Batch)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.BatchPercent = &value
	default:
		req.BatchSize = &value
	}
	return ""
}

// applyCadencePatchMaxFailures — the PATCH variant of [applyCadenceMaxFailures] over
// cadencePatchRequest. percent → req.failThresholdPercent (fail_threshold_percent
// column, resolved on spawn-scope), absolute → req.FailThreshold.
func applyCadencePatchMaxFailures(req *cadencePatchRequest) (detail string) {
	if req.MaxFailures == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.MaxFailures)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.FailThreshold != nil {
		return "voyage_batch_spec_conflict: field 'max_failures' is mutually exclusive with 'fail_threshold' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'max_failures' must be N or N%% (1-100); got %q", *req.MaxFailures)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.failThresholdPercent = &value
	default:
		req.FailThreshold = &value
	}
	return ""
}

// checkKindPermissionErr — an error-returning guard of the Voyage permission by
// kind (FULL-TYPED ADR-054 §Pattern). nil → allowed. Unknown kind → 422, revoked →
// 401, no-perm → 403 (like the (w,r) variant).
func (h *CadenceHandler) checkKindPermissionErr(aid, kind string) error {
	resource, action := "", ""
	switch cadence.Kind(kind) {
	case cadence.KindScenario:
		resource, action = "incarnation", "run"
	case cadence.KindCommand:
		resource, action = "errand", "run"
	default:
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'kind' must be one of {scenario, command}")}
	}
	if err := h.enforcer.Check(aid, resource, action, nil); err != nil {
		if errors.Is(err, rbac.ErrOperatorRevoked) {
			return &problemError{problem.New(problem.TypeOperatorRevokedToken, "", "archon "+aid+" has been revoked")}
		}
		return &problemError{problem.New(problem.TypeForbidden, "",
			"cadence recipe requires Voyage-permission "+resource+"."+action+" by kind="+kind)}
	}
	return nil
}

// checkTargetScopeErr — an error-returning per-target coven scope-check of a
// Cadence recipe (ADR-046 §7, fail-closed; FULL-TYPED ADR-054 §Pattern). nil →
// allowed. Full parity with [VoyageHandler.resolveScenarioScope] (scope loop): for
// kind=scenario it resolves the declared target → incarnation names → checks that
// the creator holds incarnation.run on EACH resolved incarnation (its covens ∪
// {name}). Otherwise a scoped Archon "run on coven=A" would create a Cadence on
// coven=B (out of scope) → the background spawn would execute out of scope =
// privilege escalation. Called AFTER [checkKindPermissionErr] (bare-check already
// passed).
//
//   - kind=command — bare-check is enough (parity with Voyage errand.run NoSelector
//     in MVP: per-host selectors deferred post-MVP); scope is not refined here.
//   - incReader=nil → fail-closed: a scenario with a non-empty target is rejected
//     (like Voyage without incReader skips per-incarnation scope, but Voyage has
//     already guaranteed the bare right there; for Cadence the same logic — the
//     bare-check above passed, scoped roles without a DB scope deny).
//     scenarioResolver=nil → 500.
//
// target — the declarative recipe form (the same [voyageTargetRequest] as Voyage).
// ctx is the request context (resolve/scope-select read it).
func (h *CadenceHandler) checkTargetScopeErr(ctx context.Context, aid, kind string, target *voyageTargetRequest) error {
	if cadence.Kind(kind) != cadence.KindScenario {
		return nil
	}
	if target == nil {
		// kind=scenario without a target — caught by cadence.validate (422 on
		// Insert); nothing to scope here.
		return nil
	}

	// Resolve the declared target → incarnation names (parity with createScenario:
	// incarnations[] ∪ service/coven filter). covenFilter is the first non-empty
	// label (the resolver takes one, like Voyage).
	var covenFilter string
	for _, c := range target.Coven {
		if !incarnationCovenLabelValid(c) {
			return &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.coven: label "+c+" must match "+soul.CovenPattern)}
		}
		if covenFilter == "" {
			covenFilter = c
		}
	}
	for _, name := range target.Incarnations {
		if !incarnation.ValidName(name) {
			return &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.incarnations: name "+name+" must match "+incarnation.NamePattern)}
		}
	}

	if h.scenarioResolver == nil {
		return &problemError{problem.New(problem.TypeInternalError, "",
			"cadence registry is not configured")}
	}
	resolved, err := h.scenarioResolver.ResolveIncarnations(ctx, VoyageScenarioFilter{
		Incarnations: target.Incarnations,
		Service:      target.Service,
		Coven:        covenFilter,
	})
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return &problemError{problem.New(problem.TypeNotFound, "", err.Error())}
		}
		h.logger.Error("cadence.scope: scenario target resolve failed", slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "",
			"resolve cadence scenario target failed")}
	}
	// An empty resolve of the declared target (coven/service matched nothing) — 422
	// before creation (parity with VoyageHandler voyage_empty_target, voyage.go: the
	// same TypeValidationFailed). No escalation (the background spawn would drop an
	// empty scope), but an honest reject on CREATE/PATCH instead of a silent 201 on a
	// "dead" recipe. command-kind never reaches here (early return for non-scenario
	// above).
	if len(resolved) == 0 {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"cadence_empty_target: resolved target is empty")}
	}

	// Per-incarnation scope-check (fail-closed, parity with createScenario): the
	// operator must hold incarnation.run on EACH resolved incarnation. incReader=nil
	// (unit test without a DB scope) → scoped roles deny (empty scope), but bare/`*`
	// already passed checkKindPermission — skip the per-incarnation check (parity
	// with Voyage incReader=nil).
	if h.incReader == nil {
		return nil
	}
	for _, name := range resolved {
		inc, sErr := incarnation.SelectByName(ctx, h.incReader, name)
		if sErr != nil {
			h.logger.Error("cadence.scope: scope-check select failed",
				slog.String("incarnation", name), slog.Any("error", sErr))
			return &problemError{problem.New(problem.TypeInternalError, "",
				"cadence scope check failed")}
		}
		contexts := incarnationCovenContexts(inc.Name, inc.Service, inc.Covens)
		if !h.allowedAnyContext(aid, "incarnation", "run", contexts) {
			return &problemError{problem.New(problem.TypeForbidden, "",
				"cadence recipe target outside operator scope: incarnation.run on resolved incarnation "+name)}
		}
	}
	return nil
}

// allowedAnyContext OR-checks a permission across a set of contexts (parity with
// [VoyageHandler.allowedAnyContext] / [middleware.RequirePermissionMulti]). An
// empty set → a single attempt with a nil context (bare/`*`).
func (h *CadenceHandler) allowedAnyContext(aid, resource, action string, contexts []map[string]string) bool {
	if len(contexts) == 0 {
		return h.enforcer.Check(aid, resource, action, nil) == nil
	}
	for _, ctx := range contexts {
		if h.enforcer.Check(aid, resource, action, ctx) == nil {
			return true
		}
	}
	return false
}

// --- GET /v1/cadences ---

// cadenceDTO — response form for GET/list. target is returned as-is (declarative,
// small — unlike Voyage target_resolved). input is NOT included (invariant A
// ADR-027: run parameters are not exposed in the read API).
type cadenceDTO struct {
	CadenceID            string          `json:"cadence_id"`
	Name                 string          `json:"name"`
	Enabled              bool            `json:"enabled"`
	ScheduleKind         string          `json:"schedule_kind"`
	IntervalSeconds      *int            `json:"interval_seconds,omitempty"`
	CronExpr             string          `json:"cron_expr,omitempty"`
	OverlapPolicy        string          `json:"overlap_policy"`
	Kind                 string          `json:"kind"`
	ScenarioName         string          `json:"scenario_name,omitempty"`
	Module               string          `json:"module,omitempty"`
	Target               json.RawMessage `json:"target,omitempty"`
	BatchSize            *int            `json:"batch_size,omitempty"`
	BatchPercent         *int            `json:"batch_percent,omitempty"`
	Concurrency          *int            `json:"concurrency,omitempty"`
	BatchMode            string          `json:"batch_mode,omitempty"`
	FailThreshold        *int            `json:"fail_threshold,omitempty"`
	FailThresholdPercent *int            `json:"fail_threshold_percent,omitempty"`
	RequireAlive         *bool           `json:"require_alive,omitempty"`
	OnFailure            string          `json:"on_failure,omitempty"`
	NextRunAt            *time.Time      `json:"next_run_at,omitempty"`
	LastRunAt            *time.Time      `json:"last_run_at,omitempty"`
	CreatedByAID         string          `json:"created_by_aid"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

func toCadenceDTO(c *cadence.Cadence) cadenceDTO {
	dto := cadenceDTO{
		CadenceID:            c.ID,
		Name:                 c.Name,
		Enabled:              c.Enabled,
		ScheduleKind:         string(c.ScheduleKind),
		IntervalSeconds:      c.IntervalSeconds,
		OverlapPolicy:        string(c.OverlapPolicy),
		Kind:                 string(c.Kind),
		Target:               json.RawMessage(c.Target),
		BatchSize:            c.BatchSize,
		BatchPercent:         c.BatchPercent,
		Concurrency:          c.Concurrency,
		FailThreshold:        c.FailThreshold,
		FailThresholdPercent: c.FailThresholdPercent,
		RequireAlive:         c.RequireAlive,
		NextRunAt:            c.NextRunAt,
		LastRunAt:            c.LastRunAt,
		CreatedByAID:         c.CreatedByAID,
		CreatedAt:            c.CreatedAt,
		UpdatedAt:            c.UpdatedAt,
	}
	if c.CronExpr != nil {
		dto.CronExpr = *c.CronExpr
	}
	if c.ScenarioName != nil {
		dto.ScenarioName = *c.ScenarioName
	}
	if c.Module != nil {
		dto.Module = *c.Module
	}
	if c.BatchMode != nil {
		dto.BatchMode = string(*c.BatchMode)
	}
	if c.OnFailure != nil {
		dto.OnFailure = string(*c.OnFailure)
	}
	return dto
}

// ListTyped — extracted domain function for GET /v1/cadences (READ, no audit;
// FULL-TYPED ADR-054 §Pattern fourth tier). Mirror of the (w,r) List: enabled
// filters (true → enabled only; false → no filter; else → 422) + kind (exact, else
// → 422); the offset/limit range is enforced by CheckPageBounds → 400 (parity with
// legacy ParsePage). Errors — *problemError: 422 bad enabled/kind / 400
// out-of-range pagination / 500 DB failure / not configured. Success —
// [CadenceListReply] (the same wire shape items/offset/limit/total as legacy →
// byte-exact).
func (h *CadenceHandler) ListTyped(ctx context.Context, enabled, kind string, offset, limit int) (CadenceListReply, error) {
	var zero CadenceListReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	var filter cadence.ListFilter
	switch enabled {
	case "":
		// do not apply the enabled filter.
	case "true":
		filter.EnabledOnly = true
	case "false":
		// false → no enabled filter (show all); explicit contract.
	default:
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"query 'enabled' must be 'true' or 'false'")}
	}
	if kind != "" {
		k := cadence.Kind(kind)
		if !cadence.ValidKind(k) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'kind' filter (must be one of scenario/command)")}
		}
		filter.Kind = k
	}

	items, total, err := cadence.List(ctx, h.store, filter, offset, limit)
	if err != nil {
		h.logger.Error("cadence.list: select failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list cadences failed")}
	}
	dtos := make([]cadenceDTO, 0, len(items))
	for _, c := range items {
		dtos = append(dtos, toCadenceDTO(c))
	}
	return CadenceListReply{Items: dtos, Offset: offset, Limit: limit, Total: total}, nil
}

// List — GET /v1/cadences (ADR-046 §6). Thin (w,r) shell over [ListTyped]
// (FULL-TYPED ADR-054): parses offset/limit via sharedapi.ParsePage (the same 400
// contract as CheckPageBounds in ListTyped) and delegates. Kept for other (w,r)
// callers; the huma route calls ListTyped directly.
func (h *CadenceHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := sharedapi.ParsePage(q)
	if err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, err.Error()))
		return
	}
	reply, err := h.ListTyped(r.Context(), q.Get("enabled"), q.Get("kind"), page.Offset, page.Limit)
	if err != nil {
		if d, ok := AsProblemDetails(err); ok {
			d.Instance = r.URL.Path
			problem.Write(w, d)
			return
		}
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "list cadences failed"))
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// Get — GET /v1/cadences/{id} (detail).
func (h *CadenceHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "cadence registry is not configured"))
		return
	}
	id := chi.URLParam(r, "id")
	if !audit.IsValidULID(id) {
		problem.Write(w, problem.New(problem.TypeValidationFailed, r.URL.Path,
			"path 'id' must be a Crockford-base32 ULID (26 chars)"))
		return
	}
	c, err := cadence.Get(r.Context(), h.store, id)
	if err != nil {
		h.writeReadError(w, r, "get", id, err)
		return
	}
	writeJSON(w, http.StatusOK, toCadenceDTO(c), h.logger)
}

// GetTyped — extracted domain function for GET /v1/cadences/{id} (READ, no audit;
// FULL-TYPED ADR-054 §Pattern). Mirror of the (w,r) Get: 422 bad id, 404 not-found,
// 500 DB failure / not configured. Success — [CadenceDTO] (the same wire shape as
// legacy toCadenceDTO → byte-exact with GET {id} on strict).
func (h *CadenceHandler) GetTyped(ctx context.Context, id string) (CadenceDTO, error) {
	var zero CadenceDTO
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	c, err := cadence.Get(ctx, h.store, id)
	if err != nil {
		return zero, &problemError{h.readErrPtr("get", id, err)}
	}
	return toCadenceDTO(c), nil
}

// --- PATCH /v1/cadences/{id} ---

// cadencePatchRequest — PATCH body. All fields optional: present → overwrite,
// omitted → the current value is kept (read-modify-write over the full-replace
// cadence.Update). Pointers to nullable recipe fields cannot semantically
// distinguish "omitted" from "explicit null" — for MVP PATCH treats key presence as
// an overwrite (absence → keep). The enabled toggle lives here too (also available
// via /enable+/disable).
type cadencePatchRequest struct {
	Name            *string              `json:"name,omitempty"`
	Enabled         *bool                `json:"enabled,omitempty"`
	ScheduleKind    *string              `json:"schedule_kind,omitempty"`
	IntervalSeconds *int                 `json:"interval_seconds,omitempty"`
	CronExpr        *string              `json:"cron_expr,omitempty"`
	OverlapPolicy   *string              `json:"overlap_policy,omitempty"`
	ScenarioName    *string              `json:"scenario_name,omitempty"`
	Module          *string              `json:"module,omitempty"`
	Input           map[string]any       `json:"input,omitempty"`
	Target          *voyageTargetRequest `json:"target,omitempty"`
	// Batch/MaxFailures — string batch fields (parity with create, ADR-043
	// amendment). Translated by applyCadencePatchBatchSpec/applyCadencePatchMaxFailures
	// into batch_size|batch_percent / fail_threshold|fail_threshold_percent before
	// applyCadencePatch. Conflict with the numeric columns in the same PATCH → 422.
	Batch         *string `json:"batch,omitempty"`
	BatchSize     *int    `json:"batch_size,omitempty"`
	BatchPercent  *int    `json:"batch_percent,omitempty"`
	Concurrency   *int    `json:"concurrency,omitempty"`
	BatchMode     *string `json:"batch_mode,omitempty"`
	MaxFailures   *string `json:"max_failures,omitempty"`
	FailThreshold *int    `json:"fail_threshold,omitempty"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     *string `json:"on_failure,omitempty"`

	// failThresholdPercent — the stashed percent from max_failures="N%" (parity with
	// create); filled by applyCadencePatchMaxFailures, carried into applyCadencePatch.
	failThresholdPercent *int
}

// scheduleChanged reports whether the PATCH touches the schedule (requires
// recomputing next_run_at).
func (p *cadencePatchRequest) scheduleChanged() bool {
	return p.ScheduleKind != nil || p.IntervalSeconds != nil || p.CronExpr != nil
}

// Patch — PATCH /v1/cadences/{id} (ADR-046 §6). Read-modify-write: reads the
// current row, applies the given fields, runs cadence.Update (full-replace +
// validate). Recomputes next_run_at on a schedule change. audit cadence.updated.
//
// Contract: 200 + cadenceDTO; 400 invalid JSON; 404 cadence_not_found; 422 invalid
// recipe/schedule; 500 DB failure. kind is NOT changed (a kind change = a different
// recipe entity; disallowed implicitly — the field is not in the PATCH body; a kind
// change = delete + create). created_by_aid is fixed (cadence.Update does not write
// it).
func (h *CadenceHandler) Patch(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	id := chi.URLParam(r, "id")

	var req cadencePatchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, "invalid JSON body: "+err.Error()))
		return
	}

	dto, err := h.PatchTyped(r.Context(), claims, id, req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	middleware.SetAuditPayload(r, middleware.AuditPayload{
		"cadence_id": dto.CadenceID,
		"name":       dto.Name,
		"kind":       dto.Kind,
	})
	writeJSON(w, http.StatusOK, dto, h.logger)
}

// PatchTyped — extracted domain function for PATCH /v1/cadences/{id} (FULL-TYPED
// unfolding, ADR-054 §Pattern (b), batch-2f self-audit): read-modify-write over the
// full-replace cadence.Update without http.ResponseWriter/*http.Request.
// claims/id/req arrive as arguments (decode/auth/{id}-bind on the caller); errors —
// *problemError, success — cadenceDTO. self-audit cadence.updated is written INSIDE
// the function (before returning the DTO — the huma wrapper does not touch it,
// §Audit).
//
// Steps (parity with the former Patch(w,r)): id validation → string batch fields →
// Get → applyCadencePatch → RBAC PATCH-guard (two-tier, ADR-046 §7) → floor limit →
// next_run_at → Update → audit emit.
func (h *CadenceHandler) PatchTyped(ctx context.Context, claims *jwt.Claims, id string, req CadencePatchRequest) (CadenceDTO, error) {
	var zero CadenceDTO
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}

	// String batch fields on PATCH (parity with create): translate
	// `batch`/`max_failures` into numeric fields before Get/applyCadencePatch. A
	// string-format conflict with a numeric column in the same PATCH and malformed
	// input → 422.
	if bErr := applyCadencePatchBatchSpec(&req); bErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bErr)}
	}
	if mfErr := applyCadencePatchMaxFailures(&req); mfErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", mfErr)}
	}

	c, err := cadence.Get(ctx, h.store, id)
	if err != nil {
		return zero, &problemError{h.readErrPtr("patch", id, err)}
	}

	scheduleChanged := req.scheduleChanged()
	applyCadencePatch(c, &req)

	// RBAC PATCH-guard (ADR-046 §7, security-critical — this was the second hole):
	// PATCH changes target/scenario_name, so it needs the same two-tier guard as
	// CREATE — otherwise a scoped Archon would create a Cadence on the allowed
	// coven=A, then PATCH the target to coven=B (out of scope) without a check. kind
	// is not changed on PATCH (taken from the loaded row c.Kind). First a bare-check
	// of the Voyage permission by kind, then the per-target scope of the new
	// (post-patch) target.
	if err := h.checkKindPermissionErr(claims.Subject, string(c.Kind)); err != nil {
		return zero, err
	}
	if err := h.checkTargetScopeErr(ctx, claims.Subject, string(c.Kind), cadenceTargetRequest(c.Target)); err != nil {
		return zero, err
	}

	// Floor limit on the period (ADR-046 Pass B): PATCH may switch the schedule to
	// interval or change interval_seconds — the same floor invariant as Create.
	// After the scope-check (RBAC deny takes priority, do not leak interval
	// validity).
	if err := cadence.ValidateIntervalFloor(c, h.pollFloorSeconds); err != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	}

	// Recompute next_run_at on a schedule change (ADR-046 §6): a new rule → a new
	// next from now. A broken cron returns an error from NextRun, but cadence.Update
	// catches it anyway (validate.ParseCron) → 422; here we just leave next
	// untouched.
	if scheduleChanged {
		if next, nErr := cadence.NextRun(c, time.Now().UTC()); nErr == nil {
			c.NextRunAt = &next
		}
	}

	if err := cadence.Update(ctx, h.store, c); err != nil {
		return zero, &problemError{h.writeWriteErrorPtr("patch", id, err)}
	}

	h.emitWrite(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventCadenceUpdated, c)
	return toCadenceDTO(c), nil
}

// cadenceTargetRequest decodes the declarative Cadence target (jsonb, the same
// shape as [voyageTargetRequest]) for the scope-check (ADR-046 §7). Empty/broken
// jsonb → nil (the caller treats this as "nothing to scope" — a kind=scenario
// without a valid target is caught by cadence.validate). Read-only decode (target
// is written to the DB by buildCadence/applyCadencePatch from the same type).
func cadenceTargetRequest(raw []byte) *voyageTargetRequest {
	if len(raw) == 0 {
		return nil
	}
	var t voyageTargetRequest
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	return &t
}

// applyCadencePatch applies the given PATCH fields onto the loaded row
// (read-modify-write). An omitted field → the current value is kept. Pointer
// nullable recipe fields: key presence → overwrite, including to null (the JSON
// decoder puts a nil pointer for `"field": null` — but omitempty in the DTO is
// indistinguishable, so for MVP a non-nil pointer is treated as set; cron/scenario/
// module are cleared with an empty string).
func applyCadencePatch(c *cadence.Cadence, req *cadencePatchRequest) {
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if req.ScheduleKind != nil {
		c.ScheduleKind = cadence.ScheduleKind(*req.ScheduleKind)
	}
	if req.IntervalSeconds != nil {
		c.IntervalSeconds = req.IntervalSeconds
	}
	if req.CronExpr != nil {
		if *req.CronExpr == "" {
			c.CronExpr = nil
		} else {
			c.CronExpr = req.CronExpr
		}
	}
	if req.OverlapPolicy != nil {
		c.OverlapPolicy = cadence.OverlapPolicy(*req.OverlapPolicy)
	}
	// On a schedule_kind change, clear the "foreign" schedule field, otherwise
	// validate rejects it (interval must not carry cron_expr and vice versa).
	switch c.ScheduleKind {
	case cadence.ScheduleKindInterval:
		c.CronExpr = nil
	case cadence.ScheduleKindCron:
		c.IntervalSeconds = nil
	}
	if req.ScenarioName != nil {
		if *req.ScenarioName == "" {
			c.ScenarioName = nil
		} else {
			c.ScenarioName = req.ScenarioName
		}
	}
	if req.Module != nil {
		if *req.Module == "" {
			c.Module = nil
		} else {
			c.Module = req.Module
		}
	}
	if req.Input != nil {
		inputJSON, _ := json.Marshal(req.Input)
		c.Input = inputJSON
	}
	if req.Target != nil {
		targetJSON, _ := json.Marshal(req.Target)
		c.Target = targetJSON
	}
	// batch_size / batch_percent — a mutually exclusive pair. A PATCH of one format
	// over the stored counterpart: null out the counterpart, otherwise the operator
	// cannot switch format without an explicit reset (Batch S3 review). nil req →
	// keep (field not set) — the null-out does NOT fire.
	if req.BatchSize != nil {
		c.BatchSize = req.BatchSize
		c.BatchPercent = nil
	}
	if req.BatchPercent != nil {
		c.BatchPercent = req.BatchPercent
		c.BatchSize = nil
	}
	if req.Concurrency != nil {
		c.Concurrency = req.Concurrency
	}
	if req.BatchMode != nil {
		if *req.BatchMode == "" {
			c.BatchMode = nil
		} else {
			bm := cadence.BatchMode(*req.BatchMode)
			c.BatchMode = &bm
		}
	}
	// fail_threshold / fail_threshold_percent — a mutually exclusive pair (validate
	// enforces XOR). A PATCH of one format (max_failures="N" → absolute, "N%" →
	// percent) over the stored counterpart: null out the counterpart, otherwise
	// validate returns 422 and the operator cannot switch format without an explicit
	// reset (Batch S3 review). nil req → keep — the null-out does NOT fire.
	if req.FailThreshold != nil {
		c.FailThreshold = req.FailThreshold
		c.FailThresholdPercent = nil
	}
	if req.failThresholdPercent != nil {
		c.FailThresholdPercent = req.failThresholdPercent
		c.FailThreshold = nil
	}
	if req.RequireAlive != nil {
		c.RequireAlive = req.RequireAlive
	}
	if req.OnFailure != nil {
		if *req.OnFailure == "" {
			c.OnFailure = nil
		} else {
			of := cadence.OnFailure(*req.OnFailure)
			c.OnFailure = &of
		}
	}
}

// --- POST /v1/cadences/{id}/enable | /disable ---

// Enable — POST /v1/cadences/{id}/enable (resume the schedule).
func (h *CadenceHandler) Enable(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, true)
}

// Disable — POST /v1/cadences/{id}/disable (pause the schedule).
func (h *CadenceHandler) Disable(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, false)
}

// setEnabled — the shared enable/disable branch: a lightweight toggle without
// rewriting the recipe ([cadence.SetEnabled]). audit cadence.updated (schedule
// state change). 200 + {cadence_id, enabled}.
func (h *CadenceHandler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	id := chi.URLParam(r, "id")
	reply, err := h.SetEnabledTyped(r.Context(), claims, id, enabled)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	middleware.SetAuditPayload(r, middleware.AuditPayload{
		"cadence_id": reply.CadenceID,
		"enabled":    reply.Enabled,
	})
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// SetEnabledTyped — extracted domain function for enable/disable (FULL-TYPED
// ADR-054 §Pattern, batch-2f self-audit): a lightweight toggle without rewriting
// the recipe ([cadence.SetEnabled]) without http.ResponseWriter/*http.Request.
// self-audit cadence.updated (schedule state change) is written INSIDE the
// function. 200 body — the generated CadenceEnabledReply.
func (h *CadenceHandler) SetEnabledTyped(ctx context.Context, claims *jwt.Claims, id string, enabled bool) (CadenceEnabledReply, error) {
	var zero CadenceEnabledReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	if err := cadence.SetEnabled(ctx, h.store, id, enabled); err != nil {
		return zero, &problemError{h.writeWriteErrorPtr("set-enabled", id, err)}
	}
	h.emitEnabledToggle(claims.Subject, middleware.ScenarioInvocationSource(ctx), id, enabled)
	return cadenceEnabledReply{CadenceID: id, Enabled: enabled}, nil
}

// --- DELETE /v1/cadences/{id} ---

// Delete — DELETE /v1/cadences/{id} (ADR-046 §9). Removes the schedule; the spawned
// Voyages remain (FK voyages.cadence_id ON DELETE SET NULL — manual runs and child
// history are preserved). audit cadence.deleted. 204 No Content; 404
// cadence_not_found.
func (h *CadenceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.DeleteTyped(r.Context(), claims, id); err != nil {
		writeProblemError(w, r, err)
		return
	}
	middleware.SetAuditPayload(r, middleware.AuditPayload{"cadence_id": id})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteTyped — extracted domain function for DELETE /v1/cadences/{id} (FULL-TYPED
// ADR-054 §Pattern, batch-2f self-audit): removes the schedule; the spawned Voyages
// remain (FK voyages.cadence_id ON DELETE SET NULL). self-audit cadence.deleted is
// written INSIDE the function. ctx is the request context (delete + the dispatcher
// TTL-snapshot invalidation read it).
func (h *CadenceHandler) DeleteTyped(ctx context.Context, claims *jwt.Claims, id string) error {
	if h.store == nil {
		return &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	if err := cadence.Delete(ctx, h.store, id); err != nil {
		return &problemError{h.writeWriteErrorPtr("delete", id, err)}
	}

	// The FK cascade (tidings.created_from_cadence_id ON DELETE CASCADE, migration
	// 074) removed the permanent notify rules at the DB level BYPASSING
	// herald.Service CRUD — reset the dispatcher's TTL snapshot EXPLICITLY, strictly
	// after deletion (parity with the Create invalidation after notify-insert and
	// HeraldHandler.DeleteHerald). The invalidation is UNCONDITIONAL: the handler
	// does not know whether there were form rules (DB-side cascade), and
	// InvalidateRules() resets the whole snapshot — id is only a diagnostic label for
	// the cross-keeper publish; delete is rare, an extra TTL-snapshot reset is cheap.
	// nil-safe: dev without herald → no-op.
	if h.tidingInvalidator != nil {
		h.tidingInvalidator.InvalidateTidings(ctx, id)
	}

	h.emitDeleted(claims.Subject, middleware.ScenarioInvocationSource(ctx), id)
	return nil
}

// --- GET /v1/cadences/{id}/runs ---

// Runs — GET /v1/cadences/{id}/runs (ADR-046 §6). The schedule's child Voyages
// (voyages WHERE cadence_id=$1), reusing the Voyage DTO/list. Cadence existence is
// checked by a probe (404 if absent — an empty list is indistinguishable from a
// nonexistent id). Pagination + status filter (parity with VoyageHandler.List).
func (h *CadenceHandler) Runs(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "cadence registry is not configured"))
		return
	}
	id := chi.URLParam(r, "id")
	if !audit.IsValidULID(id) {
		problem.Write(w, problem.New(problem.TypeValidationFailed, r.URL.Path,
			"path 'id' must be a Crockford-base32 ULID (26 chars)"))
		return
	}
	if _, err := cadence.Get(r.Context(), h.store, id); err != nil {
		h.writeReadError(w, r, "runs", id, err)
		return
	}

	q := r.URL.Query()
	page, err := sharedapi.ParsePage(q)
	if err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, err.Error()))
		return
	}
	filter := voyage.ListFilter{CadenceID: id}
	if statuses := q["status"]; len(statuses) > 0 {
		filter.Statuses = make([]voyage.Status, 0, len(statuses))
		for _, s := range statuses {
			st := voyage.Status(s)
			if !voyage.ValidStatus(st) {
				problem.Write(w, problem.New(problem.TypeValidationFailed, r.URL.Path,
					"invalid 'status' filter (scheduled/pending/running/succeeded/failed/partial_failed/cancelled)"))
				return
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}

	items, total, err := voyage.List(r.Context(), h.store, filter, page.Offset, page.Limit)
	if err != nil {
		h.logger.Error("cadence.runs: voyage list failed", slog.String("cadence_id", id), slog.Any("error", err))
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "list cadence runs failed"))
		return
	}
	dtos := make([]voyageDTO, 0, len(items))
	for _, v := range items {
		dtos = append(dtos, toVoyageDTO(v))
	}
	writeJSON(w, http.StatusOK, sharedapi.PagedResponse[voyageDTO]{
		Items:  dtos,
		Offset: page.Offset,
		Limit:  page.Limit,
		Total:  total,
	}, h.logger)
}

// CadenceRunsReply — typed output of GET /v1/cadences/{id}/runs: paged voyageDTO of
// the same wire shape as the legacy (w,r) Runs (items/offset/limit/total via
// sharedapi.PagedResponse → byte-exact). voyageDTO = Voyage.
type CadenceRunsReply = sharedapi.PagedResponse[voyageDTO]

// RunsTyped — extracted domain function for GET /v1/cadences/{id}/runs (READ, no
// audit; FULL-TYPED ADR-054 §Pattern). Mirror of the (w,r) Runs: checks the
// schedule exists (404 if not), filters Voyages by cadence_id + optional status[].
// The offset/limit range is enforced by CheckPageBounds → 400 (parity with legacy
// ParsePage). Errors — *problemError: 422 bad id / 400 out-of-range pagination / 422
// bad status / 404 no schedule / 500 DB failure. Success — [CadenceRunsReply].
func (h *CadenceHandler) RunsTyped(ctx context.Context, id string, statuses []string, offset, limit int) (CadenceRunsReply, error) {
	var zero CadenceRunsReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	if _, err := cadence.Get(ctx, h.store, id); err != nil {
		return zero, &problemError{h.readErrPtr("runs", id, err)}
	}

	filter := voyage.ListFilter{CadenceID: id}
	if len(statuses) > 0 {
		filter.Statuses = make([]voyage.Status, 0, len(statuses))
		for _, s := range statuses {
			st := voyage.Status(s)
			if !voyage.ValidStatus(st) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
					"invalid 'status' filter (scheduled/pending/running/succeeded/failed/partial_failed/cancelled)")}
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}

	items, total, err := voyage.List(ctx, h.store, filter, offset, limit)
	if err != nil {
		h.logger.Error("cadence.runs: voyage list failed", slog.String("cadence_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list cadence runs failed")}
	}
	dtos := make([]voyageDTO, 0, len(items))
	for _, v := range items {
		dtos = append(dtos, toVoyageDTO(v))
	}
	return CadenceRunsReply{Items: dtos, Offset: offset, Limit: limit, Total: total}, nil
}

// --- error mapping ---

// writeReadError maps a read-operation error (Get): not-found → 404, else 500.
func (h *CadenceHandler) writeReadError(w http.ResponseWriter, r *http.Request, op, id string, err error) {
	d := h.readErrPtr(op, id, err)
	d.Instance = r.URL.Path
	problem.Write(w, d)
}

// readErrPtr — classifier of a read error (Get) → [problem.Details] (FULL-TYPED
// ADR-054 §Pattern): not-found→404, else 500. instance empty (the caller sets it).
// The extracted core of [writeReadError] for error-returning paths (PatchTyped).
func (h *CadenceHandler) readErrPtr(op, id string, err error) problem.Details {
	if errors.Is(err, cadence.ErrCadenceNotFound) {
		return problem.New(problem.TypeNotFound, "", "cadence_not_found: "+id)
	}
	h.logger.Error("cadence."+op+": select failed", slog.String("cadence_id", id), slog.Any("error", err))
	return problem.New(problem.TypeInternalError, "", "cadence "+op+" failed")
}

// writeWriteErrorPtr — classifier of a write error → [problem.Details] (FULL-TYPED
// ADR-054 §Pattern): not-found→404, validate→422, PG→500. instance empty (the
// caller sets it). The extracted core of [writeWriteError] for error-returning
// paths (persistErr). Classification semantics identical to the (w,r) variant.
func (h *CadenceHandler) writeWriteErrorPtr(op, id string, err error) problem.Details {
	switch {
	case errors.Is(err, cadence.ErrCadenceNotFound):
		return problem.New(problem.TypeNotFound, "", "cadence_not_found: "+id)
	case isCadenceValidationError(err):
		return problem.New(problem.TypeValidationFailed, "", err.Error())
	default:
		h.logger.Error("cadence."+op+": write failed", slog.String("cadence_id", id), slog.Any("error", err))
		return problem.New(problem.TypeInternalError, "", "cadence "+op+" failed")
	}
}

// isCadenceValidationError distinguishes a recipe validate error (422) from a PG
// failure (500). cadence.validate returns bare fmt.Errorf("cadence: …") BEFORE SQL;
// PG failures are wrapped by mapWriteError into "cadence: write: …" / FK / CHECK /
// Exists. A validate error's signature is NOT Exists/NotFound and carries no PG
// wrapper. Simple and reliable test: it is neither a CRUD sentinel error nor a
// "cadence: write:" wrapper.
func isCadenceValidationError(err error) bool {
	if errors.Is(err, cadence.ErrCadenceExists) || errors.Is(err, cadence.ErrCadenceNotFound) {
		return false
	}
	// PG CHECK/FK/write errors went through mapWriteError → their text starts with
	// "cadence: write:" / "cadence: FK violation" / "cadence: CHECK violation".
	// validate errors — any other "cadence: …" from validate (no SQL wrapper).
	msg := err.Error()
	for _, pgPrefix := range []string{"cadence: write:", "cadence: FK violation", "cadence: CHECK violation"} {
		if len(msg) >= len(pgPrefix) && msg[:len(pgPrefix)] == pgPrefix {
			return false
		}
	}
	return true
}

// --- audit emitters ---

// emitWrite writes cadence.created / cadence.updated (source=api/mcp, archon_aid=
// JWT.sub). Background ctx — the HTTP server may cancel r.Context() after the write
// response. The recipe `input` is NOT included (invariant A ADR-027).
func (h *CadenceHandler) emitWrite(aid string, source audit.Source, eventType audit.EventType, c *cadence.Cadence) {
	if h.auditW == nil {
		return
	}
	payload := map[string]any{
		"cadence_id":     c.ID,
		"name":           c.Name,
		"schedule_kind":  string(c.ScheduleKind),
		"kind":           string(c.Kind),
		"overlap_policy": string(c.OverlapPolicy),
		"enabled":        c.Enabled,
	}
	if c.ScenarioName != nil {
		payload["scenario_name"] = *c.ScenarioName
	}
	if c.Module != nil {
		payload["module"] = *c.Module
	}
	h.writeAudit(&audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: c.ID,
		Payload:       payload,
	})
}

// emitEnabledToggle writes cadence.updated for the enable/disable toggle.
func (h *CadenceHandler) emitEnabledToggle(aid string, source audit.Source, id string, enabled bool) {
	if h.auditW == nil {
		return
	}
	h.writeAudit(&audit.Event{
		EventType:     audit.EventCadenceUpdated,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: id,
		Payload:       map[string]any{"cadence_id": id, "enabled": enabled},
	})
}

// emitDeleted writes cadence.deleted.
func (h *CadenceHandler) emitDeleted(aid string, source audit.Source, id string) {
	if h.auditW == nil {
		return
	}
	h.writeAudit(&audit.Event{
		EventType:     audit.EventCadenceDeleted,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: id,
		Payload:       map[string]any{"cadence_id": id},
	})
}

func (h *CadenceHandler) writeAudit(ev *audit.Event) {
	if err := h.auditW.Write(context.Background(), ev); err != nil {
		h.logger.Error("cadence: audit write failed",
			slog.String("event_type", string(ev.EventType)),
			slog.String("cadence_id", ev.CorrelationID), slog.Any("error", err))
	}
}
