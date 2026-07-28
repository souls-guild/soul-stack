-- 106_prune_projected_soul_traits.down.sql
--
-- Schema-neutral by construction: the up-migration changes DATA only (it prunes
-- projected duplicates out of `souls.traits`), so there is no structure to undo.
--
-- The data is deliberately NOT re-projected here. Re-writing every incarnation's
-- traits back onto its member hosts would recreate exactly the copies ADR-080
-- removed, and on a registry where the operator has since attached host-local
-- labels it would be indistinguishable from them. Nothing is lost by not doing
-- it: every pruned pair is still on the incarnation that carried it, one
-- membership join away, and a keeper rolled back to the pre-ADR-080 code
-- re-projects them on its next incarnation-create or host-bind.

SELECT 1;
