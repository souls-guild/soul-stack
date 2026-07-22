# smoke-nginx-live — L3b flagship smoke

L3b real-soul-in-container smoke ([ADR-039](../../../docs/adr/0039-e2e-testing.md#adr-039-e2e-testing--three-levels-without-a-new-dictionary-entity)).

Parallel to [smoke-nginx](../smoke-nginx/README.md) (L3a): same end-to-end
scenario "install nginx → render config → start service", but going through a
REAL apt-install in a Debian-12 soul container, rather than through a scripted
soul-stub.

## What it demonstrates

- `core.pkg.installed name=nginx` — a real package install (`apt-get
  install nginx`) inside the container.
- `core.file.rendered` — rendering `/etc/nginx/sites-available/default` from
  the template `templates/nginx-default.conf.tmpl` with variables from
  `input.hostname` and `essence.nginx_listen_port`.
- `core.service.running name=nginx enabled=true` — bringing up the systemd unit.
- `core.service.restarted onchanges: [nginx_default_conf]` — a reactive
  restart only when the config changes (idempotency of a repeated apply).

All four steps are inlined in the scenario (no dedicated destiny): the smoke test
isn't reused in other scenarios, so extracting a destiny for it would be
overhead (see [docs/scenario/concept.md → boundary — recommendation](../../../docs/scenario/concept.md)).

## Where it's used

- `tests/e2e-live/smoke_nginx_live_test.go` — the L3b flagship Go test,
  runs the `create` scenario through the harness, validates:
  - `apply_runs.status = success` (all rows);
  - `incarnation.state` contains `nginx_package=installed`,
    `nginx_service=active`, `nginx_config_managed=true`;
  - the `incarnation.scenario_started` audit event matches incarnation/apply_id;
  - metric `keeper_apply_runs_total >= 1`.

Container-side assertions (`AssertHostPkgInstalled`, `AssertHostServiceActive`,
`AssertHostFileExists`) appear in L3b-4 — here it's only Keeper-side
observable properties.

## What's NOT here

- An idempotent destroy scenario — the flagship focuses on the happy-path
  create (a repeated create on the same container is idempotent, see
  comments in scenario/create/main.yml, but not covered by a separate test).
- Multi-host runs — will be added in L3b-5.
- HTTPS/upstream — a minimal server block without TLS is enough for smoke testing.

## Trial checks

L0/L1 (`soul-trial`) for this example haven't been added yet — the flagship targets
L3b. A trial fixture (`_trial/`) will appear as a separate slice if
a hermetic render assertion is needed (e.g. for rendering the nginx config
without bringing up a container).

## Expected run time

~3–5 minutes on a loaded CI machine: `apt-get update` (~30 s) + installing
nginx (~30 s) + starting the systemd unit (~5 s) + polling apply_runs until success.
The test sets a timeout of 300 s (`WaitApplySuccess(t, applyID, 300)`).
