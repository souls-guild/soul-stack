-- 050_add_incarnation_drift_scan.up.sql
--
-- ADR-031 Slice C (Scry background): добавляем два поля в реестр incarnation
-- для трекинга фоновых Scry-сканов, выполняемых Reaper-правилом
-- `scry_background` (default OFF, opt-in).
--
-- * `last_drift_check_at` — момент завершения последнего dry_run-прогона
--   converge для этой incarnation (фон или on-demand из Slice B). Использует
--   iterator-предикат правила `scry_background` (ORDER BY last_drift_check_at
--   NULLS FIRST → новые incarnation всегда сканируются раньше уже-сканированных),
--   а также как идемпотентный throttle на повторный скан раньше
--   `min_interval_per_incarnation`.
--
-- * `last_drift_summary` — counts-агрегат сборки последнего DriftReport-а
--   (`{hosts_drifted, hosts_clean, hosts_unsupported, hosts_failed,
--   total_hosts, scanned_at}`). Counts-only: полный DriftReport в фоне не
--   хранится (отложено в отдельный slice; on-demand из Slice B возвращает
--   его прямо в response). Symmetric с `last_drift_check_at` — пишется тем
--   же UPDATE-ом в `incarnation.UpdateDriftScanResult`.
--
-- Partial-индекс на `last_drift_check_at IS NOT NULL` поддерживает
-- iterator-сортировку ORDER BY last_drift_check_at NULLS FIRST: NULL-строки
-- из индекса исключены (их выберет sequential scan по NULL-фильтру до выборки
-- из индекса), а для уже-сканированных индекс отдаёт ORDER без сортировки
-- in-memory. На малых таблицах (десятки incarnation) роли не играет, но на
-- сотнях-тысячах incarnation работает за тот же план.

ALTER TABLE incarnation
    ADD COLUMN last_drift_check_at TIMESTAMPTZ,
    ADD COLUMN last_drift_summary  JSONB;

CREATE INDEX incarnation_last_drift_check_at_idx
    ON incarnation (last_drift_check_at)
    WHERE last_drift_check_at IS NOT NULL;

COMMENT ON COLUMN incarnation.last_drift_check_at IS
    'ADR-031 Slice C: время завершения последнего dry_run converge (фон или on-demand). NULL для incarnation, ни разу не сканированных.';

COMMENT ON COLUMN incarnation.last_drift_summary IS
    'ADR-031 Slice C: counts-агрегат последнего DriftReport (hosts_drifted/clean/unsupported/failed + total + scanned_at). Counts-only — полный отчёт в фоне не хранится.';
