package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// VoyageStore — a narrow CRUD surface of the [voyage] package for the S5 HTTP/MCP
// handler:
//
//   - ExecQueryRower — read (SelectByID/List/SelectTargets) + cancel-UPDATE without
//     a transaction;
//   - BeginTx — atomic Insert + InsertTargets (snapshot-scope in one PG tx,
//     ADR-043: the unit set does not "jitter" between INSERTs).
//
// Claim/Lease/Finalize live in [voyageorch.VoyageWorker]. A real *pgxpool.Pool
// satisfies it; unit tests use a fake.
type VoyageStore interface {
	voyage.ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// TidingInvalidator — a narrow surface for two-level invalidation of the
// dispatcher's Tiding-rule snapshot (in-process InvalidateRules + cross-keeper Redis
// publish). Needed by the voyage.create path, which inserts ephemeral Tidings from
// notify via a direct herald.InsertTiding into its own voyage-tx, BYPASSING
// herald.Service CRUD (and its invalidation). Without an explicit call after commit
// the dispatcher holds the rule behind a TTL snapshot (DefaultRuleCacheTTL=15s) and a fast
// run (~5s) dispatches the terminal against a stale snapshot → the one-shot
// notification silently misses (race, confirmed by the architect).
//
// Implemented by [*herald.Service] (method InvalidateTidings) — single source of
// truth for the invalidator/Redis publisher. nil → no-op (dev without herald/Redis:
// degrades to TTL convergence, as before).
type TidingInvalidator interface {
	InvalidateTidings(ctx context.Context, name string)
}

// VoyageHandler — handlers for the Voyage endpoints (ADR-043, S5):
//
//	POST   /v1/voyages              — create a Voyage (kind=scenario|command).
//	GET    /v1/voyages              — paged list (filter kind/status).
//	GET    /v1/voyages/{id}         — snapshot (detail + summary).
//	GET    /v1/voyages/{id}/targets — All-runs drill (per-target batch/status/back-link).
//	DELETE /v1/voyages/{id}         — cancel pending/scheduled (running-cancel — post-MVP).
//
// RBAC-by-kind (ADR-043 §6, security-critical fail-closed guard): POST
// picks the permission BY kind from the BODY — scenario→incarnation.run,
// command→errand.run. A middleware route cannot do this (kind is visible only
// after decoding the body), so the permission check lives INSIDE Create (the router
// only wires RequireJWT). GET/list — read permission on the corresponding
// namespace (incarnation.history for scenario-read parity with Tide; common entry —
// see router.go: list/detail/targets are gated by incarnation.history, like Tide).
//
// Dependencies are optional (TideHandler / ErrandRunHandler pattern): router.go
// checks handler==nil. enforcer/store/scenarioResolver/commandResolver are
// required for production routes; incReader is needed by the RBAC-by-kind scenario
// gate. auditW may be nil (dev without audit).
type VoyageHandler struct {
	store            VoyageStore
	scenarioResolver VoyageScenarioResolver
	commandResolver  VoyageCommandResolver
	incReader        IncarnationContextReader
	enforcer         middleware.PermissionChecker
	// scoper — the read surface of the operator's scope boundary (ADR-047 S4). Used by
	// the command path to intersect target ∩ Purview (errand.run): the same resolver
	// that filters `GET /v1/souls`. nil → command resolve degrades to cluster-
	// wide (backcompat for unit tests without DB scope; production wire-up passes
	// rbac.Holder).
	scoper PurviewResolver
	auditW audit.Writer
	// tidingInvalidator flushes the dispatcher's TTL Tiding-rule snapshot after
	// committing a voyage-tx with ephemeral notify (ADR-052(g) race-fix). nil → no-op
	// (dev without herald: degrades to TTL convergence).
	tidingInvalidator TidingInvalidator
	// maxScope — upper limit on the resolved scope size (DoS-guard S-med-3).
	// 0 → unlimited. Resolved from cfg.Voyage.ResolvedMaxScope() in the constructor.
	maxScope int
	// maxBatchSize — upper bound on the effective batch/window size (DoS-guard
	// S-W4): batch_size for barrier, concurrency for window. 0 → no limit.
	// Resolved from cfg.Voyage.ResolvedMaxBatchSize() in the constructor.
	maxBatchSize int
	logger       *slog.Logger
}

// NewVoyageHandler builds the handler. logger=nil → discard. store /
// scenarioResolver / commandResolver / enforcer are required for production
// routes; incReader is needed by the RBAC-by-kind scenario gate (without it
// scenario-create fail-closed rejects scoped roles). scoper is needed by the command path for
// target ∩ Purview (ADR-047 S4); nil → command resolve cluster-wide (backcompat
// for unit tests). auditW may be nil. tidingInvalidator flushes the dispatcher's
// TTL snapshot after committing a voyage-tx with ephemeral notify (ADR-052(g) race-fix);
// nil → no-op (dev without herald). maxScope —
// upper limit on the resolved scope size (DoS-guard S-med-3); 0 → unlimited
// (caller passes cfg.Voyage.ResolvedMaxScope()). maxBatchSize — upper bound on the
// batch/window size (DoS-guard S-W4); 0 → no limit
// (cfg.Voyage.ResolvedMaxBatchSize()).
func NewVoyageHandler(
	store VoyageStore,
	scenarioResolver VoyageScenarioResolver,
	commandResolver VoyageCommandResolver,
	incReader IncarnationContextReader,
	enforcer middleware.PermissionChecker,
	scoper PurviewResolver,
	auditW audit.Writer,
	tidingInvalidator TidingInvalidator,
	maxScope int,
	maxBatchSize int,
	logger *slog.Logger,
) *VoyageHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &VoyageHandler{
		store:             store,
		scenarioResolver:  scenarioResolver,
		commandResolver:   commandResolver,
		incReader:         incReader,
		enforcer:          enforcer,
		scoper:            scoper,
		auditW:            auditW,
		tidingInvalidator: tidingInvalidator,
		maxScope:          maxScope,
		maxBatchSize:      maxBatchSize,
		logger:            logger,
	}
}

// Configuration limits for POST validation (parity ErrandRun / Tide).
const (
	// voyageDefaultConcurrency — default parallelism within a Leg (parity
	// errandRunDefaultConcurrency).
	voyageDefaultConcurrency = 50
	// voyageMaxConcurrency — upper bound on concurrency (parity ErrandRun
	// MaxConcurrency = 500; CHECK voyages_concurrency_positive does not cap from
	// above, the cap is a handler invariant).
	voyageMaxConcurrency = 500
	// voyageMaxWhereBytes — DoS-guard for the CEL predicate of command-target.where
	// (parity errandRunMaxWhereBytes = 4 KiB).
	voyageMaxWhereBytes = 4096
)

// --- POST /v1/voyages ---

// voyageCreateRequest — POST body. snake_case. Unknown fields are rejected.
//
// NOT an alias of [VoyageCreateRequest] (ADR-051 — soulCreateRequest reasoning):
// carries the server-only computed field `maxFailuresPercent` (not a wire field — stashed in
// applyMaxFailures, resolved by scope) and is mutated in-place by the whole
// validation chain (applyBatchSpec writes BatchSize/BatchPercent, applyMaxFailures —
// FailThreshold). A pure alias of the gen type (typed-enum Kind/BatchMode/OnFailure +
// pointer-optional Target/Input) would require rewriting the security-critical
// kind-RBAC and batch resolve for zero wire benefit — the struct is byte-for-byte
// identical to the VoyageCreateRequest schema. Wire shape verified against oapi (categories
// A: kind/scenario_name/module/scheduling/batch* — same keys and types).
type voyageCreateRequest struct {
	Kind         string               `json:"kind"`
	ScenarioName string               `json:"scenario_name,omitempty"`
	Module       string               `json:"module,omitempty"`
	Input        map[string]any       `json:"input,omitempty"`
	Target       *voyageTargetRequest `json:"target"`
	// Batch — string batch size ("N" hosts / "N%" of scope), S1 of the string
	// batch fields. Maps to batch_size|batch_percent (see applyBatchSpec).
	// Conflicts with batch_size/batch_percent (cannot use both formats). nil ⇒ old
	// path (batch_size/batch_percent as before).
	Batch                *string    `json:"batch,omitempty"`
	BatchSize            *int       `json:"batch_size,omitempty"`
	BatchPercent         *int       `json:"batch_percent,omitempty"`
	Concurrency          *int       `json:"concurrency,omitempty"`
	BatchMode            string     `json:"batch_mode,omitempty"`
	DryRun               bool       `json:"dry_run,omitempty"`
	ScheduleAt           *time.Time `json:"schedule_at,omitempty"`
	InterBatchIntervalMS *int       `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int       `json:"inter_unit_interval_ms,omitempty"`
	// MaxFailures — string failure threshold ("N" absolute / "N%" percent of run
	// units), S2 of the string batch fields (ADR-043 amendment 2026-06-09). At resolve
	// it decomposes into FailThreshold (see applyMaxFailures / resolveMaxFailuresPercent).
	// Conflicts with fail_threshold (cannot use both formats). nil ⇒ old path
	// (fail_threshold as before).
	MaxFailures   *string `json:"max_failures,omitempty"`
	FailThreshold *int    `json:"fail_threshold,omitempty"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     string  `json:"on_failure,omitempty"`

	// Notify — one-shot subscriptions to THIS run (ADR-052(g) amendment N2). keeper
	// materializes each element into an ephemeral Tiding (ephemeral=true, voyage_id=
	// <new voyage_id>) in the SAME transaction that creates the Voyage. Atomicity gives
	// the rule's presence in the DB by commit, but not its visibility to the dispatcher's TTL snapshot
	// — after commit, persist explicitly invalidates the cache (see persist). nil/empty ⇒ no
	// notifications. NOT an alias of [VoyageNotify]: keeper derives event_types from
	// On by run kind (server-side mapping), stores annotations as raw
	// json.RawMessage for object validation (ValidateAnnotationsJSON).
	Notify []voyageNotifyRequest `json:"notify,omitempty"`

	// maxFailuresPercent — the unwrapped percent from max_failures="N%", stashed by
	// applyMaxFailures for post-scope resolve into an absolute FailThreshold (depends on
	// the number of run units, known only after target resolve). nil ⇒ percent
	// not set (max_failures absent, empty, or absolute — already in FailThreshold).
	maxFailuresPercent *int
}

