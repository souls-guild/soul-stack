package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// OracleDeps — wire-up dependencies for the `PortentEvent` handler over EventStream
// (ADR-030, beacons reactor, slice S2). Receives a Portent → matches against the
// Decree registry → enqueues a named scenario on the work queue.
//
// Required for wire-up:
//   - DB — the decrees / oracle_fires registry (match + cooldown) + souls (resolves
//     the subject's covens by the authoritative SID);
//   - Where — sandbox CEL for Decree where-predicates (event.data);
//   - Enqueuer — enqueues the named scenario on the work queue (ADR-027);
//   - AuditWriter — `oracle.fired` / `decree.circuit_tripped` on triggers.
//
// Optional:
//   - Metrics — keeper_oracle_* descriptor (ADR-024 S4). nil → instrumentation
//     disabled (nil-safe Observe* methods — no-op), same as Metrics in [AugurDeps].
//
// nil OracleDeps (handler not wired up) → the handler logs a warning and ignores
// the Portent (minimally-invasive fallback for builds without Oracle), same as AugurDeps.
type OracleDeps struct {
	DB          oracleDB
	Where       *oracle.WhereEvaluator
	Enqueuer    ScenarioEnqueuer
	AuditWriter audit.Writer
	Metrics     *oracle.OracleMetrics

	// CircuitMaxFires / CircuitWindow — thresholds for Oracle's circuit breaker
	// (ADR-030(a), beacons S4): after CircuitMaxFires triggers of one Decree
	// within the CircuitWindow, it auto-disables (enabled=false). Default
	// resolution happens in the daemon (empty field → default). CircuitMaxFires==0
	// → breaker OFF (escape hatch): BumpCircuit is never called, the Decree never
	// auto-disables.
	CircuitMaxFires int
	CircuitWindow   time.Duration
}

// oracleDB — the combined PG surface the Oracle resolve needs: decrees /
// oracle_fires (oracle.ExecQueryRower) + a souls reader (soul.ExecQueryRower)
// for the subject's covens from the authoritative registry. *pgxpool.Pool
// satisfies both.
type oracleDB interface {
	oracle.ExecQueryRower
	soul.ExecQueryRower
}

// ScenarioEnqueuer — the narrow surface for enqueueing a named scenario on the
// work queue (ADR-027) on behalf of an Oracle reaction. An interface (rather
// than a direct scenario/applyrun import) keeps the Oracle handler independent
// of HOW the subject host's incarnation/ServiceRef is resolved — that's a
// wire-up decision (the daemon).
//
// EnqueueScenario enqueues the action_scenario on host subjectSID with input
// actionInput (vault-ref AS-IS — invariant A of ADR-027). Returns the apply_id
// of the enqueued run (for audit correlation) or a resolve/enqueue error.
type ScenarioEnqueuer interface {
	EnqueueScenario(ctx context.Context, req EnqueueScenarioRequest) (applyID string, err error)
}

// EnqueueScenarioRequest — parameters for enqueueing a scenario from an
// Oracle reaction. SubjectSID — the authoritative SID of the sending host
// (mTLS peer cert). IncarnationName — the target incarnation from the Decree
// (DECISION #1, option b): the Enqueuer resolves the ServiceRef FROM it
// (incarnation.service → the service registry), not from the Decree.
// ScenarioName — the action_scenario from the Decree (whitelist). ActionInput
// — the JSONB action_input from the Decree (vault-ref AS-IS); nil/empty →
// scenario with no input. DecreeName — the name of the matched Decree (for
// audit/enqueue diagnostics).
type EnqueueScenarioRequest struct {
	SubjectSID      string
	IncarnationName string
	ScenarioName    string
	ActionInput     map[string]any
	DecreeName      string
}

func (d *OracleDeps) validate() error {
	if d.DB == nil {
		return errors.New("grpc: OracleDeps.DB is required")
	}
	if d.Where == nil {
		return errors.New("grpc: OracleDeps.Where is required")
	}
	if d.Enqueuer == nil {
		return errors.New("grpc: OracleDeps.Enqueuer is required")
	}
	if d.AuditWriter == nil {
		return errors.New("grpc: OracleDeps.AuditWriter is required")
	}
	return nil
}

