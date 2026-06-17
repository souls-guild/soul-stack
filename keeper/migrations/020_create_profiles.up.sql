-- 020_create_profiles.up.sql
--
-- Реестр Cloud-Profile-ей (ADR-017, docs/keeper/cloud.md). Profile — VM-spec
-- поверх конкретного Provider-а: `params` (jsonb, произвольный VM-spec,
-- валидируется против CloudDriver.Schema на service-слое — Cloud.CRUD.b) +
-- optional `cloud_init` (сырая userdata).
--
-- FK:
--   - provider → providers(name) ON DELETE RESTRICT (PM-decision: не
--     CASCADE — защита от потери данных; удаление Provider-а с
--     зависимыми Profile-ями требует явного удаления профилей).
--   - created_by_aid → operators(aid) ON DELETE SET NULL (запись переживает
--     удаление оператора).

CREATE TABLE profiles (
    name           TEXT        PRIMARY KEY,
    provider       TEXT        NOT NULL,
    params         JSONB       NOT NULL,
    cloud_init     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT profiles_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT profiles_provider_fk
        FOREIGN KEY (provider) REFERENCES providers (name) ON DELETE RESTRICT,
    CONSTRAINT profiles_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Резолв Profile-ей конкретного Provider-а (SelectByProvider, проверка
-- зависимостей перед удалением Provider-а).
CREATE INDEX profiles_provider_idx
    ON profiles (provider);

-- Лента Profile-ей конкретного оператора (audit / триаж).
CREATE INDEX profiles_created_by_aid_idx
    ON profiles (created_by_aid);

COMMENT ON TABLE profiles IS
    'Реестр Cloud-Profile-ей (ADR-017). provider FK ON DELETE RESTRICT, params jsonb (VM-spec).';
