# Known limitations - what is not included in beta

The closed small beta is designed for a few operators and a fleet of up to hundreds of hosts. This document honestly lists what is **not** in the beta or what works with limitations - so that the beta tester does not silently rest on this and mistake the absence of a feature for a bug.

Each item is with a link to the canon (ADR / runbook), which describes "how it will be" or "why it's postponed." The design is captured in ADR before the code; "postponed" means "the decision has been made, the code is not in beta."

## Cloud-provisioning - NOT in beta

Beta works with **existing hosts**: the operator himself picks up the VM/hardware and onboards Soul ([getting-started.md → Step 6](getting-started.md#step-6-onboard-one-soul)). Dynamic creation of VMs from Soul Stack is not included in the beta.

- **Provider and Profile** (cloud account + VM template) - the concept is there, stored in Postgres, but **REST routes `POST /v1/providers` / `POST /v1/profiles` are postponed** - cloud-CRUD is not implemented ([keeper/cloud.md → Provider and Profile](keeper/cloud.md), [operator-api.md → Cloud](keeper/operator-api.md)). Provider/Profile management via REST / MCP / UI in beta **no**.
- Script step `core.cloud.provisioned` (`on: keeper`, calling the CloudDriver plugin) was designed ([ADR-017](adr/0017-keeper-side-core.md)), but without a configured Provider/Profile it cannot be used in beta.

If you want dynamic provisioning, this is post-beta. Now: Create a host outside of Soul Stack, then `POST /v1/souls` + `soul init`.

## MCP does not cover all domains

The primary operator interface is REST (OpenAPI) and MCP. But MCP symmetry with REST is incomplete - some domains are accessible **only through REST/UI**:

- **Cadence** (schedules of regular runs, [ADR-046](adr/0046-cadence.md)) - **MCP tools are not available** ([operator-api/cadences.md](keeper/operator-api/cadences.md)). Creating/changing schedules - only via REST `/v1/cadences*` or Web-UI.
- **Audit-read** (`GET /v1/audit`) - MCP symmetry deferred ([operator-api/audit.md](keeper/operator-api/audit.md)).
- **Choir** (host topology inside incarnation) and **Module-catalog** (`/v1/modules`) are REST-only by design ([operator-api.md → Choir / Module-catalog](keeper/operator-api.md)).

Some MCP-tools are set up as **stub**: they are marked `status=stub` in the manifest and return an honest `not_implemented` (they don't crash, they don't pretend to have worked). In beta stub:

- **`keeper.soul.list`** — `not_implemented`. For the Souls list in beta, use REST `GET /v1/souls` (filters `coven` / `status` / `transport` + pagination). Full MCP coverage - post-beta (M2).
- **`keeper.push.cleanup`** — `not_implemented`. There is no REST analogue in beta (push is performed via `keeper.push.apply`). Post-beta delayed.

If you automate via MCP, check [keeper/mcp-tools.md](keeper/mcp-tools.md): the actually installed MCP tools are listed there. The absence of a tool or its `stub` status is not a bug, but a beta coverage limit.

## Audit-scaling - designed for small beta

`audit_log` is the main consumer of Postgres volume when the fleet grows: at the target scale of 100k VM, the volume of runs depends on the INSERT-rate and the table size. For **small beta** (up to hundreds of hosts) this is not a problem, standard retention is enough (`purge_audit_old`, default 365 days, [operations/infra.md → Retention](operations/infra.md)).

What is postponed until post-beta (not needed for a small fleet):

- **Partitioning `audit_log`** by `created_at` (declarative partitioning / BRIN) - extension, not breaking ([ADR-022](adr/0022-audit-pipeline.md)).
- **Pluggable audit-sink (Kafka upload)** - **designed, not implemented in beta** ([ADR-059](adr/0059-audit-sink-pluggable.md), proposed / deferred). At the target scale, backend audit upload becomes selectable (`audit.sink: pg | kafka | off`, default `pg`); Kafka-sink (at-least-once `acks=all`, fail-closed, downstream deduced by `audit_id`) removes the PG-write load, remains strictly optional (the mandatory PG+Redis+Vault loop does not change, [ADR-053](adr/0053-dependency-tiers.md)). **Replaces the Redis-Stream-buffering option** (Kafka covers the same write-throughput axis more fully; Redis is a hot layer, not a long-term audit buffer). Requires dependency decoupling before implementation: `changed_tasks`/`GET /v1/audit` today derives data from `audit_log` into PG ([ADR-059](adr/0059-audit-sink-pluggable.md) open question).
- **Hot-cold / batched-INSERT** audit for large fleets - backlog of the next releases. **batched-INSERT remains** a cheaper alternative on the write-throughput axis (batch-flush PG-sink without new infrastructure), and is not supplanted by Kafka-sink.

If you are planning a fleet of thousands+ hosts, this is outside the beta profile; keep an eye on the size of `audit_log` and `apply_runs` ([operations/infra.md → Table size](operations/infra.md)).

## Other beta boundaries

### Supply-chain: image signing delayed

`make sign` - documented stub (prints the reason and succeeds). The real cosign/sigstore signing of Docker images requires registry + OIDC-identity from CI, which the local repository does not have ([deploy/README.md → Signing images](../deploy/README.md)). SBOM (`make sbom`) works.

### External pentest - not carried out (internal gate is sufficient for beta)

Independent external pentest was **not running** at the time of beta. The limit of guarantees rests on the internal security gate: deep information security audit 2026-06-12 (0 critical/high), threat-model, clean `govulncheck` for all modules and security-revalidation of the OpenAPI pivot (PASS) - composition and justification in [security/threat-model.md → External audit status / pentest](security/threat-model.md). Decision from 2026-06-15: for public beta this is enough; external independent pentest planned post-beta/pre-GA.

### Operator Identity: JWT only

The Archon credential form in beta is **JWT** (HS256, signing-key from Vault). mTLS-cert form and transit signature JWT - post-MVP, extension via `auth_method` enum without breaking changes ([ADR-014](adr/0014-operator-identity.md), [operations/bootstrap-rbac.md → Machine-identity](operations/bootstrap-rbac.md#machine-identity-ci--scripts)).

**Immediate recall of all living JWTs** is missing in beta: after `revoke` Archon, his active tokens work until `exp`. Emergency revocation - only through signing-key rotation ([operations/bootstrap-rbac.md → Emergency revocation](operations/bootstrap-rbac.md)). Protection - short `ttl_default`.

### Push (agentless via SSH) - narrow profile

`keeper.push` (Destiny delivery over SSH without agent) works, but without host-CA / `ssh_providers` is a no-op ([ADR-053 → optional-with-degradation](adr/0053-dependency-tiers.md)). Beta profile - pull (daemon agent `soul`), use push only when SSH provider is configured.

### Served OpenAPI describes the full surface of the product

Some of the handles relate to optional domains, which are mounted only when the corresponding feature is enabled in the Keeper config (for example, push/SSH delivery - with `plugins.ssh_providers` + `push.host_ca_ref` configured; Sigil/sigil-keys - with allow-list plugins enabled). If the feature is not enabled on a specific instance, the handle will return `404 "no such endpoint"` - including when trying to call it from /docs (RapiDoc "Try It"). This is expected: spec = stable contract for the entire product, handle availability depends on the deployment configuration (pull-only installation without push/Sigil - standard mode). The authoritative list of optional/feature-gated domains is `pathAllowlist` in `keeper/internal/api/openapi_drift_test.go` (protected by the `TestFullSpec_CoversAllRoutes` guard test).

### Recovery of interrupted runs - disabled by default

`reclaim_apply_runs` (Reaper picks up runs stuck after a Keeper instance crash) **disabled** in the default config - enabled only after rolling out fencing-Soul + `acolytes>0` ([operations/deployment.md → keeper.yml](operations/deployment.md), [keeper/reaper.md](keeper/reaper.md)). For a small single-keeper beta this is not required; The operator restarts a frozen run manually.

### UI `/oracle/fires` - stub

The `/oracle/fires` page in the Web-UI is a placeholder with an explicit WIP message: backend `GET /v1/oracle/fires` is not implemented (the table `oracle_fires` already exists, the query phase is postponed). Viewing Decree triggers in beta - via **Audit Log** with filter `type=decree.fired`. Post-beta (Oracle query-phase) is delayed.

### Sentinel-Redis: without native master-discovery

`redis.addr` accepts one TCP address. Redis Sentinel with automatic master-discovery is not natively supported - rolling out via a TCP proxy to a dynamic master ([operations/infra.md → HA Redis](operations/infra.md#ha-redis)). Single-instance and Redis Cluster are supported. For small beta - single-instance + AOF.

### Semantic-breaking: `create_scenario` (empty NO MORE does not mean auto-`create`)

Semantics of the field `create_scenario` (`POST /v1/incarnations`, MCP `keeper.incarnation.create`) **changed**: empty `create_scenario` **NO LONGER means** auto-run reserved script `create` ([ADR-009 amendment 2026-06-29](adr/0009-scenario-dsl.md)). Now with an empty value: if the service offers create scripts (at least one `create: true`) → **422 `create_scenario_required`** (selection is required, valid names are listed); if none `create: true` → **bare-incarnation** (`StatusReady` without run, `created_scenario = NULL`). Wire circuit is additive (the field existed before), breaking only the meaning. **Affected** are external API/MCP clients that used `create` without `create_scenario` when calculating auto-`create` - they need to explicitly pass `create_scenario`.

## See also

- [getting-started.md](getting-started.md) - quickstart, onboarding path for an existing host.
- [operations/](operations/README.md) - prod-runbook (deployment / infra / scaling / disaster-recovery).
- [architecture.md → Open questions](architecture.md#open-questions) - forks not yet closed by ADR.