// voyageTargetRequest — a declarative target (resolved into a unit snapshot).
// scenario mode reads Incarnations/Service/Coven; command mode — SIDs/Coven/
// Where. Fields irrelevant to the kind are ignored (the handler validates that the
// required set for the kind is non-empty).
type voyageTargetRequest struct {
	// scenario mode:
	Incarnations []string `json:"incarnations,omitempty"`
	Service      string   `json:"service,omitempty"`
	// command mode:
	SIDs  []string `json:"sids,omitempty"`
	Where string   `json:"where,omitempty"`
	// shared (incarnation env tag for scenario / host coven label for command):
	Coven []string `json:"coven,omitempty"`
}

// voyageNotifyRequest — one element of the notify block: a one-shot subscription to this
// run (ADR-052(g)/(h)). The filter/body fields match a permanent Tiding;
// event_types is NOT set by the client — it is derived by keeper from On by run kind
// (see notifyEventTypes). Annotations is kept as raw json.RawMessage to
// validate "top level is an object" (herald.ValidateAnnotationsJSON)
// BEFORE unpacking into a map.
type voyageNotifyRequest struct {
	Herald       string          `json:"herald"`
	On           []string        `json:"on,omitempty"`
	OnlyFailures bool            `json:"only_failures,omitempty"`
	OnlyChanges  bool            `json:"only_changes,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Projection   []string        `json:"projection,omitempty"`
}

// voyageNotifyTerminal — allowed values of notify.on (run terminals,
// mapped to event_types by kind, see notifyEventTypes). Closed enum, mirror of
// oapi VoyageNotifyOn.
const (
	notifyOnCompleted = "completed"
	notifyOnFailed    = "failed"
	notifyOnPartial   = "partial"
)

// voyageCreateReply — native 202 body (handler-native T5d). Flat shape 1:1 with
// the former VoyageCreateReply (all required scalars; kind/status — plain string,
// wire byte identical to a string-named enum). Serialized directly (MCP (w,r) writeJSON)
// and projected into api.VoyageCreateReply (huma schema). Conversion from a row — in
// [VoyageHandler.newCreateReply].
type voyageCreateReply struct {
	Kind      string `json:"kind"`
	Location  string `json:"location"`
	ScopeSize int    `json:"scope_size"`
	Status    string `json:"status"`
	VoyageID  string `json:"voyage_id"`
}

// VoyageCreateReply / VoyagePreviewReply — exported aliases of the reply shapes of POST
// /v1/voyages[/preview] for the FULL-TYPED huma envelope (ADR-054, batch-2f self-audit):
// the api package (huma_voyage_op.go) builds Body from the reply type of the extracted CreateTyped/
// PreviewTyped. Aliases (not new types) — the same oapi shapes the legacy (w,r) returns.
type (
	VoyageCreateReply  = voyageCreateReply
	VoyagePreviewReply = voyagePreviewReply
	// VoyageCreateRequest — an exported alias of the domain body shape of POST
	// /v1/voyages[/preview] for the FULL-TYPED huma envelope (ADR-054, batch-2f self-audit):
	// the api package builds it from the typed huma body and calls CreateTyped/PreviewTyped. Fields
	// are exported (the same shape the legacy (w,r) decodes); nested target/notify —
	// [VoyageTargetRequest]/[VoyageNotifyRequest] (shared with Cadence). The computed field
	// maxFailuresPercent — not wire, filled by applyMaxFailures during validation.
	VoyageCreateRequest = voyageCreateRequest
)

// VoyageSpecStub — a non-empty *VoyageHandler stub for generating the huma OpenAPI
// fragment (HumaVoyageSpecYAML): during dump the domain handler is not called, but
// huma.Register requires non-nil for its no-op check. All dependencies nil — the handler
// never executes in spec mode.
func VoyageSpecStub() *VoyageHandler { return &VoyageHandler{} }

// Create — POST /v1/voyages (ADR-043 §6 RBAC-by-kind, §4 target resolve).
//
// Contract:
//   - 202 + {voyage_id, kind, scope_size, status, location}.
//   - 400 — invalid JSON.
//   - 403 — RBAC deny by kind (scenario without incarnation.run / command without
//     errand.run, or at least one resolved incarnation out of scope).
//   - 404 — an explicit incarnation (scenario.incarnations[]) does not exist.
//   - 422 — invalid kind / empty scenario_name|module for the kind / no target /
//     invalid SID/coven/name / where > 4 KiB / on_failure not in {abort,
//     continue} / batch_size|concurrency <= 0 or concurrency > max / empty
//     resolve (voyage_empty_target) / resolved scope > voyage.max_scope
//     (voyage_scope_too_large, DoS-guard S-med-3).
//   - 500 — store/resolver/enforcer not configured / DB failure.
//
// RBAC-by-kind is fail-closed (ADR-043 §6): the permission is picked by kind BEFORE
// target resolve (a cheap bare-check), then for scenario — a per-incarnation
// scope-check over the resolved set (cannot start on an incarnation outside the
// permission scope = privilege escalation).
func (h *VoyageHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}

	var req voyageCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path,
			"invalid JSON body: "+err.Error()))
		return
	}

	reply, err := h.CreateTyped(r.Context(), claims, &req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	w.Header().Set("Location", reply.Location)
	writeJSON(w, http.StatusAccepted, reply, h.logger)
}

// CreateTyped — the extracted domain function POST /v1/voyages (FULL-TYPED ADR-054
// §Pattern, batch-2f self-audit): kind-independent guard (validateVoyageRequest) +
// kind branch (createScenarioTyped/createCommandTyped) without http.ResponseWriter/*http.
// Request. RBAC-by-kind (ADR-043 §6) and self-audit (scenario_run.started / command_run.
// invoked) live INSIDE the kind branches. Body decode — on the calling layer. req is mutated
// in-place (validation). *problemError on failure, success — voyageCreateReply (202).
func (h *VoyageHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) (voyageCreateReply, error) {
	var zero voyageCreateReply
	if h.store == nil || h.scenarioResolver == nil || h.commandResolver == nil || h.enforcer == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	vc, err := h.validateVoyageRequest(req)
	if err != nil {
		return zero, err
	}
	switch vc.kind {
	case voyage.KindScenario:
		return h.createScenarioTyped(ctx, claims, req, vc.onFailure, vc.concurrency, vc.batchMode)
	default: // voyage.KindCommand (validateVoyageRequest guaranteed a valid kind)
		return h.createCommandTyped(ctx, claims, req, vc.onFailure, vc.concurrency, vc.batchMode)
	}
}

// voyageRequestCommon — kind-independent parameters resolved in the common decode/
// validation prelude (see decodeAndValidateRequest). Shared by Create and Preview.
type voyageRequestCommon struct {
	kind        voyage.Kind
	onFailure   voyage.OnFailure
	concurrency int
	batchMode   voyage.BatchMode
}

// decodeAndValidateRequest — the common prelude of POST /v1/voyages and POST
// /v1/voyages/preview: decode body (DisallowUnknownFields), normalize
// on_failure/batch_mode, translate string batch/max_failures (S1/S2), the whole
// kind-independent guard (window incompatibility of batch_size/percent, XOR,
// ranges, concurrency-cap, max_batch_size for window). Preview reuses
// exactly the same path — a consistency guarantee for 422 (preview rejects where
// Create does). On any error it writes a problem and returns ok=false.
//
// Fields irrelevant to preview (dry_run / schedule_at / inter_*_interval_ms /
// on_failure / input) are decoded as usual — preview simply does not read them into the
// reply (they do not affect resolve/scope arithmetic). target / kind / batch* /
// concurrency / max_failures / require_alive — affect and are honored by both.
// validateVoyageRequest — an error-returning kind-independent guard for POST /v1/voyages
// (FULL-TYPED ADR-054 §Pattern, batch-2f self-audit): normalize on_failure/batch_mode,
// translate string batch/max_failures (S1/S2), window incompatibility / XOR /
// ranges / concurrency-cap / max_batch_size for window. Body decode — on the calling
// layer (huma typed Body / (w,r) json.Decode). req is mutated in-place (applyBatchSpec/
// applyMaxFailures). Returns voyageRequestCommon on success, *problemError on failure
// (the same 400/422 classification as the (w,r) variant).
func (h *VoyageHandler) validateVoyageRequest(req *voyageCreateRequest) (voyageRequestCommon, error) {
	var zero voyageRequestCommon
	kind := voyage.Kind(req.Kind)
	if !voyage.ValidKind(kind) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'kind' must be one of {scenario, command}")}
	}
	if req.Target == nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'target' is required")}
	}

	onFailure, ofErr := normalizeVoyageOnFailure(req.OnFailure)
	if ofErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", ofErr)}
	}
	batchMode, bmErr := normalizeVoyageBatchMode(req.BatchMode)
	if bmErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bmErr)}
	}
	if bErr := applyBatchSpec(req); bErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bErr)}
	}
	if mfErr := applyMaxFailures(req); mfErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", mfErr)}
	}
	if batchMode == voyage.BatchModeWindow && req.BatchSize != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'batch_size' is not used with batch_mode=window (window width = concurrency)")}
	}
	if batchMode == voyage.BatchModeWindow && req.BatchPercent != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'batch_percent' is not used with batch_mode=window (window width = concurrency)")}
	}
	if req.BatchSize != nil && req.BatchPercent != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"fields 'batch_size' and 'batch_percent' are mutually exclusive (set exactly one)")}
	}
	if req.BatchSize != nil && *req.BatchSize <= 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'batch_size' must be > 0")}
	}
	if req.BatchPercent != nil && (*req.BatchPercent < 1 || *req.BatchPercent > 100) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'batch_percent' must be in [1, 100]")}
	}
	if req.FailThreshold != nil && *req.FailThreshold <= 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'fail_threshold' must be > 0")}
	}
	concurrency := voyageDefaultConcurrency
	if req.Concurrency != nil {
		concurrency = *req.Concurrency
	}
	if concurrency < 1 || concurrency > voyageMaxConcurrency {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("field 'concurrency' must be in [1, %d]", voyageMaxConcurrency))}
	}
	// max_batch_size for window (ADR-043 amendment §6): the cap is concurrency (=
	// window width), known before resolve. For barrier the cap depends on len(scope) →
	// checked after resolve (the same batchSizeExceedsCapErr).
	if batchMode == voyage.BatchModeWindow {
		if err := h.batchSizeExceedsCapErr(concurrency, "concurrency"); err != nil {
			return zero, err
		}
	}

	return voyageRequestCommon{
		kind:        kind,
		onFailure:   onFailure,
		concurrency: concurrency,
		batchMode:   batchMode,
	}, nil
}

// --- POST /v1/voyages/preview ---

// voyagePreviewReply — 200 body of POST /v1/voyages/preview (ADR-043 amendment
// 2026-06-09 §4). Dry-resolve scope WITHOUT creating a Voyage and WITHOUT exposing the SID
// list (numbers only — a preview of the batch count for a late-binding target).
//
// batch_mode is ALWAYS present (barrier/window) — it explains the semantics of the
// other fields and removes null ambiguity:
//   - barrier → effective_batch_size = the resolved Leg size (ceil scope*pct/100
//     for percent, or an explicit batch_size, or the whole scope as one Leg); total_batches
//     = the Leg count = ceil(scope/effective_batch_size);
//   - window → effective_batch_size is INAPPLICABLE (window width = concurrency, not a Leg) →
//     the field is omitted (nil/omitempty), not null garbage; total_batches = 1 (a flat
//     run in one window wave, parity voyageTotalBatches). batch_mode=window
//     explicitly tells the UI "look at concurrency, not effective_batch_size".
//
// Handler-native (T5d): a flat shape 1:1 with the former VoyagePreviewReply.
// kind/batch_mode — plain string (wire byte identical to a string-named enum);
// effective_batch_size — *int with omitempty (omitted in window).
type voyagePreviewReply struct {
	BatchMode          string `json:"batch_mode"`
	EffectiveBatchSize *int   `json:"effective_batch_size,omitempty"`
	Kind               string `json:"kind"`
	ScopeSize          int    `json:"scope_size"`
	TotalBatches       int    `json:"total_batches"`
}

// Preview — POST /v1/voyages/preview (ADR-043 amendment 2026-06-09 §4, ADR-050
// amendment 2026-06-17 — its own voyage_preview bucket, ADR-047 §S4).
// Dry-resolve scope: answers EXACTLY what Create would do
// (the same validation / resolve / gates — shared decodeAndValidateRequest +
// resolveScenarioScope/resolveCommandScope), but WITHOUT persist (does not call
// BeginTx/Insert) and WITHOUT exposing the SID list.
//
// Contract (consistency with Create — preview rejects in the same places):
//   - 200 + {kind, scope_size, total_batches, batch_mode, effective_batch_size?}.
//   - 400 — invalid JSON.
//   - 403 — RBAC deny by kind / an explicit foreign host (command) / an incarnation out of scope
//     (scenario).
//   - 404 — an explicit incarnation does not exist (scenario).
//   - 422 — invalid kind/target/batch* / empty resolve (voyage_empty_target) /
//     scope > voyage.max_scope (voyage_scope_too_large) / batch_size-cap.
//   - 429 — Tempo per-AID rate-limit (middleware, voyage_preview bucket).
//   - 500 — store/resolver/enforcer not configured / DB failure.
func (h *VoyageHandler) Preview(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}

	var req voyageCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path,
			"invalid JSON body: "+err.Error()))
		return
	}

	reply, err := h.PreviewTyped(r.Context(), claims, &req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// PreviewTyped — the extracted domain function POST /v1/voyages/preview (FULL-TYPED
// ADR-054 §Pattern, batch-2f): dry-resolve scope WITHOUT persist and WITHOUT audit (a read-like
// POST — preview writes no audit event, unlike Create). The same validation/resolve/
// gates as Create (validateVoyageRequest + resolveScenarioScopeErr/
// resolveCommandScopeErr) → 422 consistency (preview rejects in the same places). Body decode
// — on the calling layer. *problemError on failure, success — voyagePreviewReply (200).
func (h *VoyageHandler) PreviewTyped(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) (voyagePreviewReply, error) {
	var zero voyagePreviewReply
	if h.store == nil || h.scenarioResolver == nil || h.commandResolver == nil || h.enforcer == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	vc, err := h.validateVoyageRequest(req)
	if err != nil {
		return zero, err
	}

	var resolved []string
	switch vc.kind {
	case voyage.KindScenario:
		resolved, err = h.resolveScenarioScopeErr(ctx, claims, req)
	default: // voyage.KindCommand
		resolved, err = h.resolveCommandScopeErr(ctx, claims, req)
	}
	if err != nil {
		return zero, err
	}

	// Effective batch_size + max_batch_size-cap (barrier) — the same arithmetic and the
	// same gate as Create (parity with the worker). resolveMaxFailuresPercent is not
	// needed in preview: max_failures does not affect batch count/scope (the failure threshold is runtime).
	effBatchSize := effectiveBatchSize(req.BatchSize, req.BatchPercent, len(resolved))
	if vc.batchMode == voyage.BatchModeBarrier && effBatchSize != nil {
		if err := h.batchSizeExceedsCapErr(*effBatchSize, "batch_size"); err != nil {
			return zero, err
		}
	}

	reply := voyagePreviewReply{
		Kind:         string(vc.kind),
		ScopeSize:    len(resolved),
		TotalBatches: voyageTotalBatches(len(resolved), effBatchSize, vc.batchMode),
		BatchMode:    string(vc.batchMode),
	}
	// effective_batch_size is meaningful only in barrier. In window the field is omitted
	// (window width = concurrency, not a Leg size) — no null garbage in the response.
	if vc.batchMode == voyage.BatchModeBarrier {
		reply.EffectiveBatchSize = effBatchSize
	}
	return reply, nil
}

// resolveScenarioScope — kind=scenario scope resolve + gates (RBAC bare-check
// incarnation.run, per-incarnation scope-check fail-closed, max_scope-cap),
// shared by Create and Preview. Returns a sorted snapshot of incarnation
// names; on any failure it writes a problem and returns ok=false. Persist —
// the caller's job (Create), Preview only reads len(resolved).
// resolveScenarioScopeErr — an error-returning kind=scenario scope resolve + gates
// (FULL-TYPED ADR-054 §Pattern, batch-2f self-audit): RBAC bare-check incarnation.run,
// per-incarnation scope-check fail-closed (ADR-043 §6), max_scope-cap. Shared by
// Create and Preview. nil error → a sorted snapshot of incarnation names; *problemError
// on failure (the same 403/404/422/500 classification as the (w,r) variant). ctx — request
// context (resolve/scope-select read it).
func (h *VoyageHandler) resolveScenarioScopeErr(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) ([]string, error) {
	if req.ScenarioName == "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=scenario requires non-empty 'scenario_name'")}
	}
	if req.Module != "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=scenario must not carry 'module'")}
	}
	if len(req.Target.Incarnations) == 0 && req.Target.Service == "" && len(req.Target.Coven) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=scenario target requires one of incarnations[]/service/coven")}
	}
	for _, name := range req.Target.Incarnations {
		if !incarnation.ValidName(name) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.incarnations: name "+name+" must match "+incarnation.NamePattern)}
		}
	}
	// scenario uses a single filter env tag (coven[0]); the list is a UI convenience,
	// but resolve (ListFilter.Coven exact any-of) accepts one label. Take the
	// first non-empty; validate the rest by format.
	var covenFilter string
	for _, c := range req.Target.Coven {
		if !incarnationCovenLabelValid(c) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.coven: label "+c+" must match "+soul.CovenPattern)}
		}
		if covenFilter == "" {
			covenFilter = c
		}
	}

	// RBAC bare-check incarnation.run (fast rejection before resolve).
	if err := h.checkPermissionErr(claims.Subject, "incarnation", "run", nil); err != nil {
		return nil, err
	}

	resolved, err := h.scenarioResolver.ResolveIncarnations(ctx, VoyageScenarioFilter{
		Incarnations: req.Target.Incarnations,
		Service:      req.Target.Service,
		Coven:        covenFilter,
	})
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return nil, &problemError{problem.New(problem.TypeNotFound, "", err.Error())}
		}
		h.logger.Error("voyage: scenario target resolve failed", slog.Any("error", err))
		return nil, &problemError{problem.New(problem.TypeInternalError, "",
			"resolve voyage scenario target failed")}
	}
	if len(resolved) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"voyage_empty_target: resolved target is empty")}
	}

	// Per-incarnation scope-check (ADR-043 §6 fail-closed): the operator must have
	// incarnation.run on EVERY resolved incarnation (its covens ∪ {name}).
	// Otherwise starting on an incarnation outside the permission scope = privilege escalation.
	// incReader=nil (unit test without DB scope) → skip per-incarnation scope,
	// the bare-check above already guaranteed the base right (cluster-admin / bare role).
	if h.incReader != nil {
		for _, name := range resolved {
			inc, sErr := incarnation.SelectByName(ctx, h.incReader, name)
			if sErr != nil {
				h.logger.Error("voyage: scope-check select failed",
					slog.String("incarnation", name), slog.Any("error", sErr))
				return nil, &problemError{problem.New(problem.TypeInternalError, "",
					"voyage scope check failed")}
			}
			contexts := incarnationCovenContexts(inc.Name, inc.Service, inc.Covens)
			if !h.allowedAnyContext(claims.Subject, "incarnation", "run", contexts) {
				return nil, &problemError{problem.New(problem.TypeForbidden, "",
					"operator lacks incarnation.run on resolved incarnation "+name)}
			}
		}
	}

	if err := h.scopeExceedsCapErr(len(resolved)); err != nil {
		return nil, err
	}
	return resolved, nil
}

// createScenarioTyped — the kind=scenario branch of Create (FULL-TYPED ADR-054 §Pattern,
// batch-2f self-audit). Scope resolve + gates — resolveScenarioScopeErr; then
// batch arithmetic, persist, self-audit scenario_run.started INSIDE the function, reply.
// ctx — request context. *problemError on failure.
func (h *VoyageHandler) createScenarioTyped(
	ctx context.Context, claims *jwt.Claims,
	req *voyageCreateRequest, onFailure voyage.OnFailure, concurrency int, batchMode voyage.BatchMode,
) (voyageCreateReply, error) {
	var zero voyageCreateReply
	resolved, err := h.resolveScenarioScopeErr(ctx, claims, req)
	if err != nil {
		return zero, err
	}

	// notify → ephemeral-Tiding templates (ADR-052(g)): validation + herald.read-guard
	// BEFORE opening the tx. voyage_id/name are stamped in persist after generating the row.
	notifyTidings, err := h.prepareNotifyErr(ctx, claims, req, voyage.KindScenario)
	if err != nil {
		return zero, err
	}

	// Effective batch_size: batch_percent → ceil(scope * pct/100); otherwise
	// req.BatchSize (as is). Depends on len(scope), so computed after
	// resolve (ADR-043 amendment §2).
	effBatchSize := effectiveBatchSize(req.BatchSize, req.BatchPercent, len(resolved))
	// max_failures="N%" → absolute fail_threshold by the number of INCARNATIONS (the scenario
	// run unit): ceil(scope*pct/100), clamp [1,scope] (ADR-043 amendment
	// 2026-06-09 §2). The same base scope=len(resolved) as effectiveBatchSize.
	resolveMaxFailuresPercent(req, len(resolved))
	// max_batch_size for barrier — the cap on the effective batch_size (S-W4).
	if batchMode == voyage.BatchModeBarrier && effBatchSize != nil {
		if err := h.batchSizeExceedsCapErr(*effBatchSize, "batch_size"); err != nil {
			return zero, err
		}
	}

	targets := make([]voyage.VoyageTarget, len(resolved))
	for i, name := range resolved {
		targets[i] = voyage.VoyageTarget{
			TargetKind: voyage.TargetKindIncarnation,
			TargetID:   name,
			BatchIndex: voyageBatchIndex(i, effBatchSize, batchMode),
			Status:     voyage.TargetStatusAwaiting,
		}
	}

	row := h.buildVoyageRow(voyage.KindScenario, req, claims.Subject, &req.ScenarioName, nil, resolved, onFailure, concurrency, batchMode, effBatchSize)
	stampEphemeralTidings(notifyTidings, row.VoyageID)
	if err := h.persistErr(ctx, row, targets, notifyTidings); err != nil {
		return zero, err
	}

	h.emitCreated(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventScenarioRunStarted, row, req.Target, len(resolved))
	return h.newCreateReply(row, len(resolved)), nil
}

// resolveCommandScope — kind=command scope resolve + gates (RBAC errand.run +
// target ∩ Purview hybrid semantics, max_scope-cap), shared by Create and Preview.
// Returns a SID snapshot (AND-merge of sids/coven, trimmed to the operator's Purview);
// on failure it writes a problem and returns ok=false. Hybrid branches (ADR-047 S4):
// an explicit foreign SID → 403, a broad target trimmed to zero → 422 voyage_empty_target.
// Persist — the caller's job (Create), Preview only reads len(resolved).
// resolveCommandScopeErr — an error-returning kind=command scope resolve + gates
// (FULL-TYPED ADR-054 §Pattern, batch-2f self-audit): RBAC errand.run + target ∩
// Purview hybrid semantics, max_scope-cap. Shared by Create and Preview. nil error → a SID
// snapshot; *problemError on failure. Hybrid branches (ADR-047 S4): an explicit foreign SID →
// 403, a broad target trimmed to zero → 422 voyage_empty_target. ctx — request context.
func (h *VoyageHandler) resolveCommandScopeErr(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) ([]string, error) {
	if req.Module == "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=command requires non-empty 'module'")}
	}
	if req.ScenarioName != "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=command must not carry 'scenario_name'")}
	}
	if len(req.Target.SIDs) == 0 && len(req.Target.Coven) == 0 && req.Target.Where == "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=command target requires one of sids[]/coven/where")}
	}
	// S-med-1 (security): where: is NOT evaluated yet (MVP, a no-op in the resolver —
	// only stored in target_origin). A where-only target (no sids, no coven)
	// therefore silently resolves to the WHOLE fleet — a narrowing predicate turns into
	// an expander, violating the invariant "invocation narrows scope, does not expand".
	// We reject the single dangerous case. where as an ADDITION to sids/coven
	// is allowed: scope is already narrowed by them, where does not expand.
	if req.Target.Where != "" && len(req.Target.SIDs) == 0 && len(req.Target.Coven) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"voyage_where_not_evaluated: target where: is not evaluated yet (MVP); "+
				"specify sids or coven to narrow the scope")}
	}
	for _, sid := range req.Target.SIDs {
		if !soul.ValidSID(sid) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.sids: SID "+sid+" must match "+soul.SIDPattern)}
		}
	}
	for _, label := range req.Target.Coven {
		if !soul.ValidCoven(label) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.coven: label "+label+" must match "+soul.CovenPattern)}
		}
	}
	if len(req.Target.Where) > voyageMaxWhereBytes {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("target.where exceeds %d bytes", voyageMaxWhereBytes))}
	}

	filter := VoyageCommandFilter{
		SIDs:         req.Target.SIDs,
		Covens:       req.Target.Coven,
		Where:        req.Target.Where,
		RequireAlive: voyage.ResolveRequireAlive(req.RequireAlive),
	}

	var resolved []string
	// RBAC + target ∩ Purview (ADR-047 S4, security-fix). scoper=nil (unit test without
	// DB scope) → cluster-wide resolve + nil-context bare-check (backcompat). Otherwise:
	// a single ResolvePurview(errand.run) gives BOTH the existence-gate (does the operator hold the right in
	// any scope — otherwise a nil-context Check falsely denies a scoped role, like souls G1)
	// AND the scope boundary for intersecting with the resolved target.
	if h.scoper == nil {
		if err := h.checkPermissionErr(claims.Subject, "errand", "run", nil); err != nil {
			return nil, err
		}
		out, err := h.commandResolver.ResolveSIDs(ctx, filter)
		if err != nil {
			h.logger.Error("voyage: command target resolve failed", slog.Any("error", err))
			return nil, &problemError{problem.New(problem.TypeInternalError, "",
				"resolve voyage command target failed")}
		}
		resolved = out
	} else {
		// Existence-gate errand.run: the operator must hold the right in AT LEAST SOME
		// scope (or Unrestricted). Empty Purview (no right / revoked → Deny) →
		// 403. Narrowing by scope is done by the resolver (parity souls HoldsAction).
		pv := h.scoper.ResolvePurview(claims.Subject, "errand", "run")
		scope := soulpurview.Resolve(pv)
		if scope.Empty() {
			// scope.Empty carries no reason (revoke vs no-perm are merged in Resolve).
			// We classify the reason via the enforcer for error-semantics parity with the
			// scenario path and the scoper==nil branch (revoked → TypeOperatorRevokedToken,
			// no-perm → 403). A nil-context here is safe: scope is empty in ANY
			// context (Empty already proved it), a false deny of a scoped role is
			// impossible — checkPermissionErr here is only a reason CLASSIFIER, not a second gate.
			if err := h.checkPermissionErr(claims.Subject, "errand", "run", nil); err != nil {
				return nil, err
			}
			// Unreachable safety net: Empty without a deny from the enforcer is impossible.
			return nil, &problemError{problem.New(problem.TypeForbidden, "",
				"operator lacks required permission errand.run")}
		}
		scoped, err := h.commandResolver.ResolveSIDsInScope(ctx, filter, scope)
		if err != nil {
			h.logger.Error("voyage: command target resolve failed", slog.Any("error", err))
			return nil, &problemError{problem.New(problem.TypeInternalError, "",
				"resolve voyage command target failed")}
		}
		// Hybrid branch 1 (anti-escalation): an explicitly listed foreign host in sids[] →
		// 403 (parity with the per-incarnation scope-check of the scenario path). Silent
		// trimming here would mask an escalation attempt.
		if len(scoped.DeniedExplicit) > 0 {
			return nil, &problemError{problem.New(problem.TypeForbidden, "",
				"operator lacks errand.run on target host "+scoped.DeniedExplicit[0])}
		}
		resolved = scoped.SIDs
	}
	// Hybrid branch 3: an empty intersection (a broad target trimmed to zero) → 422,
	// distinguished from a 403 escalation (a valid request, but nothing to execute).
	if len(resolved) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"voyage_empty_target: resolved target is empty")}
	}

	if err := h.scopeExceedsCapErr(len(resolved)); err != nil {
		return nil, err
	}
	return resolved, nil
}

// createCommandTyped — the kind=command branch of Create (FULL-TYPED ADR-054 §Pattern,
// batch-2f self-audit). Scope resolve + gates — resolveCommandScopeErr; then
// batch arithmetic, persist, self-audit command_run.invoked INSIDE the function, reply.
func (h *VoyageHandler) createCommandTyped(
	ctx context.Context, claims *jwt.Claims,
	req *voyageCreateRequest, onFailure voyage.OnFailure, concurrency int, batchMode voyage.BatchMode,
) (voyageCreateReply, error) {
	var zero voyageCreateReply
	resolved, err := h.resolveCommandScopeErr(ctx, claims, req)
	if err != nil {
		return zero, err
	}

	// notify → ephemeral-Tiding templates (ADR-052(g)): validation + herald.read-guard
	// BEFORE opening the tx. voyage_id/name are stamped in persist after generating the row.
	notifyTidings, err := h.prepareNotifyErr(ctx, claims, req, voyage.KindCommand)
	if err != nil {
		return zero, err
	}

	// Effective batch_size: batch_percent → ceil(scope * pct/100); otherwise
	// req.BatchSize. Depends on len(scope) (ADR-043 amendment §2).
	effBatchSize := effectiveBatchSize(req.BatchSize, req.BatchPercent, len(resolved))
	// max_failures="N%" → absolute fail_threshold by the number of HOSTS (the command
	// run unit): ceil(scope*pct/100), clamp [1,scope] (ADR-043 amendment
	// 2026-06-09 §2). The same base scope=len(resolved) as effectiveBatchSize.
	resolveMaxFailuresPercent(req, len(resolved))
	if batchMode == voyage.BatchModeBarrier && effBatchSize != nil {
		if err := h.batchSizeExceedsCapErr(*effBatchSize, "batch_size"); err != nil {
			return zero, err
		}
	}

	targets := make([]voyage.VoyageTarget, len(resolved))
	for i, sid := range resolved {
		targets[i] = voyage.VoyageTarget{
			TargetKind: voyage.TargetKindSID,
			TargetID:   sid,
			BatchIndex: voyageBatchIndex(i, effBatchSize, batchMode),
			Status:     voyage.TargetStatusAwaiting,
		}
	}

	row := h.buildVoyageRow(voyage.KindCommand, req, claims.Subject, nil, &req.Module, resolved, onFailure, concurrency, batchMode, effBatchSize)
	stampEphemeralTidings(notifyTidings, row.VoyageID)
	if err := h.persistErr(ctx, row, targets, notifyTidings); err != nil {
		return zero, err
	}

	h.emitCreated(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventCommandRunInvoked, row, req.Target, len(resolved))
	return h.newCreateReply(row, len(resolved)), nil
}

// scopeExceedsCap checks the resolved scope against maxScope (DoS-guard
// S-med-3). maxScope=0 → unlimited. On excess it writes 422
// voyage_scope_too_large and returns true (the caller stops processing).
// Called AFTER resolving the target into []VoyageTarget, BEFORE InsertTargets: one
// POST must not resolve the whole fleet (100k per-row INSERT in one transaction +
// an uncontrolled blast-radius).
// scopeExceedsCapErr — DoS-guard on the resolved scope size against maxScope (S-med-3,
// FULL-TYPED ADR-054 §Pattern). nil → within the limit; otherwise 422 voyage_scope_too_large.
func (h *VoyageHandler) scopeExceedsCapErr(size int) error {
	if h.maxScope <= 0 || size <= h.maxScope {
		return nil
	}
	return &problemError{problem.New(problem.TypeValidationFailed, "",
		fmt.Sprintf("voyage_scope_too_large: scope %d exceeds the limit %d; "+
			"narrow target (sids/coven/incarnations) or raise voyage.max_scope",
			size, h.maxScope))}
}

// batchSizeExceedsCap checks the batch/window size against maxBatchSize (DoS-guard
// S-W4, ADR-043 amendment §6). maxBatchSize=0 → no limit. On excess it writes
// 422 voyage_batch_size_too_large (parity voyage_scope_too_large) and returns
// true. field — the field name in the detail ("batch_size" for barrier / "concurrency" for
// window).
// batchSizeExceedsCapErr — DoS-guard on the batch/window size against maxBatchSize (S-W4,
// FULL-TYPED ADR-054 §Pattern). nil → within the limit; otherwise 422 voyage_batch_size_too_large.
func (h *VoyageHandler) batchSizeExceedsCapErr(size int, field string) error {
	if h.maxBatchSize <= 0 || size <= h.maxBatchSize {
		return nil
	}
	return &problemError{problem.New(problem.TypeValidationFailed, "",
		fmt.Sprintf("voyage_batch_size_too_large: %s %d exceeds the limit %d; "+
			"reduce %s or raise voyage.max_batch_size",
			field, size, h.maxBatchSize, field))}
}

// effectiveBatchSize resolves the effective batch size (ADR-043 amendment §2):
//   - batch_percent set → ceil(scope * pct/100), but at least 1 and at most
//     scope (a batch cannot exceed the whole scope);
//   - otherwise → batchSize as is (including nil = the whole run in one Leg).
//
// scope <= 0 (a guard; normally the caller rejected an empty resolve) → returns batchSize.
func effectiveBatchSize(batchSize, batchPercent *int, scope int) *int {
	if batchPercent == nil {
		return batchSize
	}
	if scope <= 0 {
		return batchSize
	}
	// ceil(scope * pct / 100), integer.
	eff := (scope*(*batchPercent) + 99) / 100
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	return &eff
}

// buildVoyageRow assembles a *voyage.Voyage from the request. target_resolved
// is serialized into a JSONB array of units (names / SIDs); target_origin —
// the declarative shape for audit/UI. total_batches is computed from the unit count and
// the effective batch_size. effBatchSize — the resolved batch_size (from batch_size
// or batch_percent, ADR-043 amendment §2); nil → the whole run in one Leg.
func (h *VoyageHandler) buildVoyageRow(
	kind voyage.Kind, req *voyageCreateRequest, startedByAID string,
	scenarioName, module *string, resolved []string, onFailure voyage.OnFailure, concurrency int, batchMode voyage.BatchMode,
	effBatchSize *int,
) *voyage.Voyage {
	resolvedJSON, _ := json.Marshal(resolved) // []string always serializes
	originJSON, _ := json.Marshal(req.Target)

	var inputJSON []byte
	if req.Input != nil {
		inputJSON, _ = json.Marshal(req.Input)
	}

	row := &voyage.Voyage{
		VoyageID:       audit.NewULID(),
		Kind:           kind,
		ScenarioName:   scenarioName,
		Module:         module,
		Input:          inputJSON,
		TargetResolved: resolvedJSON,
		TargetOrigin:   originJSON,
		Concurrency:    &concurrency,
		DryRun:         req.DryRun,
		ScheduleAt:     req.ScheduleAt,
		OnFailure:      &onFailure,
		TotalBatches:   voyageTotalBatches(len(resolved), effBatchSize, batchMode),
		StartedByAID:   startedByAID,
		FailThreshold:  req.FailThreshold,
		RequireAlive:   req.RequireAlive,
	}
	// batch_mode is written only for window — barrier stays NULL (forward-compat:
	// "not set" = barrier, distinguishable from an explicit value in audit/UI).
	if batchMode == voyage.BatchModeWindow {
		bm := batchMode
		row.BatchMode = &bm
	}
	// window: batch_size / batch_percent are unused (window width =
	// concurrency) — not written. barrier: write the effective batch_size (resolved
	// from percent or explicit), and keep batch_percent as is for audit/UI.
	if batchMode != voyage.BatchModeWindow {
		row.BatchSize = effBatchSize
		row.BatchPercent = req.BatchPercent
	}
	if req.InterBatchIntervalMS != nil && *req.InterBatchIntervalMS > 0 {
		d := time.Duration(*req.InterBatchIntervalMS) * time.Millisecond
		row.InterBatchInterval = &d
	}
	// inter_unit_interval — a per-unit pause, meaningful only in window (parity:
	// inter_batch_interval only in barrier). Not written in barrier.
	if batchMode == voyage.BatchModeWindow && req.InterUnitIntervalMS != nil && *req.InterUnitIntervalMS > 0 {
		d := time.Duration(*req.InterUnitIntervalMS) * time.Millisecond
		row.InterUnitInterval = &d
	}
	return row
}

// persist writes targets + voyage + ephemeral Tidings (notify, ADR-052(g)) in
// ONE PG transaction (snapshot-scope does not "jitter" between INSERTs, ADR-043).
// Atomicity guarantees the PRESENCE of ephemeral rules in the DB by commit time, but
// NOT their visibility to the dispatcher's TTL snapshot (DefaultRuleCacheTTL=15s) — so
// after commit, when notify rules are present, an explicit two-level invalidation
// is needed (in-process + cross-keeper), otherwise a fast run dispatches the terminal against
// a stale snapshot and the one-shot notification silently misses.
// Returns false and writes a problem on error. On tx rollback there is neither a Voyage
// nor ephemeral rules (and nothing to invalidate — call STRICTLY after commit).
// persistErr — an error-returning persist of a Voyage (FULL-TYPED ADR-054 §Pattern,
// batch-2f self-audit): targets + voyage + ephemeral Tidings (notify, ADR-052(g)) in
// ONE PG transaction. nil error → success; *problemError on failure (500). On tx rollback
// — no Voyage and no ephemeral rules. ctx — request context (rollback on a
// background ctx, TTL-snapshot invalidation reads ctx).
func (h *VoyageHandler) persistErr(ctx context.Context, row *voyage.Voyage, targets []voyage.VoyageTarget, notifyTidings []herald.Tiding) error {
	tx, err := h.store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("voyage.create: begin tx failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage failed")}
	}
	// Insert the Voyage itself first (FK voyage_targets → voyages); then targets.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	if err := voyage.Insert(ctx, tx, row); err != nil {
		h.logger.Error("voyage.create: insert failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage failed")}
	}
	if err := voyage.InsertTargets(ctx, tx, row.VoyageID, targets); err != nil {
		h.logger.Error("voyage.create: insert targets failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage targets failed")}
	}
	// The notify block's ephemeral Tidings — in the SAME tx (ADR-052(g)). Any failure
	// (including violating the ephemeral⟺voyage_id invariant or a name race) rolls back
	// the whole Voyage — atomicity by construction. herald.InsertTiding accepts
	// the same pgx.Tx (herald.ExecQueryRower ⊂ the tx interface).
	for i := range notifyTidings {
		if err := herald.InsertTiding(ctx, tx, &notifyTidings[i]); err != nil {
			h.logger.Error("voyage.create: insert ephemeral tiding failed",
				slog.String("voyage_id", row.VoyageID),
				slog.String("herald", notifyTidings[i].Herald),
				slog.Any("error", err))
			return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage notify failed")}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("voyage.create: commit failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage failed")}
	}
	committed = true
	// The ephemeral Tidings were inserted via direct InsertTiding, bypassing herald.Service CRUD
	// (and its invalidation) — flush the dispatcher's TTL snapshot EXPLICITLY, strictly after
	// commit (two-level: in-process + cross-keeper, HA run finalization may
	// run on another keeper). Without notify no rules were created — nothing
	// to invalidate. nil-safe: dev without herald → no-op, degrades to TTL.
	if len(notifyTidings) > 0 && h.tidingInvalidator != nil {
		h.tidingInvalidator.InvalidateTidings(ctx, row.VoyageID)
	}
	return nil
}

// newCreateReply assembles the 202 reply (voyageCreateReply) from row + scope_size
// (FULL-TYPED ADR-054 §Pattern). Location = the relative resource URL (the same one
// Create(w,r)/huma puts in the Location header).
func (h *VoyageHandler) newCreateReply(row *voyage.Voyage, scopeSize int) voyageCreateReply {
	location := "/v1/voyages/" + row.VoyageID
	return voyageCreateReply{
		VoyageID:  row.VoyageID,
		Kind:      string(row.Kind),
		ScopeSize: scopeSize,
		Status:    string(row.Status),
		Location:  location,
	}
}

// checkPermissionErr — an error-returning RBAC.Check (FULL-TYPED ADR-054 §Pattern,
// batch-2f self-audit). nil → allowed. revoked → 401 equivalent
// (TypeOperatorRevokedToken), no-perm → 403. Mapping as in middleware.RequirePermission.
func (h *VoyageHandler) checkPermissionErr(aid, resource, action string, ctx map[string]string) error {
	if err := h.enforcer.Check(aid, resource, action, ctx); err != nil {
		if errors.Is(err, rbac.ErrOperatorRevoked) {
			return &problemError{problem.New(problem.TypeOperatorRevokedToken, "", "archon "+aid+" has been revoked")}
		}
		detail := "operator lacks required permission"
		if errors.Is(err, rbac.ErrPermissionDenied) {
			detail = "operator lacks required permission " + resource + "." + action
		}
		return &problemError{problem.New(problem.TypeForbidden, "", detail)}
	}
	return nil
}

// allowedAnyContext OR-checks a permission across a set of contexts (parity
// RequirePermissionMulti). Empty set → a single attempt with nil-context (bare/`*`).
func (h *VoyageHandler) allowedAnyContext(aid, resource, action string, contexts []map[string]string) bool {
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

// --- GET /v1/voyages ---

// voyageDTO — the native response shape for GET/List (handler-native T5d). A flat
// shape 1:1 with the former Voyage: FIELD ORDER is alphabetical (like oapi-codegen),
// so json.Marshal emits keys byte-exact; kind/status/batch_mode/on_failure —
// plain string (wire identical to a string-named enum); pointer-optional with omitempty —
// all nullable. target_resolved (names/SIDs) is NOT included (the UI reads scope_size).
// Serialized directly (MCP/cadence-runs writeJSON), projected into api.Voyage
// (huma schema). Conversion from a row — [toVoyageDTO].
type voyageDTO struct {
	Attempt           int               `json:"attempt"`
	BatchMode         *string           `json:"batch_mode,omitempty"`
	BatchPercent      *int              `json:"batch_percent,omitempty"`
	BatchSize         *int              `json:"batch_size,omitempty"`
	Concurrency       *int              `json:"concurrency,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	CurrentBatchIndex int               `json:"current_batch_index"`
	DryRun            bool              `json:"dry_run"`
	FailThreshold     *int              `json:"fail_threshold,omitempty"`
	FinishedAt        *time.Time        `json:"finished_at,omitempty"`
	Kind              string            `json:"kind"`
	Module            *string           `json:"module,omitempty"`
	OnFailure         *string           `json:"on_failure,omitempty"`
	RequireAlive      *bool             `json:"require_alive,omitempty"`
	ScenarioName      *string           `json:"scenario_name,omitempty"`
	ScheduleAt        *time.Time        `json:"schedule_at,omitempty"`
	ScopeSize         int               `json:"scope_size"`
	StartedAt         *time.Time        `json:"started_at,omitempty"`
	StartedByAID      string            `json:"started_by_aid"`
	Status            string            `json:"status"`
	Summary           *voyageSummaryDTO `json:"summary,omitempty"`
	Target            *voyageTargetDTO  `json:"target,omitempty"`
	TotalBatches      int               `json:"total_batches"`
	VoyageID          string            `json:"voyage_id"`
}

