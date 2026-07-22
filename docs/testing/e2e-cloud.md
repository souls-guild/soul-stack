# Cloud live-E2E orchestrator (`scripts/e2e-cloud/`)

Runbook of a repeatable live run of Soul Stack against a **permanent** keeper on
cloud VM. The operator runs create / day-2 / destroy scripts with one command
(redis and hereafter DragonFly) via the Operator API and receives a report with assertions.
Reusable for regressions and pre-release checks.

Level context - [testing/README.md](README.md); level design L3a/L3b/L3c —
[ADR-039](../adr/0039-e2e-testing.md).

## What it is and what it is NOT

Orchestrator is **bash on top of Operator API keeper**, not Go-harness. He doesn't
raises the stand himself: keeper and Souls already live on VM, the orchestrator just pulls
HTTP routes (`POST /v1/incarnations`, `POST .../scenarios/{scenario}`,
`DELETE ...`), polls the run to the terminal and asserts the result. The only one
network boundary - function `keeper_api` (`lib/keeper-api.sh`); all classification /
survey / assertions / report are clean and covered with docker-free guard tests
(`test/guard.sh`, target `make check-e2e-cloud`).

⚠️ **This is NOT `make e2e-live` / `make e2e-live-gate`.** Those are a local docker gate
(**L3b**): real `soul` binary in a privileged container, ephemeral stack via
testcontainers. Local gate **deliberately NOT covering the cloud**: cloud-provision
(`CloudDriver`), Nexus-`install_method`, sentinel-/cluster-redis topologies,
multi-keeper - all this is booth territory (see.
[testing/README.md → what the live gate does NOT cover](README.md)).
Cloud orchestrator - **L4-adjacent operator-tooling**: live run vs.
persistent keeper, outside the ephemeral-invariant L3a/L3b.

`make e2e-cloud` **not included** in `make check` (requires cloud and teleport,
symmetrically `e2e` / `e2e-live`). `check` includes only docker-free
`check-e2e-cloud` (carrier logic guard).

## Two keeper worlds (important - non-interchangeable)

The orchestrator goes to the keeper in two ways; selection - variable `EXEC_MODE`.
The worlds differ in endpoint, JWT and CA - you can't confuse them.

| | LOCAL dev stand | CLOUD native-keeper |
|---|---|---|
| `EXEC_MODE` | `local` | `tsh` (default) |
| Where is keeper | local machine (`/tmp/keeper-dev`) | VM(`192.168.2.3`) |
| How Souls are connected | reverse-tunnel (`ssh -R`) | natively, on the VM itself |
| API access | directly curl to `$KEEPER_API` (`:8080`) | only `tsh ssh` on VM → curl `localhost:8080` |
| JWT operator | `$JWT_FILE` (`/tmp/keeper-dev/archon-alice.jwt`) | `$REMOTE_JWT` on VM (`/opt/soul-stack/archon-cloud.jwt`) |
| Who reads JWT | local shell | code **on VM** (`cat $REMOTE_JWT`) |

In the `tsh` world, the POST body is sent to the VM **base64 via env** (`BODY_B64` in
`bash -s`) so as not to drown in nested quoting; teleport noise
(`WARNING`/`self-signed`/…) is filtered. JWT in the `local` world and JWT/CA in the `tsh` world
belong to **different** keepers - a token from one world will not fit another.

Which bring-up script to which world - see canonical lists of steps.

## Preconditions (gates preflight)

`preflight` (`lib/preflight.sh`) - hard gate **until touching the cloud**: on failure
runbook exits with code **2** without sending anything to keeper. Prints checklist ✓/✗.
Checks only **presence**, does not collect anything:

- `jq` and `curl` to `PATH`;
- with `EXEC_MODE=local` - readability `$JWT_FILE`;
- at `EXEC_MODE=tsh` - `tsh` at `PATH`, **fresh teleport-login** (`tsh status` != 0
→ "do `tsh login`"), resolve proxy host;
- if `$E2E_BRINGUP_STEPS` is non-empty - execution of each step in `$SCRIPTS_DIR`
**and** presence of pre-collected artifacts in `$ARTIFACTS_DIR`
(`soul-cloud-example-linux` / `soul-mod-redis` / `mod-manifest.yaml` - overridden
via `$E2E_ARTIFACTS`). The orchestrator **checks artifacts, but does not collect them** -
collect them in advance.

**Credits - only from env or from `/root/.env` on the VM (within local scripts).
NEVER from `~/.zsh_wb`.**

