-- 115_plugin_sigils_source_identity.up.sql
--
-- Re-keys the Sigil registry from (namespace, name, ref) onto (source, ref), and
-- carries the registration alias as a separate column (NIM-377 / NIM-438).
--
-- Why the old key cannot stay
-- ---------------------------
-- Until NIM-377 an artifact declared its own `namespace` and `name` in a
-- manifest, and the grant was keyed on them. The artifact now declares NEITHER:
-- it knows only its own modules (`acl`, `config`, `info`), and address level 1
-- comes entirely from the ALIAS an operator picks at registration.
--
-- That makes the old key unusable as a trust key, and not merely absent. An
-- alias is operator-chosen text: keying an approval on it would mean the
-- identity a signature is bound to is a value the same operator can rename, so
-- renaming an alias would walk around an approved hash instead of requiring a
-- fresh approval. The only identity left that an operator ASSERTS about the
-- bytes rather than picks for them is where the artifact came from -- the git
-- SOURCE, at a REF. That is what the signed block covers since NIM-377
-- (`soul-stack/sigil/v2`), and that is what the active key becomes here.
--
-- Two partial unique indexes, doing two different jobs
-- ---------------------------------------------------
--   plugin_sigils_active_idx        (source, ref) -- the TRUST key. At most one
--       live approval per artifact identity, so re-approving the same artifact
--       is a conflict the operator resolves by revoking first, never a silent
--       second grant that a revoke of the first would leave standing.
--
--   plugin_sigils_active_alias_idx  (alias)       -- a REGISTRATION invariant,
--       NOT a trust key. One alias is one address space and one host slot:
--       `<alias>.<module>.<state>` must name one set of bytes, and the runtime
--       lookup (shared/pluginhost.SigilLookup.Get(alias)) must have exactly one
--       answer rather than "whichever row sorted first". Trust is still decided
--       by the signature over (source, ref); this index only stops two live
--       registrations from claiming the same address.
--
-- What the trust key COSTS, stated plainly
-- ----------------------------------------
-- Because at most one live row may hold a given (source, ref), one artifact
-- identity cannot be registered under two aliases AT THE SAME TIME. The concrete
-- consequence: RENAMING AN ALIAS IS REVOKE-THEN-APPROVE, NOT AN OVERLAP. An
-- operator moving `redis` to `redis-prod` must revoke `redis` first, and between
-- the two calls the artifact has no active grant -- Souls receive the revocation
-- near-instantly (S6c invalidate) and any spawn in that window fails closed with
-- reason=no_sigil. That is an availability window, never an authorization one, but
-- it is real and must be planned for rather than discovered.
--
-- The alternative -- keying only on the alias -- buys a zero-downtime rename and
-- pays for it with duplicate live approvals of the same bytes: revoking "the"
-- grant would leave the other one standing, which is exactly the property
-- NIM-438 exists to prevent. We keep the stronger invariant and document the
-- window.
--
-- manifest / manifest_raw -> schema
-- --------------------------------
-- The hand-written manifest.yaml is gone; a generated, canonical JSON schema
-- document takes its place, stamped into the artifact's trailer. It is stored
-- ONCE, as bytes: the document is already canonical JSON, so the old split into
-- a byte-exact BYTEA canon plus a derived JSONB projection would now be two
-- copies of the same value that can disagree -- and the one that could drift is
-- the one a signature was NOT placed over.
--
-- Existing rows
-- -------------
-- The table is emptied. This is not data loss weighed against convenience: every
-- pre-existing grant was signed under the v1 domain separator over a block keyed
-- on (namespace, name), so NO existing signature verifies against the v2 block
-- the code now builds. Keeping the rows would keep grants that are already
-- cryptographically dead while looking active in the UI -- and `source` is NOT
-- NULL with no value derivable from anything the old row holds. Approvals are
-- re-issued with `keeper.plugin.allow`, which is the honest state: an operator
-- approves an artifact identity, and the artifact identity changed.

DELETE FROM plugin_sigils;

DROP INDEX IF EXISTS plugin_sigils_active_idx;

ALTER TABLE plugin_sigils
    DROP COLUMN namespace,
    DROP COLUMN name,
    DROP COLUMN manifest,
    ADD COLUMN alias  TEXT NOT NULL,
    ADD COLUMN source TEXT NOT NULL;

ALTER TABLE plugin_sigils
    RENAME COLUMN manifest_raw TO schema;

-- schema is the byte-exact canon the signature covers. NOT NULL: a grant whose
-- signed bytes are absent cannot be verified by anything, so a row without them
-- is a grant that can only fail closed later -- reject it at write time instead.
ALTER TABLE plugin_sigils
    ALTER COLUMN schema SET NOT NULL;

ALTER TABLE plugin_sigils
    ADD CONSTRAINT plugin_sigils_alias_format
        -- The alias names a host-cache directory AND address level 1, so its
        -- charset is the intersection of both: lowercase kebab-case, no dots
        -- (they separate address levels), no slashes or '..' (path traversal).
        -- Mirrors shared/plugin.AliasPattern.
        CHECK (alias ~ '^[a-z][a-z0-9-]{0,62}$');

-- The TRUST key: at most one active grant per approved artifact identity.
CREATE UNIQUE INDEX plugin_sigils_active_idx
    ON plugin_sigils (source, ref)
    WHERE revoked_at IS NULL;

-- The REGISTRATION invariant: at most one active grant per alias, so the
-- alias->grant lookup on the verify path is single-valued.
CREATE UNIQUE INDEX plugin_sigils_active_alias_idx
    ON plugin_sigils (alias)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE plugin_sigils IS
    'Keeper-signed allow-list of approved plugin artifacts (ADR-026, re-keyed by NIM-377). Trust key (source, ref) -> sha256 + schema + Keeper signature; alias is the operator registration and the runtime lookup key, never signed.';

COMMENT ON COLUMN plugin_sigils.alias IS
    'Registration alias: address level 1, operator-chosen, the key a host slot and the verify path resolve by. NOT part of the signed block.';

COMMENT ON COLUMN plugin_sigils.source IS
    'Artifact source (the module repository remote) the grant is on. Signed, together with ref: with no self-name in the artifact this is its only identity.';

COMMENT ON COLUMN plugin_sigils.schema IS
    'Byte-exact canonical schema document the signature covers (stamped in the artifact trailer). Canon for verify and for the Soul broadcast.';