// vigilSource — implements [VigilSource] over the vigils + souls registry
// (connect-time broadcast VigilSnapshot, ADR-030). Resolves the host's covens
// from the authoritative souls registry, then the active Vigil set by
// sid ∪ covens, and projects it into transport [keeperv1.VigilDef]. Wired up
// in the daemon with the same pool as OracleDeps.DB.
type vigilSource struct {
	db oracleDB
}

// NewVigilSource assembles a [VigilSource] over a combined pool (vigils +
// souls). db must satisfy [oracle.ExecQueryRower] and [soul.ExecQueryRower]
// (*pgxpool.Pool satisfies both).
func NewVigilSource(db oracleDB) VigilSource {
	return &vigilSource{db: db}
}

func (s *vigilSource) ActiveVigilsForSID(ctx context.Context, sid string) ([]*keeperv1.VigilDef, error) {
	var covens []string
	su, err := soul.SelectBySID(ctx, s.db, sid)
	switch {
	case err == nil:
		covens = su.Coven
	case errors.Is(err, soul.ErrSoulNotFound):
		// The host isn't in the registry yet (onboarding incomplete) — a
		// coven Vigil won't match, but a sid Vigil might. Resolve with empty covens.
	default:
		return nil, fmt.Errorf("grpc: vigil source covens resolve: %w", err)
	}

	vigils, err := oracle.SelectActiveVigilsForSubject(ctx, s.db, sid, covens)
	if err != nil {
		return nil, err
	}
	out := make([]*keeperv1.VigilDef, 0, len(vigils))
	for _, v := range vigils {
		def := &keeperv1.VigilDef{
			Name:     v.Name,
			Interval: v.IntervalSpec,
			Check:    v.CheckAddr,
		}
		if len(v.Params) > 0 {
			params := &structpb.Struct{}
			if err := params.UnmarshalJSON(v.Params); err != nil {
				return nil, fmt.Errorf("grpc: vigil %q params unmarshal: %w", v.Name, err)
			}
			def.Params = params
		}
		out = append(out, def)
	}
	return out, nil
}