## Local toolkit `.pm/scripts/` and bring-up

The orchestrator (`scripts/e2e-cloud/`) is committed to git. **Cloud bring-up remains
local** - scripts `.pm/scripts/*` do not get into git (`.gitignore`): this
environment-specific (teleport paths, a fixed VM IP, a local secrets file).
The runner refers to them only in runtime by name from `$SCRIPTS_DIR`.

The operator declares its bring-up as an ordered list of names in `$E2E_BRINGUP_STEPS`.
`lib/bringup.sh` runs them one by one, the log of each step is in
`$LOG_DIR/<step>.log`, stop at first non-zero return code. Sequence
**not hardcoded** - the two keeper worlds require different sets. Below are the canonical ones.
lists as documentation (the scripts themselves are in local `.pm/scripts/`).

### Canonical bring-up lists

**Local-track** (`EXEC_MODE=local`, dev stand + Souls via reverse tunnel):

```
E2E_BRINGUP_STEPS="restore-after-reboot onboard-e2e distribute-plugin-e2e"
```

- `restore-after-reboot` — raise `/tmp/keeper-dev` (PG / Vault / Redis / keeper) after reboot;
- `onboard-e2e` — Soul onboarding via the reverse tunnel;
- `distribute-plugin-e2e` - Deliver `soul-mod-redis` to a locally connected Soul.

**Cloud-track** (`EXEC_MODE=tsh`, native-keeper on VM):

```
E2E_BRINGUP_STEPS="deploy-keeper deploy-service batch-onboard distribute-plugin autoprov-run poll-autoprov"
```

- `deploy-keeper` — roll out/restart keeper on the VM;
- `deploy-service` — upload service repo (`example-cloud-bootstrap`);
- `batch-onboard` - Souls fleet onboarding on VM;
- `distribute-plugin` - deliver `soul-mod-redis` + `soul-cloud-example`;
- `autoprov-run` / `poll-autoprov` - start cloud-provision and wait for the VM.

Both lists are examples. The order and composition are set by the operator; empty `$E2E_BRINGUP_STEPS`
= bring-up skipped (keeper is already ready).

## Launch

```bash
make e2e-cloud SUITE=create-destroy
# or directly:
bash scripts/e2e-cloud/runbook.sh <create|create-destroy|day2>
```

Dry run without network - `DRY_RUN=1`: prints the call sequence and generates
skeleton report on synthetic responses. It is useful to check the parameters and order
steps before the actual run.

```bash
DRY_RUN=1 make e2e-cloud SUITE=day2 SCENARIO=add_user SCENARIO_INPUT='{"name":"alice"}'
```

### Key parameters (env, defaults)

| Variable | Default | Destination |
|---|---|---|
| `EXEC_MODE` | `tsh` | `local` (direct curl) / `tsh` (curl on VM via teleport) |
| `KEEPER_API` | `http://127.0.0.1:8080` | endpoint for `EXEC_MODE=local` |
| `TSH_NODE` | `root@soul-keeper-1.$FQDN_SUFFIX` | teleport node VM (for `tsh`) |
| `FQDN_SUFFIX` | `ns.vm.example` | stand suffix FQDN |
| `TELEPORT_HOME` | `/mnt/c/Users/stf20/.tsh` | teleport-identity directory |
| `SSL_CERT_FILE` | (unset) | CA-bundle for teleport, if needed |
| `REMOTE_JWT` | `/opt/soul-stack/archon-cloud.jwt` | JWT operator **on VM** (`tsh`) |
| `REMOTE_KEEPER_API` | `http://localhost:8080` | endpoint keeper from inside the VM |
| `JWT_FILE` | `/tmp/keeper-dev/archon-alice.jwt` | JWT statement locally (`local`) |
| `AID` | `archon-alice` | operator in the report header |
| `INCARNATION` | `redis-auto` | incarnation name |
| `SERVICE` | `example-cloud-bootstrap` | service incarnations |
| `CREATE_SCENARIO` | `create` | create-script (empty = service default) |
| `PROVIDER` / `PROFILE` | `example-prod` / `redis-debian-12` | cloud provider / profile (report header) |
| `COVENS` | (empty) | covens on create (CSV → JSON array) |
| `E2E_CREATE_INPUT` | (empty) | `input`-JSON for create |
| `E2E_CREATE_MODE` | `engine` | `engine` (POST creates) / `script` (creates a bring-up, the engine catches the latest `apply_id` from `/runs`) |
| `SCENARIO` / `SCENARIO_INPUT` | (empty) | day-2: single script + its `input`-JSON |
| `SCENARIOS` | (empty) | day-2: `;`-list (`name` or `name::<json>`) |
| `STATE_ASSERT_PATH` / `STATE_ASSERT_EXPECTED` | (empty) | day-2: opt. assertion state fields after script |
| `ALLOW_DESTROY` | `true` | destroy without teardown (`allow_destroy=true`) |
| `HEALTHY_TERMINAL` | `ready` | healthy terminal `incarnation.status` |
| `SCRIPTS_DIR` | `.pm/scripts` | directory of local bring-up scripts |
| `ARTIFACTS_DIR` | `/opt/soul-stack` | catalog of pre-assembled artifacts |
| `E2E_BRINGUP_STEPS` | (empty) | ordered list of bring-up steps |
| `REPORT_DIR` | `.pm/e2e-reports` | report catalog |
| `LOG_DIR` | `$REPORT_DIR/logs` | bring-up step logs |
| `POLL_INTERVAL` / `POLL_MAX` | `30` / `40` | run poll: interval (s) and maximum iterations |
| `DRY_RUN` | `0` | `1` - Print calls without network |
| `INSECURE_TLS` | `0` | `1` — `curl -k` (self-signed at the stand) |

