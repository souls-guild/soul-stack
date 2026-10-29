package conductor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/shellgate"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// cadenceTxBeginner is the narrow surface for the spawn tick: open a tx in
// which due cadences are selected FOR UPDATE SKIP LOCKED, child Voyages are
// spawned and schedules advanced — all atomically (ADR-046 §4, against
// double-spawn on crash). The real *pgxpool.Pool satisfies it; unit tests
// substitute a fake pool.
type cadenceTxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CadenceSpawner is the concrete executor of due-cadence spawning, OWNED by
// Conductor (ADR-048 §3, moved from reaper without logic changes). Implements
// [Spawner]. On each Conductor-leader tick: SELECT due cadences (enabled AND
// next_run_at <= NOW()) FOR UPDATE SKIP LOCKED → for each, apply
// overlap_policy → (if allowed) insert a Voyage from the recipe + advance
// next_run_at/last_run_at — ALL in one PG tx. Single-executor via the
// Conductor leader (Redis lease conductor:leader, ADR-006) + SKIP LOCKED give
// exactly-one-spawn per tick without racing.
//
// Run's signature is compatible with [Spawner] (`(ctx, duration, batch) →
// (int64, error)`). The duration argument is NOT used in spawn (the predicate
// is next_run_at <= NOW() directly); batchSize caps the number of schedules
// per tick (anti-avalanche after long downtime). audit is nil-safe; resolvers
// are required (without them spawn is impossible — Run returns an error).
type CadenceSpawner struct {
	pool      cadenceTxBeginner
	scenarioR cadence.ScenarioResolver
	commandR  cadence.CommandResolver
	audit     audit.Writer

	// enforcer / gate — the console gate on the background path (ADR-0074
	// amendment, NIM-197). This is the surface the deprecation window was built
	// for: everywhere else an operator is waiting on an HTTP status, here the
	// only witness is the schedule itself. Both nil-safe.
	enforcer ConsoleChecker
	gate     *shellgate.Gate
	// soulReader resolves the target hosts' Coven labels for that gate, so the
	// recipe author's `soul.console on coven=web` narrows the spawn instead of
	// killing it (NIM-650). nil-safe, and nil means coven-scoped grants fail
	// closed here.
	//
	// The POOL and not the tick's own tx, deliberately: a failed statement
	// poisons its transaction, and this one runs between the FOR UPDATE select
	// and AdvanceSchedule. Read it on the tx and a transient error would turn a
	// recorded skip into a rolled-back tick that stalls every other due
	// schedule — the exact failure mode this path is written to avoid.
	soulReader soul.ExecQueryRower

	logger *slog.Logger

	// now is the clock the window and freeze decisions read; nil is time.Now.
	now func() time.Time
}

// ConsoleChecker is the narrow RBAC surface the spawn path needs — one
// scope-aware permission check. Satisfied by *rbac.Holder and *rbac.Enforcer;
// declared here so the conductor does not take the whole enforcer surface for a
// single call.
type ConsoleChecker interface {
	Check(aid, resource, action string, context map[string]string) error
}

// NewCadenceSpawner constructs a spawner. resolvers are required (production
// wire-up uses handlers' PG resolvers via an adapter); audit is nil-safe;
// logger is nil-safe. enforcer/gate carry the console gate (NIM-197) and are
// nil-safe: without an enforcer the gate records `unconfigured` instead of
// deciding either way.
func NewCadenceSpawner(
	pool *pgxpool.Pool,
	scenarioR cadence.ScenarioResolver,
	commandR cadence.CommandResolver,
	auditW audit.Writer,
	enforcer ConsoleChecker,
	gate *shellgate.Gate,
	logger *slog.Logger,
) *CadenceSpawner {
	s := &CadenceSpawner{
		pool:      pool,
		scenarioR: scenarioR,
		commandR:  commandR,
		audit:     auditW,
		enforcer:  enforcer,
		gate:      gate,
		logger:    logger,
	}
	// Guarded assignment: a nil *pgxpool.Pool stored straight into the interface
	// would be a non-nil interface holding a nil pointer, and the coven read
	// would panic instead of falling back to the host-only context.
	if pool != nil {
		s.soulReader = pool
	}
	return s
}

// newCadenceSpawnerFromBeginner is an internal constructor for unit tests.
func newCadenceSpawnerFromBeginner(
	pool cadenceTxBeginner,
	scenarioR cadence.ScenarioResolver,
	commandR cadence.CommandResolver,
	auditW audit.Writer,
	logger *slog.Logger,
) *CadenceSpawner {
	return &CadenceSpawner{pool: pool, scenarioR: scenarioR, commandR: commandR, audit: auditW, logger: logger}
}

