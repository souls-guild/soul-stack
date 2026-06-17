package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// ErrIncarnationNotDestroyable — destroy отклонён: текущий статус incarnation
// не входит в множество разрешённых для инициации destroy (409). Handler-сторона
// маппит в incarnation-locked problem-type.
var ErrIncarnationNotDestroyable = errors.New("incarnation: status does not allow destroy")

// destroyScenarioLabel — значение `state_history.scenario` для перехода
// инициации destroy. Сам destroy (teardown) — это будущий прогон scenario
// `destroy` (S-D2), но S-D1 фиксирует только сам факт перевода в `destroying`
// под этой меткой (state_history требует non-null scenario), симметрично
// unlock / migration.
const destroyScenarioLabel = "destroy"

// DestroyResult — итог инициации destroy: статус до перевода (для reply / audit)
// и идентификатор записанного state_history-snapshot-а.
type DestroyResult struct {
	PreviousStatus Status
	HistoryID      string
}

// canDestroyFrom — множество статусов, из которых разрешено инициировать
// destroy. ready — штатный путь; error_locked / migration_failed — снос
// «застрявшего» инстанса без обязательного unlock (оператор сознательно
// сносит, а не чинит). applying отвергается: идёт прогон, FOR UPDATE+статус
// сериализуют гонку с scenario-runner-ом. destroying отвергается:
// повторная инициация (идемпотентность — задача S-D3, здесь — явный отказ).
//
// drift (ADR-031, Scry, информационный статус) — допустим: drift НЕ блокирует
// remediation (как ready). Оператор может снести incarnation в drift точно так
// же, как из ready, не дожидаясь fix-апплая.
func canDestroyFrom(s Status) bool {
	switch s {
	case StatusReady, StatusErrorLocked, StatusMigrationFailed, StatusDrift:
		return true
	}
	return false
}