// handlePortentEvent — handler for the [keeperv1.PortentEvent] payload (ADR-030).
//
// SID comes from the session's mTLS peer cert (passed by the caller), NOT from
// PortentEvent.sid — the authority for a Soul's identity is the certificate
// (ADR-012(i), ADR-030: a beacon event is an untrusted input).
//
// Flow (default-deny):
//  1. SelectDecreesByBeacon(beacon_name) — enabled Decrees on this Vigil.
//     Empty → nothing (no rule → no action).
//  2. Subject covens from the souls registry (NOT from the payload).
//  3. For each Decree: SubjectMatches (sid/coven) — no → skip;
//     where-CEL (if set) over event.data — false → skip.
//  4. Cooldown check per-(decree, subject): within the window → skip + debug log.
//  5. EnqueueScenario(action_scenario, action_input) → RecordFire → audit
//     oracle.fired.
//
// A failure on one Decree doesn't interrupt processing of the others
// (best-effort — multiple Decrees may react to one Portent). Strict
// default-deny: any uncertainty (no subject match / where false / resolve
// failure) → skip, not action.
func (h *eventStreamHandler) handlePortentEvent(ctx context.Context, sid, sessionID string, evt *keeperv1.PortentEvent) {
	deps := h.deps.Oracle
	if deps == nil {
		h.logger.Warn("eventstream: PortentEvent received but oracle not wired up",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if evt == nil {
		h.logger.Warn("eventstream: PortentEvent payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	beacon := evt.GetBeaconName()
	if beacon == "" {
		h.logger.Warn("eventstream: PortentEvent with empty beacon_name — ignoring",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	// A valid Portent was accepted (non-empty beacon_name) — the denominator for the other metrics.
	deps.Metrics.ObservePortentReceived()

	decrees, err := oracle.SelectDecreesByBeacon(ctx, deps.DB, beacon)
	if err != nil {
		h.logger.Warn("eventstream: oracle select decrees failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("beacon", beacon),
			slog.Any("error", err),
		)
		return
	}
	if len(decrees) == 0 {
		// Default-deny: no matching Decree → the event triggers no action.
		h.logger.Debug("eventstream: oracle no decree for beacon — default-deny",
			slog.String("sid", sid), slog.String("beacon", beacon))
		return
	}

	// Subject covens from the authoritative souls registry (NOT from the payload).
	covens, err := h.oracleSubjectCovens(ctx, deps, sid)
	if err != nil {
		h.logger.Warn("eventstream: oracle subject covens resolve failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}

	// where-CEL reads the full event (typed payload V5-1 + legacy data, both
	// access styles): we pass evt whole into evaluateDecree. The activation is
	// assembled in WhereEvaluator.EvalEvent.
	for _, decree := range decrees {
		h.evaluateDecree(ctx, deps, sid, sessionID, beacon, decree, covens, evt)
	}
}

// oracleSubjectCovens resolves the subject's covens by authoritative SID from
// the souls registry. ErrSoulNotFound → empty covens (the host isn't
// registered yet; a sid-Decree can still match by SID, a coven-Decree cannot).
func (h *eventStreamHandler) oracleSubjectCovens(ctx context.Context, deps *OracleDeps, sid string) ([]string, error) {
	s, err := soul.SelectBySID(ctx, deps.DB, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return s.Coven, nil
}

// subjectInIncarnation reports whether the sending host belongs to the
// Decree's target incarnation: incarnation.name is the root Coven label
// (ADR-008), so membership reduces to incarnationName ∈ subject covens.
// covens are authoritative, from the souls registry. An empty incarnationName
// (theoretically impossible — NOT NULL in the schema) is treated fail-closed
// as "not a member."
func subjectInIncarnation(incarnationName string, covens []string) bool {
	if incarnationName == "" {
		return false
	}
	for _, c := range covens {
		if c == incarnationName {
			return true
		}
	}
	return false
}

// evaluateDecree applies one Decree to a Portent: subject match → where-CEL →
// membership check → cooldown → enqueue → record fire → audit. Any failure is
// logged and does NOT fail processing of the other Decrees (best-effort).
// default-deny: every uncertainty means skip.
func (h *eventStreamHandler) evaluateDecree(
	ctx context.Context,
	deps *OracleDeps,
	sid, sessionID, beacon string,
	decree *oracle.Decree,
	covens []string,
	evt *keeperv1.PortentEvent,
) {
	if !oracle.SubjectMatches(decree, sid, covens) {
		h.logger.Debug("eventstream: oracle decree subject mismatch — skip",
			slog.String("sid", sid), slog.String("decree", decree.Name))
		return
	}

	// Membership sanity check (DECISION #3): the sending host must belong to
	// the Decree's target incarnation. incarnation.name is the root Coven
	// label (ADR-008), so membership = incarnation_name ∈ subject covens
	// (covens are authoritative, from the souls registry). Not a member →
	// skip + warn; fire is NOT recorded, oracle.fired audit is NOT written:
	// this forbids enqueueing an incarnation's scenario on a host outside it
	// (protection against cross-incarnation escalation, ADR-030(b)).
	if !subjectInIncarnation(decree.IncarnationName, covens) {
		h.logger.Warn("eventstream: oracle decree subject not in target incarnation — skip",
			slog.String("sid", sid),
			slog.String("decree", decree.Name),
			slog.String("incarnation", decree.IncarnationName),
		)
		return
	}

	if decree.WhereCEL != nil && *decree.WhereCEL != "" {
		ok, err := deps.Where.EvalEvent(*decree.WhereCEL, evt)
		if err != nil {
			// A broken where_cel (compile error) is a Decree configuration
			// problem; default-deny: don't fire, log it for the operator.
			h.logger.Warn("eventstream: oracle decree where-CEL compile failed — skip (default-deny)",
				slog.String("sid", sid),
				slog.String("decree", decree.Name),
				slog.Any("error", err),
			)
			return
		}
		if !ok {
			h.logger.Debug("eventstream: oracle decree where-CEL false — skip",
				slog.String("sid", sid), slog.String("decree", decree.Name))
			return
		}
	}

	now := time.Now().UTC()

	// Cooldown-check per-(decree, subject) (loop-prevention, ADR-030(a)).
	lastFired, hasFired, err := oracle.LastFiredAt(ctx, deps.DB, decree.Name, sid)
	if err != nil {
		h.logger.Warn("eventstream: oracle cooldown read failed — skip (fail-safe)",
			slog.String("sid", sid), slog.String("decree", decree.Name), slog.Any("error", err))
		return
	}
	if oracle.WithinCooldown(decree.Cooldown, lastFired, hasFired, now) {
		deps.Metrics.ObserveCooldownBlocked()
		h.logger.Debug("eventstream: oracle decree within cooldown — skip",
			slog.String("sid", sid),
			slog.String("decree", decree.Name),
			slog.String("cooldown", decree.Cooldown),
		)
		return
	}

	// The Decree passed the whole filter (subject/membership/where/cooldown)
	// and will be enqueued — we record the match BEFORE enqueue (so the
	// attempt counts even if enqueue fails; a matched↔enqueued gap signals
	// enqueue errors, visible in the series).
	deps.Metrics.ObserveDecreeMatched()

	// action_input (JSONB) → map for RunSpec. Malformed JSON is a Decree
	// configuration error (validated at the service layer, S3); default-deny: skip.
	var actionInput map[string]any
	if len(decree.ActionInput) > 0 {
		if err := json.Unmarshal(decree.ActionInput, &actionInput); err != nil {
			h.logger.Warn("eventstream: oracle decree action_input is not a JSON object — skip",
				slog.String("sid", sid), slog.String("decree", decree.Name), slog.Any("error", err))
			return
		}
	}

	applyID, err := deps.Enqueuer.EnqueueScenario(ctx, EnqueueScenarioRequest{
		SubjectSID:      sid,
		IncarnationName: decree.IncarnationName,
		ScenarioName:    decree.ActionScenario,
		ActionInput:     actionInput,
		DecreeName:      decree.Name,
	})
	if err != nil {
		h.logger.Warn("eventstream: oracle scenario enqueue failed",
			slog.String("sid", sid),
			slog.String("decree", decree.Name),
			slog.String("scenario", decree.ActionScenario),
			slog.Any("error", err),
		)
		return
	}
	deps.Metrics.ObserveScenarioEnqueued()

	// Cooldown state is recorded AFTER a successful enqueue: recording a fire
	// without an actual enqueue would falsely block future reactions.
	if err := oracle.RecordFire(ctx, deps.DB, decree.Name, sid, now); err != nil {
		// The run is already enqueued — the fire record is best-effort: on
		// failure, cooldown isn't activated for this pair (a repeat is possible
		// until the next successful record), but an idempotent scenario
		// dampens the loop at the action level (ADR-030(a)).
		h.logger.Warn("eventstream: oracle record fire failed — cooldown not persisted",
			slog.String("sid", sid), slog.String("decree", decree.Name), slog.Any("error", err))
	}

	// circuit breaker (ADR-030(a), beacons S4): the second loop-prevention
	// barrier after cooldown. cooldown dampens per-(decree, subject);
	// circuit-breaker counts rule triggers TOTAL (fixed window) and
	// auto-disables the Decree once the threshold is exceeded. breaker-off
	// (max_fires==0, escape hatch) → BumpCircuit is never called at all.
	// now is the same instant as cooldown/audit.
	h.tripCircuitIfTripped(ctx, deps, decree, now)

	h.auditOracleFired(ctx, sid, beacon, decree.Name, decree.ActionScenario, applyID)
	h.logger.Info("eventstream: oracle fired",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.String("beacon", beacon),
		slog.String("decree", decree.Name),
		slog.String("scenario", decree.ActionScenario),
		slog.String("apply_id", applyID),
	)
}

// tripCircuitIfTripped increments the fixed-window trigger counter for a
// Decree and, once the threshold is reached, auto-disables the rule
// (circuit breaker, ADR-030(a)). Called AFTER a successful enqueue+RecordFire
// (only an actually-enqueued trigger counts). breaker-off (CircuitMaxFires==0)
// — no-op (BumpCircuit isn't called). Any failure is best-effort: logged and
// does NOT fail processing (the run is already enqueued).
//
// The trip is a single-winner operation: TripDecree flips enabled true→false
// atomically, and only the instance with RowsAffected==1 writes
// metric+audit+warn — under a concurrent trip from multiple Keeper instances,
// exactly one alerts.
func (h *eventStreamHandler) tripCircuitIfTripped(ctx context.Context, deps *OracleDeps, decree *oracle.Decree, now time.Time) {
	if deps.CircuitMaxFires <= 0 {
		return // breaker OFF (escape-hatch)
	}

	cnt, err := oracle.BumpCircuit(ctx, deps.DB, decree.Name, now, deps.CircuitWindow)
	if err != nil {
		h.logger.Warn("eventstream: oracle circuit bump failed — breaker counter not updated",
			slog.String("decree", decree.Name), slog.Any("error", err))
		return
	}
	if cnt < deps.CircuitMaxFires {
		return
	}

	tripped, err := oracle.TripDecree(ctx, deps.DB, decree.Name, now)
	if err != nil {
		h.logger.Warn("eventstream: oracle circuit trip failed — decree not auto-disabled",
			slog.String("decree", decree.Name), slog.Any("error", err))
		return
	}
	if !tripped {
		// Another Keeper instance already won the trip (or the Decree was
		// disabled by an operator) — don't duplicate alert/audit/metric.
		return
	}

	deps.Metrics.ObserveCircuitTripped()
	h.auditDecreeCircuitTripped(ctx, decree.Name, cnt, deps.CircuitWindow)
	h.logger.Warn("eventstream: oracle circuit tripped — decree auto-disabled",
		slog.String("decree", decree.Name),
		slog.Int("fire_count", cnt),
		slog.String("window", deps.CircuitWindow.String()),
		slog.Int("max_fires", deps.CircuitMaxFires),
	)
}

// auditDecreeCircuitTripped writes the `decree.circuit_tripped` event when a
// Decree is auto-disabled by the circuit breaker (ADR-030(a)). The payload is
// a property of the rule (decree / fire_count / window / trigger), WITHOUT
// subject/beacon/event.data: we don't include the untrusted event payload —
// the trip is tied to the rule, not a specific host. Best-effort: an audit
// failure doesn't undo the already-performed auto-disable.
func (h *eventStreamHandler) auditDecreeCircuitTripped(ctx context.Context, decree string, fireCount int, window time.Duration) {
	if err := h.deps.Oracle.AuditWriter.Write(ctx, &audit.Event{
		EventType: audit.EventDecreeCircuitTripped,
		Source:    audit.SourceSoulGRPC,
		Payload: map[string]any{
			"decree":     decree,
			"fire_count": fireCount,
			"window":     window.String(),
			"trigger":    "circuit_breaker",
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: oracle circuit-tripped audit write failed",
			slog.String("decree", decree),
			slog.Any("error", err),
		)
	}
}

// auditOracleFired writes the `oracle.fired` event when the reactor triggers
// (ADR-030(b), category soul_grpc). event.data values are NEVER included in
// the payload (untrusted source). Best-effort: an audit failure doesn't undo
// the already-enqueued run (same pattern as the other event handlers).
func (h *eventStreamHandler) auditOracleFired(ctx context.Context, sid, beacon, decree, scenario, applyID string) {
	if err := h.deps.Oracle.AuditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventOracleFired,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload: map[string]any{
			"sid":      sid,
			"beacon":   beacon,
			"decree":   decree,
			"scenario": scenario,
			"apply_id": applyID,
		},
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: oracle audit write failed",
			slog.String("sid", sid),
			slog.String("decree", decree),
			slog.Any("error", err),
		)
	}
}
