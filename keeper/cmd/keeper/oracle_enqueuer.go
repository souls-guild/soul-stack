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

// oracleEnqueuerDB — узкая поверхность PG для Oracle-enqueue-а: чтение
// incarnation (SelectByName) + запись planned-задания (InsertPlanned). Обе
// функции тянут идентичный [incarnation.ExecQueryRower] / [applyrun.ExecQueryRower];
// *pgxpool.Pool удовлетворяет обоим. Интерфейс (а не прямой pool) держит
// enqueuer unit-тестируемым без подъёма PG.
type oracleEnqueuerDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: enqueuerDB совпадает с обоими CRUD-интерфейсами.
var (
	_ incarnation.ExecQueryRower = (oracleEnqueuerDB)(nil)
	_ applyrun.ExecQueryRower    = (oracleEnqueuerDB)(nil)
)

// oracleScenarioEnqueuer — реализация [keepergrpc.ScenarioEnqueuer] (ADR-030 S2,
// РЕШЕНИЕ #1 вариант b). Превращает сматчившую Oracle-реакцию в planned-задание
// под Acolyte-claim тем же путём, что планировщик-путь scenario.dispatchPlanned
// (ADR-027): InsertPlanned(Recipe) + Summons-сигнал.
//
// ServiceRef резолвится ИЗ таргет-incarnation (точная калька
// incarnation/destroy_prepare.go:88): SelectByName(incarnation_name) →
// resolver.Resolve(inc.Service) с override ref = inc.ServiceVersion (сценарий
// катится РАЗВЁРНУТОЙ версией сервиса, а не tip-ом ветки). Incarnation не
// найдена → fail-closed: возвращается [ErrEnqueueIncarnationNotFound] (handler
// логирует warn и НЕ пишет fire/audit — реакция гасится).
type oracleScenarioEnqueuer struct {
	db       oracleEnqueuerDB
	resolver incarnation.ServiceResolver
	summons  summonsPublisher // best-effort Summons-сигнал planned-заданий
	logger   *slog.Logger
}

// ErrEnqueueIncarnationNotFound — таргет-incarnation Decree-а отсутствует в
// реестре (пересоздана/удалена; FK на incarnation у decrees НЕТ осознанно,
// существование проверяется здесь fail-closed). handler маппит в skip + warn,
// fire/audit НЕ пишутся.
var ErrEnqueueIncarnationNotFound = errors.New("oracle enqueue: target incarnation not found")

// EnqueueScenario ставит named-scenario из Oracle-реакции в work-queue на хост
// subjectSID (ADR-027 planned-путь). Шаги:
//  1. SelectByName(IncarnationName) — fail-closed на отсутствие.
//  2. Resolve(inc.Service) → git-координаты сервиса; ref override на
//     inc.ServiceVersion (калька destroy_prepare.go:88).
//  3. Recipe{ServiceRef, ScenarioName, Input (vault-ref КАК ЕСТЬ, инвариант A
//     ADR-027), StartedByAID: nil (Soul-инициированная реакция без identity
//     Архонта)} → InsertPlanned(ApplyRun{ApplyID(новый ULID), SID, ...}).
//  4. best-effort PublishSummons (poll-fallback Acolyte подхватит при потере).
//
// Возвращает apply_id поставленного прогона (audit-correlation oracle.fired).
func (e *oracleScenarioEnqueuer) EnqueueScenario(ctx context.Context, req keepergrpc.EnqueueScenarioRequest) (string, error) {
	inc, err := incarnation.SelectByName(ctx, e.db, req.IncarnationName)
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			e.logger.Warn("oracle enqueue: таргет-incarnation не найдена — skip (fail-closed)",
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
	// Сценарий катится развёрнутой версией сервиса, а не tip-ом ветки
	// (калька incarnation/destroy_prepare.go:88).
	ref.Ref = inc.ServiceVersion

	applyID := audit.NewULID()
	recipe := &applyrun.Recipe{
		ServiceRef:   ref,
		ScenarioName: req.ScenarioName,
		Input:        req.ActionInput, // vault-ref КАК ЕСТЬ — инвариант A ADR-027
		StartedByAID: nil,             // Soul-инициированная реакция без identity Архонта
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

	// Summons — best-effort: persisted planned-задание подхватит poll-fallback
	// Acolyte даже при потере сигнала (ADR-027(a)). Ошибку только логируем.
	if e.summons.redis != nil {
		if err := e.summons.PublishSummons(ctx); err != nil {
			e.logger.Warn("oracle enqueue: publish Summons провален — poll-fallback подхватит",
				slog.String("apply_id", applyID), slog.Any("error", err))
		}
	}

	e.logger.Info("oracle enqueue: planned-задание записано",
		slog.String("sid", req.SubjectSID),
		slog.String("incarnation", inc.Name),
		slog.String("scenario", req.ScenarioName),
		slog.String("apply_id", applyID),
	)
	return applyID, nil
}
