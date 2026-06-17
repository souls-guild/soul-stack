-- 007_create_souls.up.sql
--
-- Реестр Soul-агентов под ADR-002 / ADR-012 + docs/soul/identity.md.
-- PK — `sid` (= FQDN хоста), `transport` — enum (`agent` | `ssh`),
-- `status` — узкий MVP-enum (`pending` | `connected` | `disconnected` |
-- `revoked` | `expired`). Расширяется значением `destroyed` в миграции 016
-- (ADR-017 cascade от `core.cloud.provisioned destroyed`).
--
-- Реальный pull/push-флоу:
--   * `pending` — оператор выписал bootstrap-токен, Soul ещё не пришёл.
--   * `connected` — стрим жив, Keeper держит lease в Redis.
--   * `disconnected` — стрим закрыт, lease истёк.
--   * `revoked` — оператор отозвал, новые подключения отвергаются на mTLS-уровне.
--   * `expired` — Жнец передвинул pending → expired после TTL bootstrap-токена.
--   * `destroyed` — добавлен миграцией 016: terminal-state после cloud-destroy.
--
-- FK `created_by_aid` → operators(aid) (ADR-014). ON DELETE SET NULL —
-- история Soul-а важнее ссылочной целостности (revoke оператора не должен
-- сносить реестр Souls).
--
-- `coven` — `text[]` (множественные стабильные метки, ADR-008).
-- `last_seen_at` в PG — flush из Redis (актуальное значение в Redis-кэше).

CREATE TABLE souls (
    sid                TEXT        PRIMARY KEY,
    transport          TEXT        NOT NULL DEFAULT 'agent',
    status             TEXT        NOT NULL DEFAULT 'pending',
    coven              TEXT[]      NOT NULL DEFAULT ARRAY[]::TEXT[],
    registered_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at       TIMESTAMPTZ,
    last_seen_by_kid   TEXT,
    created_by_aid     TEXT,
    requested_at       TIMESTAMPTZ,
    note               TEXT,

    CONSTRAINT souls_sid_format
        CHECK (sid ~ '^[a-z0-9][a-z0-9.-]{0,253}$'),
    CONSTRAINT souls_transport_valid
        CHECK (transport IN ('agent', 'ssh')),
    CONSTRAINT souls_status_valid
        CHECK (status IN ('pending', 'connected', 'disconnected', 'revoked', 'expired')),
    CONSTRAINT souls_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Типовой запрос Reaper-а и Operator API — «все pending старше X» или
-- «все connected для health-overview».
CREATE INDEX souls_status_idx
    ON souls (status);

-- Поддержка таргетинга по coven-меткам (ADR-008, scenario `on:`).
-- GIN-индекс по text[] — стандартный путь для `coven && ARRAY['db','prod']`.
CREATE INDEX souls_coven_idx
    ON souls USING GIN (coven);

-- Для Жнеца: pending Souls старше TTL bootstrap-токена → expired.
CREATE INDEX souls_pending_requested_at_idx
    ON souls (requested_at)
    WHERE status = 'pending';

COMMENT ON TABLE souls IS
    'Реестр Soul-агентов (ADR-002 / ADR-012). PK = sid (FQDN), coven — text[]-метки.';