// spawnedRecord is a snapshot of one tick's result for audit AFTER commit (we
// don't hold events under an open tx; best-effort emit after the DB commit).
type spawnedRecord struct {
	cadenceID    string
	voyageID     string // empty for a skip record
	scheduledFor time.Time
	scheduleAt   *time.Time // the spawned Voyage's deferred start; nil when it starts at once
	scopeSize    int
	skipped      bool   // true → a skip event, false → spawned
	skipReason   string // why the spawn was skipped; empty ⇒ overlap
	module       string // verb-shell module of a gate-skipped recipe (skipReasonConsoleRequired)
	noWindow     *noWindow
}

// Skip reasons carried by [spawnedRecord]. `overlap` is the original
// overlap_policy skip; `console_required` is the console gate refusing a recipe
// whose creator no longer satisfies it (NIM-197); `no_window` is an
// in_maintenance_window recipe whose incarnations share no open moment
// (NIM-1075).
const (
	skipReasonOverlap         = "overlap"
	skipReasonConsoleRequired = "console_required"
	skipReasonNoWindow        = "no_window"
)

// Run performs one iteration of due-cadence spawning. Returns the number of
// spawned Voyages (skip/queue ticks don't count — affected = "how many runs
// were actually created"). All work on due cadences happens in one tx: on a
// crash before commit nothing is persisted, on the next tick the rows are due
// again (next_run_at not advanced) — no double-spawn.
//
// batchSize=0 → default cap ([defaultBatch]).
func (s *CadenceSpawner) Run(ctx context.Context, _ time.Duration, batchSize int) (int64, error) {
	if s.pool == nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: pool is nil")
	}
	if s.scenarioR == nil || s.commandR == nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: resolvers are required")
	}
	if batchSize <= 0 {
		batchSize = defaultBatch
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	due, err := cadence.SelectDueForUpdate(ctx, tx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: %w", err)
	}

	var (
		spawned int64
		records []spawnedRecord
	)
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	for _, c := range due {
		rec, didSpawn, perr := s.processOne(ctx, tx, c, now)
		if perr != nil {
			// An error processing one due cadence (resolve/spawn/advance) rolls
			// back the whole tick: atomicity matters more than partial progress
			// (on the next tick the rows are due again, we retry the whole
			// batch). Return without committing.
			return spawned, fmt.Errorf("conductor.spawn_due_cadence: cadence %s: %w", c.ID, perr)
		}
		if rec != nil {
			records = append(records, *rec)
		}
		if didSpawn {
			spawned++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("conductor.spawn_due_cadence: commit: %w", err)
	}
	committed = true

	// Audit AFTER commit (best-effort): an emit error doesn't undo the
	// already-committed spawn.
	for i := range records {
		s.emit(ctx, &records[i])
	}
	return spawned, nil
}

// processOne handles one due cadence in an open tx: applies overlap_policy,
// and if allowed spawns a Voyage from the recipe + advances the schedule.
// Returns (record for audit | nil, whether a spawn happened, error).
//
// overlap_policy decisions (ADR-046 §5):
//   - parallel — spawn alongside running children; only a deferred one holds
//     it back (below).
//   - skip     — live child exists → do NOT spawn, audit skipped_overlap,
//     next_run_at is still recalculated (the series doesn't stall).
//   - queue    — live child exists → do NOT spawn and do NOT advance
//     next_run_at: the next tick retries as soon as the previous child
//     terminates (simple queue semantics — "wait for completion, the nearest
//     due tick picks it up").
//
// A child deferred to a window or past a freeze is `scheduled`, so it is live
// for skip and queue as it stands. Under parallel it is the one child that
// holds the spawn back, as skip would (ADR-0093 §4): otherwise every tick of the
// wait would add a run, and all of them would start together when it ends.
func (s *CadenceSpawner) processOne(ctx context.Context, tx pgx.Tx, c *cadence.Cadence, now time.Time) (*spawnedRecord, bool, error) {
	scheduledFor := now
	if c.NextRunAt != nil {
		scheduledFor = c.NextRunAt.UTC()
	}

	var (
		held bool
		err  error
	)
	switch {
	case c.OverlapPolicy != cadence.OverlapPolicyParallel:
		held, err = cadence.HasLiveChild(ctx, tx, c.ID)
	case c.Kind == cadence.KindScenario:
		held, err = cadence.HasDeferredChild(ctx, tx, c.ID)
	}
	if err != nil {
		return nil, false, err
	}
	if held {
		if c.OverlapPolicy == cadence.OverlapPolicyQueue {
			// queue: don't advance next_run_at — leave the row due, the next
			// tick retries the check. No audit (this is waiting, not a skip).
			// The due pick leaves such a row out already (NIM-1113); this
			// check, under the row lock, is the one that decides.
			return nil, false, nil
		}
		// skip (and parallel behind a deferred child): spawn skipped, but the
		// schedule advances (the series doesn't stall). Anchored to the planned
		// slot (scheduledFor), not to now — otherwise the skip series would
		// drift the same way as the spawn path (ADR-046 §4).
		nextRun, nerr := cadence.NextRunAnchored(c, scheduledFor, now)
		if nerr != nil {
			return nil, false, nerr
		}
		if aerr := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, nil); aerr != nil {
			return nil, false, aerr
		}
		return &spawnedRecord{
			cadenceID:    c.ID,
			scheduledFor: scheduledFor,
			skipped:      true,
			skipReason:   skipReasonOverlap,
		}, false, nil
	}

	// Spawn allowed: nothing above held it back.
	resolved, err := s.resolveScope(ctx, c)
	if err != nil {
		return nil, false, err
	}
	// Anchored to the planned slot (scheduledFor = next_run_at before
	// recalculation), not to the actual now: tick drift doesn't accumulate in
	// next_run_at, the slot grid stays aligned (ADR-046 §4, drift-free).
	nextRun, nerr := cadence.NextRunAnchored(c, scheduledFor, now)
	if nerr != nil {
		return nil, false, nerr
	}

	if len(resolved) == 0 {
		// Empty resolve: nothing to spawn (no live hosts / incarnations under
		// target). Not a fail — advance the schedule (last_run_at = now: tick
		// handled), no Voyage and no audit-spawned. Symmetric to the
		// voyage_empty_target handler, but in the background this is normal,
		// not an operator error.
		if aerr := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, &now); aerr != nil {
			return nil, false, aerr
		}
		if s.logger != nil {
			s.logger.Info("conductor.spawn_due_cadence: empty target resolve, spawn skipped",
				slog.String("cadence_id", c.ID))
		}
		return nil, false, nil
	}

	// Console gate (ADR-0074 amendment, NIM-197). The spawned Voyage runs on
	// behalf of the recipe's creator (ADR-046 §7), so the creator is who must
	// still hold `soul.console` on the hosts the target just resolved to.
	//
	// A refusal is a RECORDED SKIP, never an error: an error here rolls back the
	// whole tick and stalls every other due schedule. The schedule still advances
	// (parity with an overlap skip) so the series does not wedge, and
	// `cadence.skipped_forbidden` says why — the one thing this path must not do
	// is stop working quietly.
	if !s.authorizeShell(ctx, c, resolved) {
		if aerr := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, nil); aerr != nil {
			return nil, false, aerr
		}
		return &spawnedRecord{
			cadenceID:    c.ID,
			scheduledFor: scheduledFor,
			skipped:      true,
			skipReason:   skipReasonConsoleRequired,
			module:       derefStr(c.Module),
		}, false, nil
	}

	voyageID := cadence.NewVoyageID()
	row, targets := cadence.BuildVoyage(c, voyageID, resolved)
	var startAt time.Time
	if c.Kind == cadence.KindScenario {
		at, nw, serr := spawnStart(ctx, tx, row, resolved, now)
		if serr != nil {
			return nil, false, serr
		}
		if nw != nil {
			// Recorded skip, as the console gate's: a Voyage request here is
			// refused with 422, and a schedule has nobody to answer it.
			if aerr := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, nil); aerr != nil {
				return nil, false, aerr
			}
			return &spawnedRecord{
				cadenceID:    c.ID,
				scheduledFor: scheduledFor,
				skipped:      true,
				skipReason:   skipReasonNoWindow,
				noWindow:     nw,
			}, false, nil
		}
		startAt = at
	}
	if startAt.After(now) {
		// Status set here rather than left to Insert, which compares
		// schedule_at with the wall clock, not with this tick's now.
		at := startAt.UTC()
		row.ScheduleAt, row.Status = &at, voyage.StatusScheduled
	}
	if err := voyage.Insert(ctx, tx, row); err != nil {
		return nil, false, fmt.Errorf("insert voyage: %w", err)
	}
	if err := voyage.InsertTargets(ctx, tx, voyageID, targets); err != nil {
		return nil, false, fmt.Errorf("insert voyage targets: %w", err)
	}
	if err := cadence.AdvanceSchedule(ctx, tx, c.ID, nextRun, &now); err != nil {
		return nil, false, fmt.Errorf("advance schedule: %w", err)
	}

	return &spawnedRecord{
		cadenceID:    c.ID,
		voyageID:     voyageID,
		scheduledFor: scheduledFor,
		scheduleAt:   row.ScheduleAt,
		scopeSize:    len(resolved),
	}, true, nil
}

