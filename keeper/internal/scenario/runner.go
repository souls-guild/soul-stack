package scenario

import (
	"context"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
)

// Start регистрирует прогон и спавнит run-goroutine, возвращаясь сразу (async).
//
// runCtx отсоединён от cancel/deadline request-ctx (context.WithoutCancel):
// прогон переживает возврат HTTP-ответа 202, НО наследует values родителя —
// прежде всего OTel SpanContext/baggage, чтобы span `scenario.run` сшивался
// с request-span-ом (ADR-024). Отмена возможна через [Runner.Cancel] (по
// applyID) или [Runner.Shutdown] (все активные). Per-scenario timeout
// (runTimeout) навешивается на runCtx.
//
// Возврат:
//   - [ErrShuttingDown] — Runner останавливается.
//   - [ErrAlreadyRunning] — applyID уже активен (дубль-вызов).
//   - валидационная ошибка на пустые поля spec.
func (r *Runner) Start(parent context.Context, spec RunSpec) error {
	if spec.ApplyID == "" {
		return fmt.Errorf("scenario: empty apply_id")
	}
	if spec.IncarnationName == "" {
		return fmt.Errorf("scenario: empty incarnation_name")
	}
	if spec.ScenarioName == "" {
		return fmt.Errorf("scenario: empty scenario_name")
	}

	r.mu.Lock()
	if r.shuttingDown {
		r.mu.Unlock()
		return ErrShuttingDown
	}
	if _, exists := r.active[spec.ApplyID]; exists {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	// runCtx живёт независимо от request-ctx — прогон не должен умирать с
	// возвратом 202. WithoutCancel: сохраняем trace-baggage (SpanContext
	// родителя для сшивки трассы), не наследуем cancel/deadline request-а.
	// Timeout — защита от вечного barrier.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), r.runTimeout)
	r.active[spec.ApplyID] = cancel
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer cancel()
		defer r.unregister(spec.ApplyID)
		r.run(runCtx, spec)
	}()

	return nil
}

// StartDestroy инициирует teardown incarnation: прогон scenario `destroy`
// против её хостов в режиме [TerminalDestroy] (S-D2b). Тонкая обёртка над
// [Runner.Start] — фиксирует ScenarioName=`destroy` и TerminalMode=Destroy,
// остальное (async, lockRun-gate, dispatch, barrier) идёт общим путём run().
//
// Семантика финала (run.go): success teardown → incarnation остаётся в
// `destroying` (state не трогается, ready НЕ коммитится; DELETE строки — S-D3);
// провал teardown → `destroy_failed` (НЕ error_locked). Стартует только из
// `destroying` (lockRun отвергает иной статус как ErrNotRunnable) — destroy уже
// инициирован S-D1 и pre-check наличия scenario `destroy` (PrepareDestroy)
// прошёл ДО перевода в destroying.
//
// Caller (handler S-D4) делает Destroy() → StartDestroy(), передавая тот же
// applyID, что в state_history-snapshot инициации. ScenarioName/Input в spec
// игнорируются — StartDestroy выставляет их сам.
//
// Возврат — те же ошибки, что [Runner.Start] (ErrShuttingDown /
// ErrAlreadyRunning / валидация пустых полей spec).
func (r *Runner) StartDestroy(parent context.Context, spec RunSpec) error {
	spec.ScenarioName = DestroyScenarioName
	spec.TerminalMode = TerminalDestroy
	return r.Start(parent, spec)
}

// Cancel отменяет активный прогон по applyID НА ЭТОМ инстансе. Возвращает
// true, если прогон был активен локально (cancel вызван), false — если такого
// applyID нет в active-map (завершён, не запускался, либо run-goroutine живёт
// на ДРУГОМ Keeper-инстансе — для cross-Keeper используйте [RequestCancel]).
//
// Cancel лишь отменяет runCtx: dispatch-цикл прерывается, уже отправленным
// Soul-ам Keeper НЕ шлёт CancelApply в pilot-е (best-effort cancel — слой
// admin-API через Outbound.SendCancel, отдельно). Incarnation остаётся в
// том статусе, в котором run.go зафиксирует прерванный прогон (error_locked).
func (r *Runner) Cancel(applyID string) bool {
	r.mu.Lock()
	cancel, ok := r.active[applyID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// RequestCancel — cluster-wide отмена прогона (G1): работает независимо от
// того, на каком Keeper-инстансе живёт run-goroutine (ADR-002, stateless-
// кластер). Точка входа для admin-API (HTTP/MCP Cancel).
//
// Два пути, оба идемпотентны:
//   - PG-флаг (cross-Keeper): [applyrun.RequestCancel] ставит cancel_requested
//     на все ещё-running строки прогона. Инстанс-владелец goroutine увидит флаг
//     в barrier-поллинге (waitBarrier) на ближайшем тике и отменит прогон тем
//     же путём, что локальный Cancel (abort → error_locked). Терминальный
//     прогон не трогается (фильтр status='running') — Cancel завершённого =
//     no-op.
//   - локальный Cancel (быстрый путь): если goroutine здесь — [Runner.Cancel]
//     отменяет runCtx немедленно, не дожидаясь барьерного тика. Если на другом
//     инстансе — false, отмену довезёт флаг.
//
// Возврат:
//   - found — прогон был активен (затронута хотя бы одна running-строка в PG
//     ИЛИ локальный Cancel сработал). false → прогона нет либо он уже
//     терминален (caller трактует как no-op / 404).
//   - ошибка — только сбой PG-апдейта флага (локальный Cancel ошибок не даёт).
func (r *Runner) RequestCancel(ctx context.Context, applyID string) (found bool, err error) {
	if applyID == "" {
		return false, fmt.Errorf("scenario: empty apply_id")
	}
	// Сперва PG-флаг — cluster-wide путь авторитетен (переживает cross-Keeper).
	affected, perr := applyrun.RequestCancel(ctx, r.deps.DB, applyID)
	if perr != nil {
		return false, fmt.Errorf("scenario: request cancel: %w", perr)
	}
	// Локальный быстрый путь: если goroutine на этом инстансе — отменяем сразу,
	// не дожидаясь, пока барьер вычитает только что выставленный флаг.
	local := r.Cancel(applyID)
	return affected > 0 || local, nil
}

// Shutdown останавливает приём новых прогонов и ждёт завершения активных
// (graceful). Если ctx отменится раньше — отменяет все активные runCtx
// (force) и ждёт их выхода без ограничения по времени (run-goroutine-ы
// реагируют на cancel быстро — dispatch-цикл проверяет ctx).
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.shuttingDown = true
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		r.cancelAll()
		// Дожидаемся фактического выхода goroutine-ов: после cancelAll
		// dispatch-цикл прерывается на ближайшей ctx-проверке.
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			r.logger.Warn("scenario: run goroutines did not exit within 15s after shutdown — leak suspected")
		}
		return ctx.Err()
	}
}

// unregister удаляет applyID из active-map (вызывается из run-goroutine при
// завершении).
func (r *Runner) unregister(applyID string) {
	r.mu.Lock()
	delete(r.active, applyID)
	r.mu.Unlock()
}

// cancelAll отменяет runCtx всех активных прогонов (force-shutdown).
func (r *Runner) cancelAll() {
	r.mu.Lock()
	for _, cancel := range r.active {
		cancel()
	}
	r.mu.Unlock()
}