// Destroy инициирует destroy incarnation: переводит строку в `destroying`
// (S-D1). Teardown (scenario `destroy`, S-D2) и DELETE строки (S-D3) — НЕ в
// этом слайсе.
//
// Атомарность — тот же транзакционный паттерн, что [Unlock]: одна tx
// SELECT … FOR UPDATE → guard статуса → INSERT zero-diff state_history →
// UPDATE status=destroying. FOR UPDATE сериализует destroy относительно
// конкурентного scenario-runner-а (lockRun лочит ту же строку), закрывая
// TOCTOU между probe статуса и переводом.
//
// guard переходов ([canDestroyFrom]):
//   - ready / error_locked / migration_failed → destroy разрешён;
//   - applying → [ErrIncarnationNotDestroyable] (идёт прогон);
//   - destroying → [ErrIncarnationNotDestroyable] (destroy уже инициирован).
//
// force — намерение «destroy без teardown» (force=true → S-D3 удалит строку
// напрямую, без прогона scenario `destroy`). В S-D1 поведение teardown НЕ
// реализуется: force только сохраняется в `status_details.force`, чтобы S-D3
// прочитал намерение из уже залоченной строки.
//
// state НЕ меняется (destroy не правит state-граф; teardown работает с хостами,
// не с jsonb). Пишется zero-diff state_history-snapshot — фиксируем сам факт
// инициации, симметрично unlock.
//
// audit-event `incarnation.destroy_started` пишется ПОСЛЕ commit-а (как
// UpdateStateFromRun: DB-консистентность не должна зависеть от audit-write).
// Фейл audit-write логируется, но НЕ откатывает destroy — переход уже
// зафиксирован, терять audit-trail молча нельзя, но и блокировать destroy из-за
// него нельзя. w == nil → trail не пишется (unit/L0). source / archonAID —
// инициатор (api / mcp), пробрасываются caller-ом.
//
// Возврат:
//   - [ErrIncarnationNotFound] — name не существует (404).
//   - [ErrIncarnationNotDestroyable] — статус не разрешает destroy (409).
func Destroy(
	ctx context.Context,
	pool TxBeginner,
	w audit.Writer,
	name string,
	force bool,
	source audit.Source,
	archonAID, historyID string,
	logger *slog.Logger,
) (*DestroyResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}
	if historyID == "" {
		return nil, fmt.Errorf("incarnation: empty history_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin destroy tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, name).Scan(&stateBytes, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: destroy select: %w", err)
	}
	previous := Status(statusStr)
	if !canDestroyFrom(previous) {
		return nil, fmt.Errorf("%w: %s", ErrIncarnationNotDestroyable, previous)
	}

	var changedByArg any
	if archonAID != "" {
		changedByArg = archonAID
	}

	// state_before == state_after: destroy-инициация не меняет state.
	// apply_id = history_id ($1): инициация не привязана к apply-прогону (схема
	// требует NOT NULL, FK на apply_runs нет) — подставляем history_id как
	// уникальный non-null маркер, симметрично unlock.
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $1)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, destroyScenarioLabel, stateBytes, changedByArg,
	); err != nil {
		return nil, fmt.Errorf("incarnation: insert destroy state_history: %w", err)
	}

	// status_details.force — намерение для S-D3: force=true → DELETE без
	// teardown. Без маскинга: force — bool, секретов не несёт.
	detailsBytes, err := json.Marshal(map[string]any{"force": force})
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal destroy status_details: %w", err)
	}

	const updateSQL = `
UPDATE incarnation
SET status = $2, status_details = $3, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(ctx, updateSQL, name, string(StatusDestroying), detailsBytes); err != nil {
		return nil, fmt.Errorf("incarnation: destroy update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit destroy tx: %w", err)
	}

	// TODO(S-D4/S-D3): запуск teardown + DELETE row. Исполнение teardown уже
	// реализовано — scenario.Runner.StartDestroy прогоняет scenario `destroy`
	// против хостов incarnation в режиме TerminalDestroy (S-D2b): success
	// оставляет `destroying` (DELETE — S-D3), провал → destroy_failed. Здесь
	// (service-слой Destroy) — только перевод в `destroying`; вызов StartDestroy
	// из handler-а после commit-а этой транзакции — S-D4 (force=true → S-D3
	// удалит строку напрямую, без teardown).

	writeDestroyAudit(ctx, w, source, archonAID, name, previous, force, logger)

	return &DestroyResult{PreviousStatus: previous, HistoryID: historyID}, nil
}

// DeleteResult — итог [DeleteAfterTeardown]. Deleted=false означает no-op:
// строки в статусе `destroying` не оказалось (кто-то уже снёс её / сменил
// статус — повторный вызов после успешного DELETE идемпотентен).
type DeleteResult struct {
	Deleted bool
}

// DeleteAfterTeardown физически сносит incarnation после успешного teardown-а
// (S-D3, каскад V3). Одна PG-транзакция, single-winner:
//
//  1. INSERT INTO incarnation_archive SELECT … FROM incarnation
//     WHERE name=$1 AND status='destroying' — снимок compliance-минимума ДО
//     удаления.
//  2. INSERT INTO state_history_archive SELECT … FROM state_history
//     WHERE incarnation=$1 — снимок журнала переходов ДО каскада.
//  3. DELETE FROM incarnation WHERE name=$1 AND status='destroying' — снос
//     строки. WHERE status='destroying' — single-winner guard: выигрывает
//     ровно один обработчик, владеющий destroying-переходом. RowsAffected==0
//     (строки нет / статус сменился / кто-то уже удалил) → транзакция
//     откатывается, [DeleteResult.Deleted]=false, no-op идемпотентно.
//
// Каскад (ON DELETE CASCADE на live state_history / apply_runs /
// apply_task_register) срабатывает при DELETE; архив записан ДО него, поэтому
// compliance-данные сохраняются. Архив пишется внутри ТОЙ ЖЕ tx, что и DELETE:
// либо архив+DELETE атомарно вместе, либо ничего (rollback).
//
// Порядок INSERT-ов: incarnation_archive до state_history_archive, оба до
// DELETE — селекты читают ещё живые строки.
//
// audit `incarnation.destroy_completed` пишется ПОСЛЕ commit-а (паттерн
// [Destroy]: DB-консистентность не зависит от audit-write). Пишется ТОЛЬКО при
// фактическом удалении (Deleted=true): no-op события не порождает. force идёт
// в payload (намерение destroy без teardown). w == nil → trail не пишется.
//
// Возврат [ErrIncarnationNotFound] НЕ используется: отсутствие destroying-строки
// — это легитимный no-op (Deleted=false), а не ошибка (идемпотентность S-D3).
func DeleteAfterTeardown(
	ctx context.Context,
	pool TxBeginner,
	w audit.Writer,
	name string,
	force bool,
	logger *slog.Logger,
) (*DeleteResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (a) Архив строки incarnation — только если она в destroying (тот же guard,
	// что у DELETE: если статус уже сменился, не архивируем неподходящую строку).
	const archiveIncarnationSQL = `