// voyageTargetDTO — the native declarative-target reply shape (origin, handler-native
// T5d). All fields pointer-optional with omitempty (1:1 VoyageTarget); ORDER
// alphabetical (coven/incarnations/service/sids/where), wire key `sids` matches.
// Filled by unmarshaling the target_origin JSONB.
type voyageTargetDTO struct {
	Coven        *[]string `json:"coven,omitempty"`
	Incarnations *[]string `json:"incarnations,omitempty"`
	Service      *string   `json:"service,omitempty"`
	Sids         *[]string `json:"sids,omitempty"`
	Where        *string   `json:"where,omitempty"`
}

// voyageSummaryDTO — native run aggregates (handler-native T5d). no_match — *int
// with omitempty (0 → key omitted, like noMatchPtr); the rest — int required.
type voyageSummaryDTO struct {
	Cancelled int  `json:"cancelled"`
	Failed    int  `json:"failed"`
	NoMatch   *int `json:"no_match,omitempty"`
	Succeeded int  `json:"succeeded"`
	Total     int  `json:"total"`
}

// voyageTargetEntryDTO — a native voyage_targets row (All-runs drill, handler-native
// T5d). target_kind/status — plain string; apply_id/errand_id/finished_at —
// pointer-optional with omitempty. Field ORDER alphabetical (like oapi-codegen).
type voyageTargetEntryDTO struct {
	ApplyID    *string    `json:"apply_id,omitempty"`
	BatchIndex int        `json:"batch_index"`
	ErrandID   *string    `json:"errand_id,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Status     string     `json:"status"`
	TargetID   string     `json:"target_id"`
	TargetKind string     `json:"target_kind"`
}

// scopeSizeOf decodes the target_resolved JSONB array and counts its length.
// Invalid/empty → 0 (graceful; terminal counters come from summary).
func scopeSizeOf(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0
	}
	return len(items)
}

// toVoyageDTO projects domain [voyage.Voyage] into the wire type [Voyage]
// (category C). Typed-enum Kind/Status [VoyageKind]/[VoyageStatus] —
// wire is the same string. scenario_name/module/batch_mode/on_failure — pointer-optional
// with omitempty: the domain carries `*string`/`*BatchMode`/`*OnFailure`, gen — the same
// `*string`/`*enum` (nil → key omitted, byte-for-byte with the old `string ...,omitempty`
// on an empty value). target_origin — a typed-struct JSONB (NOT byte-passthrough):
// unmarshal into [VoyageTarget] (domain `SIDs` → gen `Sids`, wire key `sids`
// matches); invalid → target omitted (graceful, as before). date-time
// created_at/started_at/finished_at/schedule_at — nanosecond wire (the domain is
// a bare time.Time): assigned as is WITHOUT .UTC()/Truncate, keeping byte-for-byte
// parity with the former direct serialization of v.CreatedAt.
func toVoyageDTO(v *voyage.Voyage) voyageDTO {
	dto := voyageDTO{
		VoyageID:          v.VoyageID,
		Kind:              string(v.Kind),
		Status:            string(v.Status),
		ScopeSize:         scopeSizeOf(v.TargetResolved),
		BatchSize:         v.BatchSize,
		BatchPercent:      v.BatchPercent,
		Concurrency:       v.Concurrency,
		DryRun:            v.DryRun,
		TotalBatches:      v.TotalBatches,
		CurrentBatchIndex: v.CurrentBatchIndex,
		FailThreshold:     v.FailThreshold,
		RequireAlive:      v.RequireAlive,
		ScheduleAt:        v.ScheduleAt,
		Attempt:           v.Attempt,
		StartedByAID:      v.StartedByAID,
		CreatedAt:         v.CreatedAt,
		StartedAt:         v.StartedAt,
		FinishedAt:        v.FinishedAt,
	}
	if v.BatchMode != nil {
		bm := string(*v.BatchMode)
		dto.BatchMode = &bm
	}
	dto.ScenarioName = v.ScenarioName
	dto.Module = v.Module
	if v.OnFailure != nil {
		of := string(*v.OnFailure)
		dto.OnFailure = &of
	}
	if len(v.TargetOrigin) > 0 {
		var t voyageTargetDTO
		if err := json.Unmarshal(v.TargetOrigin, &t); err == nil {
			dto.Target = &t
		}
	}
	if v.Summary != nil {
		dto.Summary = &voyageSummaryDTO{
			Total:     v.Summary.Total,
			Succeeded: v.Summary.Succeeded,
			Failed:    v.Summary.Failed,
			Cancelled: v.Summary.Cancelled,
			NoMatch:   noMatchPtr(v.Summary.NoMatch),
		}
	}
	return dto
}

// noMatchPtr wraps summary.no_match into a pointer-optional [VoyageSummary].
// The old DTO carried `int json:"no_match,omitempty"` (0 → key omitted); gen — `*int`
// with omitempty. We keep byte-for-byte: 0 → nil pointer (key omitted), otherwise —
// a pointer to the value.
func noMatchPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// List — GET /v1/voyages (ADR-043 §5).
//
// Query filters: kind (exact), status (multi-value OR). Pagination —
// sharedapi.ParsePage (max=1000). Sort created_at DESC.
func (h *VoyageHandler) List(w http.ResponseWriter, r *http.Request) {
	page, err := sharedapi.ParsePage(r.URL.Query())
	if err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, err.Error()))
		return
	}
	q := r.URL.Query()
	reply, perr := h.ListTyped(r.Context(), VoyageListInput{
		Kind:     q.Get("kind"),
		Statuses: q["status"],
		Page:     page,
	})
	if perr != nil {
		writeProblemError(w, r, perr)
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// VoyageListInput — the typed input of [VoyageHandler.ListTyped] (FULL-TYPED ADR-054 §Pattern,
// batch-2f). Kind/Statuses — query filters (enum validation — in ListTyped → 422); Page —
// already-parsed pagination (the caller checks the range via ParsePage/CheckPageBounds
// → 400). Empty → filter not applied.
type VoyageListInput struct {
	Kind     string
	Statuses []string
	Page     sharedapi.Page
}

// VoyageListReply — the native typed output of list (handler-native T5d). A flat shape 1:1
// with the former VoyageListReply (items/offset/limit/total). items — native voyageDTO.
type VoyageListReply struct {
	Items  []voyageDTO `json:"items"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
	Total  int         `json:"total"`
}

