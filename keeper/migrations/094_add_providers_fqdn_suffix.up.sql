-- 094_add_providers_fqdn_suffix.up.sql
--
-- Provider FQDN suffix for self-onboard "Variant T" (ADR-017(h) amendment:
-- keeper assigns the VM name -> FQDN is predictable -> per-VM token gets baked into userdata
-- BEFORE create).
--
-- Onboarding chicken-egg: SID = FQDN is assigned by the provider AFTER create, but
-- userdata is built BEFORE. In "Variant T" Keeper sets the base name of the VM batch
-- (CreateRequest.name) and knows the provider's FQDN suffix, so it predicts
-- the full FQDN of each VM: `<name>-<index>.<fqdn_suffix>` (e.g.
-- `redis-0.fedorovstepan2-dev.vm.xc.clv3`). Knowing the FQDN in advance, keeper issues
-- per-VM bootstrap tokens and puts them into userdata (a shared blob, cloud-init picks
-- its own by hostname) - before create, without a claim-callback.
--
-- The suffix is a function of the provider's namespace+cluster (WB: `<namespace>.vm.<cluster>`),
-- stable across all VMs of the provider, so it lives in the Provider registry next to
-- region/credentials_ref, rather than in profile/essence (Provider is the authority over
-- where and how this provider's VMs are named).
--
-- Nullable: not all drivers form the FQDN by the `<name>.<suffix>` scheme (AWS gives
-- instance-private-dns, GCP - internal DNS). NULL/empty -> keeper cannot
-- predict the FQDN -> self-onboard is unavailable for this provider (the
-- core.cloud.created step with self_onboard: true will return a clear error). Leading dot
-- - the suffix is WITHOUT a leading dot (keeper joins the pieces via '.').

ALTER TABLE providers
    ADD COLUMN fqdn_suffix TEXT;

-- Suffix format: dot-separated DNS labels, no leading/trailing dot, no
-- underscore (RFC-1035-compatible, keeper will join `<name>.<suffix>` into a valid
-- FQDN). NULL is allowed (a provider without a predictable FQDN). An empty string is NOT
-- allowed (use NULL - "no suffix"), otherwise the FQDN would end up with a
-- trailing dot.
ALTER TABLE providers
    ADD CONSTRAINT providers_fqdn_suffix_format
        CHECK (fqdn_suffix IS NULL OR
               fqdn_suffix ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$');

COMMENT ON COLUMN providers.fqdn_suffix IS
    'Provider FQDN suffix (self-onboard Variant T, ADR-017(h)): keeper predicts the VM FQDN as <name>-<index>.<fqdn_suffix>. NULL -> self-onboard unavailable for the provider.';
