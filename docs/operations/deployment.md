# Deployment

Rolling out Soul Stack binaries to production hosts. Minimum single-keeper, HA multi-keeper, OS requirements, systemd units, configs.

## Binari

Accordingly, [ADR-004](../adr/0004-binaries.md#adr-004-binary-layout--keeper-soul-soul-lint-push-mode-as-a-module-inside-keeper) - **three operator binaries** (`keeper` / `soul` / `soul-lint`), rolled out to the infrastructure. The fourth artifact `soul-trial` is an offline test runner ([ADR-023](../adr/0023-trial-test-runner.md)) of the authoring cycle (CI / dev machine), not installed on production hosts:

| Binar | Role | What | Where does it start |
|---|---|---|---|
| `keeper` | operator | Central server (gRPC bidi to Soul, OpenAPI, MCP, push module, Reaper). | Keeper host (`/usr/local/bin/keeper`). |
| `soul` | operator | Agent daemon. In push mode - the same binary, launched `soul apply` via SSH. | Managed host (`/usr/local/bin/soul`). |
| `soul-lint` | operator | Offline linter Destiny/Service/Manifest/Scenario. | CI / dev machine. |
| `soul-trial` | test tool | Offline destiny/scenario/migration challenge runner ([ADR-023](../adr/0023-trial-test-runner.md)). Not a camera artifact. | CI / dev machine. |

### Where to get artifacts

| Method | Team/source | When |
|---|---|---|
| From sources | `make build` ([`Makefile`](../../Makefile)) | dev / staging. Binary in `<module>/bin/<name>`. |
| Native deb/rpm packages | `make pkg` (requires `nfpm`, see [`deploy/README.md`](../../deploy/README.md)). Artifacts in `dist/pkg/`. | Prod installation on Linux. nfpm configs - [`deploy/nfpm/`](../../deploy/nfpm/). |
| Docker images | `docker build -f deploy/docker/<name>.Dockerfile -t soul-stack/<name> --build-arg VERSION=$(git describe …) .` (multi-stage, distroless runtime; see [`deploy/README.md`](../../deploy/README.md)). | Container rolling. |
| SBOM | `make sbom` (CycloneDX via `cyclonedx-gomod`, mode `app`). Artifacts in `dist/sbom/`. | Compliance / supply-chain audit requirements. |

`make pkg` rebuilds Linux binaries under `PKG_ARCH` (`amd64` default, overridden by `make pkg PKG_ARCH=arm64`) with `CGO_ENABLED=0 -trimpath -ldflags '-s -w'` and injects `VERSION` ldflags (see [`Makefile`](../../Makefile)).

Signing images (cosign / sigstore) - postponed until the appearance of CI + registry (`make sign` - documented stub), see [`deploy/README.md` § "Signing images"](../../deploy/README.md).

## System Requirements

### Keeper host

| Parameter | Minimum | Recommended |
|---|---|---|
| OS | Linux x86_64 / arm64, kernel 5.10+ | RHEL/Alma/Rocky 9, Debian 12, Ubuntu 22.04 LTS |
| systemd | 245+ (ProtectSystem=strict + LoadCredential) | 252+ |
| CPU | 2 vCPU | 4-8 vCPU per-keeper instance |
| RAM | 1 GB | 4 GB per-keeper instance (depending on the size of the fleet and Acolyte pool) |
| Disk (root FS) | 5 GB | 20 GB |
| Drive `/var/lib/keeper` | 1 GB | 10 GB (TLS material, plugin cache, git-resolve work root) |
| Network Ports | see § Network ports | — |

The host is launched under a separate system user `soul-stack` (see header [`deploy/systemd/keeper.service`](../../deploy/systemd/keeper.service)). Hardening - hard profile (`ProtectSystem=strict`, `MemoryDenyWriteExecute`, `PrivateDevices`, ...) - Keeper does not change the host, isolation does not interfere with it.

### Soul host (managed)

| Parameter | Minimum | Recommended |
|---|---|---|
| OS | Linux x86_64 / arm64, kernel 5.4+ | any distribution supported in [Soulprint OsFacts](../soul/soulprint.md) (debian/ubuntu/redhat-family/alpine) |
| systemd | 245+ | by distro |
| CPU | 1 vCPU | 2 vCPU |
| RAM | 256 MB | 1 GB (more at the time of apply - depends on the modules) |
| Drive `/var/lib/soul-stack` | 200 MB | 2 GB (module cache by SHA-256, SoulSeed) |
| Network Ports | egress to Keeper EventStream + bootstrap-listener | — |

Hardening Soul - **soft profile** (see [`deploy/systemd/soul.service`](../../deploy/systemd/soul.service)): Soul uses Destiny (installs packages, edits files, manages services), so writing to the system is NOT prohibited. **Do not tighten** `ProtectSystem=strict` without checking the apply loop - it will break core modules.

### Soul-host in push mode (without agent)

- SSH access from Keeper host.
- Linux + base utilities (bash, coreutils, systems-pkg-mgr).
- Directory `/var/lib/soul-stack/{bin,modules}/` - Keeper caches the `soul` binary and SHA-256 modules there; a second run does not download it (see [`docs/keeper/push.md`](../keeper/push.md)).
- The cache directory is cleared by an optional step in the same SSH session (see [`docs/keeper/push.md`](../keeper/push.md)), not by Reaper.

## Network ports

### Keeper (default listen addresses from [config.md](../keeper/config.md#listen))

| Port | Destination | Listener | TLS |
|---|---|---|---|
| `9442` | Bootstrap-RPC (Soul onboarding) | `listen.grpc.bootstrap.addr` | server-only TLS |
| `8443` | EventStream Keeper↔Soul (long-lived bidi) | `listen.grpc.event_stream.addr` | mTLS (validation of SoulSeed certificates according to `tls.ca`) |
| `8080` | OpenAPI Operator API | `listen.openapi.addr` | over HTTPS (terminated by LB or directly by Keeper, depending on the installation) |
| `8081` | MCP server | `listen.mcp.addr` | same |
| `9090` | Prometheus `/metrics` | `listen.metrics.addr` | without TLS; protection - Basic-auth (`metrics.auth.basic`) optional |

In a production installation - usually for L4-LB / VIP (see [`scaling.md`](scaling.md)). Bootstrap-listener (`9442`) and EventStream (`8443`) are **required** forwarded to the Soul fleet with the correct TLS material.

### Soul

| Port | Destination | Listener |
|---|---|---|
| `9091` (default) | Soul-side `/metrics` | `metrics.listen` - **default loopback `127.0.0.1`** ([config.md → metrics](../soul/config.md#metrics)). Security - Basic-auth optional, password source - `password_file` (Soul does not have a vault client). |

Soul **never listens to incoming connections from Keeper** - all communications are initiated by Soul via EventStream to Keeper ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). There is no need to open any inbound ports on managed hosts.

## File system layout

### Keeper host

```
/etc/keeper/
  keeper.yml                      # working config (mode 0640, owner soul-stack)
  keeper.env                      # KEEPER_CONFIG=/etc/keeper/keeper.yml (EnvironmentFile)
  vault-secret-id                 # ONLY with vault.auth.method=approle, mode 0400
  tls/
    server.crt                    # Keeper server certificate (Bootstrap + EventStream)
    server.key                    # private
    ca.crt                        # CA for SoulSeed validation of incoming Souls

/var/lib/keeper/                  # ReadWritePaths systemd unit
  state/                          # cache/temporary TLS stuff (if used)

/var/lib/soul-stack-keeper/
  plugins/                        # plugin cache_root (commit_sha slots of resolved binaries)
  plugin-src/                     # plugin work_root (git resolver clones); STRICTLY out of plugins/

/var/run/soul-stack-keeper/
  plugins/                        # Unix-domain plugin sockets (mode 0700)

/var/log/keeper/
  keeper.log                      # when logging.file is specified; rotation built-in
  keeper-<timestamp>.log.gz       # archives (rotation.max_files / max_age_days)
```

### Soul host

```
/etc/soul/
  soul.yml                        # working config
  soul.env                        # SOUL_CONFIG=/etc/soul/soul.yml
  bootstrap-token                 # one-time use, deleted AFTER onboarding (see soul/onboarding.md)
  seed/
    soul.crt                      # SoulSeed (released via CSR; private does not leave host)
    soul.key

/var/lib/soul-stack/              # ReadWritePaths
  bin/                            # SHA-256 binary cache
  modules/                        # SHA-256 module cache
```

## SystemD units

Ready units are in [`deploy/systemd/`](../../deploy/systemd/). Installation (one by one with instructions in the unit header):

```sh
# on Keeper host
useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
install -m0755 dist/keeper /usr/local/bin/keeper           # or from deb/rpm
install -d -o soul-stack -g soul-stack /etc/keeper /var/lib/keeper
install -m0640 keeper.yml /etc/keeper/keeper.yml
install -m0644 deploy/systemd/keeper.service /etc/systemd/system/keeper.service
install -m0644 deploy/systemd/keeper.env     /etc/keeper/keeper.env
systemctl daemon-reload && systemctl enable --now keeper
```

Accordingly for the Soul host with `deploy/systemd/soul.service`.

Units have moved the path to the config to `EnvironmentFile` (`/etc/keeper/keeper.env` → `KEEPER_CONFIG=…`) - the operator changes the file, does not edit the unit. `Restart=on-failure` + `StartLimit{IntervalSec=60s,Burst=5}` - restart if it crashes, don't get stuck in a broken config.

### Hot-reload via SIGHUP

Config changes are applied on the fly via `systemctl reload keeper` ([ADR-021](../adr/0021-hot-reload-config.md)). Per-block policy - which fields are reloadable and which require a restart - in [`docs/keeper/config.md` → Hot-reload](../keeper/config.md#hot-reload).

If the reload is successful - audit-event `config.reload_succeeded` (`source=signal`), if it fails - `config.reload_failed` (the old snapshot remains active, error in the logs).

## Config `keeper.yml` - required minimum

Full contract - [`docs/keeper/config.md`](../keeper/config.md). Minimum for production installation:

```yaml
kid: keeper-prod-01                       # stable human-readable instance ID

listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"
      tls: { cert: /etc/keeper/tls/server.crt, key: /etc/keeper/tls/server.key }
    event_stream:
      addr: "0.0.0.0:8443"
      tls: { cert: /etc/keeper/tls/server.crt, key: /etc/keeper/tls/server.key, ca: /etc/keeper/tls/ca.crt }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }

postgres:
  dsn_ref: vault:secret/keeper/postgres   # plaintext DSN is disabled, see config.md
  pool: { min: 5, max: 50 }

redis:
  addr: "redis.internal:6379"
  password_ref: vault:secret/keeper/redis

vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle                       # prod - NOT token, see docs/keeper/prod-setup.md
    role_id: keeper-prod
    secret_id_file: /etc/keeper/vault-secret-id
  pki_mount: "pki/soulstack"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    ttl_default: 24h
    ttl_bootstrap: 720h

otel:
  enabled: true
  exporter: otlp
  endpoint: "otel-collector.internal:4317"

logging:
  level: info
  format: json
  file: /var/log/keeper/keeper.log
  rotation: { max_size_mb: 100, max_age_days: 7, max_files: 10, compress: true }

reaper:
  enabled: true
  interval: 1h
  rules:
    expire_pending_seeds: { enabled: true, max_age: 24h, action: delete }
    purge_used_tokens:    { enabled: true, max_age: 90d, action: delete }
    purge_souls:          { enabled: true, statuses: [disconnected, expired], max_age: 30d, action: delete }
    purge_old_seeds:      { enabled: true, statuses: [superseded, expired, revoked], max_age: 90d, action: delete }
    mark_disconnected:    { enabled: true, stale_after: 90s, action: set_status, target_status: disconnected }
    purge_audit_old:      { enabled: true, max_age: 365d, action: delete }
    purge_apply_runs:     { enabled: true, max_age: 30d, action: delete }
    purge_apply_task_register: { enabled: true, max_age: 1h, action: delete }
    archive_state_history: { enabled: true, keep_last_n: 50, keep_version_bump_snapshots: true, action: soft_delete }
    # reclaim_apply_runs — DISABLED, turns on only after rolling fencing-Soul + acolytes>0,
    # see docs/keeper/reaper.md → Enabling recovery
    # reap_orphan_vault_keys — disabled, report-only; enable only when Vault list-policy is configured
```

The complete reference example is [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml). Vault AppRole + persistent storage + auto-unseal + JWT signing-key rotation details - [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md).

## Config `soul.yml` - required minimum

Full contract - [`docs/soul/config.md`](../soul/config.md). Minimum:

```yaml
sid: host-01.example.com                  # SID = FQDN, resolves via hostname -f by default

keeper:
  endpoints:
    - { addr: "keeper-1.internal:8443", priority: 1 }
    - { addr: "keeper-2.internal:8443", priority: 1 }   # inside priority - shuffle
    - { addr: "keeper-3.internal:8443", priority: 2 }   # reserve (other DC)
  failback:
    interval: 1h
    spray: 5m

tls:
  cert: /etc/soul/seed/soul.crt
  key:  /etc/soul/seed/soul.key
  ca:   /etc/soul/seed/ca.crt              # CA Keeper

bootstrap_token_file: /etc/soul/bootstrap-token  # is removed after onboarding

metrics:
  listen: "127.0.0.1:9091"

otel:
  enabled: true
  endpoint: "otel-collector.internal:4317"

logging:
  level: info
  format: json
  file: /var/log/soul/soul.log
  rotation: { max_size_mb: 50, max_age_days: 7, max_files: 5 }
```

Connection algorithm (priority + failback + shuffle) - [`docs/soul/connection.md`](../soul/connection.md).

## Multi-keeper HA - topology

Several Keeper instances with different `kid` on top of the common Postgres + Redis ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)). Stateless - any instance serves any request.

```
                    ┌────────────── L4-LB / VIP ──────────────┐
                    │                                          │
                    ▼                  ▼                  ▼
              ┌─────────┐        ┌─────────┐        ┌─────────┐
              │ keeper  │        │ keeper  │   …    │ keeper  │
              │  K1     │        │  K2     │        │  KN     │
              │ acolytes:N        │ acolytes:N      │ acolytes:N
              └────┬────┘        └────┬────┘        └────┬────┘
                   │                  │                  │
                   └──────────────────┼──────────────────┘
                                      │
                            ┌─────────┴─────────┐
                            ▼                   ▼
                      ┌──────────┐        ┌──────────┐
                      │  Redis   │        │ Postgres │
                      │ (cluster │        │  HA      │
                      │ / sentinel)       │ (Patroni)│
                      └──────────┘        └──────────┘
```

Rolling out multi-keeper in detail - [`scaling.md`](scaling.md). Main invariants:

- **Each instance has its own `kid`** (kebab-case, unique in the cluster; conflict → `ErrConclaveKIDTaken`, see [Conclave](../adr/0006-cache-redis.md)).
- **`acolytes > 0` MANDATORY** for N > 1 live Keeper instances ([ADR-027](../adr/0027-apply-work-queue.md)). Refuse-guard refuses to start if violated.
- **JWT signing-key, mTLS material, Vault configuration are the same on all instances** (signing-key from the common Vault KV, mTLS - one issuing CA).
- **`tls.cert` / `tls.key` may differ** between instances if different VIPs have different SANs, but usually there is one wildcard certificate for the entire cluster.

## L4 balancer

EventStream - long-lived gRPC bidi stream (hours/days). Therefore:

- **L4 load balancer (TCP)**, not L7. gRPC via L7-proxy (envoy / haproxy in HTTP mode) tolerates, but does not give anything extra - bidi requires transparent TCP.
- **least-connections** distributes the EventStream load evenly at scale-out.
- **Sticky-session is NOT needed** - joining Soul via SID-lease in Redis ([ADR-006](../adr/0006-cache-redis.md)) already gives "one Soul → one instance" on the Soul-Stack side, the invariant does not depend on LB.
- **Health-check** — `/readyz` Keeper (depends on listener `openapi.addr`; for L4-LB the TCP-probe port `8443` is sufficient).
- **Drain with scale-down** — graceful shutdown Keeper will give SIGTERM (`shutdown_grace`); Conclave-presence is removed → Watchman-cascade does NOT work on other instances (Watchman only responds to isolation). Souls receive EOF on the current EventStream → failback to the next endpoint from the priority list.

OpenAPI / MCP (`8080` / `8081`) can be placed behind L7-proxy (TLS termination + HTTP routing). Bootstrap-RPC (`9442`) - single unary, possible for both L4 and L7.

## Rolling out step by step - single-keeper (minimum for smoke)

1. **Infra:** raise Postgres + Redis + Vault (by [`infra.md`](infra.md)).
2. **Vault provision:** write `secret/keeper/postgres` (field `dsn`), `secret/keeper/jwt-signing-key` (field `signing_key`), `secret/keeper/redis` (field `password`). Create AppRole `keeper-prod` + policy ([`docs/keeper/prod-setup.md`](../keeper/prod-setup.md)).
3. **TLS:** issue Keeper and CA server certificate for SoulSeed.
4. **Keeper host:** install deb/rpm, create `/etc/keeper/keeper.yml` (minimum above), run via systemd.
5. **Bootstrap of the first Archon:** `keeper init --archon=archon-alice --config=/etc/keeper/keeper.yml --credential-out=/etc/keeper/archon-alice.jwt` ([`bootstrap-rbac.md`](bootstrap-rbac.md)).
6. **Smoke:** `curl -H "Authorization: Bearer $(cat /etc/keeper/archon-alice.jwt)" https://keeper-1.internal:8080/v1/operators` - should return `200` with a list of Archons.

## Rolling out step by step - multi-keeper HA

After single-keeper:

1. **Prepare N-1 hosts** - same requirements, same deb/rpm, same `keeper.yml` (with different `kid:` on each).
2. **acolytes > 0** in `keeper.yml` of all instances (see [`scaling.md`](scaling.md)).
3. **L4-LB** before EventStream / Bootstrap ports + L7-proxy before OpenAPI / MCP. Health-check - TCP-probe `8443`.
4. **Soul-configs** — specify all Keeper-endpoints (you can use LB VIP, you can direct addresses with priority). Depends on the failover model in the installation.
5. **Conclave check:** `redis-cli KEYS 'keeper:instance:*'` shows N keys 10s after the start of the last instance (TTL 30s, renew 10s).
6. **Reaper-leader unique:** `sum(keeper_reaper_lease_held) == 1` in Prometheus (see [`monitoring.md`](monitoring.md)).

## See also

- [`docs/keeper/prod-setup.md`](../keeper/prod-setup.md) — Vault AppRole + persistent + auto-unseal + JWT signing-key rotation.
- [`docs/keeper/config.md`](../keeper/config.md) - complete regulatory contract `keeper.yml`.
- [`docs/soul/config.md`](../soul/config.md) — `soul.yml`.
- [`deploy/README.md`](../../deploy/README.md) — Docker / systemd / nfpm.
- [`scaling.md`](scaling.md) — multi-keeper / Acolyte / Conclave / Watchman.
- [`bootstrap-rbac.md`](bootstrap-rbac.md) - `keeper init`, second+ Archon, RBAC.