// ListTyped — the extracted domain function GET /v1/voyages (READ, no audit; FULL-TYPED
// ADR-054 §Pattern). enum validation of kind/status → 422; DB failure → 500.
func (h *VoyageHandler) ListTyped(ctx context.Context, in VoyageListInput) (VoyageListReply, error) {
	var zero VoyageListReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	var filter voyage.ListFilter
	if in.Kind != "" {
		kind := voyage.Kind(in.Kind)
		if !voyage.ValidKind(kind) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'kind' filter (must be one of scenario/command)")}
		}
		filter.Kind = kind
	}
	if len(in.Statuses) > 0 {
		filter.Statuses = make([]voyage.Status, 0, len(in.Statuses))
		for _, s := range in.Statuses {
			st := voyage.Status(s)
			if !voyage.ValidStatus(st) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
					"invalid 'status' filter (scheduled/pending/running/succeeded/failed/partial_failed/cancelled)")}
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}
	items, total, err := voyage.List(ctx, h.store, filter, in.Page.Offset, in.Page.Limit)
	if err != nil {
		h.logger.Error("voyage.list: select failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list voyages failed")}
	}
	dtos := make([]voyageDTO, 0, len(items))
	for _, v := range items {
		dtos = append(dtos, toVoyageDTO(v))
	}
	return VoyageListReply{Items: dtos, Offset: in.Page.Offset, Limit: in.Page.Limit, Total: total}, nil
}