### Suites

- **`create`** — `[bring-up] → create → poll → assert`. `E2E_CREATE_MODE=engine`
sends `POST /v1/incarnations`; `=script` - creation is done by bring-up scripts, engine
picks up the last `apply_id` via `GET /runs?limit=1`. Then poll until
terminal, `assert_run_success`, secondary assertion `incarnation.status==ready`.
If `POST` returned 202 **without** `apply_id` (`lifecycle.auto_create:false` - bare
incarnation without running) - the step is marked SKIP and only the status `ready` is checked.
- **`create-destroy`** - idempotent full loop. **Pre-clean:** if incarnation
is already there and locked (`error_locked` / `migration_failed`) - `unlock` + destroy +
wait for disappearance; then `create`-suite; then `DELETE` and demolition confirmation.
**Re-run must be completed without manual intervention** - this is an acceptance criterion.
- **`day2`** — generic engine `run_scenario`: `POST /scenarios/{scenario}` → poll →
`assert_run_success`. Single script (`$SCENARIO` + `$SCENARIO_INPUT`) or
`;`-list (`$SCENARIOS`, element `name` or `name::<json>`). Script name -
any service-defined (`add_user` / `update_config` / `restart` / `rotate_tls` /
cluster-ops). Opt. after a single successful scenario - assertion of the state field
  (`$STATE_ASSERT_PATH` == `$STATE_ASSERT_EXPECTED`).

Example day-2:

```bash
EXEC_MODE=tsh INCARNATION=redis-auto \
  SCENARIO=add_user SCENARIO_INPUT='{"name":"alice","acl":"~* +@read"}' \
  STATE_ASSERT_PATH='.state.users[]?.name' STATE_ASSERT_EXPECTED=alice \
  make e2e-cloud SUITE=day2
```

## What assertit

**Run success - by apply_run, not by HTTP.** `POST` returns **202 Accepted** -
this is only "accepted", not "applied". The engine is polling
`GET /v1/incarnations/{name}/runs/{apply_id}` (`RunDetailReply`) to the terminal:

- Unit `.status` ∈ `applying | success | failed | cancelled` (source of truth -
  `keeper/internal/applyrun/applyrun.go`). `classify_status`:
`success → PASS`, `failed|cancelled → FAIL`, `applying → CONTINUE`. Unknown
status → `CONTINUE` (safe: will reach timeout, not falsely count success).
- **`assert_run_success` = `.status=="success"` And each `.hosts[].status` ∈
{`success`, `no_match`}.** `no_match` - benign terminal: script targeting
subset (e.g. `add_user` on master → replica `no_match`), considered success
- the same as the backend aggregate (`applyrun.AggregateRunStatus`). On fail in stderr
forensics of actually crashed hosts is printed: `failed_task_idx` /
  `failed_plan_index` / `error_summary`.

**Secondary assertion - `incarnation.status`.** `GET /v1/incarnations/{name}` →
`.status`; healthy terminal - **only `ready`** (enum from
`keeper/internal/api/huma_enums.go`: `applying | destroy_failed | destroying |
drift | error_locked | migration_failed | provisioning | ready`).

**Destroy** will assert on disappearance: `GET /{name}` → **404** (more reliable than status
teardown-run - covers `allow_destroy=true` without teardown).

