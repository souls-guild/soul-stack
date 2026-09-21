-- 106_prune_projected_soul_traits.down.sql
--
-- Schema-neutral by construction: the up-migration changes DATA only (it prunes
-- projected duplicates out of `souls.traits`), so there is no structure to undo.
--
-- The data is deliberately NOT re-projected here. Re-writing every incarnation's
-- traits back onto its member hosts would attach labels to hosts nobody ever
-- labelled, and on a registry where the operator has since attached host-local
-- labels the copies would be indistinguishable from them. Every pruned pair is
-- still on the incarnation that carried it; a keeper rolled back to the code
-- that projected re-creates its own copies on the next incarnation-create or
-- host-bind, without help from here.

SELECT 1;
