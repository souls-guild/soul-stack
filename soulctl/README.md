# soulctl

The Soul Stack operator client CLI — a thin wrapper over the Keeper's Operator API
(paired with the `soul` agent, like `kubectl` ↔ `kubelet`). Per
[ADR-004](../docs/adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper)
the primary operator interface is OpenAPI and MCP; the CLI is acceptable as a thin
wrapper over OpenAPI, not as a separate behavioral contract.

API contract — [docs/keeper/operator-api.md](../docs/keeper/operator-api.md) and
[docs/keeper/openapi.yaml](../docs/keeper/openapi.yaml).

## Commands

Command tree — seven top-level groups. Each command is a thin wrapper over the
Operator API; links to endpoints — [operator-api.md](../docs/keeper/operator-api.md).

### `incarnation` — runtime instances of services

| Command | Purpose | Flags |
|---|---|---|
| `incarnation list` | list incarnations | `--service`, `--status`, `--coven` (client-side), `--limit`, `--offset` |
| `incarnation get <name>` | show an incarnation (spec/state/status/covens), always JSON | — |
| `incarnation run <name> <scenario>` | run a scenario on an incarnation | `--input <json>`, `--dry-run`, `--wait`, `--wait-timeout` (default `5m`) |
| `incarnation history <name>` | `state_history` entries | `--limit`, `--offset` |
| `incarnation check-drift <name>` | Scry drift check (ADR-031) | `--input <json>` (override converge-input) |

`--wait` on `run` polls the incarnation's `history` + `status` (there's no separate
`/v1/applies/{apply_id}` in MVP), fail-fast on `error_locked` /
`migration_failed` / `destroy_failed`.

### `souls` — registry of managed agents (plural)

