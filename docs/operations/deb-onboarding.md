# Onboarding a cluster from deb packages

This guide is a step-by-step instruction "from scratch to connected Soul" for an operator who installs Soul Stack from our deb packages on his infrastructure. It covers installation, Vault provisioning, release of Keeper TLS material, filling out configs, bootstrap of the first Archon and onboarding of the first Soul.

This is an **operational tutorial**, not a reference spec. Where a complete config grammar or RPC contract is needed, I provide a link to the regulatory document. The guide itself is based on real repository files: packages - [`deploy/nfpm/`](../../deploy/nfpm/), units - [`deploy/systemd/`](../../deploy/systemd/), example configs - [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) / [`examples/soul/soul.yml`](../../examples/soul/soul.yml).

> **Package frame.** Our deb packages carry **only** binaries (`keeper` / `soul` / `soul-lint`), systemd units and example configs. **We DO NOT package Postgres, Redis and Vault** - this is an external infrastructure that the operator raises and operates himself. The guide assumes that they are already available. Product requirements for them - [infra.md](infra.md); differences dev↔prod - [keeper/prod-setup.md](../keeper/prod-setup.md).

Related documents: general deployment dock (topologies, LB, HA) - [deployment.md](deployment.md); prod-setup Vault/AppRole/policy - [keeper/prod-setup.md](../keeper/prod-setup.md); RBAC and the first Archon - [bootstrap-rbac.md](bootstrap-rbac.md); bootstrap token mechanism and `soul init` - [soul/onboarding.md](../soul/onboarding.md).

## 1. Prerequisites

### External infrastructure

Before installing packages, the operator must have three external components available. The minimum required loop is **Postgres + Redis**; Vault in production is needed for PKI (SoulSeed) and storing secrets.