// Get — GET /v1/voyages/{id} (detail + summary).
func (h *VoyageHandler) Get(w http.ResponseWriter, r *http.Request) {
	dto, err := h.GetTyped(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dto, h.logger)
}

// VoyageDTO / VoyageSummaryDTO / VoyageTargetDTO / VoyageTargetEntryDTO — exported
// aliases of the native handler DTOs (handler-native T5d). Needed by the huma projection (huma_voyage_reply.go
// builds api-native from these types) and the cadence-runs envelope alias (PagedResponse[VoyageDTO]).
type (
	VoyageDTO            = voyageDTO
	VoyageSummaryDTO     = voyageSummaryDTO
	VoyageTargetDTO      = voyageTargetDTO
	VoyageTargetEntryDTO = voyageTargetEntryDTO
)

// GetTyped — the extracted domain function GET /v1/voyages/{id} (READ, no audit;
// FULL-TYPED ADR-054 §Pattern). 404 not-found, 422 bad id, 500 DB failure.
func (h *VoyageHandler) GetTyped(ctx context.Context, id string) (VoyageDTO, error) {
	var zero VoyageDTO
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	v, err := voyage.SelectByID(ctx, h.store, id)
	if err != nil {
		if errors.Is(err, voyage.ErrVoyageNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "voyage "+id+" not found")}
		}
		h.logger.Error("voyage.get: select failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get voyage failed")}
	}
	return toVoyageDTO(v), nil
}

