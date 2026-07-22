# smoke-nginx

A minimal service for the L3a E2E pilot ([ADR-039](../../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity)).

## What it demonstrates

- A service with a non-empty `state_schema` and two scenario fields
  (`nginx_package`, `nginx_service`) that are committed to `incarnation.state`
  after a successful apply (via `state_changes.sets`).
- A `create` scenario made of two sequential core tasks:
  - `core.pkg.installed name=nginx` — installs the package;
  - `core.service.running name=nginx enabled=true` — starts and enables the systemd unit.
- A minimal `input:` (only `hostname`, required) — an example of a required
  field without shape validation on a concrete value (any non-empty string).

## Where it's used

- `tests/e2e/smoke_nginx_test.go` — the Go test for the L3a pilot, runs the
  `create` scenario through the harness, the soul stub responds with a scripted
  `RunResult: success`, the test validates apply_runs / incarnation.state / audit / metrics.

## What's not here

- A real `nginx.conf` template — the pilot tests the scenario-runner ↔
  apply_runs ↔ audit contract, not a real apply. L3b (real soul binary in a
  container) will get its own fixture with a real config render.
- An idempotent destroy scenario — the pilot focuses on the happy-path create.

## Trial checks

L0/L1 (`soul-trial`) for this example haven't been added yet — the pilot targets
L3a. A trial fixture (`_trial/`) will appear as a separate slice if a
purely-hermetic-render assertion is needed for smoke-nginx.