// authorizeShell applies the console gate to one due Cadence: true = spawn may
// proceed. A recipe that does not name a verb-shell module never reaches the
// enforcer.
//
// The probe requires `soul.console` on EVERY resolved host, matching the
// all-or-nothing rule of the interactive Voyage path — a partial spawn would
// quietly change the recipe the operator wrote.
//
// The identity checked is the recipe's author (`created_by_aid`), so this is an
// operator's grant like any other and reads the same context shape: `host=<sid>`
// plus one context per Coven label of that host, granted if ANY ONE matches
// (NIM-650). A host-only context here would have made a coven-scoped author's
// schedule stop spawning silently the day enforcement turned on — the one
// surface where nobody is watching an HTTP status.
func (s *CadenceSpawner) authorizeShell(ctx context.Context, c *cadence.Cadence, resolved []string) bool {
	module := derefStr(c.Module)
	if !shellgate.Required(module) {
		return true
	}
	var check func() error
	if s.enforcer != nil {
		check = func() error {
			contexts := soul.HostContextsBySIDs(ctx, s.soulReader, resolved)
			for _, sid := range resolved {
				if err := soul.AllowAnyContext(s.enforcer, c.CreatedByAID, "soul", "console", contexts[sid]); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return s.gate.Authorize(shellgate.SurfaceCadenceSpawn, module, check) == nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// resolveScope resolves the Cadence recipe's declarative target into a
// snapshot of units via the scenario/command resolvers (a thin wrapper over
// cadence.ResolveScope for conductor-package access to the unexported
// resolve).
func (s *CadenceSpawner) resolveScope(ctx context.Context, c *cadence.Cadence) ([]string, error) {
	return cadence.ResolveScope(ctx, c, s.scenarioR, s.commandR)
}

// emit writes cadence.spawned or one of the cadence.skipped_* (ADR-046 §8). source
// = background (ADR-048 §4: a new `scheduler` source is NOT introduced —
// `background` is semantically accurate for a periodic background keeper rule
// with no initiating operator; the Reaper→Conductor executor change doesn't
// change the event's nature). archon_aid = NULL: a background spawn has no
// identified initiating operator (SourceBackground semantics). This is NOT
// the Cadence's created_by_aid — recipe authorship lives in
// Voyage.started_by_aid (see cadence.BuildVoyage), not in audit source.
// nil-safe.
func (s *CadenceSpawner) emit(ctx context.Context, rec *spawnedRecord) {
	if s.audit == nil {
		return
	}
	var ev *audit.Event
	switch {
	case rec.skipped && rec.skipReason == skipReasonConsoleRequired:
		// A distinct event type, not `skipped_overlap` with another reason: an
		// overlap skip is normal scheduling, this one is a schedule that has
		// stopped running and needs an operator. The module is named because it
		// is the actionable half — the fix is a `soul.console` grant on it.
		ev = &audit.Event{
			EventType:     audit.EventCadenceSkippedForbidden,
			Source:        audit.SourceBackground,
			CorrelationID: rec.cadenceID,
			Payload: map[string]any{
				"cadence_id":    rec.cadenceID,
				"scheduled_for": rec.scheduledFor,
				"reason":        skipReasonConsoleRequired,
				"module":        rec.module,
			},
		}
	case rec.skipped && rec.skipReason == skipReasonNoWindow:
		ev = &audit.Event{
			EventType:     audit.EventCadenceSkippedNoWindow,
			Source:        audit.SourceBackground,
			CorrelationID: rec.cadenceID,
			Payload: map[string]any{
				"cadence_id":        rec.cadenceID,
				"scheduled_for":     rec.scheduledFor,
				"reason":            skipReasonNoWindow,
				"incarnation_count": rec.noWindow.incarnations,
				"next_starts":       rec.noWindow.rows,
			},
		}
	case rec.skipped:
		ev = &audit.Event{
			EventType:     audit.EventCadenceSkippedOverlap,
			Source:        audit.SourceBackground,
			CorrelationID: rec.cadenceID,
			Payload: map[string]any{
				"cadence_id":    rec.cadenceID,
				"scheduled_for": rec.scheduledFor,
				"reason":        skipReasonOverlap,
			},
		}
	default:
		ev = &audit.Event{
			EventType:     audit.EventCadenceSpawned,
			Source:        audit.SourceBackground,
			CorrelationID: rec.voyageID,
			Payload: map[string]any{
				"cadence_id":    rec.cadenceID,
				"voyage_id":     rec.voyageID,
				"scheduled_for": rec.scheduledFor,
				"scope_size":    rec.scopeSize,
			},
		}
		if rec.scheduleAt != nil {
			ev.Payload["schedule_at"] = *rec.scheduleAt
		}
	}
	if err := s.audit.Write(ctx, ev); err != nil && s.logger != nil {
		s.logger.Warn("conductor.spawn_due_cadence: audit write failed",
			slog.String("cadence_id", rec.cadenceID),
			slog.String("event", string(ev.EventType)),
			slog.Any("error", err))
	}
}