// Targets — GET /v1/voyages/{id}/targets (All-runs drill).
func (h *VoyageHandler) Targets(w http.ResponseWriter, r *http.Request) {
	reply, err := h.TargetsTyped(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// VoyageTargetsReply — the native typed output of targets (handler-native T5d). A flat shape
// 1:1 with the former VoyageTargetsReply (voyage_id + targets[], both required).
type VoyageTargetsReply struct {
	Targets  []voyageTargetEntryDTO `json:"targets"`
	VoyageID string                 `json:"voyage_id"`
}

// TargetsTyped — the extracted domain function GET /v1/voyages/{id}/targets (READ, no
// audit; FULL-TYPED ADR-054 §Pattern). Existence-probe → 404; 422 bad id; 500 DB failure.
func (h *VoyageHandler) TargetsTyped(ctx context.Context, id string) (VoyageTargetsReply, error) {
	var zero VoyageTargetsReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	// Existence-probe: 404 if the Voyage does not exist (otherwise an empty list
	// is indistinguishable from a "nonexistent id").
	if _, err := voyage.SelectByID(ctx, h.store, id); err != nil {
		if errors.Is(err, voyage.ErrVoyageNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "voyage "+id+" not found")}
		}
		h.logger.Error("voyage.targets: probe failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get voyage targets failed")}
	}
	targets, err := voyage.SelectTargets(ctx, h.store, id)
	if err != nil {
		h.logger.Error("voyage.targets: select failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get voyage targets failed")}
	}
	out := make([]voyageTargetEntryDTO, 0, len(targets))
	for i := range targets {
		t := &targets[i]
		out = append(out, voyageTargetEntryDTO{
			TargetKind: string(t.TargetKind),
			TargetID:   t.TargetID,
			BatchIndex: t.BatchIndex,
			Status:     string(t.Status),
			ApplyID:    t.ApplyID,
			ErrandID:   t.ErrandID,
			FinishedAt: t.FinishedAt,
		})
	}
	return VoyageTargetsReply{VoyageID: id, Targets: out}, nil
}

