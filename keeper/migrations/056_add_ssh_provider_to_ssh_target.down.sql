-- 056_add_ssh_provider_to_ssh_target.down.sql
--
-- Откат расширенного CHECK к виду миграции 053. Существующие rows с
-- проставленным `ssh_provider` пройдут downgrade-CHECK только если поле
-- отсутствует в payload — иначе CHECK violation. Поэтому оператор обязан
-- удалить `ssh_provider` из всех `souls.ssh_target` до запуска down-migration
-- (или принять, что строки с провайдером не пройдут downgrade — это намеренная
-- защита: down-migration не должна молча терять данные).

ALTER TABLE souls DROP CONSTRAINT IF EXISTS souls_ssh_target_shape;

ALTER TABLE souls ADD CONSTRAINT souls_ssh_target_shape CHECK (
    ssh_target IS NULL OR (
        jsonb_typeof(ssh_target->'ssh_port') = 'number' AND
        jsonb_typeof(ssh_target->'ssh_user') = 'string' AND
        jsonb_typeof(ssh_target->'soul_path') = 'string'
    )
);

COMMENT ON COLUMN souls.ssh_target IS
    'Per-host SSH-реквизиты push-flow (ADR-032 amendment 2026-05-26, S7-1): {ssh_port, ssh_user, soul_path}. NULL → fallback на keeper.yml::push.targets[] под флагом push.allow_legacy_push_targets.';
