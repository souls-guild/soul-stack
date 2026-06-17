-- 063_voyage_targets_dispatch_idx.up.sql
--
-- C1 (ADR-043): partial-UNIQUE индексы на dispatch-ссылках voyage_targets для
-- back-link history → Voyage. soul.SelectHistory LEFT JOIN-ит voyage_targets по
-- apply_id (scenario) и errand_id (errand); без индекса — seq scan на каждой
-- странице per-host timeline. Partial WHERE отсекает NULL-строки (target до
-- dispatch), индекс покрывает только заполненные ссылки. UNIQUE одновременно
-- гарантирует на уровне БД инвариант «один apply_id/errand_id → максимум одна
-- строка voyage_targets» (а не только логикой записи MarkTargetRunning).
CREATE UNIQUE INDEX voyage_targets_apply_id_idx
    ON voyage_targets (apply_id) WHERE apply_id IS NOT NULL;

CREATE UNIQUE INDEX voyage_targets_errand_id_idx
    ON voyage_targets (errand_id) WHERE errand_id IS NOT NULL;
