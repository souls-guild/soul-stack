# upgrade-demo — live stand for NIM-34 (upgrade v2)

A demo service + script for **manual testing of incarnation upgrades**
([ADR-0068](../../docs/adr/0068-service-upgrade-v2.md)):
`GET /v1/incarnations/{name}/upgrade-paths` (+`?to=`) and `POST .../upgrade`. Shows
all resolution branches — cheap / found / legacy / state migrations — and a live
legacy upgrade to `drift`.

Why a separate fixture: no `examples/service/*` carries an `upgrade/<slug>/`
directory or has several version tags, so the feature can't be shown on them.
The fixture lives in `dev/` (not `examples/`) so it doesn't affect trial/soul-lint
of the examples or `make check`.

## What it demonstrates

The `upgrade-demo` service with three tags (built by the script into the git repo
`/tmp/keeper-dev/repos/upgrade-demo` from `tree/` snapshots):

| Tag | schema | `upgrade/` | Role in the demo |
|---|---|---|---|
| `v1.0.0` | 1 | none | starting pin of a bare incarnation |
| `v2.0.0` | 2 | `upgrade/to_v2/` (`from: ["v1.0.0"]`) | target of **found** mode |
| `v2.0.1` | 2 | none | target of **legacy** mode (same migration `001_to_002`) |

The incarnation is created **bare** (the service has no create scenario -> no
hosts, goes straight to `ready` at pin `v1.0.0`, `state_schema_version=1`).

## Covered cases (proven by the run)

- **cheap** — `GET .../upgrade-paths` without `?to=`: list of registry tags
  (`v1.0.0`/`v2.0.0`/`v2.0.1`/`main`) + `is_current=true` for `v1.0.0`.
- **found** — `?to=v2.0.0`: `direction=forward`, `mode=found`, `slug=to_v2`,
  `reachable=true`, `state_migrations=[{from:1,to:2,path:migrations/001_to_002.yml}]`
  (the scanner found `upgrade/to_v2/main.yml`, whose `from:` contains the current pin).
- **legacy** — `?to=v2.0.1`: `mode=legacy` (no `upgrade/` scenario, `slug` omitted),
  `reachable=true`, the same migration chain.
- **live legacy upgrade** — `POST .../upgrade {to_version:"v2.0.1"}` -> `202 {apply_id}`
  -> pin change `v1.0.0->v2.0.1` + state migration (schema `1->2`, `state.schema_v2=true`)
  -> `status=drift`. A single PG transaction, no hosts.

## Prerequisites

A live dev stand (keeper on `:8080` + PG/Redis/Vault). One-time setup:

```bash
make dev-up          # docker PG(5434)/Redis(6381)/Vault(8200)
make dev-provision   # TLS + Vault KV/PKI (expensive — only if not done yet)
VAULT_TOKEN=root make dev-keeper   # keeper on :8080 (BUILT FROM THIS worktree — has the NIM-34 feature)
```

> **★ keeper must be built from this worktree** (`feat/nim34-upgrade-v2`): the
> `upgrade-paths` route was introduced by this feature, it's not in a binary from
> `main`/an old build. The script detects this and prints the exact rebuild
> command. If keeper is already on `:8080` — the script reuses it.
>
> **★ VAULT_TOKEN**: dev scripts take `VAULT_TOKEN` from the env; if it's a prod
> token there (a common trap), force `VAULT_TOKEN=root`. `run.sh` does this
> automatically.

## Running it

```bash
bash dev/upgrade-demo/run.sh
```

Idempotent: the git repo/registry are safely recreated; keeper is reused if
already on `:8080`. Every run creates a new incarnation `updemo-<rand>` (the
teardown command is at the end of the output). The script prints the actual
curl responses for each case and fails with a clear error if there's a blocker
(no keeper/feature/Vault).

## Clickable UI stand (`ui-stand.sh`) — see it in the browser

Spins up an **isolated** demo keeper on `:8090` (config `keeper-demo.yml`, no
`push` block — the standard `keeper.dev.yml` fails in dev on a missing
teleport identity) + web UI on `:5174` (companion repo `soul-stack-web`, branch
`feat/nim34-upgrade-paths-ui`, where the Upgrade modal renders an
`upgrade-paths` preview). Dedicated ports — to live alongside the shared stand
on `:8080` without interfering with another session (its own `kid`,
`acolytes:0`, reaper/voyage OFF on the shared PG).

```bash
bash dev/upgrade-demo/ui-stand.sh
```

The script brings up docker/Vault itself (if needed, including an ed25519
sigil key), seeds the service + incarnation `updemo-ui` at `v1.0.0`, and
prints the **URL + JWT + what to click**. Open
`http://localhost:5174/ui/`, log in with the token, go to `updemo-ui` ->
**Upgrade** -> pick a version in the dropdown:

| Choice | Preview panel in the modal |
|---|---|
| `v2.0.0` | **found** (green) · direction forward · migrations 1->2 · "host orchestration will start" |
| `v2.0.1` | **legacy** (gray) · direction forward · migrations 1->2 · "version change -> drift" |

Clicking **Upgrade** on `v2.0.1` — a live legacy transition of the incarnation
to `drift` at `v2.0.1` (schema 2, no hosts). `v2.0.0` (found) shows the
preview in the UI, but the actual upgrade is at e2e-live level (needs souls,
see below).

> **★ web repo** must be on branch `feat/nim34-upgrade-paths-ui` (that's where
> the UI consumer of upgrade-paths lives); the script will warn if it isn't.
> To stop the stand:
> `pkill -f 'keeper-demo.yml' ; pkill -f 'vite.*--port 5174'`.

## What's not covered

- **found auto-run** (`POST upgrade` on `v2.0.0`): the found branch, after the
  migration, runs the upgrade scenario on the hosts (`Runner.Start` ->
  `applying` -> `ready`). This requires running souls — e2e-live level, outside
  the dev stand. Here `?to=v2.0.0` only **shows** that the transition would be
  `found` (scanner + `slug`); the actual upgrade only runs on the legacy path
  (`v2.0.1`, no hosts).
- Auto-chaining `v1->v3`, glob/semver in `from:`, bulk upgrade (NIM-35) — non-goals
  for the MVP ([ADR-0068 §8](../../docs/adr/0068-service-upgrade-v2.md)).

## Structure

```
dev/upgrade-demo/
  ui-stand.sh                # ★ clickable UI stand (demo keeper :8090 + web :5174)
  keeper-demo.yml            # isolated demo keeper config (no push; ports 8090/8091/9091)
  run.sh                     # curl run through the cases (boot/reuse -> build -> seed -> cases)
  README.md                  # this file
  tree/
    v1.0.0/service.yml + essence/_default.yaml
    v2.0.0/… + migrations/001_to_002.yml + upgrade/to_v2/main.yml   (found)
    v2.0.1/… + migrations/001_to_002.yml                            (legacy)
```
