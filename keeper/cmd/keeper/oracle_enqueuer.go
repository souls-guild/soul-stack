package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// oracleEnqueuerDB -- narrow PG surface for Oracle enqueueing: reading the
// incarnation (SelectByName) + writing a planned job (InsertPlanned). Both
// functions pull the identical [incarnation.ExecQueryRower] / [applyrun.ExecQueryRower];
// *pgxpool.Pool satisfies both. The interface (rather than the raw pool)
// keeps the enqueuer unit-testable without standing up PG.
type oracleEnqueuerDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: enqueuerDB satisfies both CRUD interfaces.
var (
	_ incarnation.ExecQueryRower = (oracleEnqueuerDB)(nil)
	_ applyrun.ExecQueryRower    = (oracleEnqueuerDB)(nil)
)

// oracleScenarioEnqueuer -- implementation of [keepergrpc.ScenarioEnqueuer]
// (ADR-030 S2, DECISION #1 variant b). Turns a matched Oracle reaction into
// a planned job for Acolyte-claim, following the same path as the
// scheduler path scenario.dispatchPlanned (ADR-027): InsertPlanned(Recipe) +
// Summons signal.
//
// ServiceRef is resolved FROM the target incarnation (an exact copy of
// incarnation/destroy_prepare.go:88): SelectByName(incarnation_name) →
// resolver.Resolve(inc.Service) with an override ref = inc.ServiceVersion
// (the scenario runs the DEPLOYED version of the service, not the branch
// tip). Incarnation not found → fail-closed: returns
// [ErrEnqueueIncarnationNotFound] (the handler logs warn and does NOT write
// fire/audit -- the reaction is suppressed).
type oracleScenarioEnqueuer struct {
	db       oracleEnqueuerDB
	resolver incarnation.ServiceResolver
	summons  summonsPublisher // best-effort Summons signal for planned jobs
	logger   *slog.Logger
}

// ErrEnqueueIncarnationNotFound -- the Decree's target incarnation is
// missing from the registry (recreated/deleted; there is deliberately NO FK
// from decrees to incarnation, existence is checked here fail-closed). The
// handler maps it to skip + warn, fire/audit are NOT written.
var ErrEnqueueIncarnationNotFound = errors.New("oracle enqueue: target incarnation not found")

// EnqueueScenario puts a named scenario from an Oracle reaction into the
// work-queue for host subjectSID (ADR-027 planned path). Steps:
//  1. SelectByName(IncarnationName) -- fail-closed if absent.
//  2. Resolve(inc.Service) → git coordinates of the service; ref override
//     to inc.ServiceVersion (copy of destroy_prepare.go:88).
//  3. Recipe{ServiceRef, ScenarioName, Input (vault-ref AS-IS, invariant A
//     ADR-027), StartedByAID: nil (Soul-initiated reaction with no Archon
//     identity)} → InsertPlanned(ApplyRun{ApplyID(new ULID), SID, ...}).
//  4. best-effort PublishSummons (poll-fallback Acolyte will pick it up if
//     lost).
//
// Returns the apply_id of the enqueued run (audit-correlation oracle.fired).
func (e *oracleScenarioEnqueuer) EnqueueScenario(ctx context.Context, req keepergrpc.EnqueueScenarioRequest) (string, error) {
	inc, err := incarnation.SelectByName(ctx, e.db, req.IncarnationName)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			e.logger.Warn("oracle enqueue: target incarnation not found -- skip (fail-closed)",
				slog.String("sid", req.SubjectSID),
				slog.String("decree", req.DecreeName),
				slog.String("incarnation", req.IncarnationName),
			)
			return "", fmt.Errorf("%w: %s", ErrEnqueueIncarnationNotFound, req.IncarnationName)
		}
		return "", fmt.Errorf("oracle enqueue: select incarnation %q: %w", req.IncarnationName, err)
	}

	ref, ok := e.resolver.Resolve(inc.Service)
	if !ok {
		return "", fmt.Errorf("oracle enqueue: service %q of incarnation %q not registered",
			inc.Service, inc.Name)
	}
	// The scenario runs the deployed version of the service, not the
	// branch tip (copy of incarnation/destroy_prepare.go:88).
	ref.Ref = inc.ServiceVersion

	applyID := audit.NewULID()
	recipe := &applyrun.Recipe{
		ServiceRef:   ref,
		ScenarioName: req.ScenarioName,
		Input:        req.ActionInput, // vault-ref AS-IS -- invariant A ADR-027
		StartedByAID: nil,             // Soul-initiated reaction, no Archon identity
	}
	if err := applyrun.InsertPlanned(ctx, e.db, &applyrun.ApplyRun{
		ApplyID:         applyID,
		SID:             req.SubjectSID,
		IncarnationName: inc.Name,
		Scenario:        req.ScenarioName,
		StartedByAID:    nil,
		Recipe:          recipe,
	}); err != nil {
		return "", fmt.Errorf("oracle enqueue: insert planned apply_run (%s/%s): %w",
			req.SubjectSID, applyID, err)
	}

	// Summons -- best-effort: the persisted planned job will be picked up by
	// the poll-fallback Acolyte even if the signal is lost (ADR-027(a)).
	// Only log the error.
	if e.summons.redis != nil {
		if err := e.summons.PublishSummons(ctx); err != nil {
			e.logger.Warn("oracle enqueue: publish Summons failed -- poll-fallback will pick it up",
				slog.String("apply_id", applyID), slog.Any("error", err))
		}
	}

	e.logger.Info("oracle enqueue: planned job recorded",
		slog.String("sid", req.SubjectSID),
		slog.String("incarnation", inc.Name),
		slog.String("scenario", req.ScenarioName),
		slog.String("apply_id", applyID),
	)
	return applyID, nil
}
