# soul-legion - Soul Stack load generator

Test-only tool (NOT a shipped binary - [ADR-004](../../docs/adr/0004-binaries.md)
only pins `keeper`/`soul`/`soul-lint`). Spins up N concurrent
fake-Soul streams (gRPC bidi over mTLS `EventStream`) against a live Keeper and measures
load **on the Keeper**, not the realism of apply on the host.

Normative plan, methodology and measured numbers - [docs/testing/load-testing.md](../../docs/testing/load-testing.md).
This README is only about running it.

## Running it: `make stress`

From the repo root, against a **running dev stand**:

```sh
make stress                          # axis A: 1000 connections, cleanup
make stress COUNT=500 API=1 VOYAGE=1 # + axis B (API) + axis C (single Voyage)
make stress COUNT=2000 RAMP=500 DURATION=60s
```

The target builds the binary itself (`tests/load/bin/soul-legion`); with `API=1`/`VOYAGE=1`
it mints an admin JWT using the same mechanism as `make dev-jwt` (`dev/mint-jwt.sh`, key from
Vault - not duplicated), runs soul-legion, and cleans the legion out of the registry
(`--cleanup`, the stand stays clean).

`load-test` is an alias for `stress`.

### Precondition

Requires a running dev stand: Keeper (event-stream `:9443`, metrics `:9090`, openapi
`:8080`) + dev-PKI (`/tmp/keeper-dev/tls/vault-ca.crt`) + PG/Redis/Vault from
docker-compose. The target checks `/healthz` before building; if unavailable it
suggests `make dev-stand` (full stand-up) or `make dev-keeper` (keeper only).

### What it measures

- **Axis A (always):** mass of EventStream streams - achieved-N, connect latency
  p50/p99/max, stream retention, drain (stream/goroutine leaks), Keeper RSS/goroutines/FD
  with extrapolation per [scaling.md](../../docs/operations/scaling.md).
- **Axis B (`API=1`):** concurrent run of `GET /v1/souls` + `POST /v1/voyages/preview`
  against the legion - RPS, latency, errors.
- **Axis C (`VOYAGE=1`):** a single command-Voyage against the legion's `coven` - create- and
  end-to-end latency, outcome, audit-INSERT rate.

Some quantities have no metric (Redis lease, PG claim/audit-INSERT, Conclave
live-count) - soul-legion prints ready-made CLI commands for manual measurement from outside
(see [load-testing.md §4.2](../../docs/testing/load-testing.md#42-observational-gaps---no-metrics-measure-from-outside-in-the-1st-phase)).

## `make stress` env variables

| Variable | Default | Purpose |
|---|---|---|
| `COUNT` | `1000` | number of fake-Soul streams (axis A) |
| `RAMP` | `250` | streams per ramp step (0 -> all at once) |
| `RAMP_INTERVAL` | `300ms` | pause between steps |
| `DURATION` | `30s` | how long to hold streams after full ramp (axis A without B/C) |
| `COVEN` | `legion` | legion's coven label (target for axes B/C) |
| `API` | `0` | `1` -> enable axis B (API load) |
| `VOYAGE` | `0` | `1` -> enable axis C (single Voyage) |
| `API_DURATION` | `15s` | duration of the API run (axis B) |
| `KEEPER_ENDPOINT` | `127.0.0.1:9443` | Keeper event_stream (mTLS) |
| `OPENAPI` | `http://127.0.0.1:8080` | OpenAPI listener (axes B/C) |
| `METRICS` | `http://127.0.0.1:9090` | Keeper `/metrics` (empty -> don't scrape) |
| `PG` | `postgres://keeper:keeper@localhost:5434/keeper?sslmode=disable` | PG DSN (setup/cleanup) |
| `VAULT` | `http://127.0.0.1:8200` | Vault (dev-PKI for batch-issuing leaf certs) |
| `STRESS_CA` | `/tmp/keeper-dev/tls/vault-ca.crt` | root CA of the Keeper server cert (PEM) |

Other soul-legion flags (sid-prefix, open-concurrency, issue-concurrency,
voyage-module, etc.) have sensible defaults in the code - for direct invocation see
`go run ./cmd/soul-legion --help` in `tests/load/`.