// --- DELETE /v1/voyages/{id} ---

// Cancel — DELETE /v1/voyages/{id} (ADR-043 §"deferred": cancel pending/
// scheduled — a simple transition to cancelled; running-cancel — post-MVP, see below).
//
// Contract:
//   - 202 + {voyage_id, status:"cancelled"} — a pending/scheduled run is cancelled.
//   - 404 — voyage_id does not exist.
//   - 409 — a running run (`voyage_running_cancel_unsupported`) or an already
//     terminal one (`voyage_already_terminal`). A complex abort of a running run is
//     deferred post-MVP (inherits deferred Tide/ErrandRun).
//   - 500 — store not configured / DB failure.
func (h *VoyageHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	reply, err := h.CancelTyped(r.Context(), claims, chi.URLParam(r, "id"))
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, reply, h.logger)
}

// VoyageCancelReply — the native typed output of cancel (handler-native T5d). A flat shape
// 1:1 with the former VoyageCancelReply (voyage_id + status:cancelled, both required).
type VoyageCancelReply struct {
	Status   string `json:"status"`
	VoyageID string `json:"voyage_id"`
}

// CancelTyped — the extracted domain function DELETE /v1/voyages/{id} (FULL-TYPED
// ADR-054 §Pattern, batch-2f self-audit): cancel pending/scheduled. RBAC-by-kind
// (ADR-043 §6 fail-closed) AFTER loading the row (kind is visible from the DB). self-audit
// scenario_run.cancelled / command_run.cancelled is written INSIDE the function. *problemError
// on failure (404 not-found, 409 running/terminal, 422 bad id), success — 202 reply.
func (h *VoyageHandler) CancelTyped(ctx context.Context, claims *jwt.Claims, id string) (VoyageCancelReply, error) {
	var zero VoyageCancelReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	v, err := voyage.SelectByID(ctx, h.store, id)
	if err != nil {
		if errors.Is(err, voyage.ErrVoyageNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "voyage "+id+" not found")}
		}
		h.logger.Error("voyage.cancel: select failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cancel voyage failed")}
	}
	// RBAC-by-kind (ADR-043 §6, fail-closed): cancel is mutating, requires the same
	// right as create by kind. Checked AFTER loading the row (kind is visible
	// only from the DB). scenario→incarnation.run, command→errand.run.
	resource, action := "errand", "run"
	if v.Kind == voyage.KindScenario {
		resource, action = "incarnation", "run"
	}
	if h.enforcer != nil {
		if err := h.checkPermissionErr(claims.Subject, resource, action, nil); err != nil {
			return zero, err
		}
	}

	if voyage.IsTerminal(v.Status) {
		return zero, &problemError{problem.New(problem.TypeErrandNotCancellable, "",
			"voyage_already_terminal: status="+string(v.Status))}
	}
	if v.Status == voyage.StatusRunning {
		return zero, &problemError{problem.New(problem.TypeErrandNotCancellable, "",
			"voyage_running_cancel_unsupported: running-Voyage cancel is post-MVP")}
	}

	// pending / scheduled → cancelled (a simple transition; running-abort — post-MVP).
	prev := string(v.Status)
	if err := cancelNonRunningVoyage(ctx, h.store, id); err != nil {
		if errors.Is(err, errVoyageCancelRaceLost) {
			// Between SELECT and UPDATE the Voyage was claimed by a VoyageWorker (became running).
			return zero, &problemError{problem.New(problem.TypeErrandNotCancellable, "",
				"voyage_running_cancel_unsupported: voyage was claimed by a worker")}
		}
		h.logger.Error("voyage.cancel: update failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cancel voyage failed")}
	}

	eventType := audit.EventScenarioRunCancelled
	if v.Kind == voyage.KindCommand {
		eventType = audit.EventCommandRunCancelled
	}
	h.emitCancelled(claims.Subject, middleware.ScenarioInvocationSource(ctx), eventType, v, prev)

	return VoyageCancelReply{
		VoyageID: id,
		Status:   string(voyage.StatusCancelled),
	}, nil
}

// --- audit emitters ---

// emitCreated writes scenario_run.started / command_run.invoked (source=api/mcp,
// archon_aid=JWT.sub). Background ctx — the HTTP server may have canceled r.Context()
// after the write response. `input` is NOT included (invariant A ADR-027).
func (h *VoyageHandler) emitCreated(aid string, source audit.Source, eventType audit.EventType, row *voyage.Voyage, target *voyageTargetRequest, scopeSize int) {
	if h.auditW == nil {
		return
	}
	payload := map[string]any{
		"voyage_id":   row.VoyageID,
		"kind":        string(row.Kind),
		"scope_size":  scopeSize,
		"concurrency": derefInt(row.Concurrency),
		"dry_run":     row.DryRun,
		"on_failure":  derefOnFailure(row.OnFailure),
	}
	if row.ScenarioName != nil {
		payload["scenario_name"] = *row.ScenarioName
	}
	if row.Module != nil {
		payload["module"] = *row.Module
	}
	if row.BatchSize != nil {
		payload["batch_size"] = *row.BatchSize
	}
	if t := targetAuditPayload(target); len(t) > 0 {
		payload["target"] = t
	}
	ev := &audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: row.VoyageID,
		Payload:       payload,
	}
	if err := h.auditW.Write(context.Background(), ev); err != nil {
		h.logger.Error("voyage.create: audit created write failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
	}
}

