-- 110_drop_incarnation_spec_essence.up.sql
--
-- ADR-0082 (NIM-414): `incarnation.spec.essence` is removed. The service
-- parameter layer it overrode is now `<service>/vars/`, read in CEL as `vars.*`,
-- and nothing outside the service repository overrides it — a fleet that needs
-- different defaults forks the service repo and re-pins its `ServiceRef`
-- (ADR-007).
--
-- The key had two readers (`scenario/state.go`, `grpc/events_telemetry.go`, both
-- through `specEssence()`) and NO writer: `IncarnationCreateRequest` never
-- carried a field for it, and the only write of the key anywhere was a unit-test
-- fixture. It was a declared extension point that was never finished, so in
-- practice this migration matches nothing on a live database — which is the
-- point of running it anyway rather than assuming.
--
-- `spec` is a freeform jsonb, so there is no column to drop — only the key. The
-- alternative, leaving it in place, was rejected for the reason migration 108
-- gives for `hosts[]`: `spec` is returned verbatim by
-- `GET /v1/incarnations/{name}` and rendered in the UI, so a leftover `essence`
-- would keep advertising an override no resolver reads. A year from now it would
-- look like data rather than debris.
--
-- Only the `essence` key is removed; every other key of `spec` is preserved by
-- the `-` operator. Rows whose spec has no `essence` key are not touched at all
-- (the WHERE clause), so this does not rewrite the whole table to no effect.
-- The column itself goes in NIM-408, once `input` has moved to `state_history`.
--
-- Idempotent: a re-run matches nothing.

UPDATE incarnation
SET spec = spec - 'essence'
WHERE spec ? 'essence';
