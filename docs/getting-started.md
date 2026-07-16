# Getting Started - raise Soul Stack in ~30 minutes

Quickstart for an operator who is new to the Soul Stack: one Keeper, required infrastructure locally (Postgres + Redis + Vault), first Archon, one onboarded Soul and the applied script `hello-world`. Step by step, with commands.

This is a **local demo setup** for acquaintance, not a production installation. Product deployment (HA, managed infrastructure, persistent Vault, TLS material, systemd) - [docs/operations/deployment.md](operations/deployment.md). What is not included in beta - [known-limitations.md](known-limitations.md).

## What you need

| Tool | Why |
|---|---|
| Go 1.26+ | build binaries `keeper` / `soul` from sources |
| Docker + docker compose | raise Postgres/Redis/Vault/OTel locally (`dev/docker-compose.yml`) |
| `curl`, `git`, `openssl` | Operator API calls, service repo materialization, key generation |
| `make` | dev-targets (`make dev-up` / `make build` / `make dev-smoke`) |

All commands are from the root of the repository.

## Step 0. Get sources

There are no binary releases in beta distribution - Soul Stack is compiled from source code. Access to the code at this stage is **by invite to a private GitHub repository** (how to get an invite - [SUPPORT.md](../SUPPORT.md)). After accepting the invite:

```sh
git clone git@github.com:<org>/soul-stack.git
cd soul-stack
```

Building binaries - `make build` (step 3). The version of the assembled binary is printed by `keeper version` (format `keeper <version> (<go-runtime>)` - the version is injected by the linker at `make build`):

```sh
./keeper/bin/keeper version
```

## Step 1. Infra: Postgres + Redis + Vault

The required outline of a Keeper cluster is three components: PostgreSQL + Redis + Vault ([ADR-053](adr/0053-dependency-tiers.md)). All three are checked at the start; Without any of them, Keeper will not start (fail-fast).

The local dev-stack is raised by one command - it launches `dev/docker-compose.yml` (Postgres on `:5434`, Vault dev-mode on `:8200`, Redis on `:6381`, plus OTel-collector + Jaeger):

```sh
make dev-up
```