// emitCancelled writes scenario_run.cancelled / command_run.cancelled.
func (h *VoyageHandler) emitCancelled(aid string, source audit.Source, eventType audit.EventType, v *voyage.Voyage, prevStatus string) {
	if h.auditW == nil {
		return
	}
	ev := &audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: v.VoyageID,
		Payload: map[string]any{
			"voyage_id":       v.VoyageID,
			"kind":            string(v.Kind),
			"previous_status": prevStatus,
		},
	}
	if err := h.auditW.Write(context.Background(), ev); err != nil {
		h.logger.Error("voyage.cancel: audit cancelled write failed",
			slog.String("voyage_id", v.VoyageID), slog.Any("error", err))
	}
}

// targetAuditPayload builds the declarative-target shape for audit (without sensitive data).
func targetAuditPayload(t *voyageTargetRequest) map[string]any {
	if t == nil {
		return nil
	}
	out := map[string]any{}
	if len(t.Incarnations) > 0 {
		out["incarnations"] = t.Incarnations
	}
	if t.Service != "" {
		out["service"] = t.Service
	}
	if len(t.SIDs) > 0 {
		out["sids"] = t.SIDs
	}
	if t.Where != "" {
		out["where"] = t.Where
	}
	if len(t.Coven) > 0 {
		out["coven"] = t.Coven
	}
	return out
}

// --- helpers ---

func derefOnFailure(p *voyage.OnFailure) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// normalizeVoyageOnFailure: "" → continue (parity ErrandRun default), a valid value →
// itself, otherwise a detail error.
func normalizeVoyageOnFailure(s string) (voyage.OnFailure, string) {
	switch s {
	case "", string(voyage.OnFailureContinue):
		return voyage.OnFailureContinue, ""
	case string(voyage.OnFailureAbort):
		return voyage.OnFailureAbort, ""
	default:
		return "", "field 'on_failure' must be one of {abort, continue}"
	}
}

// applyBatchSpec translates the string field `batch` (S1) into req.BatchSize /
// req.BatchPercent, reusing the whole existing downstream (window-guard /
// XOR / ranges / effectiveBatchSize / max_batch_size). Returns the detail
// of a 422 error or "" (ok / field not set).
//
// Semantics:
//   - req.Batch == nil → no-op (old path batch_size/batch_percent).
//   - trim(*req.Batch) == "" → "not set": the whole scope in one Leg (no-op, not 422).
//   - non-empty + (batch_size|batch_percent set) → conflict (cannot use both formats).
//   - "N%" → percent (as batch_percent=N); "N" → hosts (as batch_size=N).
//   - malformed → a human-readable detail showing the source string.
//
// The field is already parsed via [voyage.ParseBatchSpec] (fail-closed grammar);
// window incompatibility and the max_batch_size cap are checked below on the common path.
func applyBatchSpec(req *voyageCreateRequest) (detail string) {
	if req.Batch == nil {
		return ""
	}
	raw := *req.Batch
	mode, value, err := voyage.ParseBatchSpec(raw)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		// An explicit empty string — "not set", the whole scope in one Leg. Not an error.
		return ""
	}
	// Reject the two-format conflict BEFORE parsing the value: a non-empty batch together
	// with numeric batch_size/batch_percent is a contradictory payload.
	if req.BatchSize != nil || req.BatchPercent != nil {
		return "voyage_batch_spec_conflict: field 'batch' is mutually exclusive with 'batch_size'/'batch_percent' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'batch' must be N or N%% (1-100); got %q", raw)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.BatchPercent = &value
	default: // BatchSpecHosts
		req.BatchSize = &value
	}
	return ""
}

// applyMaxFailures translates the string field `max_failures` (S2) into the failure threshold
// req.FailThreshold, reusing the fail-closed grammar of [voyage.ParseBatchSpec]
// (`N`|`N%`, the same as batch). Returns the detail of a 422 error or "" (ok / field not
// set). ADR-043 amendment 2026-06-09 §2/§3.
//
// Semantics (symmetric with applyBatchSpec):
//   - req.MaxFailures == nil → no-op (old path fail_threshold).
//   - trim(*req.MaxFailures) == "" → "not set" (no-op, not 422).
//   - non-empty + fail_threshold set → conflict (cannot use both formats).
//   - "N" → absolute: writes req.FailThreshold = N (behaves like fail_threshold:N).
//   - "N%" → percent: stashes N in req.maxFailuresPercent; the absolute threshold
//     is resolved later by scope (resolveMaxFailuresPercent after target resolve).
//   - malformed → a human-readable detail showing the source string.
//
// The batch grammar shares the hosts/percent semantics with the threshold: hosts mode here =
// an absolute failure count, percent mode = a percent of run units.
func applyMaxFailures(req *voyageCreateRequest) (detail string) {
	if req.MaxFailures == nil {
		return ""
	}
	raw := *req.MaxFailures
	mode, value, err := voyage.ParseBatchSpec(raw)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		// An empty string — "not set" (no-op, not an input error).
		return ""
	}
	// Reject the two-format conflict BEFORE parsing the value: a non-empty max_failures
	// together with an int fail_threshold is a contradictory payload (the same error code
	// as batch, ADR-043 amendment 2026-06-09 §3).
	if req.FailThreshold != nil {
		return "voyage_batch_spec_conflict: field 'max_failures' is mutually exclusive with 'fail_threshold' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'max_failures' must be N or N%% (1-100); got %q", raw)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.maxFailuresPercent = &value
	default: // BatchSpecHosts → an absolute failure count
		req.FailThreshold = &value
	}
	return ""
}

// resolveMaxFailuresPercent resolves max_failures="N%" into an absolute
// req.FailThreshold AFTER scope resolve (ADR-043 amendment 2026-06-09 §2): the threshold
// is computed by run units (incarnations for scenario / hosts for command — the
// same scope base as effectiveBatchSize). ceil(scope*pct/100), clamp [1, scope].
// No-op if percent is unset (req.maxFailuresPercent == nil) or scope <= 0.
func resolveMaxFailuresPercent(req *voyageCreateRequest, scope int) {
	if req.maxFailuresPercent == nil || scope <= 0 {
		return
	}
	eff := (scope*(*req.maxFailuresPercent) + 99) / 100 // ceil
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	req.FailThreshold = &eff
}

// normalizeVoyageBatchMode: "" → barrier (default, ADR-043 amendment), a valid value →
// itself, otherwise a detail error.
func normalizeVoyageBatchMode(s string) (voyage.BatchMode, string) {
	switch s {
	case "", string(voyage.BatchModeBarrier):
		return voyage.BatchModeBarrier, ""
	case string(voyage.BatchModeWindow):
		return voyage.BatchModeWindow, ""
	default:
		return "", "field 'batch_mode' must be one of {barrier, window}"
	}
}

// voyageBatchIndex — a unit's batch_index at Insert time. barrier → Leg index
// (chunk by batch_size); window → 0 for all (a flat run, no Legs; ADR-043
// amendment §7).
func voyageBatchIndex(i int, batchSize *int, batchMode voyage.BatchMode) int {
	if batchMode == voyage.BatchModeWindow {
		return 0
	}
	return batchIndexFor(i, batchSize)
}

// batchIndexFor — the 0-based Leg index for the i-th unit (chunk by batch_size).
// batch_size nil/<=0 → the whole run in one Leg (batch_index=0), parity
// chunkIncarnations/chunkSIDs of the worker.
func batchIndexFor(i int, batchSize *int) int {
	if batchSize == nil || *batchSize <= 0 {
		return 0
	}
	return i / *batchSize
}

// voyageTotalBatches — total_batches at Insert time. barrier → the Leg count; window
// → 1 (a flat run in one window wave, batch_index=0 for all units).
func voyageTotalBatches(n int, batchSize *int, batchMode voyage.BatchMode) int {
	if n == 0 {
		return 0
	}
	if batchMode == voyage.BatchModeWindow {
		return 1
	}
	return totalBatches(n, batchSize)
}

// totalBatches — the Leg count for n units at a given batch_size.
func totalBatches(n int, batchSize *int) int {
	if n == 0 {
		return 0
	}
	if batchSize == nil || *batchSize <= 0 {
		return 1
	}
	bs := *batchSize
	return (n + bs - 1) / bs
}

// incarnationCovenLabelValid — the incarnation env-tag format matches the
// host coven label (ADR-008 amendment a uses the same predicate).
func incarnationCovenLabelValid(label string) bool { return soul.ValidCoven(label) }

// errVoyageCancelRaceLost — CAS-cancel returned 0 rows: between SELECT and UPDATE
// the Voyage was claimed by a VoyageWorker (pending/scheduled → running). The caller treats it
// as 409 "running-cancel unsupported".
var errVoyageCancelRaceLost = errors.New("voyage: cancel race lost (claimed by worker)")

// cancelNonRunningVoyageSQL — CAS transition pending/scheduled → cancelled.
// WHERE narrowed to non-running statuses (idempotent + race-safe): if the Voyage
// became running between SELECT and UPDATE, 0 rows → errVoyageCancelRaceLost.
// finished_at = NOW() (CHECK voyages_terminal_finished_at: cancelled — terminal).
const cancelNonRunningVoyageSQL = `
UPDATE voyages
SET status      = 'cancelled',
    finished_at = NOW()
WHERE voyage_id = $1
  AND status IN ('pending', 'scheduled')
`

// cancelNonRunningVoyage transitions a pending/scheduled Voyage to cancelled. 0 rows
// (the Voyage became running / is already terminal — the latter the caller rejected via probe) →
// errVoyageCancelRaceLost. running-abort — post-MVP (inherits deferred
// Tide/ErrandRun).
func cancelNonRunningVoyage(ctx context.Context, db voyage.ExecQueryRower, id string) error {
	tag, err := db.Exec(ctx, cancelNonRunningVoyageSQL, id)
	if err != nil {
		return fmt.Errorf("voyage: cancel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errVoyageCancelRaceLost
	}
	return nil
}