| Command | Purpose | Flags |
|---|---|---|
| `souls list` | list registered Souls | `--coven` (repeatable), `--status`, `--transport` (`agent\|ssh`), `--limit`, `--offset` |
| `souls get <sid>` | show a Soul by SID (fallback via list — `soul.get` isn't exposed in MVP), JSON | — |
| `souls ssh-target set <sid>` | set per-host `ssh_target` push flow ↔ `PUT /v1/souls/{sid}/ssh-target` | `--port` (default `22`), `--user` (default `root`), `--soul-path` (default `/usr/local/bin/soul`), `--ssh-provider` |
| `souls ssh-target bulk-set` | bulk-set `ssh_provider` for all Souls in a Coven (client-side list→per-SID PUT) | `--coven` (required), `--ssh-provider` (required), `--port`, `--user`, `--soul-path` |

### `soul` — single actions on one specific host (singular)

Deliberately separate from `souls`: `souls` is the registry (list/get), `soul` is an action on one host.

| Command | Purpose | Flags |
|---|---|---|
| `soul exec <sid>` | ad-hoc single module on a Soul (Errand, ADR-033) ↔ `POST /v1/souls/{sid}/exec` | `--module` (required), `--input <json>` (default `{}`), `--timeout` (sec, default `30`, 1..300), `--dry-run`, `--poll` (default `true`) |

The module whitelist and stdout/stderr caps are enforced by the Soul-side errand runner.
`--poll` drives the async result to completion (when the server-side cap is exceeded) via
`errand get` until it reaches a terminal state.

### `errand` — Errand registry (ADR-033)

| Command | Purpose | Flags |
|---|---|---|
| `errand list` | list Errands | `--sid`, `--status`, `--started-after <RFC3339>`, `--limit`, `--offset` |
| `errand get <errand_id>` | show an Errand's state | `--poll` (default `false`) — drive running to a terminal state |
| `errand cancel <errand_id>` | cancel an in-flight Errand (permission `errand.cancel`) | — |

### `archon` — operator authentication and identity

| Command | Purpose | Flags |
|---|---|---|
| `archon login` | save keeper_url + JWT into credentials.yaml (validated with a ping) | `--keeper-url` (required), `--jwt-file` (required) |
| `archon whoami` | show the current Archon (AID + claims from the JWT) | — |
| `archon logout` | delete credentials.yaml | — |

Details — the "Authentication" section below.

### `push-providers` (alias `push-provider`) — push-flow SSH plugin params

Replaces the inline form `keeper.yml::push.providers[]` (ADR-032 amendment).
Sensitive params (`secret_id`/`token`/`password`/`private_key`) must be
vault refs (`vault:<path>`).

| Command | Purpose | Flags |
|---|---|---|
| `push-providers create <name>` | create an entry (permission `push-provider.create`) | `--params <json>` |
| `push-providers update <name>` | replace params (replace semantics) | `--params <json>` (required) |
| `push-providers delete <name>` | delete an entry | — |
| `push-providers list` | list, JSON | `--name-pattern` (LIKE form, e.g. `vault%`), `--limit` (default `100`), `--offset` |
| `push-providers get <name>` | read an entry, JSON | — |

### `run` — high-level UX umbrella (Salt-parity)

Coexists with `incarnation run` / `soul exec` with no deprecation: `run` is the
operator-facing frontend, the low-level direct commands remain for CI/scripts.

| Command | Purpose | Backend |
|---|---|---|
| `run scenario <service>/<scenario>` | batched scenario via Voyage | `POST /v1/voyages` (`kind=scenario`, ADR-043) |
| `run cmd '<command>'` | ad-hoc shell command on N hosts | `POST /v1/voyages` (`kind=command`, ADR-043) |
| `run push <destiny@ref>` | push-apply a destiny via an SSH provider | `POST /v1/push/apply` |

Per-command flags:

- `run scenario`: `--incarnation` (if not set — auto-detect: exactly one
  incarnation per service), `--input <json>`, `--batch-size`, `--batch N|N%`,
  `--max-failures N|N%`, `--concurrency` (0→default 50, max 500),
  `--on-failure` (`continue`\|`abort`), `--wait`, `--wait-timeout` (default `10m`).
  Target flags don't apply to a scenario (the target is an incarnation, not a host) —
  passing `--target-*` here is an error.
- `run cmd`: target is required; `--module` (default `core.cmd.shell`),
  `--concurrency`, `--on-failure`, `--batch-size`, `--batch N|N%`,
  `--max-failures N|N%`, `--wait`, `--wait-timeout` (default `10m`).
- `run push`: `--ssh-provider` (empty → server default), `--input <json>`,
  `--cleanup-stale-versions`. Target is limited to `--target-sids` (inventory
  exact-match); `coven`/`glob`/`regex`/`where` aren't available for push — a
  CLI validation error.

Universal `--target-*` flags (`run cmd`/`run push`):

| Flag | Semantics |
|---|---|
| `--target-sids host1,host2` | CSV of exact-match SIDs |
| `--target-coven prod-eu,dc1` | CSV of Coven labels (AND over `souls.coven`) |
| `--target-glob 'web-*'` | shell glob → CEL `sid.glob("X")` |
| `--target-regex 'host-[0-9]+'` | regex → CEL `sid.matches("X")` |
| `--target-where '<CEL>'` | raw CEL predicate; AND-merged with glob/regex |

`glob`/`regex`/`where` are joined into one final `where` via `&&`; `sids`
and `coven` remain separate fields (the backend does an AND intersection —
invocation narrows the scope, never widens it).

## Global flags

| Flag | Purpose |
|---|---|
| `--output / -o table\|json\|yaml` | Output format. `table` (default) — kubectl-style via `text/tabwriter`. `json` — pretty-JSON. `yaml` is reserved, currently matches `json`. Some commands (`get` forms, `push-providers list`) always print JSON. |
| `--config <path>` | Path to credentials.yaml instead of `~/.config/soul-stack/credentials.yaml`. |
| `--version` | Binary version (injected via `-ldflags`, see [RELEASING.md](../RELEASING.md)). |

## Authentication

```yaml
# ~/.config/soul-stack/credentials.yaml — mode 0600
keeper_url: https://keeper.example.com:8443
archon_jwt: <JWT>
```

- `soulctl archon login --keeper-url <url> --jwt-file <path>` — reads the JWT from
  a file, validates it via `GET /v1/incarnations?limit=1` (any authorized
  endpoint), saves credentials.
- `soulctl archon whoami` — prints the AID + claims from the local JWT
  (signature isn't re-checked — Keeper already accepted the JWT at login).
- `soulctl archon logout` — deletes credentials.yaml.

## Errors

| HTTP | CLI message |
|---|---|
| 401 | `not authenticated. Run `soulctl archon login`` |
| 403 | `forbidden: <detail from RFC 7807>` |
| 404 | `not found: <detail>` |
| 5xx | `keeper error: <detail>` |

`--output json` for list commands on error returns a non-zero exit code and standard
ProblemDetails on stderr (not empty JSON), so scripts don't get confused.

## Build

```sh
make build-soulctl   # → soulctl/bin/soulctl
make build           # includes soulctl
make test            # unit tests
```

## Known limitations (TODO)

These are gaps between the openapi MVP and CLI needs. They're not worked around with
hacks; they get raised with the PM when a relevant ADR / contract extension appears.

- **`/v1/whoami` is missing.** The Operator API MVP has no separate whoami; AID
  and roles are extracted from JWT claims. The signature isn't verified locally (Keeper
  already validated the JWT at login via the ping).
- **`/v1/applies/{apply_id}` is missing** ([operator-api.md → Async operations](../docs/keeper/operator-api.md)).
  `incarnation run --wait` polls `GET /v1/incarnations/{name}/history` (an entry with
  apply_id appears after a successful commit) and `GET /v1/incarnations/{name}`
  (incarnation status, fail-fast on error_locked/migration_failed/destroy_failed).
- **`GET /v1/souls/{sid}` is missing** in MVP (no `soul.get` permission,
  [operator-api.md → ID in path](../docs/keeper/operator-api.md)). `soulctl souls
  get <sid>` uses a list fallback + client-side filter. Large clusters
  (≥10⁴ hosts) are a candidate for a dedicated endpoint once the permission exists.
- **The `coven` filter on incarnation list is client-side.** In openapi,
  `/v1/incarnations` has no `coven` query parameter (only `/v1/souls` does).
  The `total` field in the response reflects the server's service/status filter, not the
  client-side coven filter. An openapi extension if this becomes necessary.
- **history command: STATUS/DURATION are empty.** `state_history` entries
  only exist on a successful commit, so two columns in the table are empty.
  If an `apply_runs` endpoint with the full lifecycle shows up in openapi, they'll be filled in.

## Package structure

```
soulctl/
  cmd/soulctl/main.go               # entry, version-bind
  internal/
    cmd/                            # cobra commands (root + seven groups)
      root.go                       # global flags, loadClient, renderAPIError
      archon.go                     # archon login / whoami / logout
      incarnation.go                # incarnation list / get / run / history / check-drift
      souls.go                      # souls list / get / ssh-target + soul exec
      errand.go                     # errand list / get / cancel
      pushprovider.go               # push-providers create / update / delete / list / get
      run.go                        # run umbrella (root)
      run_scenario.go               # run scenario
      run_cmd.go                    # run cmd
      run_push.go                   # run push
      run_target.go                 # shared --target-* flags
    client/                         # typed HTTP client
    config/config.go                # credentials.yaml loader
    output/output.go                # table / json renderers
```
