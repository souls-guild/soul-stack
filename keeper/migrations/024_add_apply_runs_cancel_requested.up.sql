-- 024_add_apply_runs_cancel_requested.up.sql
--
-- Cluster-wide Cancel прогона (HA-фикс G1). Проблема: run-goroutine прогона
-- живёт в памяти ОДНОГО Keeper-инстанса (ADR-002, stateless-кластер), а
-- Runner.Cancel отменяет только локальный runCtx. Если оператор вызвал Cancel
-- на Keeper-B, а run-goroutine на Keeper-A — отмена не доходит.
--
-- Механизм — PG-флаг (multi-instance-safe), ложится на существующий
-- barrier-поллинг: run-goroutine уже поллит apply_runs в waitBarrier
-- (SelectStatusesByApplyID, dispatch.go), теперь дополнительно читает
-- cancel_requested. Любой Keeper при Cancel ставит флаг
-- (UPDATE ... SET cancel_requested=true), инстанс-владелец goroutine видит его
-- на ближайшем тике барьера и отменяет себя — через тот же abort-путь, что и
-- локальный Cancel. Не нужен новый Redis-канал; переживает cross-Keeper;
-- консистентно с PG-fan-in модели прогона.
--
-- Флаг ставится только на running-строки (терминальные не трогаются → Cancel
-- уже завершённого прогона = no-op). Период поллинга (defaultPollInterval =
-- 200ms) = верхняя граница задержки отмены — приемлемо.
--
-- NOT NULL DEFAULT false: существующие строки получают false, новые dispatch-ы
-- стартуют без запрошенной отмены. Forward-only (ADR-007 миграции).

ALTER TABLE apply_runs
    ADD COLUMN cancel_requested BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN apply_runs.cancel_requested IS
    'Cluster-wide Cancel (G1): любой Keeper ставит true; run-goroutine-владелец видит флаг в barrier-поллинге и отменяет прогон. Только running-строки.';