| Component | Why | Readiness checklist |
|---|---|---|
| **Postgres** | The only cold storage of the Keeper cluster ([ADR-005](../adr/0005-storage-postgres.md#adr-005-keeper-state-storage--postgres)): registries `souls` / `operators`, Destiny directory, logs | Accessible over the network from keeper hosts; there is a database + role with rights to DDL (migrations are applied idempotently when Keeper starts); removed by DSN string |
| **Redis** | Heartbeat cache, lease on SID, pub/sub between Keeper instances, leader Reaper ([ADR-006](../adr/0006-cache-redis.md)) | Available online; known `addr` and password |
| **Vault** | PKI for SoulSeed release (mTLS), storing KV secrets (DSN / Redis password / JWT signing-key), persistent backend + auto-unseal | Unsealed, available via HTTPS; you have the rights to create a PKI mount + AppRole (see step 3) |

Prod requirements for each (versions, persistent backend, auto-unseal, least-privilege policy) - normative in [infra.md](infra.md) and [keeper/prod-setup.md → Vault](../keeper/prod-setup.md#vault-approle--persistent--auto-unseal). Here they are already raised by your operations team.

### Ports and firewall

Keeper listens to several listeners on different ports (values from [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) - the operator can change):

| Port | Listener | Protocol | Who's walking |
|---|---|---|---|
| `9442` | `listen.grpc.bootstrap` | server-only TLS | Soul at phase `soul init` (bootstrap token + CSR) |
| `8443` | `listen.grpc.event_stream` | mTLS | Soul on phase `soul run` (long-lived EventStream stream) |
| `8080` | `listen.openapi` | HTTP | Operators (Operator API), health-check (`/readyz`) |
| `8081` | `listen.mcp` | HTTP | MCP clients |
| `9090` | `listen.metrics` | HTTP | Prometheus scrape (`/metrics`) |

On Soul hosts, only metrics-listeners listen outside (`metrics.listen`, by default `127.0.0.1:9091` in [`examples/soul/soul.yml`](../../examples/soul/soul.yml) - locally, not outside). Soul initiates the connection to the Keeper itself ([ADR-002](../adr/0002-transport-grpc-ha.md#adr-002-transport-keeper--souls--grpc-bidirectional-stream-over-mtls-ha-keeper-cluster)).

Firewall rules:

- **On keeper hosts** — open incoming `9442` and `8443` for the subnet of managed hosts; `8080` / `8081` - for operator/MCP network; `9090` - for Prometheus.
- **From keeper hosts to the outside** - access to Postgres / Redis / Vault and (for git resolution of services/plugins) to git hosting.
- **From soul hosts to the outside** - access to keepers on `9442` and `8443`.

Full network topology (LB, L4-probe `8443`, port spacing) - [deployment.md](deployment.md).

### FQDN and SID

`SID` managed host = its **FQDN** (Soul Identity, [soul/identity.md](../soul/identity.md)). `soul init` by default takes the SID from `os.Hostname()`, resulting in lower-case. Requirements:

- FQDN of hosts must resolve (DNS / `/etc/hosts`) and match `^[a-z0-9][a-z0-9.-]{0,253}$` - otherwise `soul init` will fall from `invalid sid`.
- The keeper FQDNs that you put in `soul.yml::keeper.endpoints[].host` must be included in the SAN of the Keeper server certificate (see step 4) - otherwise TLS verification on bootstrap will not work.

## 2. Installing packages

Three packages (`make pkg` builds deb + rpm into `dist/`):

| Package | Where to put | What does it carry |
|---|---|---|
| `soul-stack-keeper` | central node (1+ instance) | `keeper` + systemd-unit + env + example config |
| `soul-stack-soul` | each managed host | `soul` + systemd-unit + env + example config |
| `soul-stack-soul-lint` | operator/CI workstation | `soul-lint` only (CLI, no daemon/config) |

### Keeper (on the central node)

```sh
sudo dpkg -i soul-stack-keeper_<version>_amd64.deb
```

Package ([`deploy/nfpm/keeper.yaml`](../../deploy/nfpm/keeper.yaml)) decomposes:

| Path | What | Note |
|---|---|---|
| `/usr/local/bin/keeper` | binary, `0755` | — |
| `/etc/systemd/system/keeper.service` | systemd-unit | `Type=exec`, `User=soul-stack`, hardening (`ProtectSystem=strict`, single writable `/var/lib/keeper`) |
| `/etc/keeper/keeper.env` | env file, `config|noreplace` | sets `KEEPER_CONFIG=/etc/keeper/keeper.yml`; upgrade doesn't erase it |
| `/etc/keeper/keeper.yml.example` | example config, `0640` | **working config is created by operator** by copying (step 5) |

> **Why `.yml.example` and not `.yml`.** The package deliberately puts the example in a separate name so that `dpkg upgrade` never touches your worker `/etc/keeper/keeper.yml`. You make the primary copy by hand.

### Soul (on each managed host)

```sh
sudo dpkg -i soul-stack-soul_<version>_amd64.deb
```

Package ([`deploy/nfpm/soul.yaml`](../../deploy/nfpm/soul.yaml)) decomposes symmetrically: `/usr/local/bin/soul`, `/etc/systemd/system/soul.service`, `/etc/soul/soul.env` (`SOUL_CONFIG=/etc/soul/soul.yml`), `/etc/soul/soul.yml.example`.

> **Hardening soul unit is softer than keeper.** Soul uses Destiny (installs packages, edits files, manages services) - this requires real privileges on the host, so hard `ProtectSystem=strict` / `MemoryDenyWriteExecute` are **not** set for it ([`deploy/systemd/soul.service`](../../deploy/systemd/soul.service), comment in the header). The only writable path is `/var/lib/soul-stack` (SHA-256 + SoulSeed module cache).

### System user and directories

Both units are running under the system user `soul-stack`. The package pulls `adduser` (deb) as a dependency, but **creation of the user and state directories is up to the operator** (units expect them ready). Once on each host:

**On keeper host:**

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
sudo install -d -o soul-stack -g soul-stack /etc/keeper /var/lib/keeper
```

**On soul host:**

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin soul-stack
sudo install -d -o soul-stack -g soul-stack /etc/soul /var/lib/soul-stack
```

(Specific commands are in the [`deploy/systemd/keeper.service`](../../deploy/systemd/keeper.service) and [`deploy/systemd/soul.service`](../../deploy/systemd/soul.service) headers.)

`soul-lint` - just a CLI without a daemon, installed by one `dpkg -i`, does not require any configuration.

## 3. Vault provisioning

The operator performs these steps **on his product Vault** (under a token/policy with rights to mounts). The commands have been transferred from real dev provisioning [`dev/provision.sh`](../../dev/provision.sh) to prod - in dev there is a dev-mode Vault and a root token, in prod the same operations are done under admin access to Vault with persistent backend and auto-unseal.

> **dev↔cont.** In dev `provision.sh` writes secrets under the `root` token in dev-mode (secrets in RAM). In production: persistent backend, auto-unseal, narrow least-privilege policy for Keeper itself ([`examples/keeper/vault-policy.hcl`](../../examples/keeper/vault-policy.hcl)). Provisioning below is a one-time admin operation, separate from Keeper runtime access.

### 3.1. KV secrets

Keeper resolves Postgres DSN, Redis password and JWT signing-key via `vault:`-refs from the config. Write them to KV (mount `secret/`):

```sh
# DSN of external Postgres operator (field `dsn`)
vault kv put secret/keeper/postgres \
  dsn="postgres://keeper:<password>@pg.internal:5432/keeper?sslmode=require"

# External Redis operator password (field pointed to by redis.password_ref)
vault kv put secret/keeper/redis \
  password="<redis-password>"

# JWT signing-key operators (field `signing_key`) - 32 bytes of random, base64.
# Generate ONCE and commit: changing the key invalidates all live JWTs.
vault kv put secret/keeper/jwt-signing-key \
  signing_key="$(openssl rand -base64 32)"
```

> **JWT signing-key should not be regenerated.** If the key has already been written down, do not touch it. Any change invalidates all previously issued operator JWTs (the signature will not match). Rotation is a separate planned procedure ([keeper/prod-setup.md → Rotation signing-key](../keeper/prod-setup.md)).

### 3.2. PKI: mount + root + role `soul-seed`

PKI issues SoulSeed certificates (mTLS Soul ↔ Keeper pairs). In [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml) mount is `pki/soulstack`; Below, for example, we use `pki/` (as in dev), correct the paths to suit your mount.

```sh
# 1. Enable PKI-engine and raise max-lease-ttl
vault secrets enable -path=pki pki
vault secrets tune -max-lease-ttl=87600h pki

# 2. Generate a root certificate (CN - common trust anchor for all SoulSeed
#    and Keeper server cert; see step 4)
vault write pki/root/generate/internal \
  common_name="soul-stack" ttl=87600h

# 3. Soul-seed role: domains/SANs allowed for issued certificates.
#    allowed_domains - for the FQDN scheme of your hosts (NOT example.com/test from dev).
vault write pki/roles/soul-seed \
  allowed_domains="internal,<your-domain>" \
  allow_subdomains=true \
  max_ttl=720h
```

### 3.3. AppRole for Keeper runtime access

In production, Keeper authenticates to Vault via an AppRole (not a root token). Create a role by binding least-privilege policy:

```sh
# Narrow policy (template with commented paths - examples/keeper/vault-policy.hcl)
vault policy write keeper-prod examples/keeper/vault-policy.hcl

# Role with policy binding
vault write auth/approle/role/keeper-prod \
  token_policies=keeper-prod \
  secret_id_ttl=720h token_ttl=1h token_max_ttl=24h

# role_id - NOT a secret, will go to keeper.yml::vault.auth.role_id
vault read auth/approle/role/keeper-prod/role-id

# secret_id — SECRET, put mode 0400 in file (step 5)
vault write -f auth/approle/role/keeper-prod/secret-id
```

`role_id` - role identifier, not secret (stored openly in `keeper.yml`). `secret_id` - secret, **not stored in the plaintext config**, source - local file `secret_id_file` (`mode 0400`) or env `secret_id_env`. AppRole-credentials are intentionally NOT read from Vault (chicken-egg: this is what Keeper logs in with). Contract - [keeper/prod-setup.md → AppRole](../keeper/prod-setup.md).

## 4. Keeper TLS material

This is the **neatest place onboarding** - described explicitly because two independent chains of trust converge here.

### What to put

Keeper listens to both gRPC listeners (bootstrap `9442` and event_stream `8443`) with the server certificate. According to [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml), the files are located in `/etc/keeper/tls/`:

| File | Role |
|---|---|
| `/etc/keeper/tls/server.crt` | server leaf-cert Keeper (presented on bootstrap + event_stream) |
| `/etc/keeper/tls/server.key` | leaf's private key |
| `/etc/keeper/tls/ca.crt` | CA for validating **client** SoulSeed certificates on mTLS event_stream (`event_stream.tls.ca` = ClientCAs) |

### Why one PKI root for everything

Keeper's server cert **must cling to the same PKI root** as SoulSeed certificates. Otherwise, on EventStream (mTLS), Soul does not trust Keeper's server cert, and Keeper does not trust Soul's client cert. Therefore, the server leaf Keeper is released from the same role `pki/issue/soul-seed` as SoulSeed (see comment in [`dev/provision.sh`](../../dev/provision.sh), function `issue_keeper_cert`).

### Release Procedure

Release leaf from your Vault PKI and distribute it into files (by analogy with `issue_keeper_cert` in [`dev/provision.sh`](../../dev/provision.sh), transferred to the keeper's prod-FQDN; `keeper.internal` is an example, substitute the real FQDN under which Souls will address this Keeper in `soul.yml::keeper.endpoints[].host`):

```sh
vault write -format=json pki/issue/soul-seed \
  common_name="keeper.internal" \
  alt_names="keeper.internal" \
  ip_sans="10.0.0.10" \
  ttl=720h > /tmp/keeper-issue.json
```

From the JSON response, decompose the three fields into files (`certificate` → `server.crt`, `private_key` → `server.key`, `issuing_ca` → `ca.crt`) and set the permissions:

```sh
sudo install -d -o soul-stack -g soul-stack -m 0750 /etc/keeper/tls
# certificate → server.crt, private_key → server.key, issuing_ca → ca.crt
sudo install -o soul-stack -g soul-stack -m 0640 server.crt /etc/keeper/tls/server.crt
sudo install -o soul-stack -g soul-stack -m 0600 server.key /etc/keeper/tls/server.key
sudo install -o soul-stack -g soul-stack -m 0640 ca.crt    /etc/keeper/tls/ca.crt
```

> **SAN is required.** The server cert must contain in the SAN the FQDN (or IP) that Souls put in `keeper.endpoints[].host` - Soul checks the hostname of the server cert in the bootstrap phase. Mismatch → `certificate validation failed` (see step 9).

> **Rotation.** Leaf expires at `ttl` (above - 720h). Re-release - repeat this procedure + restart the keeper; The CA root does not change, so already onboarded Souls are not affected. The rotation policy is on the side of the PKI operations team.

## 5. Config `keeper.yml`

Copy the example to the working path and fill in:

```sh
sudo cp /etc/keeper/keeper.yml.example /etc/keeper/keeper.yml
sudo chown soul-stack:soul-stack /etc/keeper/keeper.yml
sudo chmod 0640 /etc/keeper/keeper.yml
```

Required blocks to check/edit (full example - [`examples/keeper/keeper.yml`](../../examples/keeper/keeper.yml), regulatory contract - [keeper/config.md](../keeper/config.md)):

```yaml
# Instance identity is unique in the cluster (several keepers = different kids)
kid: keeper-eu-west-01

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

# External operator storage - via vault:-ref (values are set in step 3.1)
postgres:
  dsn_ref: vault:secret/keeper/postgres
redis:
  addr: "redis.internal:6379"
  password_ref: vault:secret/keeper/redis

# Vault - AppRole (step 3.3) + PKI-mount (step 3.2)
vault:
  addr: "https://vault.internal:8200"
  auth:
    method: approle
    role_id: keeper-prod                        # role_id from step 3.3 (not a secret)
    secret_id_file: /etc/keeper/vault-secret-id # secret_id, file mode 0400
  pki_mount: "pki/soulstack"                    # your PKI-mount (in dev - pki/)

# JWT operators (signing-key was set in step 3.1)
auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
    ttl_default: 24h
    ttl_bootstrap: 30d
```

Put `secret_id` (from step 3.3) in the file pointed to by `secret_id_file`:

```sh
# output `vault write -f auth/approle/role/keeper-prod/secret-id` → secret_id field
echo -n "<secret_id>" | sudo tee /etc/keeper/vault-secret-id >/dev/null
sudo chown soul-stack:soul-stack /etc/keeper/vault-secret-id
sudo chmod 0400 /etc/keeper/vault-secret-id
```

> The service registry and RBAC directory in `keeper.yml` **are not configurable** - they live in Postgres and are managed via the Operator API / MCP after startup ([ADR-029](../adr/0029-service-registry.md), [ADR-028](../adr/0028-rbac-storage.md)). The appearance of the `services:` / `rbac:` keys in the config is rejected as `unknown_key`.

Before starting, you can check the path to the config in the env file `/etc/keeper/keeper.env` (`KEEPER_CONFIG=/etc/keeper/keeper.yml`) - the unit reads the path from there.

## 6. Launch Keeper

Directories and user have already been created (step 2). Enable and run:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now keeper
```

Check:

```sh
systemctl status keeper
journalctl -u keeper -n 100 --no-pager
# health-check (listener openapi.addr): 200 when ready to receive traffic
curl -fsS http://127.0.0.1:8080/readyz && echo OK
```

`/readyz` depends on Postgres + Redis (and is written again upon restart) - 200 means that the instance is ready to receive traffic. For an L4 balancer, a TCP probe of port `8443` ([deployment.md](deployment.md)) is sufficient.

> **Keeper will refuse to start** if the registry `operators` is empty and not passed to `--initialize`: `operators registry is empty; run 'keeper init …'`. This is normal on the very first launch - follow step 7. Restart semantics (`--initialize` / `KEEPER_INITIALIZE`) - [bootstrap-rbac.md → Restart semantics](bootstrap-rbac.md).

## 7. Bootstrap of the first Archon

The operator registry in the fresh database is empty - all APIs will return 403 until the first Archon is created. Bootstrap is an administrative subcommand of the `keeper` binary itself (not a separate mode). Runs **once on one instance** ([ADR-013](../adr/0013-bootstrap-archon.md)):

```sh
sudo -u soul-stack keeper init \
  --archon=archon-alice \
  --config=/etc/keeper/keeper.yml \
  --credential-out=/etc/keeper/archon-alice.jwt
```

What's happening ([bootstrap-rbac.md → `keeper init`](bootstrap-rbac.md)): under PG advisory lock it is checked that `operators` is empty; the first Archon is created with the role `cluster-admin` (`permissions: ["*"]`); is issued JWT (TTL = `auth.jwt.ttl_bootstrap`, default 30 days) and written to `--credential-out` with `mode 0400`.

> **AID format.** `--archon` - kebab-case `^archon-[a-z0-9-]{1,62}$` (`archon-alice`, `archon-ops-01`). See [naming-rules.md](../naming-rules.md).

> **Bootstrap-JWT storage.** The `--credential-out` file is the raw material for the first setup, not long-term storage. Immediately transfer to the password manager / Vault operator and **do not leave** in `/etc/keeper/` for a long time: this is an admin-credential with `*` rights. Further Archons (for people, CI, machine-identity) can be created via the Operator API - [bootstrap-rbac.md → Release of additional Archons](bootstrap-rbac.md).

After this, Keeper starts normally (if it was launched with `--initialize` in read-only mode, it will start servicing the API; otherwise, `systemctl restart keeper`).

Save the token into a variable for the next steps:

```sh
TOKEN=$(sudo cat /etc/keeper/archon-alice.jwt)
```

## 8. Onboarding Soul

Soul onboarding is two-way: the operator registers the host in Keeper and receives a one-time bootstrap token; on the host `soul init` exchanges the token + CSR for SoulSeed (mTLS pair). Full mechanics - [soul/onboarding.md](../soul/onboarding.md).

### 8.1. Host registration and token issuance

On the Keeper side (via Operator API; SID = FQDN of the future Soul host):

```sh
curl -s -X POST http://keeper.internal:8080/v1/souls \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sid": "host01.dc1.internal", "covens": ["demo"], "transport": "agent"}'
```

> **`transport` is required.** The `transport` field must be `agent` (onboarding daemon, pull mode) or `ssh` (SSH push mode). If absent, 422 Unprocessable Entity will be returned. In the example above - `agent` (Soul daemon initiates a connection to Keeper).

The entry in `souls` appears in status `pending`; in the response **one time** a plain bootstrap token is returned (one-time, default TTL 24h). Lost - cannot be restored, only re-issued via `POST /v1/souls/{sid}/issue-token` ([onboarding.md → Recovery](../soul/onboarding.md)).

### 8.2. Trust material on the host: how Soul trusts Keeper up to `soul init`

Before `soul init` Soul does not yet have SoulSeed, but the bootstrap phase (`9442`) goes via **server-only TLS** - Soul is required to check the Keeper's server cert. Trust here is established **not TOFU**, but by explicit preloading of the CA: Soul verifies the Keeper server cert against the CA file from `soul.yml::keeper.tls.ca` (in the code - `bootstrap.Run` → `tlsx.LoadClientTLS{CAPath: keeper.tls.ca}`, [`soul/internal/bootstrap/bootstrap.go`](../../soul/internal/bootstrap/bootstrap.go)). If the file is empty/not specified, `soul init` falls from `keeper.tls.ca is empty`.

Therefore **the operator is obliged to put a PKI root** on the host in advance** (the same `ca.crt` / `issuing_ca` as the Keeper server cert, step 4) along the path from `soul.yml::keeper.tls.ca`:

```sh
sudo install -d -o soul-stack -g soul-stack -m 0750 /var/lib/soul-stack/seed
# CA from step 4 (issuing_ca PKI root) - for example, copy ca.crt from the keeper host
sudo install -o soul-stack -g soul-stack -m 0644 ca.crt /var/lib/soul-stack/seed/ca.crt
```

> **Two chains of trust - two roles of one CA.** `keeper.tls.ca` (preloaded file) validates the Keeper server cert in the **bootstrap** phase. After a successful `soul init`, Keeper returns a PKI chain (`BootstrapReply.ca_chain_pem`), which Soul stores in the SoulSeed directory (`paths.seed/current/ca.pem`) and uses to verify the server in the **EventStream** phase. These are different files, both from the same PKI root. The preloaded `ca.crt` is needed only before the first `soul init`; Further, Soul relies on seed-CA.

### 8.3. Config `soul.yml` and token delivery

Copy the example and fill in:

```sh
sudo cp /etc/soul/soul.yml.example /etc/soul/soul.yml
sudo chown soul-stack:soul-stack /etc/soul/soul.yml
sudo chmod 0640 /etc/soul/soul.yml
```

Minimum that can be corrected (full contract - [soul/config.md](../soul/config.md)):

```yaml
keeper:
  endpoints:
    - host: keeper.internal       # FQDN from SAN server cert (step 4)
      event_stream_port: 8443     # mTLS, phase `soul run`
      bootstrap_port: 9442        # server-only TLS, phase `soul init`
  tls:
    ca: /var/lib/soul-stack/seed/ca.crt   # preloaded in step 8.2

paths:
  seed: /var/lib/soul-stack/seed          # here `soul init` will put SoulSeed
```

> `event_stream_port` and `bootstrap_port` **both are required** and both are explicit - there is no silent leaving of bootstrap to the event-stream port ([ADR-012(b)](../adr/0012-keeper-soul-grpc.md), [config.md → keeper.endpoints](../soul/config.md)). Multiple keepers are listed as multiple `endpoints[]` entries with `priority`.

**Token delivery.** The method of physical delivery of the bootstrap token to the host is the choice of operator ([onboarding.md → Delivery methods](../soul/onboarding.md)): script step `core.bootstrap.delivered`, Ansible-role, SSH/SCP, CI/CD, cloud-init. Security Advisory: token file `mode 0400` owner `soul-stack`, directory `mode 0700`; on systemd ≥ 250 - `LoadCredential=` (token in tmpfs, not to disk).

### 8.4. `soul init` - token exchange for SoulSeed

`soul init` takes the token from the `--token` flag or from the env `SOUL_BOOTSTRAP_TOKEN` (flag beats env; stdin is not readable). The Env form is preferable - the value `--token` would appear in `ps`/shell-history:

```sh
sudo -u soul-stack sh -c 'SOUL_BOOTSTRAP_TOKEN="$(cat /run/soul-bootstrap-token)" soul init --config=/etc/soul/soul.yml'
```

Command: determines SID (= FQDN), generates private key + CSR (key **never leaves host**), connects to Keeper's bootstrap-listener, presents token + CSR, gets signed SoulSeed and atomically distributes it to `paths.seed`. If SoulSeed is already available, `init` crashes (protection against accidental re-release); re-release is a separate procedure ([identity.md → SoulSeed Rotation](../soul/identity.md)).

### 8.5. Start the daemon and verify

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now soul
systemctl status soul
journalctl -u soul -n 100 --no-pager
```

Soul initiates an EventStream stream to Keeper (mTLS, port `8443`). Check that the host has moved to `connected` from the Keeper side:

```sh
curl -s http://keeper.internal:8080/v1/souls/host01.dc1.internal \
  -H "Authorization: Bearer $TOKEN"
# response contains status: connected
```

After this, the host is ready to receive Destiny. Building the first end-to-end service - [guides/first-service.md](../guides/first-service.md).

## 9. Troubleshooting

| Symptom | Probable Cause | What to check |
|---|---|---|
| `soul init`: **connection refused** | Keeper does not listen to the bootstrap port / firewall cuts `9442` | Keeper is running (`systemctl status keeper`); incoming `9442` from the soul host is open; `host`/`bootstrap_port` in `soul.yml` point to the correct keeper |
| `soul init`: **certificate validation failed** / x509 hostname mismatch | preloaded `keeper.tls.ca` from a different PKI root than the server cert; or FQDN of the keeper and not in the SAN of the server cert | `keeper.tls.ca` = `issuing_ca` PKI (step 8.2); FQDN from `endpoints[].host` is included in the SAN of the server cert (step 4) |
| `soul init`: **keeper.tls.ca is empty** | CA file not specified/preloaded | fill `keeper.tls.ca` into `soul.yml` and put the file (step 8.2) |
| `soul init`: **bootstrap token invalid / expired / used** (403) | token is burned (already used), expired (TTL 24h) or SID does not match | reissue token `POST /v1/souls/{sid}/issue-token` (`force` if active); verify SID = host FQDN |
| `soul init`: **invalid sid** | FQDN does not match `^[a-z0-9][a-z0-9.-]{0,253}$` | set hostname to a valid lower-case FQDN or set `sid:` explicitly to `soul.yml` |
| Keeper at startup: **Vault unreachable** / sealed | Vault unavailable via HTTPS, sealed, or invalid AppRole-credentials | Vault is unsealed and accessible by `vault.addr`; `secret_id_file` exists with `mode 0400`; `role_id`/policy in place (step 3.3) |
| Keeper: **operators registry is empty; refusing to start** | first launch without bootstrap | run `keeper init` (step 7) |
| Keeper does not start, **apply migrations** in logs | no DDL rights / Postgres unavailable | role in DSN has DDL rights; Postgres is reachable through `dsn_ref` |
| Soul starts, but `status` remains `pending`/not `connected` | EventStream phase fails (mTLS on `8443`) | incoming `8443` is open; SoulSeed is laid out (`paths.seed/current/`); server cert and SoulSeed are from one PKI root (step 4) |

Extended set of incidents and metrics - [faq.md](faq.md) and [monitoring.md](monitoring.md).

## 10. Upgrade

Packages are updated with the usual `dpkg -i` new version. What is important to know:

- **Working configs are not overwritten.** `*.yml.example` comes from the package, but your `/etc/keeper/keeper.yml` and `/etc/soul/soul.yml` were created by you - upgrade does not affect them. Env files are marked `config|noreplace`. After the upgrade, you should check your config with the new `*.yml.example` for new keys.
- **DDL migrations of the Keeper database schema** are applied idempotently when Keeper starts (on `keeper run`, as well as in `keeper init` before bootstrap - automatic rollover when restarting the daemon of a new version, `migrate.Apply` in daemon.go:600). Before upgrading Keeper, backup Postgres and check the changelog for breaking migration.
- **state_schema-incarnation migrations** (ADR-019) is a **separate** operator-initiated operation via the Operator API (`POST /v1/incarnations/{name}/upgrade`), forward-only, does not start automatically when Keeper is restarted. Not to be confused with database schema migrations.
- **Hot-reload configuration.** Part of the `keeper.yml` keys is reread without restart; some require a restart (TLS files, listeners, subsystems start once). The "hot-reload/restart-required" map for each key is [keeper/config.md](../keeper/config.md).

The complete procedure for a rolling-upgrade Keeper cluster without downtime (drain LB, one instance at a time, verify metrics) and Soul fleet is standard in [upgrade.md](upgrade.md). Rollback package: `dpkg -i` previous version + `systemctl restart`; Please note that past state_schema migrations are not rolled back ([upgrade.md → Rollback](upgrade.md)).
