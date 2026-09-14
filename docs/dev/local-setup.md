# Local dev stack

Local infrastructure for development and interactive debugging of Keeper.
Raised via docker-compose, persistent volume. For automated
integration tests use a separate mechanism - testcontainers-go
(see [Integration tests](#integration-tests) below), he raises his
ephemeral per-package container, not intersecting with `dev-up`.

## What rises

| Component | Destination | ADR |
|---|---|---|
| **postgres:16-alpine** | Keeper cold storage (`audit_log`, hereinafter `souls`/`operators`/`incarnation`). | [ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres) |
| **hashicorp/vault:1.18** | Vault in dev mode for reading JWT signing key (`secret/keeper/jwt-signing-key`, ADR-014) and other KVs. Root token = `root`. | [ADR-014](../adr/0014-operator-identity.md), [ADR-017](../adr/0017-keeper-side-core.md) |
| **redis:7-alpine** | Reaper-lease, SoulLease, Outbound pub/sub between Keeper instances. No password in dev. | [ADR-006](../adr/0006-cache-redis.md) |
| **otel/opentelemetry-collector-contrib** | Receiving OTLP gRPC (`:4317`) traces from keeper/soul → export to Jaeger + debug log. The pipeline config is [`dev/otel-collector.yaml`](../../dev/otel-collector.yaml). | [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) |
| **jaegertracing/all-in-one** | Storage + UI traces (`:16686`, in-memory storage). Receives from the OTLP collector inside the docker network. | [ADR-024](../adr/0024-observability.md#adr-024-observability-prometheus-primary--otel-bridge) |

Backlog for the following slices: Vault PKI (Keeper-side issuance mTLS,
separate mount from `secret/`), `audit.otel_export`
([ADR-022](../adr/0022-audit-pipeline.md#adr-022-audit-pipeline-storage-schema-retention)).

## Teams

| Team | What does |
|---|---|
| `make dev-up` | `docker compose up -d` to `dev/`. persist data in named volume `postgres_data`. |
| `make dev-stop` | Stops local `keeper run`/`soul run` dev-workflow daemons (foreground processes from [Smoke recipe](#smoke-recipe-e2e)). Pidfile is not written - matches `pkill -f` with a specific dev-config pattern (`keeper.dev.yml`/`soul.dev.yml`), does not affect other people's keeper/soul; does not crash if there are no processes. |
| `make dev-down` | `make dev-stop` (extinguishes local daemons) → `docker compose down` without `-v` - the container stops, the data is saved. |
| `make dev-reset` | `docker compose down -v && docker compose up -d` - full reset with data loss. |
| `make dev-provision` | Idempotent bootstrap: Vault KV (`secret/keeper/postgres`, `secret/keeper/jwt-signing-key`) + Vault PKI (`pki/` engine, root cert, role `soul-seed`) + TLS stuff from Vault PKI to `/tmp/keeper-dev/tls/` + directories `plugins/`, `plugin-sockets/` + **git repo service/destiny-artifacts** from `examples/` under file://-URLs from `keeper.dev.yml` (see Service/destiny artifacts). The script is [`dev/provision.sh`](../../dev/provision.sh), safe to run again. |
| `make dev-smoke` | Full cycle: `dev-up` → `dev-provision` → assemble `keeper` → `keeper init --archon=archon-alice`. Operator JWT file → `/tmp/keeper-dev/archon-alice.jwt`. A second run requires `make dev-reset && make dev-smoke` (operators registry is no longer empty). |
| `make dev-keeper` | Restarting keeper in the background with a FULL dev-env (see Background dev-daemons): extinguishes the old process using the `keeper.dev.yml` pattern, clears leader-leases in Redis (`conductor:leader`/`reaper:leader`), creates cache directories, exposes `VAULT_ADDR`/`VAULT_TOKEN`/`KEEPER_SERVICE_CACHE_DIR`/`KEEPER_DESTINY_CACHE_DIR`/`SOUL_STACK_ALLOW_FILE_REPOS=1`, picks up `nohup keeper run` and waits for healthz 200 on `:8080`. **Always rebuilds the binary** (`make build-keeper`, with the version stamp) and refuses when healthz reports a version other than the one just built — a foreign keeper on the port is named, not reported as ready (see Which code is this stand running?). No TLS - prompts `dev-provision`. Log → `/tmp/keeper-dev/keeper.log`. Script - [`dev/keeper-run.sh`](../../dev/keeper-run.sh). |
| `make dev-jwt [AID=… ROLES=… TTL=…]` | Prints to stdout HS256-JWT Archon for ad-hoc API calls **without** `keeper init`. The key is taken from the same Vault KV as keeper (`secret/keeper/jwt-signing-key`, field `signing_key`, base64-decode), `iss=keeper-dev-01`. Defaults: `AID=archon-alice`, `ROLES='["cluster-admin"]'`, `TTL=43200` (12h). Only the token in stdout (service - in stderr) → `TOKEN=$(make dev-jwt)`. Requires python3 + raised Vault. Script - [`dev/mint-jwt.sh`](../../dev/mint-jwt.sh). |
| `make dev-souls` | Re-raises the local fleet of souls according to the database registry (`SELECT sid FROM souls`): for each sid writes per-sid `soul.yml` (if not), onboards (`issue-token?force=true` → `soul init`) ONLY in the absence of a valid seed (three files `cert/key/ca.pem` by `seed/current`), (re)launch `soul run`. Covens are saved in the database - they do NOT register again. At the end it prints `SELECT status, count(*) FROM souls`. Repairs "all souls disconnected". Script - [`dev/souls-up.sh`](../../dev/souls-up.sh). |
| `make dev-web [WEB_DIR=…]` | Vite dev-server companion-repo (`WEB_DIR`, default `../soul-stack-web`) with the required `--host` - otherwise vite binds only to IPv6 `[::1]` and `http://127.0.0.1:5173` fails. Extinguishes the old vite of this repo, raises `nohup npm run dev -- --host`, waits for 200 on `:5173`. Log → `/tmp/keeper-dev/web-dev.log`. Script - [`dev/web-run.sh`](../../dev/web-run.sh). |
| `make dev-stand` | Full rise of the stand with one command: `dev-provision` → `dev-keeper` → `dev-souls` → `dev-web` + address summary and reminder about `make dev-jwt`. Use after restart/change of day (see Quick stand recovery). |

> **Two UI: dev vite server vs embed on the keeper - do not confuse it.** UI with [ADR-055](../adr/0055-embed-ui-bundle.md) is built into the `keeper` binary (`go:embed`) and in prod/beta it is given by the keeper itself on **`http://<keeper>:8080/ui`** (toggle [`web_ui_enabled`](../keeper/config.md#web_ui_enabled-top-level), default-ON). For **front development** `make dev-web` raises a live vite dev server from HMR to **`http://127.0.0.1:5173/ui/`** (separate process, hot-reload sources from companion `soul-stack-web`) - this is the "real" UI when working on the front. Embed on `:8080/ui` shows a **vendored snapshot** (`keeper/internal/webui/assets/`, updated by `make sync-webui`) - it may lag behind the companion source until the snapshot is re-synced. That is: you correct the front and look at `:5173/ui/`; "as the beta user sees" you check for `:8080/ui` after `make sync-webui`. More information about vendoring - [docs/web/README.md](../web/README.md).

## Connection details

Ports are chosen so as not to conflict with typical user
docker-stacks (`agent-platform-postgres:5432`, `agent-platform-valkey:6380`,
`dba-salt-redis:6379`). If `5434`/`6381`/`8200` is busy - see
[Troubleshooting](#troubleshooting).

### Postgres

| Parameter | Meaning |
|---|---|
| Host / port | `127.0.0.1:5434` |
| Database | `keeper` |
| User / password | `keeper` / `keeper` |
| DSN | `postgres://keeper:keeper@127.0.0.1:5434/keeper?sslmode=disable` |

### Vault

| Parameter | Meaning |
|---|---|
| Host / port | `127.0.0.1:8200` |
| UI | http://127.0.0.1:8200/ui |
| Root token | `root` |
| KV mount | `secret` (v2, activated automatically in dev mode) |
| Vault address for CLI | `export VAULT_ADDR=http://127.0.0.1:8200` |
| Vault token for CLI | `export VAULT_TOKEN=root` |

### Redis

| Parameter | Meaning |
|---|---|
| Host / port | `127.0.0.1:6381` |
| Password | empty (dev) |
| URL for CLI | `redis-cli -h 127.0.0.1 -p 6381 ping` |

### OTel stack (traces)

| Parameter | Meaning |
|---|---|
| OTLP gRPC (reception from keeper/soul) | `127.0.0.1:4317` (insecure, no TLS) |
| Jaeger UI | http://127.0.0.1:16686 |
| collector config | [`dev/otel-collector.yaml`](../../dev/otel-collector.yaml) |

dev-configs keeper and soul already point to the collector: `otel.enabled: true`,
`endpoint: 127.0.0.1:4317` ([`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) /
[`dev/soul.dev.yml`](../../dev/soul.dev.yml)).

**View traces:**

1. Raise the stack (`make dev-up`) - Collector and Jaeger start with PG/Vault/Redis.
2. Run keeper/soul through dev configs (see [Smoke recipe](#smoke-recipe-e2e)).
3. Open Jaeger UI http://127.0.0.1:16686, select service `keeper` or `soul`,
click **Find Traces**. The through route operator → Keeper → Soul is visible as one
trace (trace-context goes to `ApplyRequest.trace_context`, [observability.md §4](../observability.md)).
4. Without UI - `docker compose -f dev/docker-compose.yml logs -f otel-collector`
prints accepted spans (debug-exporter).

> **Prod - not this stack.** all-in-one Jaeger stores traces in-memory (lost
> upon restart) and accepts OTLP without TLS - for local only. Prod-`keeper.yml`
> ([`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml)) leaves
> `otel:`-block configurable (endpoint of real collector + TLS),
> dev endpoint is not hardcoded there.

Bootstrap provisioning of secrets for local-dev (ADR-014/M0.5b/M0.5d) - after
`make dev-up` with one command:

```sh
make dev-provision
```

The script [`dev/provision.sh`](../../dev/provision.sh) is idempotent itself
does the following steps (same as done manually before):

```sh
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=root

# JWT signing-key (auth.jwt.signing_key_ref → secret/keeper/jwt-signing-key, field `signing_key`)
vault kv put secret/keeper/jwt-signing-key signing_key="$(openssl rand -base64 32)"

# Postgres DSN (postgres.dsn_ref → secret/keeper/postgres, field `dsn`)
vault kv put secret/keeper/postgres \
  dsn="postgres://keeper:keeper@127.0.0.1:5434/keeper?sslmode=disable"
```

If there is no `vault`-CLI on the host, the script transparently proxies commands
via `docker exec soul-stack-vault vault ...`.

After `make dev-down`/`dev-reset` run `make dev-provision` again -
dev-mode Vault stores secrets only in RAM.

Vault links `vault:secret/...` to `keeper.yml` are resolved at the start of the binary
keeper (M0.5b — `auth.jwt.signing_key_ref`, M0.5d — `postgres.dsn_ref`).
Convention of field name inside KV: short (`signing_key` / `dsn`), documented
in [docs/keeper/config.md](../keeper/config.md). Other `_ref` fields
(`redis.password_ref`, AppRole credentials) - postponed to the next slices.

### When Vault forgets and Postgres does not (`DEV_VAULT_REISSUE_ANCHORS`)

The dev Vault holds its secrets in RAM; Postgres holds its data on a named
volume. So restarting the stack does not reset the stand — it *desynchronises*
it. The registries come back holding rows that name trust anchors which no
longer exist, and `dev-provision` being idempotent means it would cheerfully
mint fresh ones over them. Nothing errors out, everything reports ready, and
the stand you get is not the one you left.

What the mismatch costs, per anchor:

| Anchor gone | Registry that survives | What breaks |
|---|---|---|
| `secret/keeper[/<slug>]/jwt-signing-key` | `operators` | Every JWT ever issued to an Archon fails signature verification — 401, including `/tmp/keeper-dev/archon-alice.jwt`. The rights in the registry are untouched; only the signature no longer matches. |
| `pki/` root | `soul_seeds` | The seeds chain to a CA that is not there: mTLS fails and every soul has to be re-onboarded. |
| `secret/keeper[/<slug>]/sigil-signing-key` | `plugin_sigils` | The grants were signed by an anchor that no longer exists, so no plugin they cover can be admitted ([ADR-026](../adr/0026-sigil.md)). |

`dev-provision` checks this before it generates any anchor (step 1b) and
refuses, printing exactly which rows are at stake and how to get out. (Step 1,
the Postgres DSN, is written first, but it is an additive idempotent write and
not an anchor.) Note the `pki/` row: the KV prefix is per-stand
(`secret/keeper/<slug>/`, see Parallel stands), but `pki/` is **one engine per
Vault** — every lightweight stand shares one Vault and therefore one root, so
regenerating it breaks the neighbours' souls as well as yours. That is why the
guard counts `soul_seeds` across every `keeper*` database and, when a neighbour
is among the losers, stops offering the drop-your-own-database way out. (A
stand with `DEDICATED_INFRA=1` *and* a slug runs its own Vault, so there the
only neighbour is itself.)

Ways out, narrowest blast radius first:

```sh
# 1. named stand only — drop this stand's database, nobody else is touched.
#    Stop this stand's keeper first. Postgres refuses to drop a database that
#    still has a session on it ("is being accessed by other users"), and a
#    restarted Vault leaves the keeper running — which is exactly the state you
#    are in when the guard fires.
#    The container is named after the compose project: soul-stack-postgres on the
#    shared one, soul-stack-<slug>-postgres for a stand with DEDICATED_INFRA=1
#    *and* a slug. provision prints the name that is true for you.
docker exec -i soul-stack-postgres psql -U keeper -d keeper \
  -c 'DROP DATABASE "keeper_<slug>"' && DEV_STAND=<slug> make dev-provision

# 2. accept the loss on purpose, keeping every database
DEV_VAULT_REISSUE_ANCHORS=1 make dev-provision
#    then re-mint a token (AID=archon-alice dev/mint-jwt.sh) and re-onboard the souls

# 3. make dev-reset — WIPES THE SHARED POSTGRES VOLUME: every stand that shares
#    it, not only yours. Check who else is running first.
#    A stand with DEDICATED_INFRA=1 *and* DEV_STAND set owns its compose project,
#    so there this is the cheapest option rather than the most expensive: `down -v`
#    reaches its own volumes and nobody else's. DEDICATED_INFRA=1 with no slug
#    still lands on the shared soul-stack-* containers. provision prints
#    whichever is true for you.
```

A first-ever stand is unaffected **on a machine with no neighbours**: with every
registry empty there is nothing to lose, so provision says so
(`vault trust anchors missing, but no registry depends on them yet - generating`)
and goes ahead, exactly as before the guard existed. On a shared host a brand-new stand can still be refused, and
that is the guard working rather than a bug. `soul_seeds` is counted across every
`keeper*` database because `pki/` is one engine per Vault, so after a
`docker restart <prefix>-vault` on a host where somebody else already has souls,
the next `DEV_STAND=fresh make dev-provision` stops: this stand's own registries
are empty, but the root their seeds chain to is gone, and the root provision
would generate here is that same one. The refusal then says to ask whoever runs
the other stand instead of offering to drop a database — dropping yours would
not put their root back. Expect this on the first contact with the guard, since
standing up a new stand is what most people do first. If Postgres is unreachable
the guard cannot tell a first-ever stand from a wiped Vault, so it warns and
generates rather than blocking a machine that has nothing on it yet.

## Parallel stands (`DEV_STAND`)

By default, all dev targets work with **one** stand on fixed ports
(`8080`/`8081`/`9090`/`9442`/`9443`, web `5173`) and directory `/tmp/keeper-dev/`.
While the stand is busy (the validating rule requires a **live** local stand - keeper
raised, data seeded), it is impossible to take the second ticket/feature into work: port collision,
DB and dev directory. Mechanism `DEV_STAND` (NIM-25) places **2–4 stands** in parallel
- each has its own ports, database and Vault prefix on top of common infrastructure containers.

### How to set a stand

The stand is selected by the environment variable `DEV_STAND=<slug>` before any dev target
(`slug` - ticket/feature, e.g. `nim30`; validation `^[a-z0-9][a-z0-9-]{0,30}$`):

- **Empty (`DEV_STAND` not specified)** - default stand, everything is byte-by-byte as
historically: slot `0`, offset `0`, directory `/tmp/keeper-dev`, ports `8080…`, database
`keeper`, KV `secret/keeper`. No existing command is changing behavior.
- **Non-empty** - second+ stand: own catalog `/tmp/keeper-dev-<slug>`, DB
`keeper_<slug>`, Vault prefix `secret/keeper/<slug>/`, custom ports.

**Slot** (`1..3`) is allocated automatically from the registry file
`/tmp/soul-stack-stands.tsv` (lines `slug<TAB>slot`, read-modify-write under `flock`
- ​​parallel first launch of different slugs will not take one slot). Slug slot
is reused between runs until released (see
Release slot). Override - `DEV_STAND_SLOT=<1..3>`.
Offset of all stand ports = `slot × 10`.

The first run of any stand-aware target prints a stand summary:

```
[stand] slug=nim30 slot=1 offset=10 dir=/tmp/keeper-dev-nim30 dedicated=0
[stand] kid=keeper-dev-nim30 pg_db=keeper_nim30 kv=secret/keeper/nim30 stack=soul-stack
[stand] ports: openapi=8090 mcp=8091 metrics=9100 bootstrap=9452 es=9453 web=5183 soul-metrics=9201
```

Single source of derived variables - sourced helper
[`dev/stand-env.sh`](../../dev/stand-env.sh) (read by each `make dev-*` target and
`dev/*.sh`-script; does not run directly).

### Port offset

All bench ports = base port `+ slot×10`. Base (slot 0) - historical values:

| Port | Variable | slot 0 (default) | slot 1 | slot 2 | slot 3 |
|---|---|---|---|---|---|
| OpenAPI | `OPENAPI_PORT` | `8080` | `8090` | `8100` | `8110` |
| MCP | `MCP_PORT` | `8081` | `8091` | `8101` | `8111` |
| Metrics | `METRICS_PORT` | `9090` | `9100` | `9110` | `9120` |
| Bootstrap (gRPC) | `BOOTSTRAP_PORT` | `9442` | `9452` | `9462` | `9472` |
| Event stream (gRPC) | `ES_PORT` | `9443` | `9453` | `9463` | `9473` |
| Web (vite) | `WEB_PORT` | `5173` | `5183` | `5193` | `5203` |
| Soul-metrics | `SOUL_METRICS_PORT` | `9191` | `9201` | `9211` | `9221` |

Example - stand `nim30` (slot 1): keeper on `http://127.0.0.1:8090`, MCP `:8091`,
web `http://127.0.0.1:5183/ui/`, soul-metrics `:9201`.

### What is divorced, what is general (easy mode, default)

Easy mode (`DEDICATED_INFRA=0`, default) only disables **free**,
reusing one set of containers:

| Resource | Default Stand | Stand `<slug>` |
|---|---|---|
| Directory of dev-material | `/tmp/keeper-dev` | `/tmp/keeper-dev-<slug>` |
| DB (Generally PG) | `keeper` | `keeper_<slug>` |
| Vault KV-prefix (generally Vault) | `secret/keeper/` | `secret/keeper/<slug>/` |
| KID keeper | `keeper-dev-01` | `keeper-dev-<slug>` |
| Reference soul SID | `web-01.example.com` | `web-01.<slug>.example.com` |
| Ports keeper/web/soul | `8080…` | `+ slot×10` |
| PG/Vault/Redis Containers | general kit `soul-stack-*` | the same general set |

PG, Vault and Redis containers are **shared**. This is harmless for PG and Vault: different databases
(`keeper_<slug>`) and KV prefixes (`secret/keeper/<slug>/`) completely isolate data
stands. **Redis is generic and NOT bred** in easy mode, and this is the border:

> **Light mode boundary: shared Redis.** Background keeper coordination lives in
> Redis: Conductor/Reaper leadership (single keys `conductor:leader`/`reaper:leader`),
> presence-registry Conclave, SoulLease, pub/sub between instances (terms in
> [name dictionary](../naming-rules.md)). On the general Redis this background life is rummaging around
> between stands**: the leader of Conductor/Reaper becomes the keeper of only one stand,
> and Conclave considers keepers of different stands to be instances of the same cluster. For
> UI work, data browsing and manual API calls are fine. But **parallel
> apply runs** and **HA/failover demo** (two keeper, soul-shedding) in easy mode
> **not supported** - for them take `DEDICATED_INFRA=1`.

### Full isolation: `DEDICATED_INFRA=1`

`DEDICATED_INFRA=1` gives the stand **its own set of containers** - a complete solution,
including Redis:

- separate `docker compose`-project `COMPOSE_PROJECT_NAME=soul-stack-<slug>` (own
containers, volumes, network); `dev-up`/`dev-down`/`dev-reset` they beat **him**;
- infra-ports are also shifted by `slot×10` (in easy mode - general, without shift):

| Infra-port | default (general) | dedicated slot 1 |
|---|---|---|
| Postgres | `5434` | `5444` |
| Vault | `8200` | `8210` |
| Redis | `6381` | `6391` |
| OTLP gRPC | `4317` | `4327` |
| Jaeger UI | `16686` | `16696` |

For a dedicated stand, `make dev-up` is required - it raises its own compose project. B
in easy mode, the second stand reuses the already raised common infrastructure, `dev-up` for it
not needed.

### Recipes

```sh
# Default stand (as historically) - total infra goes up once.
make dev-up dev-provision dev-keeper dev-web

# Second nim30 stand in parallel (easy mode, general infra is already raised):
DEV_STAND=nim30 make dev-provision \
  && DEV_STAND=nim30 make dev-keeper \
  && DEV_STAND=nim30 make dev-web \
  && DEV_STAND=nim30 make dev-souls-docker

# Completely isolated stand (its own containers, its own Redis - for apply/HA demo):
DEV_STAND=big DEDICATED_INFRA=1 make dev-up dev-provision dev-keeper

# Demolition of nim30 stand (souls → demons → slot free):
DEV_STAND=nim30 make dev-souls-docker-down \
  && DEV_STAND=nim30 make dev-stop \
  && DEV_STAND=nim30 make dev-stand-free
```

`make dev-stop` extinguishes demons **only** of his stand (keeper/web by pidfile in
`/tmp/keeper-dev-<slug>/`, souls according to stand-scoped pattern) - adjacent stands are not
hurts. The token for stand API calls is `TOKEN=$(DEV_STAND=nim30 make dev-jwt)`.

### WSL2: docker souls on a custom stand

Docker souls (`dev-souls-docker`) call the keeper via host-IP, and on WSL2
`host.docker.internal` from Docker-Desktop-VM does not reach the keeper (general analysis - in
Docker-soul). For a non-standard stand the same three
conditions, but with `DEV_STAND`:

```sh
make build-linux                              # fresh soul binary (bind-mount ro into container)
IP=$(hostname -I | awk '{print $1}')
DEV_STAND=nim30 DEV_KEEPER_EXTRA_IP=$IP make dev-provision   # host-IP in ip_sans stand certificate
DEV_STAND=nim30 make dev-keeper
DEV_STAND=nim30 KEEPER_HOST=$IP make dev-souls-docker         # souls dial to host-IP
```

Stand containers are named `soul-docker-<slug>-1..N` (sid == container name), not
intersecting with the souls of other stands. `make build-linux` before `dev-souls-docker`
is required - the container mounts a fresh `soul/bin/soul-linux-amd64` ro.

### Freeing a slot

The slot is held in the `/tmp/soul-stack-stands.tsv` registry until it is explicitly released:

```sh
DEV_STAND=nim30 make dev-stand-free   # remove the slug line from the registry (idempotent)
```

After this, the slot ports are again available to the next stand. Alternative - manually
remove the line `nim30<TAB>…` from `/tmp/soul-stack-stands.tsv`. The registry lives in `/tmp` and
is cleared upon reboot (on macOS - when the day changes) along with dev material; at
DEDICATED_INFRA demolition of stand containers - `DEV_STAND=<slug> make dev-down`.

## Ready dev-config

There is a committed config for the smoke run of Keeper
[`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) - targeted at the compose stack
above (PG 5434, Redis 6381, Vault 8200, root-token, TLS from
`/tmp/keeper-dev/tls/`, OTel traces to collector on `127.0.0.1:4317`,
plugin cache in `/tmp/keeper-dev/`).
`services[]` two services are registered - `hello-world` and `redis` (both
file://-repo from `/tmp/keeper-dev/repos/`, ref `main`);
`default_destiny_source` — `file:///tmp/keeper-dev/destiny/{name}`. The repo themselves
creates `make dev-provision` (see Artifacts
service/destiny). Launch:

```sh
./keeper/bin/keeper init --archon=archon-alice \
  --config=dev/keeper.dev.yml \
  --credential-out=/tmp/keeper-dev/archon-alice.jwt

# CACHE_DIRs point to /var/lib/soul-stack-keeper/ by default (not
# writable in LAN); file:// repos require an explicit flag. See section below.
export KEEPER_SERVICE_CACHE_DIR=/tmp/keeper-dev/services
export KEEPER_DESTINY_CACHE_DIR=/tmp/keeper-dev/destiny-cache
export SOUL_STACK_ALLOW_FILE_REPOS=1
./keeper/bin/keeper run --config=dev/keeper.dev.yml
```

Differences from `examples/keeper/keeper.yml`: dev-shortcut `vault.token: "root"`
instead of AppRole; HTTP-Vault without TLS; TLS-leaf from Vault PKI to
`/tmp/keeper-dev/tls/`; `otel.endpoint` points to dev-collector
(`127.0.0.1:4317`, insecure); `services[]` point to local file:// repo.
For prod configuration use example, not dev copy.

### Push transport (`DEV_PUSH_TRANSPORT`)

The stand renders `push.transport: direct` (NIM-266). That is not a preference:
`teleport` requires `push.teleport.identity_file`, which is minted by `tctl auth
sign` against a **live** Teleport, and nothing in the repo creates it — a
committed `transport: teleport` made `make dev-keeper` die at startup on any
clean machine with `build bootstrap teleport dialer: identity file could not be
decoded`. `TestDevStand_PushTransportNeedsNoOutOfBandIdentity` keeps the
committed default startable.

Against a real Teleport, opt in through the stand profile — `dev/stand-env.sh`
renders the `teleport:` block and `keeper-run.sh` refuses to start when the
identity file is missing, instead of letting the daemon fail a layer deeper:

```sh
DEV_PUSH_TRANSPORT=teleport \
DEV_TELEPORT_PROXY_ADDR=proxy.example.com:443 \
DEV_TELEPORT_CLUSTER=example.com \
DEV_TELEPORT_IDENTITY_FILE=/tmp/keeper-dev/keeper-push.identity \
  make dev-keeper
```

`DEV_TELEPORT_IDENTITY_FILE` defaults to `${STAND_DEV_DIR}/keeper-push.identity`,
so a second stand carries its own credential rather than borrowing the first
one's.

Full analysis of product differences (AppRole instead of root token, persistent Vault +
auto-unseal, least-privilege policy, JWT signing-key rotation) - in
[prod-setup.md](../keeper/prod-setup.md).

## Service/destiny artifacts for resolution

Keeper prod-resolve (`artifact.ServiceLoader` / `DestinyLoader`,
ADR-007/ADR-009) pulls service and destiny artifacts as **git repositories by
ref**, not from the local directory. `make dev-provision` materializes these
repo from `examples/` under file://-URLs pointed to
`dev/keeper.dev.yml`:

| Artifact | git-URL (from `keeper.dev.yml`) | ref | source in `examples/` |
|---|---|---|---|
| service `hello-world` | `file:///tmp/keeper-dev/repos/hello-world` | `main` | `examples/service/hello-world` |
| destiny `redis` | `file:///tmp/keeper-dev/destiny/redis` | `v1.0.0` | `examples/destiny/redis` |
| destiny `redis-exporter` | `file:///tmp/keeper-dev/destiny/redis-exporter` | `v1.0.0` | `examples/destiny/redis-exporter` |
| destiny `node-exporter` | `file:///tmp/keeper-dev/destiny/node-exporter` | `v1.0.0` | `examples/destiny/node-exporter` |

destiny-URL is `default_destiny_source` (`file:///tmp/keeper-dev/destiny/{name}`)
with substitution `{name}` from the service's `service.yml::destiny[]`; ref `v1.0.0`
announced there. The destiny-repo directory is named `{name}` (`redis`), and the directory
in `examples/` is now also naked `{name}` (`redis`, without the prefix `destiny-`).

Provision creates a repo deterministically (fixed author/date → stable
commit-SHA with unchanged content): repeated `make dev-provision` does not produce results
orphans in Keeper's snapshot cache.

For Keeper to be able to clone them locally, two envs are needed at `keeper run`:

| Env | Why | Value for dev |
|---|---|---|
| `SOUL_STACK_ALLOW_FILE_REPOS` | `file://`-repos are prohibited in production (`artifact/scheme.go`) - the flag enables them for dev/test. | `1` |
| `KEEPER_SERVICE_CACHE_DIR` | snapshot-cache service-repo. Default `/var/lib/soul-stack-keeper/services` is not writable in LAN. | `/tmp/keeper-dev/services` |
| `KEEPER_DESTINY_CACHE_DIR` | snapshot-cache destiny-repo. Default `/var/lib/soul-stack-keeper/destiny` is not writable. | `/tmp/keeper-dev/destiny-cache` |

> `KEEPER_DESTINY_CACHE_DIR` is NOT the same as the destiny-**repo** directory
> (`/tmp/keeper-dev/destiny/`): the first is the Keeper snapshot cache, the second is
> source git repos where `default_destiny_source` points to. Different paths
> intentionally so that the cache does not overwrite the sources.

## Background dev daemons (wrapper over `keeper run`/`soul run`)

Manual `keeper run` / `soul run` / `npm run dev` from the smoke recipe below remain
as a **low-level alternative** (useful when you need a foreground log in the terminal
or non-standard config). For everyday debugging, the same runs are wrapped in
dev targets are a **wrapper over the same binaries** that adds three things on top
manual step:

1. **background launch** (`nohup … &`, log in `/tmp/keeper-dev/`), not occupying a terminal;
2. **healthz-wait** - the target is not returned until the component responds 200
(keeper `:8080/healthz`, web `:5173`), otherwise it prints the tail of the log and fails;
3. **correct env** - a verified set of variables that, when run manually
gets lost all the time (especially `SOUL_STACK_ALLOW_FILE_REPOS=1` + writable
cache-dirs for file://-resolution, see Artifacts
service/destiny).

| Target | Wrap over | What adds |
|---|---|---|
| `make dev-keeper` | `keeper run --config=dev/keeper.dev.yml` | **rebuild through `make build-keeper`** (always, with the version stamp) → kill the old one using the pattern `keeper.dev.yml` → DEL leader-leases (`conductor:leader`/`reaper:leader`) → wait `:9090` free → full dev-env → `nohup` → wait healthz `:8080` **and refuse when the version it reports is not the one just built**. There is no TLS - `dev-provision` suggests. |
| `make dev-souls` | `soul init` + `soul run` for each sid | **rebuild through `make build-soul`** (always), onboarding only if the seed is invalid, does not touch covens from the database, summary `status, count(*)` at the end. |
| `make dev-web` | `npm run dev -- --host` | mandatory `--host` (IPv4-loopback) + wait `:5173`. |
| `make dev-stand` | all at once | `dev-provision` → `dev-keeper` → `dev-souls` → `dev-web`. |

`make dev-stop` extinguishes background keeper/soul-demons raised by both these targets,
and manually (match according to the dev-config pattern).

**Example `dev-jwt`** - token for ad-hoc Operator API calls without `keeper init`:

```sh
# admin-token by default (archon-alice / cluster-admin / 12h):
TOKEN=$(make dev-jwt)
curl -H "Authorization: Bearer ${TOKEN}" 127.0.0.1:8080/v1/souls

# arbitrary subject and roles (for example, for the RBAC keyset demo):
make dev-jwt AID=archon-keyset ROLES='["keyset-demo"]'
```

### Which code is this stand running?

Ask the stand, do not infer it. `/healthz` is public (no token) and reports the build
version of the instance that answers — during a rolling upgrade, of the very instance
that answered (ADR-0076(h)):

```sh
curl -s 127.0.0.1:8080/healthz
# {"status":"ok","version":"v0.1.0-beta.1-127-g1a226729-dirty"}
```

The version is `git describe --tags --always --dirty`, so it names the commit
(`g<sha>`) and says when the tree had uncommitted changes (`-dirty`). That turns the
acceptance question into one command:

```sh
served=$(curl -s 127.0.0.1:8080/healthz | sed -n 's/.*"version"[^"]*"\([^"]*\)".*/\1/p')
git merge-base --is-ancestor "${served##*-g}" release/R5 && echo "the stand runs code that is in release/R5"
```

`version: 0.0.0-dev` means the binary carries no stamp — the stand cannot be
identified and nothing observed on it can be held to a commit. `make dev-keeper`
warns when it produces such a build.

Why this matters: a worktree per ticket means a dozen `keeper/bin/keeper` binaries,
and every one of them binds the same default stand (`:8080` plus shared
Postgres/Redis/Vault). Before this was checkable, a stand served an eight-day-old
binary for over a week without a word, and an observation made against it — "a
narrowly scoped role sees everything" — cost a critical priority and a dedicated
session before the binary turned out not to be the merged code (NIM-342).

Two rules follow:

- **An acceptance or demo stand is brought up only from the release worktree** of both
  repos (`tasks/<REL>/soul-stack` and `tasks/<REL>/soul-stack-web`), never from a
  ticket worktree. A demo shows the release, not somebody's branch.
- **Before asking anyone to accept a change, prove the stand contains it** — via
  `/healthz` for the backend, and by querying your own key/string for the UI.

## Docker-souls (isolated fleet)

Host fleet (`make dev-souls`) raises souls as processes on the host - they all share
FS, packages and services of the developer's machine. For **day-2 scenarios** (installation
packages, `core.service.*`, editing files) and UI tests without the cloud need isolation:
each soul is its own privileged Debian-12 systemd container with a separate FS.

`make dev-souls-docker` raises `N` such containers (`SOULS_COUNT`, default 3)
with predictable names `soul-docker-1..N` (sid == container name), onboards them to
keeper process on the host and waits for `connected`. `make dev-souls-docker-down` demolishes
containers, cleans them from the registry (cascaded psql-DELETE - DELETE endpoint in
Operator API no) and deletes per-soul dev directories.

```sh
make build-linux          # required: fresh soul/bin/soul-linux-amd64 (bind-mount ro into container)
make dev-souls-docker              # raise SOULS_COUNT soul (default 3)
make dev-souls-docker SOULS_COUNT=5
bash dev/souls-docker-up.sh 5      # the same directly, N as a positional argument
make dev-souls-docker-down         # demolish everything
```

The image reuses the base Dockerfile e2e-live (`tests/e2e-live/dockerfiles/`);
the fresh binary is mounted ro, so rebuilding the image after `make build-linux` is not possible
needed. Container flags (privileged, `--cgroupns=host`, tmpfs `/run`,
`/sys/fs/cgroup`) - parity with e2e-live harness.

**Keeper listens to gRPC on `0.0.0.0`.** The container calls keeper at
host-gateway (native Linux: docker-bridge ~172.17.0.1) or host-IP (WSL2) - on
loopback `127.0.0.1` keeper for container is not reachable in **any** environment. Therefore
`dev/keeper.dev.yml` binds bootstrap/event_stream to `0.0.0.0` (openapi/mcp/metrics
remain on loopback). The use case is `tests/e2e-live/harness/config_builder.go`.

**Dependency on re-provision (SAN).** Docker soul dials to keeper via
`host.docker.internal` (or host-IP), so this SAN must be in the keeper certificate.
It is added to `dev/provision.sh` - after updating the provision script, one is needed
times `make dev-provision` (cert re-release) + `make dev-keeper` (restart). Script
will warn you if the certificate is outdated.

**WSL2.** From Docker-Desktop-VM `host.docker.internal` resolves to DD-VM gateway, where
keeper does NOT listen → bootstrap crashes. On WSL2, the keeper endpoint must be host-IP,
and it must be added to the SAN of the server:

```sh
IP=$(hostname -I | awk '{print $1}')
DEV_KEEPER_EXTRA_IP=$IP make dev-provision   # host-IP in ip_sans keeper-cert
make dev-keeper
KEEPER_HOST=$IP make dev-souls-docker         # souls dial to host-IP
```

## Fast recovery of the stand after /tmp cleaning

On macOS, day change (as well as reboot) **cleans `/tmp`** - all disappears
dev-stuff under `/tmp/keeper-dev/`: TLS (`tls/`), plugins (`plugins/`) and
per-soul seed (`<sid>/seed/*.pem`). After this the stand looks "broken"
although neither the code nor the database were affected.

**Typical symptoms:**

| Symptom | What's Lost |
|---|---|
| keeper crashes at start `load bootstrap TLS … no such file or directory` | `/tmp/keeper-dev/tls/` (TLS-leaf from Vault PKI) |
| souls in `disconnected`, `soul run` drops `SoulSeed not found` | `/tmp/keeper-dev/<sid>/seed/` (mTLS pairs) |
| scripts resolve to 502 `file:// forbidden` | `SOUL_STACK_ALLOW_FILE_REPOS=1` in the env of the keeper process (env, not a file - lost when manually restarted) |

**Recovery recipe** - re-decompose the material and re-raise the demons:

```sh
make dev-provision   # tls/ + plugins/ + git repo of artifacts from Vault PKI/examples
make dev-keeper      # keeper with full dev-env (including SOUL_STACK_ALLOW_FILE_REPOS=1)
make dev-souls       # will re-board souls with a broken/disappeared seed and re-raise run
```

Or with one command - `make dev-stand` (does the same `dev-provision → dev-keeper
→ dev-souls` + raises web). The DB (`souls`/`operators`/`incarnation`, covens)
is experiencing /tmp cleanup - `keeper init` does NOT need to be repeated (operators registry does not
empty); `dev-souls` restores only seed and run, registry sid and their covens
takes from the database.

> If the **DB** itself is lost (after `make dev-reset` or `docker compose down -v`)
> is a different case: you need the full `make dev-smoke` (with `keeper init`), not
> restore /tmp. /tmp-database cleaning does not affect it.

## Smoke recipe (E2E)

Reproducible sequence for a full smoke run. Steps 1–4
are automated via `make dev-smoke` (raises stack, provisioning,
collects `keeper`, makes `keeper init`); `keeper run` remains as
separate foreground step - it should not be launched from `dev-smoke`.

> Foreground-`keeper run` below is a low-level option. To run in the background with
> healthz-wait and the same dev-env there is a wrapper `make dev-keeper` (see Background
> dev-demons) - she does
> exactly these three `export` + `run` itself in one step.

```sh
make dev-smoke

# keeper run - a separate foreground step, with dev-env for file:// resolution
# service/destiny artifacts (see "Service/destiny artifacts"):
export KEEPER_SERVICE_CACHE_DIR=/tmp/keeper-dev/services
export KEEPER_DESTINY_CACHE_DIR=/tmp/keeper-dev/destiny-cache
export SOUL_STACK_ALLOW_FILE_REPOS=1
./keeper/bin/keeper run --config=dev/keeper.dev.yml

# When you've had enough of playing, stop the local keeper (Ctrl-C in the foreground), or
# if the demon has gone into the background/orphaned:
make dev-stop
```

`make dev-smoke` runs `make dev-provision` under the hood
([`dev/provision.sh`](../../dev/provision.sh)) - single source of truth for
provisioning steps (Vault KV + PKI + TLS-leaf from Vault PKI + git repo
artifacts). Manual layout is no longer duplicated here to doc and script
did not disperse; read the actual steps in the script itself.

> **Foot-gun: `dev-provision` is needed TWICE on a fresh database.** `make dev-smoke`
> does it itself (`dev-provision` → `keeper init` → `dev-provision`), but when
> manual run, the order is important: database schema (`service_registry`/`keeper_settings`)
> creates `keeper init` (`migrate.Apply`), so the **first** provision pass
> on a fresh database (`dev-reset`) skips the seed service registry - there are no tables yet.
> The service registry is seeded only in the **second** pass, after `keeper init`.
> `provision.sh` is idempotent - the double call is safe; no second pass
> resolve reads an empty service registry (`services[]` removed from `keeper.dev.yml`).
>
> `keeper init` prints `Bootstrap complete. Token written to
> <path>` (with a path argument: the first Archon's JWT in a file with
> `mode 0400`; with `-`, no file and no mode at all) — on
> **stderr**, so that `--credential-out=-` can keep stdout for the token
> alone ([bootstrap-rbac.md → Token on stdout](../operations/bootstrap-rbac.md#token-on-stdout)).

Verified manipulations after `run`:

- `curl 127.0.0.1:8080/healthz`, `/readyz`, `/openapi.yaml`, `/metrics` → 200.
- `POST /v1/operators` with Bearer JWT of the first Archon → 201 + JWT of the new one
operator; repeat → 409 `operator-already-exists`.
- `POST /v1/incarnations` for `service: redis` / `scenario: create` → 202 +
`apply_id`. redis + 3 destiny resolves from file://-repo
(created by `dev-provision`), rendered by Keeper-side (CEL + text/template).
- `POST /v1/operators/archon-alice/revoke` → 409 `would-lock-out-cluster`
(invariant ADR-013: the last `*`-permission cannot be deleted).
- MCP on `127.0.0.1:8081`, endpoint `POST /mcp` (JSON-RPC 2.0; root `/`
gives 404) — `initialize` → `tools/list` (41 tool, the number is fixed by test
  `mcp.TestCatalog_TotalCount`) → `tools/call`.
- `audit_log` (psql or `GET /v1/audit`) contains records of three `source`:
  `keeper_internal` / `api` / `mcp` (ADR-022b).
- Repeat `keeper init` → exit 1 `ErrAlreadyInitialized`.
- Graceful shutdown by `SIGTERM` — 5 listeners stopped clean.

The complete chain along with example commit fields is recorded in
commit-message `97c67e2`.

## Soul-failover demo (two keeper)

Manual procedure to check **soul-failover live**: soul, having lost
priority-1-keeper, reconnects to priority-2 and returns back when
recovery (ADR-002 multi-endpoint + failback). Production code Soul-failback
(`soul/internal/grpc` DialPriority/orderedEndpoints + `soul/cmd/soul` reconnect/
failback-loop) is covered with unit and integration tests; this procedure validates it
on two **real** keeper processes - closes the previous stand-limitation
mega-test (previously `dev/soul.dev.yml` hardcoded one endpoint and
`failback.enabled: false`, so live-failover did not play).

Configs:

| File | kid | bootstrap | event_stream | openapi | mcp | metrics |
|---|---|---|---|---|---|---|
| [`dev/keeper.dev.yml`](../../dev/keeper.dev.yml) | `keeper-dev-01` | 9442 | 9443 | 8080 | 8081 | 9090 |
| [`dev/keeper-b.dev.yml`](../../dev/keeper-b.dev.yml) | `keeper-dev-b` | 9542 | 9543 | 8082 | 8083 | 9092 |

Both keepers share PG/Redis/Vault and the same TLS from `/tmp/keeper-dev/tls/`. **Both**
with `acolytes: 2` - symmetrical HA work-queue participants (ADR-027). This
mandatory: with two live instances in Conclave refuse-guard soul-shedding
(S3, `allow_unsafe_single_path_multi_keeper: false`) will refuse to start the instance
with `acolytes: 0` on single-path. With `acolytes: 0` on keeper the demo would have broken on
**phase 4** (restart keeper-a while keeper-b is alive → `CountLive=2` → refuse);
`acolytes>0` on both removes guard on all phases. `dev/soul.dev.yml`
lists both keeper in `keeper.endpoints` (priority 1 = keeper-a, 2 =
keeper-b) and includes `keeper.failback.enabled: true`.

> **Quick demo: shorten `failback.interval`.** In `dev/soul.dev.yml` interval
> omitted → default `loadFailback` = **1h** (in production failback is intentionally lazy,
> so as not to disrupt the session). For failback (phase 4) to happen in seconds,
> in the local copy of the config add under `failback:` lines
> `interval: 5s` and `spray: 0s`. Fallback (phase 2) does not depend on interval -
> is triggered immediately on the priority-1 break.

### Procedure

```sh
# 0. Stack + provision + keeper init (as in Smoke recipe). Once.
make dev-smoke

# dev-env for file://-resolve - common to both keeper processes.
export KEEPER_SERVICE_CACHE_DIR=/tmp/keeper-dev/services
export KEEPER_DESTINY_CACHE_DIR=/tmp/keeper-dev/destiny-cache
export SOUL_STACK_ALLOW_FILE_REPOS=1

# 1. keeper-a (priority 1) - terminal A.
./keeper/bin/keeper run --config=dev/keeper.dev.yml

# 2. keeper-b (priority 2) - terminal B.
./keeper/bin/keeper run --config=dev/keeper-b.dev.yml

# 3. soul init + run — terminal C. init goes to priority-1 bootstrap (9442);
#    if priority-1 is unavailable, onboarding itself goes to priority-2 (9542).
./soul/bin/soul init --config=dev/soul.dev.yml
./soul/bin/soul run  --config=dev/soul.dev.yml
```

**Phase 1 - initial connect.** In the soul log:
`eventstream: connected ... priority=1 kid=keeper-dev-01`. Soul leads a session on
keeper-a.

**Phase 2 - fall of keeper-a → fallback to keeper-b.** Kill keeper-a - Ctrl-C in
terminal A (`make dev-stop` matches only `keeper.dev.yml`, keeper-b with
`keeper-b.dev.yml` does not fall under this pattern - extinguish it with Ctrl-C separately).
Soul loses stream, reconnect-loop dials by priority: priority-1 unavailable →
takes priority-2. In the log:
`eventstream: connected ... priority=2 kid=keeper-dev-b`.
Registry check/apply - soul is visible through keeper-b:

```sh
curl 127.0.0.1:8082/metrics | grep soul   # keeper-b openapi/metrics live
# or POST /v1/incarnations to keeper-b (8082) → apply reaches soul
```

**Phase 3 - Watchman option (no kill).** Alternative to phase 2: NOT killing
keeper-a, isolate it from PG/Redis (for example, `docker stop soul-stack-redis`).
Watchman keeper-a (probe interval 5s, threshold of 3 consecutive failures) detects loss
dependencies and **itself** closes local EventStream streams (soul-shedding S2).
Soul sees the gap and goes to keeper-b just like in phase 2 - but keeper-a at
this remains "alive" as a process. Return Redis (`docker start soul-stack-redis`),
for keeper to host streams again.

**Phase 4 - restore keeper → failback.** Raise keeper again
(step 1). Failback-loop soul (abbreviated `interval`, see sidebar above) proactively
dials higher-priority endpoint, opens a new session on keeper and
gracefully closes the old one on keeper-b (zero-downtime swap). In the log:
`eventstream: connected ... priority=1 kid=keeper-dev-01` - again on keeper.

```sh
# when you've played enough: soul (terminal C) and keeper-a (terminal A) - Ctrl-C or
# `make dev-stop` (matches keeper.dev.yml + soul.dev.yml). keeper-b
# (keeper-b.dev.yml) does not fall under the dev-stop pattern - Ctrl-C in terminal B.
make dev-stop
```

### Vault PKI

PKI-backend is needed to issue SoulSeed certificates via gRPC `Bootstrap`-RPC
(ADR-012, ADR-014). Raises separately from KV - on the mount specified in
`keeper.yml::vault.pki_mount` (e.g. `pki/`), with PKI role
`soul-seed` (`vault.pki_role`). `make dev-provision` does this
automatically; Below are the steps in the form of manual commands for your reference:

```sh
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=root

# Enable PKI secrets engine.
vault secrets enable -path=pki pki
vault secrets tune -max-lease-ttl=87600h pki

# Root certificate.
vault write pki/root/generate/internal \
  common_name="soul-stack" ttl=87600h

# Role `soul-seed` - issues SoulSeed certificates for domain(s)
# test hosts. In production, `allowed_domains` coincides with the FQDN convention
# organizations. Through PKI_ROLE_DOMAINS you can override the list of domains
# for `make dev-provision`.
vault write pki/roles/soul-seed \
  allowed_domains=example.com,test,localhost \
  allow_subdomains=true \
  allow_localhost=true \
  max_ttl=720h
```

After `make dev-down`/`dev-reset` run `make dev-provision` again.

`dev-reset` recreates the Vault dev server, and therefore the PKI root (new serial).
TLS release step in `provision.sh` reset-aware: it skips re-release only
if `tls/vault-ca.crt` is still the same as the current `vault read pki/cert/ca`
and `keeper.crt` cling to it; otherwise the certificates are reissued. It holds
ClientCAs Keeper (`event_stream.tls.ca`) synchronous with SoulSeeds, which
are signed by the current root - otherwise mTLS onboarding of the new Soul would break after reset.

## Logs and status

| Action | Team |
|---|---|
| Postgres logs | `docker compose -f dev/docker-compose.yml logs -f postgres` |
| psql inside the container | `docker exec -it soul-stack-postgres psql -U keeper -d keeper` |
| List of tables | `\dt` in psql |
| View `audit_log` | `SELECT audit_id, event_type, source, created_at FROM audit_log ORDER BY created_at DESC LIMIT 20;` |
| Vault logs | `docker compose -f dev/docker-compose.yml logs -f vault` |
| Vault status inside the container | `docker exec soul-stack-vault vault status` (Sealed: false in dev mode) |
| Vault CLI inside a container | `docker exec -it -e VAULT_TOKEN=root soul-stack-vault vault kv list secret/` |
| Redis logs | `docker compose -f dev/docker-compose.yml logs -f redis` |
| Redis CLI inside a container | `docker exec -it soul-stack-redis redis-cli` |
| Ping Redis from Host | `redis-cli -h 127.0.0.1 -p 6381 ping` |
| OTel-collector logs (accepted spans) | `docker compose -f dev/docker-compose.yml logs -f otel-collector` |
| Jaeger logs | `docker compose -f dev/docker-compose.yml logs -f jaeger` |
| Jaeger UI | http://127.0.0.1:16686 (service `keeper` / `soul`) |

## Troubleshooting

### Port conflict with other docker stacks

The Compose stack intentionally uses non-default ports:

| Service | Host port | Default | Reason |
|---|---|---|---|
| Postgres | `5434` | `5432` | avoids `agent-platform-postgres:5432` |
| Redis | `6381` | `6379` | avoids `dba-salt-redis:6379` and `agent-platform-valkey:6380` |
| Vault | `8200` | `8200` | no changes |
| OTLP gRPC (collector) | `4317` | `4317` | standard OTLP port |
| Jaeger UI | `16686` | `16686` | standard Jaeger UI port |

If busy and `5434` / `6381` / `8200` / `4317` / `16686` (`docker compose up`
drops from `bind: address already in use`):

1. Find process - `lsof -nP -iTCP:5434 -sTCP:LISTEN` (or `:6381` / `:8200` /
   `:4317` / `:16686`).
2. Stop the conflicting container (`docker stop <name>`), or
change port-mapping to `dev/docker-compose.yml` (only the left part -
`"5435:5432"`) and at the same time correct the related configs: for Redis -
`dev/keeper.dev.yml` (`redis.addr`) + Vault KV (`postgres.dsn`); for OTLP -
`otel.endpoint` to `dev/keeper.dev.yml` and `dev/soul.dev.yml` (Jaeger UI port
is not programmed anywhere in the configs - only in compose).

There is no need to change the right side of the mapping (inside the container) - `dsn` and
healthcheck-and operate the internal port.

### `keeper init` falls `pq: connection refused`

`POSTGRES_DSN` in Vault is pointing to the wrong port. After `make dev-reset`
secrets are lost - run `make dev-provision` again.

### `keeper run` crashes on TLS certificate

Certificate in `/tmp/keeper-dev/tls/` not generated or deleted (on macOS
`/tmp` is cleaned during reboot **and when the day changes**). Regenerate -
`make dev-provision`. If both souls and file://-resolve fail at once, it's the same
/tmp cleanup entirely, see Quick recovery
stand (`make dev-stand`).

### Resolve service/destiny crashes on git clone or `file:// forbidden`

- `file:// forbidden in prod (set SOUL_STACK_ALLOW_FILE_REPOS=1 …)` —
env flag forgotten at `keeper run`. See Artifacts
service/destiny.
- `permission denied` / `mkdir /var/lib/soul-stack-keeper/...` - not set
`KEEPER_SERVICE_CACHE_DIR` / `KEEPER_DESTINY_CACHE_DIR` to `/tmp/keeper-dev/`.
- `ref "v1.0.0" does not resolve` / repo not found - not running
`make dev-provision` (or `/tmp` was cleaned during reboot). Restart.

## Integration tests

Automated tests that need real Postgres are raced through
[testcontainers-go](https://golang.testcontainers.org/). Everyone
`internal/<pkg>/integration_test.go` raises `postgres:16-alpine` to
ephemeral port, applies migrations from `keeper/migrations/`, drives
write/read round-trip and deletes the container on exit `TestMain`.

| Team | What does |
|---|---|
| `make test-integration` | `go test -tags=integration -race -count=1 -p 4` over the packages that carry integration-tagged tests (43, listed by `scripts/integration-packages.sh`) in every module, with `SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1`. Default script. |
| `make test-integration PKG=./internal/auditpg/` | Sighting one package — same flags, so a green result means the same thing. |

Requirements:

- **Docker**. Testcontainers uses docker-sock; on macOS - Docker
Desktop / OrbStack / Colima; on Linux - `dockerd` + rights to the socket.
- `make test` / `make test-race` (without `-integration`) **do not require docker** -
files under `//go:build integration` are excluded from the normal build. The two
cover the same packages; `test-race` adds the race detector and takes ~3 min
against ~90 s (see [docs/testing/README.md](../testing/README.md) for which target
carries `-race` and which does not).
- `SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1` (or `true`): if testcontainers could
not start, the tests **fail** with `log.Fatalf` instead of skipping. Without it
`TestMain` logs the reason and returns exit 0 - the package is silently not run,
and a whole suite can report success having executed nothing. `make test-integration`
therefore sets it itself (`make test-integration SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=0`
restores the skipping behaviour); a bare `go test -tags=integration` still needs it
passed by hand.
- `make test-integration INTEGRATION_PARALLEL=<n>` caps how many packages - i.e.
how many container sets - start at once (default 4). Unbounded, the
GOMAXPROCS-wide default start swamps the docker daemon and packages fail on the
testcontainers reaper rather than on their own assertions.

#### WSL2: `docker-credential-desktop.exe: exec format error`

On WSL2 with Docker Desktop, any test that BUILDS an image (`keeper/internal/trial`
L2 stands) can fail like this:

```
create container: build options: auth configs from Dockerfile:
get credentials from store: execute "docker-credential-desktop.exe":
fork/exec /usr/bin/docker-credential-desktop.exe: exec format error
```

This is machine configuration, not a code regression. Docker Desktop writes
`"credsStore": "desktop.exe"` into `~/.docker/config.json`, so every registry auth
lookup shells out to a **Windows** binary; that only works while WSL
Windows-interop is registered. Check it:

```bash
ls /proc/sys/fs/binfmt_misc/     # WSLInterop present?
```

If `WSLInterop` is missing (it can disappear on a WSL restart or when
`systemd-binfmt` re-runs) no `.exe` can be executed from this instance and the
credential helper fails for every registry. Either re-register interop, or drop
the `credsStore` line from `~/.docker/config.json` - the Linux docker CLI does
not need it unless you pull from a private registry.

### Build-tags

| Tag | Where | Why |
|---|---|---|
| `integration` | `keeper/internal/*/integration_test.go` | testcontainers-go; default path for CI and local verification. |
| `smoke` | `keeper/internal/migrate/smoke_test.go` | Manual fallback in case docker-sock is not available (Codespaces, restricted CI). Starts with `SOUL_STACK_SMOKE_DSN=postgres://... go test -tags=smoke ./internal/migrate/`. You raise your `make dev-up` yourself. |

### CI

There is no GitHub Actions / GitLab CI in the repository yet. When setting up for the first time
pipeline:

- In most GitHub Actions runners (`ubuntu-latest`) docker daemon
available out of the box; on restricted runners - configure
Docker-in-Docker or `DOCKER_HOST`. In GitLab - explicit `DOCKER_HOST` or
  privileged runner.
- `SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1` is already the default of
`make test-integration`, so a CI job that calls the target needs nothing extra.
A job that calls `go test -tags=integration` directly must set it itself -
without the flag the tests silently skip when docker is unavailable, which is
unacceptable for CI.
