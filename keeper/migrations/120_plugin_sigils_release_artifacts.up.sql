-- 120_plugin_sigils_release_artifacts.up.sql
--
-- A grant approves a RELEASE, not one binary (NIM-793 / NIM-795): the single
-- `sha256` column becomes an `artifacts` list, and the source `kind` the bytes
-- are reached by joins it.
--
-- Why one digest cannot stay
-- --------------------------
-- A plugin published for several platforms is several BINARIES. linux/amd64
-- and linux/arm64 are different bytes and therefore different digests, so a
-- column holding one of them can only ever be right on one platform -- on
-- every other, verification fails against a digest nobody ever intended it to
-- match. Keying a grant on that digest also meant one Archon confirmation per
-- platform per release, which is N approvals of one decision and buys no
-- guarantee the single approval does not already give: the signature covers
-- every row, so no row can be edited without breaking all of them.
--
-- The alternative that was NOT taken: keep one digest and put `{os}` in a path
-- template. That fails for the same arithmetic reason (one digest, N sets of
-- bytes) and, worse, turns the address executable code arrives from into a
-- computed expression. The artifacts list is enumerated on purpose.
--
-- `kind` is stored and signed
-- ---------------------------
-- `git` or `artifact` -- how a host reaches the bytes. It is in the signed
-- block, not only in this table: it decides WHERE a Soul goes to download,
-- and an unsigned answer to that question would let a rewritten catalog
-- redirect a fetch to an address the Archon never approved. The digest check
-- would still catch it, but as a mismatch after the download rather than as a
-- refusal to go there.
--
-- `commit_sha` keeps its meaning and gains a NULL case
-- ---------------------------------------------------
-- It is still the git commit a granted artifact was resolved from: audit
-- ORIGIN, outside the signature. A `kind: artifact` grant has none -- there is
-- no commit -- and the column stays NULL for it rather than being filled with
-- the slot's directory name, which would put a value that resolves to nothing
-- in a field an operator reads as provenance. The artifact kind's provenance
-- is the signed (source, ref) plus a digest per file, all of which the grant
-- already carries.
--
-- Existing rows
-- -------------
-- The table is emptied, exactly as migration 115 did when NIM-377 re-keyed the
-- block onto (source, ref). The reason is the same and it is not a convenience
-- trade: every pre-existing grant was signed under `soul-stack/sigil/v2` over a
-- block carrying ONE digest, and the code now builds v3 over a list, so NO
-- existing signature verifies. Keeping the rows would keep grants that are
-- already cryptographically dead while still looking active in the UI -- and
-- `artifacts` is NOT NULL with no value derivable from anything an old row
-- holds beyond the one digest, which is not a release.
--
-- Approvals are re-issued with `keeper.plugin.allow`. That is the honest state:
-- an operator approves an artifact identity, and what the signature says about
-- that identity changed.

DELETE FROM plugin_sigils;

ALTER TABLE plugin_sigils
    DROP CONSTRAINT plugin_sigils_sha256_format;

ALTER TABLE plugin_sigils
    DROP COLUMN sha256,
    ADD COLUMN kind      TEXT  NOT NULL,
    ADD COLUMN artifacts JSONB NOT NULL;

ALTER TABLE plugin_sigils
    ADD CONSTRAINT plugin_sigils_kind_allowed
        -- The closed set, mirroring shared/plugin.SourceKind*. A third kind is a
        -- migration, deliberately: a new way to reach executable bytes is not
        -- something a config value should be able to introduce.
        CHECK (kind IN ('git', 'artifact')),
    ADD CONSTRAINT plugin_sigils_artifacts_shape
        -- A non-empty JSON array whose every element carries the four string keys
        -- with a well-formed digest.
        --
        -- The FULL rule (canonical order, one row per platform, no duplicate
        -- `(os, arch)`, GOOS/GOARCH token shape) lives in
        -- shared/pluginhost.CanonicalArtifacts, which is also what the signature is
        -- computed over -- so a list that violates it cannot be signed and therefore
        -- cannot be written by the service. What this CHECK is for is the floor under
        -- a row inserted by anything else, and it is deliberately stronger than a
        -- shape assertion: the column it replaces carried
        -- `sha256 ~ '^[0-9a-f]{64}$'`, and dropping to "is an array" would have
        -- traded a real constraint away. It is NOT the whole rule — a duplicate
        -- platform, a half-stated one and a `..` in an os token all pass here and
        -- are caught by CanonicalArtifacts, which is what the signature covers.
        --
        -- A row that slips past this anyway is skipped on read rather than fataling
        -- the whole allow-list (keeper/internal/sigil.ListActive) -- because the list
        -- IS the ReplaceAll snapshot revocation travels on.
        CHECK (
            jsonb_typeof(artifacts) = 'array'
            AND jsonb_array_length(artifacts) > 0
            -- STRICT for the element type: in lax mode `$[*]` unwraps a nested array,
            -- so `[[]]` yields no elements at all and every filter below is vacuously
            -- satisfied. Strict mode yields the inner array itself, which is then not
            -- an object and is refused.
            AND NOT jsonb_path_exists(artifacts, 'strict $[*] ? (@.type() <> "object")')
            AND NOT jsonb_path_exists(artifacts, '$[*] ? (
                    !(exists(@.os))     || !(exists(@.arch))
                 || !(exists(@.path))   || !(exists(@.sha256))
                 || @.os.type()     <> "string" || @.arch.type()   <> "string"
                 || @.path.type()   <> "string" || @.sha256.type() <> "string"
                 || !(@.sha256 like_regex "^[0-9a-f]{64}$"))')
        );

COMMENT ON TABLE plugin_sigils IS
    'Keeper-signed allow-list of approved plugin releases (ADR-026, re-keyed by NIM-377, list-valued by NIM-793). Trust key (source, ref) -> kind + artifacts[] + schema + Keeper signature; alias is the operator registration and the runtime lookup key, never signed.';

COMMENT ON COLUMN plugin_sigils.kind IS
    'Source kind the grant is on: git (resolved from a repository) or artifact (published under a base URL). Signed - it decides where a Soul fetches the bytes from.';

COMMENT ON COLUMN plugin_sigils.artifacts IS
    'The approved release: [{os, arch, path, sha256}], one row per platform, canonically ordered. Signed as a whole. A git grant carries exactly one row with empty os/arch/path - that source declares no platform, so its single binary answers for every one.';

COMMENT ON COLUMN plugin_sigils.commit_sha IS
    'Git commit the granted artifact was resolved from. Audit origin, OUTSIDE the signature. NULL for kind=artifact, which has no commit: its provenance is the signed (source, ref) plus a digest per file.';