INSERT INTO incarnation_archive (
    name, service, service_version, state_schema_version,
    spec, state, status, status_details, created_by_aid,
    created_at, updated_at
)
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at
FROM incarnation
WHERE name = $1 AND status = 'destroying'
`
	if _, err := tx.Exec(ctx, archiveIncarnationSQL, name); err != nil {
		return nil, fmt.Errorf("incarnation: archive incarnation: %w", err)
	}

	// (b) Архив журнала state_history (вся история удаляемой incarnation, ДО
	// каскада). Не привязан к status — журнал архивируется целиком; если строки
	// incarnation в destroying нет, DELETE ниже даст RowsAffected==0 и tx
	// откатится, отменив и этот INSERT.
	const archiveHistorySQL = `
INSERT INTO state_history_archive (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id, at
)
SELECT history_id, incarnation_name, scenario, state_before, state_after,
       changed_by_aid, apply_id, at
FROM state_history
WHERE incarnation_name = $1
`
	if _, err := tx.Exec(ctx, archiveHistorySQL, name); err != nil {
		return nil, fmt.Errorf("incarnation: archive state_history: %w", err)
	}

	// (c) Single-winner DELETE. status='destroying' гарантирует, что снос
	// выполняет только владелец destroying-перехода. RowsAffected==0 → no-op.
	const deleteSQL = `
DELETE FROM incarnation
WHERE name = $1 AND status = 'destroying'
`
	tag, err := tx.Exec(ctx, deleteSQL, name)
	if err != nil {
		return nil, fmt.Errorf("incarnation: delete incarnation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Никто не выиграл строку: статус сменился / строка уже удалена. Откат
		// (defer Rollback) отменяет и записанный выше архив — он не нужен без DELETE.
		return &DeleteResult{Deleted: false}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit delete tx: %w", err)
	}

	writeDestroyCompletedAudit(ctx, w, name, force, logger)

	return &DeleteResult{Deleted: true}, nil
}

// writeDestroyCompletedAudit пишет audit-event `incarnation.destroy_completed`
// после физического сноса строки. source=keeper_internal (write-path —
// scenario-runner после барьера, archon_aid колонка NULL). Фейл write-а не
// валит destroy (строка уже снесена, tx закоммичена), только логируется.
// Секретов в payload нет (name + force).
func writeDestroyCompletedAudit(
	ctx context.Context,
	w audit.Writer,
	name string,
	force bool,
	logger *slog.Logger,
) {
	if w == nil {
		return
	}
	ev := &audit.Event{
		EventType: audit.EventIncarnationDestroyCompleted,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"name":  name,
			"force": force,
		},
	}
	if err := w.Write(ctx, ev); err != nil && logger != nil {
		logger.Warn("incarnation: запись audit incarnation.destroy_completed провалена",
			slog.String("name", name), slog.Any("error", err))
	}
}

// writeDestroyAudit пишет audit-event инициации destroy. Вынесено, чтобы
// логика перехода не смешивалась с best-effort audit-write-ом. Фейл write-а не
// валит destroy (переход уже закоммичен), только логируется.
func writeDestroyAudit(
	ctx context.Context,
	w audit.Writer,
	source audit.Source,
	archonAID, name string,
	previous Status,
	force bool,
	logger *slog.Logger,
) {
	if w == nil {
		return
	}
	ev := &audit.Event{
		EventType: audit.EventIncarnationDestroyStarted,
		Source:    source,
		ArchonAID: archonAID,
		Payload: map[string]any{
			"name":            name,
			"previous_status": string(previous),
			"force":           force,
		},
	}
	if err := w.Write(ctx, ev); err != nil && logger != nil {
		logger.Warn("incarnation: запись audit incarnation.destroy_started провалена",
			slog.String("name", name), slog.Any("error", err))
	}
}
