-- 032_create_omens.up.sql
--
-- Реестр Omen-ов — внешних систем, к которым Augur посредничает доступ Soul-у
-- (ADR-025, docs/keeper/augur.md → §4.1). Omen — managed-через-API запись:
-- один Vault-mount / один Prometheus / один ELK-кластер. Аналог Provider для
-- облаков (миграция 019).
--
-- PK — `name` (kebab-case, уникальное в кластере). `source_type` —
-- descriptive closed enum (`vault` / `prometheus` / `elk`, augur.md §7);
-- расширение enum-а — propose-and-wait + PR в augur.md и naming-rules.md.
--
-- Инвариант: `auth_ref` ВСЕГДА vault-ref (`vault:<mount>/<path>`) на
-- master-credential Keeper-а — сам secret в БД НЕ хранится, только ссылка
-- (симметрично providers.credentials_ref). Формат vault-ref здесь CHECK-ом НЕ
-- ловится — это делает service-слой через vault.ParseRef (augur.md §4.1).
--
-- FK:
--   - created_by_aid → operators(aid) ON DELETE SET NULL (запись Omen-а
--     переживает удаление оператора; симметрично providers/incarnation).

CREATE TABLE omens (
    name           TEXT        PRIMARY KEY,
    source_type    TEXT        NOT NULL,
    endpoint       TEXT        NOT NULL,
    auth_ref       TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_aid TEXT,

    CONSTRAINT omens_name_format
        CHECK (name ~ '^[a-z0-9-]{1,63}$'),
    CONSTRAINT omens_source_type_enum
        CHECK (source_type IN ('vault', 'prometheus', 'elk')),
    CONSTRAINT omens_created_by_aid_fk
        FOREIGN KEY (created_by_aid) REFERENCES operators (aid) ON DELETE SET NULL
);

-- Лента Omen-ов конкретного оператора (audit / триаж).
CREATE INDEX omens_created_by_aid_idx
    ON omens (created_by_aid);

COMMENT ON TABLE omens IS
    'Реестр внешних систем Augur (ADR-025). auth_ref = vault:<mount>/<path>, master-credential в БД не пишется.';