**Day-2 state** (optional) - `assert_state_field` retrieves the jq path from `.state` and
compares with the expected (contains semantics for streams like `.state.users[]?.name`).

Confirmed Operator API routes (all under `/v1`, `Authorization: Bearer <jwt>`):

| Operation | Route |
|---|---|
| create | `POST /v1/incarnations` → 202 `IncarnationCreateReply` |
| day-2 (generic) | `POST /v1/incarnations/{name}/scenarios/{scenario}` → 202 `IncarnationRunReply` |
| destroy | `DELETE /v1/incarnations/{name}?allow_destroy=<bool>` → 202 `IncarnationDestroyReply` |
| unlock | `POST /v1/incarnations/{name}/unlock` → 200 |
| run status | `GET /v1/incarnations/{name}/runs/{apply_id}` → `RunDetailReply` |
| list of runs | `GET /v1/incarnations/{name}/runs` → `RunSummaryEntry[]` |
| get / state | `GET /v1/incarnations/{name}` → `.state` / `.status` / `.status_details` |

## Report

The report is written **incrementally** to `.pm/e2e-reports/<date>-<suite>.md`
(`<date>` = UTC `YYYY-MM-DD`; gitignored directory). Each line is added at once
- during an abortion, the report is not empty. Logs of bring-up steps are located next to `logs/<step>.log`.

Structure:

- **Environment header** — suite, exec_mode (+ DRY-RUN mark), endpoint,
  incarnation / service, provider / profile, canon (short-SHA core), operator (aid),
  bring-up steps.
- **Table "Steps"** - columns:
`# | step / scenario | apply_id | start (UTC) | duration_s | http | run_status | assert | result`.
The result of each step is `PASS` / `FAIL` / `SKIP`.
- **Summary** - PASS / FAIL / SKIP, RESULT, exit-code counters.

How to read: `apply_id` from line → `GET /runs/{apply_id}` for full forensics;
`run_status` = run unit; column `assert` = assertion verdict (`run=success` /
`hosts!=success` / `got!=ready` / `incarnation removed (404)` / …).

**Exit codes** (the same ones that end `runbook.sh`):

| Code | Meaning |
|---|---|
| `0` | all steps PASS |
| `1` | assertion or run failure (`failed` / `cancelled` / host not success / poll timeout) |
| `2` | preflight or infrastructure failure (no `jq`/`tsh`, teleport failed, no artifact, unknown suite) |

## Troubleshooting

- **`apply_id` of 202 is empty.** Norm for `lifecycle.auto_create:false` (bare
incarnation - create-suite marks SKIP and checks only `ready`) and for
`E2E_CREATE_MODE=script` (creates a script → the engine takes the latest from
`GET /runs?limit=1`). If you were waiting for a run, but it didn't come, check that the service is working at all
runs the create script.
- **202, but the run is "not visible" when polling (http != 200).** `GET /runs/{apply_id}`
may return non-200 for some time until the run materializes - the engine is
endures and interrogates further until `POLL_MAX`. **202 ≠ success**: no successful poll
step is not counted.
- **Style path to artifacts.** Single `$ARTIFACTS_DIR` (`/opt/soul-stack`) for all
artifacts; don't put it in different directories - preflight checks it.
Artifacts are **pre-assembled**, the orchestrator does not assemble them.
- **Two JWT / CA are mixed up.** `local`-world and `tsh`-world are different keepers with different
JWT and CA. The `archon-alice.jwt` token will not be suitable for the cloud keeper and vice versa -
401/403 usually means "took JWT from the wrong world."
- **`no_match`-hosts in the report.** Not an error: script for a subset (master-only)
leaves the remaining hosts `no_match`, the assertion considers this a success (as a backend).
FAIL gives only the real `failed` / `cancelled` host - its forensics in stderr.
- **`tsh status` expired → exit 2.** Teleport-identity expires; preflight catches it
before touching the cloud. Update `tsh login` and restart.
- **Repeated `create-destroy` trips over a stuck incarnation.** Shouldn't:
pre-clean removes `error_locked` / `migration_failed` via `unlock` and demolishes
remainder before create. If it's still stuck, remove it manually using the recipe from
applying-zombie-cleanup, then restart.

## See also

- [testing/README.md](README.md) - index of testing levels (L0–L4) and boundaries
local live gate.
- [testing/e2e.md](e2e.md) - regulatory spec L3a Go-harness (keeper↔soul contract).
- [ADR-039](../adr/0039-e2e-testing.md)
- E2E three levels + amendment about the cloud orchestrator.