> **Vault here is dev-mode** (in-memory, auto-unseal, HTTP without TLS, root-token `root`). Suitable **only** for local use - data is lost when the container is restarted. Prod requires persistent storage + auto-unseal + TLS, see [operations/infra.md → Vault](operations/infra.md#vault).

## Step 2. Provisioning secrets and TLS

Keeper reads Postgres DSN, JWT signing-key and issues SoulSeed certificates through Vault. Local provisioning (recording KV secrets, enabling PKI engine, releasing self-issued Keeper TLS material) is done by one idempotent script:

```sh
make dev-provision
```

What does it put (details - [dev/provision.sh](../dev/provision.sh)):

- `secret/keeper/postgres` (field `dsn`), `secret/keeper/jwt-signing-key`, PKI-engine `pki/` with role `soul-seed`;
- Keeper's TLS material in `/tmp/keeper-dev/tls/` (leaf-cert + root Vault-CA, which SoulSeed later clings to).

Restarting is safe - each step checks its state.

## Step 3. Collect binaries

```sh
make build
```

Result - `keeper/bin/keeper`, `soul/bin/soul`, `soul-lint/bin/soul-lint`.

## Step 4. Bootstrap of the first Archon

**Archon** - Soul Stack Operator (Species ID `archon-<name>`). When first initialized, the operator registry is empty, and without bootstrap any API call will return `403` (default-deny). Bootstrap - administrative subcommand `keeper init` ([ADR-013](adr/0013-bootstrap-archon.md), [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md)):

```sh
./keeper/bin/keeper init \
  --archon=archon-alice \
  --config=dev/keeper.dev.yml \
  --credential-out=/tmp/keeper-dev/archon-alice.jwt
```

What happens: under PG advisory lock, it is checked that the operator registry is empty, the first Archon with the role `cluster-admin` (permissions `["*"]`) is created, a JWT (bootstrap-TTL) is issued, the JWT is written to `--credential-out` from `mode 0400`. The same command applies the database (migration) scheme idempotently.

> File `archon-alice.jwt` - admin-credential with rights `*`. In a real installation, it is immediately hidden in the password manager / Vault and revoked after setting up the remaining operators.

After bootstrap, the service registry is still empty. Reprovisioning seeds it with demo records (see [Step 7, dev-shortcut](#step-7-apply-apply-script-hello-world)):

```sh
make dev-provision
```

## Step 5. Launch Keeper

`make dev-keeper` raises Keeper with a verified dev environment (writable cache directories for resolving service artifacts, `file://` repo are allowed), waits for `healthz`:

```sh
make dev-keeper
```

Readiness check:

```sh
curl -s http://127.0.0.1:8080/healthz        # → ok
```

Dev-Keeper listening ports (`dev/keeper.dev.yml`): OpenAPI `:8080`, MCP `:8081`, metrics `:9090`, Bootstrap-RPC `:9442`, EventStream `:9443`. Mapping of ports and listeners in production - [operations/deployment.md → Network ports](operations/deployment.md).

> **View the API in the browser - `GET /docs`** (RapiDoc viewer, [ADR-054](adr/0054-openapi-code-first.md)). Open `http://127.0.0.1:8080/docs`: the page shows the Archon JWT input field - insert the token (see below) and it will load the full spec with endpoint search (full-text) and a "Try It" button. The spec itself (`GET /openapi.json`) is behind the JWT - without a token, only the input field is visible, the API surface is not exposed. The token lives only in the current tab (session storage) and is not saved persistently.

Convenient alias: put the Archon token for API calls in the environment variable.

```sh
TOKEN=$(cat /tmp/keeper-dev/archon-alice.jwt)
```

## Step 6. Onboard one Soul

**Soul** is an agent on a managed host. Onboarding - via CSR: the private key is generated on the host and never leaves it ([ADR-002](adr/0002-transport-grpc-ha.md), [soul/onboarding.md](soul/onboarding.md)). Flow in two steps: the operator registers the host and receives a one-time bootstrap token, then `soul init` on the host exchanges the token + CSR for a SoulSeed certificate.

### 6.1. Register a host (get a bootstrap token)

`SID` Soul = Host FQDN. For local demo, we use the name of the current machine (or any FQDN from `example.com` - it is covered by the PKI role `soul-seed`):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/souls \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"sid": "host-01.example.com", "transport": "agent", "covens": ["demo"]}'
```

The response contains `bootstrap_token` (**returned once**), `expires_at` (default TTL 24h). `covens: ["demo"]` is a stable host label that can later be used to target scripts. A lost token cannot be restored - only reissued via `POST /v1/souls/{sid}/issue-token` ([soul/onboarding.md → Recovery](soul/onboarding.md)).

### 6.2. Apply token on host (`soul init`)

`soul init` takes a token from the `--token` flag or from the env `SOUL_BOOTSTRAP_TOKEN` (the flag defeats the env; the env form is safer - the flag value is displayed in `ps`/shell-history), generates a private key + CSR, connects to Keeper's Bootstrap-listener and lays out the received SoulSeed. You need `soul.yml` with the Keeper's address and the path to the trusted CA.

Minimum `soul.yml` for local demo (CA - root Vault-CA from step 2):

```yaml
sid: host-01.example.com
paths:
  modules: /tmp/soul-demo/modules
  seed:    /tmp/soul-demo/seed
keeper:
  endpoints:
    - host: 127.0.0.1
      event_stream_port: 9443
      bootstrap_port: 9442
      priority: 1
  tls:
    ca: /tmp/keeper-dev/tls/vault-ca.crt
```

Full contract `soul.yml` - [soul/config.md](soul/config.md). Onboarding:

```sh
SOUL_BOOTSTRAP_TOKEN='<bootstrap_token from 6.1>' ./soul/bin/soul init --config /tmp/soul-demo/soul.yml
```

If successful, SoulSeed (cert/key/ca) is expanded to `paths.seed`, the entry in `souls` goes to `pending → connected`. Start the daemon (holds EventStream to Keeper):

```sh
./soul/bin/soul run --config /tmp/soul-demo/soul.yml &
```

Checking - the host is visible as `connected`:

```sh
curl -s http://127.0.0.1:8080/v1/souls -H "Authorization: Bearer $TOKEN"
```

> **Method of delivering the token and binary to the real host** - operator choice (SSH/SCP, cloud-init, CI, script step `core.bootstrap.delivered`). List - [soul/onboarding.md → Delivery methods](soul/onboarding.md).

## Step 7. Apply: apply script `hello-world`

What we use: **service** `hello-world` ([examples/service/hello-world/](../examples/service/hello-world/)) - a minimal service with the script `create`, which writes the greeting file `/tmp/soul-stack-hello` on each incarnation host and fixes the path to `incarnation.state`.

For Keeper to resolve a service, it must be in the service registry (git source + ref). In production it is `POST /v1/services`:

```sh
curl -s -X POST http://127.0.0.1:8080/v1/services \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "hello-world", "git": "https://git.internal/svc/hello-world.git", "ref": "main"}'
```

> **Dev-shortcut.** For local dating, keeping a git repo is inconvenient. `make dev-provision` (step 2) already materializes `hello-world` as a local `file://` repo and seeds the service registry with demo entries - a separate `POST /v1/services` is then not needed. This is **dev-only**: `file://`-resolve is enabled by the `SOUL_STACK_ALLOW_FILE_REPOS=1` flag, which sets `make dev-keeper`; in production, the source of the service is a real git-URL.

Create incarnation - runs the `create` service script on the hosts. Let's bind to coven `demo` (our Soul from step 6 is there):

```sh
curl -s -X POST http://127.0.0.1:8080/v1/incarnations \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "hello-demo",
    "service": "hello-world",
    "covens": ["demo"],
    "input": { "greeting": "hello from soul-stack" }
  }'
```

Reply `202 Accepted` with `apply_id` - the operation is asynchronous ([operator-api/incarnations.md → POST /v1/incarnations](keeper/operator-api/incarnations.md)).

## Step 8. See the result

Query incarnation status (`applying` → `ready` on success, `error_locked` on failure):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo -H "Authorization: Bearer $TOKEN"
```

When `status: ready` - `incarnation.state.greeting_file` points to the created path. Directly on the host:

```sh
cat /tmp/soul-stack-hello        # → hello from soul-stack
```

Run history (snapshots in `state_history`):

```sh
curl -s http://127.0.0.1:8080/v1/incarnations/hello-demo/history -H "Authorization: Bearer $TOKEN"
```

## All-in-one (dev-smoke)

Steps 1-4 + registry seed are automated with one command (`make dev-smoke` = `dev-up` → `dev-provision` → build keeper → `keeper init` → repeat `dev-provision` for registry seed):

```sh
make dev-smoke      # infra + provisioning + bootstrap of the first Archon + seed demo services
make dev-keeper     # launch Keeper
```

Next is onboarding Soul (step 6) and apply (steps 7–8). Full development of the dev stand (including Web-UI from the companion repository) - `make dev-stand`; the token for ad-hoc calls is `TOKEN=$(make dev-jwt)`.

## Troubleshooting first launch

Typical stumbles in a demo setup and what to do about them. Product problems - [operations/infra.md](operations/infra.md).

- **After the restart, secrets disappeared / Keeper crashes at the start of "cannot read postgres dsn".** Dev-Vault is in-memory dev-mode (step 1): data lives only as long as the container is alive, restart `make dev-up` erases KV/PKI. It can be cured by repeating `make dev-provision` (step 2) - it idempotently overwrites `secret/keeper/*` and the PKI role. For persistent storage you need prod-Vault (persistent storage + auto-unseal), see [operations/infra.md → Vault](operations/infra.md#vault).
- **`soul init` crashes on TLS / "certificate is not valid for host".** `SID` Soul = FQDN (step 6.1), and the PKI role `soul-seed` writes SoulSeed only to names from its allowed domain (in the demo - `*.example.com`). Use the FQDN from this domain (`host-01.example.com`), rather than a short hostname or a random name. The CA in `soul.yml` (`keeper.tls.ca`) must point to the root Vault-CA from step 2 (`/tmp/keeper-dev/tls/vault-ca.crt`) - otherwise Soul does not trust the Keeper chain.
- **`422 Unprocessable Entity` on the API call.** This is schema validation of the request (huma): the body/queries did not pass the contract - unknown field, value outside the enum, format violation. The response carries `detail` with a specific field path; check the payload with the spec in `/docs` (valid, not guessing). This is not a server bug - 422 is sent before the business logic.
- **API returned `401` although the token "is".** JWT is short-lived and **there is no immediate revocation of the token content** - a revoked Archon works until `exp`, and an expired token simply stops being accepted. If you received `401`, issue a fresh token (bootstrap-JWT reissue - repeated `keeper init` is not available after the first Archon; for dev - `TOKEN=$(make dev-jwt)`; in production - through `POST /v1/operators/...`, see [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md)).
- **`soul init` has passed, but the host has not yet `connected` to `GET /v1/souls`.** The `pending → connected` transition is not instantaneous: the status in the registry is a snapshot `souls.status`, which Keeper combines with the background presence fact (Reaper rule `mark_disconnected`, lease-aware reconcile - [keeper/reaper.md](keeper/reaper.md)). After `soul run` wait a few seconds and poll again. The authority of "Soul online" is a live stream (Redis-lease), the photo catches up with it with a lag - this is by-design, not freezing.
- **WSL2-rake** (if you run the demo under Windows/WSL2):
  - `ETXTBSY` when `make build` / binary is launched - the binary file is held by another process. Stop the previously running `keeper`/`soul` (`pkill -x keeper`, `pkill -x soul` - exactly `-x`, the exact name, not `-f`) and rebuild.
  - Docker infrastructure is not up - check that **Docker Desktop is running** and WSL2 integration is enabled; without it, `make dev-up` will not find the docker daemon.
  - After reboot, `/tmp/keeper-dev/*` (JWT, TLS material) disappeared - WSL2 clears `/tmp` at startup. Run `make dev-smoke` again (it recreates bootstrap-Archon and provisioning).

If the behavior does not match what is described and looks like a bug, create a **GitHub Issue** (template "Bug report", how and where - [SUPPORT.md](../SUPPORT.md)). Beta support - best-effort, no SLA.

## What's next

- [known-limitations.md](known-limitations.md) - what is not included in beta (cloud-provisioning, MCP cadence coverage, audit-scaling).
- [operations/deployment.md](operations/deployment.md) — prod rollout: HA multi-keeper, managed-infra, systemd, deb/rpm.
- [operations/bootstrap-rbac.md](operations/bootstrap-rbac.md) - second+ Archon, roles, permission lines, scope.
- [operations/infra.md](operations/infra.md) — advanced configuration of Postgres / Redis / Vault, backup/restore.
- [scenario/](scenario/README.md) and [destiny/](destiny/README.md) - how to write your own scenarios and Destiny.
- [keeper/operator-api.md](keeper/operator-api.md) - complete Operator API.
