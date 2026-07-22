-- 088_add_incarnation_traits.up.sql
--
-- Trait is RELOCATED from per-soul to per-incarnation (ADR-060 amend, R1). Before this
-- migration Trait lived only on the host (`souls.traits`, migration 087) as
-- operator-set-per-soul; the user clarified: traits belong to the INCARNATION
-- (an organizational label for the owner/product/namespace of the whole instance, not
-- an individual host). This column is the NEW operator-set source of truth for Traits:
-- set in incarnation.spec at create, projected MATERIALIZED into
-- souls.traits of member hosts via a sync-hook (incarnation create + host bind
-- via core.soul.registered).
--
-- Mirrors 046 (incarnation.covens): jsonb instead of TEXT[] - the Trait value
-- is polymorphic (scalar | list), same as in souls.traits (087); `TEXT[]` can't
-- express that. NOT NULL DEFAULT '{}' - an incarnation with no traits = an empty object, not
-- NULL (symmetry with covens DEFAULT '{}', the read/projection path doesn't distinguish "no
-- column" / "no labels").
--
-- souls.traits REMAINS the projection target (the read layer soulprint.self.traits /
-- where:traits / soul-lint / topology is reused unchanged). The old
-- per-soul bulk-write (POST /v1/souls/traits) still works directly on souls.traits,
-- but during the transition period it gets overwritten by the projection on the next
-- incarnation.traits sync (relocate per-soul -> per-incarnation is the next slice).

ALTER TABLE incarnation
    ADD COLUMN traits JSONB NOT NULL DEFAULT '{}'::jsonb;

-- GIN index for targeting by incarnation.traits (`traits @> '{"team":"dba"}'`
-- containment) - mirrors souls_traits_idx (087) and parallels the GIN over covens[].
-- Supports a future RBAC-scope-by-traits on the incarnation dimension
-- (unlocked by a later slice), does not block the R1 foundation.
CREATE INDEX incarnation_traits_idx
    ON incarnation USING GIN (traits);

COMMENT ON COLUMN incarnation.traits IS
    'Trait - operator-set key-value labels for the incarnation (ADR-060 amend, R1); value is scalar|list. Source of truth, projected into souls.traits of member hosts via a sync-hook.';
