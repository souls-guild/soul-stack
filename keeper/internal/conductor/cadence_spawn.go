package conductor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
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
	logger    *slog.Logger
}

// NewCadenceSpawner constructs a spawner. resolvers are required (production
// wire-up uses handlers' PG resolvers via an adapter); audit is nil-safe;
// logger is nil-safe.
func NewCadenceSpawner(
	pool *pgxpool.Pool,
	scenarioR cadence.ScenarioResolver,
	commandR cadence.CommandResolver,
	auditW audit.Writer,
	logger *slog.Logger,
) *CadenceSpawner {
	return &CadenceSpawner{
		pool:      pool,
		scenarioR: scenarioR,
		commandR:  commandR,
		audit:     auditW,
		logger:    logger,
	}
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
	scopeSize    int
	skipped      bool // true → skipped_overlap, false → spawned
}

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
//   - parallel — always spawn (no live check needed).
//   - skip     — live child exists → do NOT spawn, audit skipped_overlap,
//     next_run_at is still recalculated (the series doesn't stall).
//   - queue    — live child exists → do NOT spawn and do NOT advance
//     next_run_at: the next tick retries as soon as the previous child
//     terminates (simple queue semantics — "wait for completion, the nearest
//     due tick picks it up").
func (s *CadenceSpawner) processOne(ctx context.Context, tx pgx.Tx, c *cadence.Cadence, now time.Time) (*spawnedRecord, bool, error) {
	scheduledFor := now
	if c.NextRunAt != nil {
		scheduledFor = c.NextRunAt.UTC()
	}

	if c.OverlapPolicy != cadence.OverlapPolicyParallel {
		live, err := cadence.HasLiveChild(ctx, tx, c.ID)
		if err != nil {
			return nil, false, err
		}
		if live {
			switch c.OverlapPolicy {
			case cadence.OverlapPolicyQueue:
				// queue: don't advance next_run_at — leave the row due, the next
				// tick retries the check. No audit (this is waiting, not a skip).
				return nil, false, nil
			case cadence.OverlapPolicySkip:
				// skip: spawn skipped, but the schedule advances (the series
				// doesn't stall). Anchored to the planned slot (scheduledFor),
				// not to now — otherwise the skip series would drift the same
				// way as the spawn path (ADR-046 §4).
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
				}, false, nil
			}
		}
	}

	// Spawn allowed (parallel, or skip/queue with no live child).
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

	voyageID := cadence.NewVoyageID()
	row, targets := cadence.BuildVoyage(c, voyageID, resolved)
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
		scopeSize:    len(resolved),
	}, true, nil
}

// resolveScope resolves the Cadence recipe's declarative target into a
// snapshot of units via the scenario/command resolvers (a thin wrapper over
// cadence.ResolveScope for conductor-package access to the unexported
// resolve).
func (s *CadenceSpawner) resolveScope(ctx context.Context, c *cadence.Cadence) ([]string, error) {
	return cadence.ResolveScope(ctx, c, s.scenarioR, s.commandR)
}

// emit writes cadence.spawned or cadence.skipped_overlap (ADR-046 §8). source
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
	if rec.skipped {
		ev = &audit.Event{
			EventType:     audit.EventCadenceSkippedOverlap,
			Source:        audit.SourceBackground,
			CorrelationID: rec.cadenceID,
			Payload: map[string]any{
				"cadence_id":    rec.cadenceID,
				"scheduled_for": rec.scheduledFor,
				"reason":        "overlap",
			},
		}
	} else {
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
	}
	if err := s.audit.Write(ctx, ev); err != nil && s.logger != nil {
		s.logger.Warn("conductor.spawn_due_cadence: audit write failed",
			slog.String("cadence_id", rec.cadenceID),
			slog.String("event", string(ev.EventType)),
			slog.Any("error", err))
	}
}
